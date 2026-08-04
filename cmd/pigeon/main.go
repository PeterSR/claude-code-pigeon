// Command pigeon passes messages between live Claude Code sessions.
//
// One static binary is the whole product: it installs its own plugin, runs as
// that plugin's background monitor, serves MCP to the session it belongs to,
// and works as a plain CLI from any shell.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/PeterSR/claude-code-pigeon/internal/pigeon"
)

// Overridden at build time via -ldflags; see the Makefile and .goreleaser.yaml.
var (
	version = "dev"
	commit  = ""
	date    = ""
)

func versionString() string {
	out := "pigeon " + version
	if commit != "" {
		out += " (" + commit + ")"
	}
	if date != "" {
		out += " built " + date
	}
	return out
}

const usage = `pigeon -- message passing between live Claude Code sessions

  pigeon arm                     arm this session now, without the plugin
  pigeon install                 install the plugin (restart Claude Code after)
  pigeon uninstall [--purge]     remove the plugin
  pigeon ls [--all] [--json]     list registered sessions
  pigeon send <target> <text>    send a message to one session
  pigeon publish <topic> <text>  publish to a topic (everyone subscribed)
  pigeon subscribe <topic>       start receiving a topic in this session
  pigeon unsubscribe <topic>     stop receiving it
  pigeon listen [topic...]       receive messages in this shell (blocks)
  pigeon inbox [--all] [--peek]  read this session's mail with full text
  pigeon topics                  list topics and subscriber counts
  pigeon namespaces              list namespaces and their session counts
  pigeon namespace [<name>]      show or set the namespace this shell uses
  pigeon as [<name>]             show or set the identity this shell acts as
  pigeon whoami                  show this session's identity and address
  pigeon name [<name>]           declare this session's name (usable as address)
  pigeon describe [<text>]       declare what this session is working on
  pigeon doctor [--json]         check whether this session can receive mail
  pigeon weaverbird spec|value   status widgets for a weaverbird status line
  pigeon prune                   forget dead sessions and reclaim topic logs
  pigeon monitor                 run the inbox monitor (used by the plugin)
  pigeon mcp                     run the MCP server (used by the plugin)
  pigeon version

Targets resolve as: exact session id, declared name, id prefix, cwd basename.

Sessions are grouped into namespaces and only see their own. ls, send, publish,
topics and prune take -n/--namespace <ns>; ls, topics and prune also take
--all-namespaces. A topic written @name is machine-wide and reaches every
namespace; a plain name resolves inside one.

name and describe also take --template '{{.Dir}}-{{.Seq}}', rendered against
this session. See the README for every field and function.`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run is main with its edges injected, so the CLI surface -- argument parsing,
// output, exit codes -- is testable without spawning a process.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stdout, usage)
		return 0
	}
	cmd, rest := args[0], args[1:]

	var err error
	switch cmd {
	case "arm":
		err = pigeon.Arm(stdout)
	case "install":
		err = pigeon.Install(version, stdout)
	case "uninstall":
		err = cmdUninstall(rest, stdout, stderr)
	case "ls", "list":
		err = cmdList(rest, stdout, stderr)
	case "send":
		err = cmdSend(rest, stdout, stderr)
	case "publish", "pub":
		err = cmdPublish(rest, stdout, stderr)
	case "subscribe", "sub":
		err = cmdSubscribe(rest, stdout)
	case "unsubscribe", "unsub":
		err = cmdUnsubscribe(rest, stdout)
	case "listen":
		err = cmdListen(rest, stdout, stderr)
	case "inbox":
		err = cmdInbox(rest, stdout, stderr)
	case "topics":
		err = cmdTopics(rest, stdout, stderr)
	case "namespaces":
		err = cmdNamespaces(rest, stdout, stderr)
	case "namespace", "ns":
		err = cmdNamespace(rest, stdout, stderr)
	case "as":
		err = cmdAs(rest, stdout, stderr)
	case "whoami":
		err = cmdWhoami(stdout)
	case "name":
		err = cmdName(rest, stdout, stderr)
	case "describe":
		err = cmdDescribe(rest, stdout, stderr)
	case "doctor":
		err = cmdDoctor(rest, stdout, stderr)
	case "weaverbird":
		err = cmdWeaverbird(rest, stdin, stdout)
	case "prune":
		err = cmdPrune(rest, stdout, stderr)
	case "monitor":
		err = pigeon.RunMonitor(stdout, stderr)
	case "mcp":
		err = pigeon.RunMCP(stdin, stdout, version)
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, versionString())
	case "help", "--help", "-h":
		fmt.Fprintln(stdout, usage)
	default:
		err = fmt.Errorf("unknown command %q (try: pigeon help)", cmd)
	}

	if err != nil {
		fmt.Fprintf(stderr, "pigeon: %v\n", err)
		return 1
	}
	return 0
}

// flags builds a flag set that reports errors instead of calling os.Exit, so a
// bad flag surfaces as a normal error rather than killing the test binary.
func flags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// nsFlag adds -n and --namespace as two spellings of one value, so neither is
// a second setting that could disagree with the other.
func nsFlag(fs *flag.FlagSet, into *string) {
	fs.StringVar(into, "n", "", "namespace to act in (default: this session's)")
	fs.StringVar(into, "namespace", "", "same as -n")
}

// asFlag adds --as, the per-call spelling of the acting identity. Left empty it
// falls through to PIGEON_AS and then `pigeon as`; a real Claude Code session
// still outranks the ambient layers (see pigeon.ActingIdentity).
func asFlag(fs *flag.FlagSet, into *string) {
	fs.StringVar(into, "as", "", "act as the ephemeral inbox <name> (see: pigeon as)")
}

// checkAs rejects a --as value that is not a usable name up front, so a typo
// stamps nothing: the alternative is a message quietly sent as a plain shell.
func checkAs(name string) error {
	if strings.TrimSpace(name) == "" {
		return nil
	}
	return pigeon.ValidName(name)
}

// namespaceOf resolves a -n value, or this process's own namespace when the
// flag was not given. A bad name is refused rather than replaced: silently
// acting on "default" instead of the namespace someone typed is how a message
// reaches the wrong people.
func namespaceOf(name string) (pigeon.Namespace, error) {
	if strings.TrimSpace(name) == "" {
		return pigeon.CurrentNamespace(), nil
	}
	ns, err := pigeon.ParseNamespace(name)
	if err != nil {
		return ns, err
	}
	// Naming a namespace explicitly is the one way to reach into one that would
	// not be listed, so it is the one place the privacy rule has to hold. It
	// only refuses inside a Claude Code session: from your own terminal this is
	// the escape hatch.
	if err := pigeon.CheckNamespaceAccess(ns); err != nil {
		return ns, err
	}
	return ns, nil
}

// elsewhere counts the sessions this listing is deliberately not showing.
// Isolation you have forgotten about looks exactly like an empty machine, so
// the count is what makes the mechanism discoverable again.
func elsewhere(ns pigeon.Namespace) (sessions, spaces int) {
	all, err := pigeon.ListNamespaces()
	if err != nil {
		return 0, 0
	}
	for _, info := range all {
		if info.Name == ns.String() {
			continue
		}
		if n := info.Live + info.Deaf; n > 0 {
			sessions += n
			spaces++
		}
	}
	return sessions, spaces
}

func printElsewhere(w io.Writer, ns pigeon.Namespace) {
	if sessions, spaces := elsewhere(ns); sessions > 0 {
		fmt.Fprintf(w, "\n%d session(s) in %d other namespace(s) (--all-namespaces)\n", sessions, spaces)
	}
}

func cmdUninstall(args []string, w, stderr io.Writer) error {
	fs := flags("uninstall", stderr)
	purge := fs.Bool("purge", false, "also delete state and queued messages")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return pigeon.Uninstall(*purge, w)
}

func cmdList(args []string, w, stderr io.Writer) error {
	fs := flags("ls", stderr)
	all := fs.Bool("all", false, "include sessions whose process has exited")
	asJSON := fs.Bool("json", false, "machine-readable output")
	allNS := fs.Bool("all-namespaces", false, "list every namespace, not only this one")
	var nsName string
	nsFlag(fs, &nsName)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *allNS && strings.TrimSpace(nsName) != "" {
		return fmt.Errorf("give either --all-namespaces or -n, not both")
	}
	ns, err := namespaceOf(nsName)
	if err != nil {
		return err
	}

	var entries []*pigeon.Entry
	if *allNS {
		entries, err = pigeon.ListAllSessions(*all, false)
	} else {
		entries, err = ns.ListSessions(*all, false)
	}
	if err != nil {
		return err
	}
	if *asJSON {
		// Status is derived, not stored, so expose it explicitly for scripts.
		type view struct {
			*pigeon.Entry
			Status string `json:"status"`
			Addr   string `json:"addr"`
		}
		out := make([]view, 0, len(entries))
		for _, e := range entries {
			out = append(out, view{Entry: e, Status: string(e.Status), Addr: e.Addr()})
		}
		return printJSON(w, out)
	}
	if len(entries) == 0 {
		fmt.Fprintf(w, "no registered pigeon sessions in namespace %s\n", ns)
		fmt.Fprintln(w, "(a session registers when its monitor arms at startup;")
		fmt.Fprintln(w, " run `pigeon install` then restart Claude Code)")
		if !*allNS {
			printElsewhere(w, ns)
		}
		return nil
	}

	me := pigeon.CurrentSessionID()
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if *allNS {
		fmt.Fprintln(tw, "\tNAMESPACE\tSESSION\tNAME\tCLAUDE\tPID\tSTATUS\tCWD\tDESCRIPTION")
	} else {
		fmt.Fprintln(tw, "\tSESSION\tNAME\tCLAUDE\tPID\tSTATUS\tCWD\tDESCRIPTION")
	}
	for _, e := range entries {
		mark := " "
		if e.SessionID == me {
			mark = "*"
		}
		if *allNS {
			fmt.Fprintf(tw, "%s\t%s\t", mark, e.Namespace)
		} else {
			fmt.Fprintf(tw, "%s\t", mark)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			pigeon.Short(e.SessionID), dash(e.Name), claudeCol(e),
			pidCol(e.PID), e.Status,
			abbrev(e.Cwd, 32), dash(truncate(e.Description, 40)))
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	// Say plainly which row is the caller, and -- more useful -- say so when
	// none of them is.
	self := (*pigeon.Entry)(nil)
	for _, e := range entries {
		if e.SessionID == me {
			self = e
			break
		}
	}
	switch {
	case self != nil:
		fmt.Fprintf(w, "\n* this session, reachable as: pigeon send %s\n", self.Addr())
	case me != "":
		fmt.Fprintf(w, "\nthis session (%s) is not registered, so nothing can reach it.\n", pigeon.Short(me))
		fmt.Fprintln(w, "run `pigeon arm`, or install the plugin and restart Claude Code.")
	default:
		fmt.Fprintln(w, "\nnot inside a Claude Code session; anything you send is stamped")
		fmt.Fprintf(w, "%s and cannot be replied to.\n", pigeon.ShellIdentity())
	}

	for _, e := range entries {
		if e.Status == pigeon.StatusDeaf {
			fmt.Fprintf(w, "\nwarning: %s is running but its monitor is not listening.\n",
				e.Display())
			fmt.Fprintln(w, "messages queue on its spool, but only a monitor for the same session id")
			fmt.Fprintln(w, "will read them (claude --resume); a new session gets a new id.")
			break
		}
	}
	if !*allNS {
		printElsewhere(w, ns)
	}
	return nil
}

func cmdSend(args []string, w, stderr io.Writer) error {
	fs := flags("send", stderr)
	var nsName, asName, subject string
	nsFlag(fs, &nsName)
	asFlag(fs, &asName)
	fs.StringVar(&subject, "subject", "", "one-line subject, max 120 characters; the only part guaranteed to arrive")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := checkAs(asName); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) < 2 {
		return fmt.Errorf("usage: pigeon send [-n <namespace>] [--as <name>] [--subject <text>] <target> <text>")
	}
	if err := misplacedFlag(rest[1:]); err != nil {
		return err
	}
	target, text := rest[0], strings.Join(rest[1:], " ")

	// Sending across a namespace is allowed rather than blocked: anyone who can
	// write the state directory could append to that spool by hand, so refusing
	// would buy inconvenience and no isolation.
	ns, err := namespaceOf(nsName)
	if err != nil {
		return err
	}
	to, err := ns.ResolveTarget(target)
	if err != nil {
		return err
	}
	msg, err := ns.Send(to, pigeon.Draft{Text: text, Subject: subject}, pigeon.ActingSender(asName))
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "sent -> %s (%s) in %s\n", pigeon.Short(to.SessionID), to.Display(), ns)
	if msg.Payload != "" {
		fmt.Fprintf(w, "body exceeded %d chars; full text at %s\n", pigeon.BodyBudget, msg.Payload)
	}
	if to.Status == pigeon.StatusDeaf {
		fmt.Fprintf(stderr,
			"warning: %s has no listening monitor. The message is queued on its spool, but\n"+
				"only a monitor for the same session id will ever read it (claude --resume).\n"+
				"A brand-new session gets a new id and will not see it.\n",
			to.Display())
	}
	if nudge := pigeon.SubjectNudge(msg); nudge != "" {
		fmt.Fprintln(w, nudge)
	}
	return nil
}

// misplacedFlag catches the trap Go's flag package sets for a caller that
// writes the topic before the flags: parsing stops at the first positional
// argument, so `pigeon publish topic --subject x body` files "--subject" and
// its value away as message text. The subject then never arrives, the send
// reports success, and nothing anywhere says otherwise -- which is the exact
// class of silent failure this tool exists to make loud. Only names we
// actually define are rejected, so a message body may still legitimately begin
// with a dash.
func misplacedFlag(rest []string) error {
	for _, a := range rest {
		name := strings.TrimLeft(a, "-")
		if name == a {
			continue
		}
		if i := strings.IndexByte(name, '='); i >= 0 {
			name = name[:i]
		}
		switch name {
		case "subject", "n", "namespace", "as":
			return fmt.Errorf("%q came after a positional argument, so it was read as message text rather than as a flag; put flags before the target and the body", a)
		}
	}
	return nil
}

func cmdPublish(args []string, w, stderr io.Writer) error {
	fs := flags("publish", stderr)
	var nsName, asName, subject string
	nsFlag(fs, &nsName)
	asFlag(fs, &asName)
	fs.StringVar(&subject, "subject", "", "one-line subject, max 120 characters; the only part guaranteed to arrive")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := checkAs(asName); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) < 2 {
		return fmt.Errorf("usage: pigeon publish [-n <namespace>] [--as <name>] [--subject <text>] <topic> <text>")
	}
	if err := misplacedFlag(rest[1:]); err != nil {
		return err
	}
	topic, text := rest[0], strings.Join(rest[1:], " ")
	ns, err := namespaceOf(nsName)
	if err != nil {
		return err
	}
	msg, err := ns.Publish(topic, pigeon.Draft{Text: text, Subject: subject}, pigeon.ActingSender(asName))
	if err != nil {
		return err
	}
	live, deaf := ns.SubscriberBreakdown(msg.Topic, pigeon.CurrentSessionID())
	fmt.Fprintf(w, "published to %s (%d subscriber(s) besides you)\n",
		pigeon.TopicLabel(msg.Topic), live)
	if deaf > 0 {
		fmt.Fprintf(w, "NOTE: %d subscriber(s) are deaf -- running but not listening. "+
			"They will only see this if they resume under the same session id.\n", deaf)
	}
	if live == 0 {
		if deaf > 0 {
			fmt.Fprintln(w, "Nobody is listening right now. The message is on the log, but a "+
				"claim or a question sent to an empty topic protects nothing.")
		} else {
			fmt.Fprintln(w, "Nobody is listening right now, but the message is on the log for "+
				"anyone who subscribes later.")
		}
	}
	if msg.Payload != "" {
		fmt.Fprintf(w, "body exceeded %d chars; full text at %s\n", pigeon.BodyBudget, msg.Payload)
	}
	if nudge := pigeon.SubjectNudge(msg); nudge != "" {
		fmt.Fprintln(w, nudge)
	}
	return nil
}

func cmdSubscribe(args []string, w io.Writer) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: pigeon subscribe <topic>")
	}
	e, err := ownEntry()
	if err != nil {
		return err
	}
	if err := pigeon.Subscribe(e.SessionID, args[0]); err != nil {
		return err
	}
	fmt.Fprintf(w, "subscribed to %s (takes effect within a second, no restart)\n",
		pigeon.TopicLabel(args[0]))
	return nil
}

func cmdUnsubscribe(args []string, w io.Writer) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: pigeon unsubscribe <topic>")
	}
	e, err := ownEntry()
	if err != nil {
		return err
	}
	if err := pigeon.Unsubscribe(e.SessionID, args[0]); err != nil {
		return err
	}
	fmt.Fprintf(w, "unsubscribed from %s\n", pigeon.TopicLabel(args[0]))
	return nil
}

// cmdListen is the receive half of pigeon for a plain shell. With no identity it
// is an anonymous tail of the named topics; with one (--as / PIGEON_AS /
// `pigeon as`) it opens a visible ephemeral inbox others can address directly.
func cmdListen(args []string, stdout, stderr io.Writer) error {
	fs := flags("listen", stderr)
	var nsName, asName string
	nsFlag(fs, &nsName)
	asFlag(fs, &asName)
	asJSON := fs.Bool("json", false, "one JSON object per line (default when stdout is not a terminal)")
	plain := fs.Bool("plain", false, "human-readable lines (default at a terminal)")
	replay := fs.Bool("replay", false, "deliver messages already in the logs, not only new ones")
	count := fs.Int("count", 0, "stop after receiving this many messages")
	timeout := fs.Duration("timeout", 0, "stop after this long, e.g. 30s or 5m")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *asJSON && *plain {
		return fmt.Errorf("give either --json or --plain, not both")
	}
	if err := checkAs(asName); err != nil {
		return err
	}
	ns, err := namespaceOf(nsName)
	if err != nil {
		return err
	}
	// The inbox identity is ephemeral-only: --as, then PIGEON_AS, then the
	// standing `pigeon as`. A resolved name opens a visible inbox; none leaves an
	// anonymous tail, which needs at least one topic (checked by Listen).
	name, _ := pigeon.ActingName(asName)
	return pigeon.Listen(pigeon.ListenOptions{
		Namespace: ns,
		As:        name,
		Topics:    fs.Args(),
		JSON:      *asJSON,
		Plain:     *plain,
		Replay:    *replay,
		Count:     *count,
		Timeout:   *timeout,
		TTY:       isTerminal(stdout),
	}, stdout, stderr)
}

// cmdInbox is the CLI twin of the MCP inbox tool: same query knobs, same
// renderer, so a human at a terminal and a model in the same session see
// identical text.
func cmdInbox(args []string, w, stderr io.Writer) error {
	fs := flags("inbox", stderr)
	limit := fs.Int("limit", 0, "how many messages (default 10, max 50)")
	all := fs.Bool("all", false, "include messages already read, not only new ones")
	peek := fs.Bool("peek", false, "do not mark returned messages as read")
	subjects := fs.Bool("subjects", false, "print only subject lines, not full bodies")
	var topic string
	fs.StringVar(&topic, "topic", "", "only this topic")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// This command is session-bound by definition -- it reads THIS session's
	// mail -- so a plain shell gets a message that says that, rather than the
	// raw "not inside a Claude Code session" Self returns for every caller.
	if pigeon.CurrentSessionID() == "" {
		return fmt.Errorf("inbox reads this session's own mail, and this shell is not inside a Claude Code session")
	}
	ns, e, err := pigeon.Self()
	if err != nil {
		return fmt.Errorf("this session is not registered with pigeon, so it has no inbox to read " +
			"(install the plugin and restart, or run `pigeon arm`)")
	}

	wantDetail := ""
	if *subjects {
		wantDetail = "subject"
	}
	detail, err := pigeon.ResolveInboxDetail(wantDetail)
	if err != nil {
		return err
	}
	unreadOnly := !*all
	items, err := ns.ReadInbox(e.SessionID, pigeon.InboxQuery{
		Limit:      *limit,
		UnreadOnly: unreadOnly,
		Topic:      topic,
		MarkRead:   !*peek,
	})
	if err != nil {
		return err
	}
	fmt.Fprintln(w, pigeon.RenderInbox(items, unreadOnly, detail, "--all"))
	return nil
}

func cmdTopics(args []string, w, stderr io.Writer) error {
	fs := flags("topics", stderr)
	allNS := fs.Bool("all-namespaces", false, "list every namespace's topics, not only this one's")
	var nsName string
	nsFlag(fs, &nsName)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *allNS && strings.TrimSpace(nsName) != "" {
		return fmt.Errorf("give either --all-namespaces or -n, not both")
	}
	ns, err := namespaceOf(nsName)
	if err != nil {
		return err
	}

	// Only mark rows when the listing is this session's own namespace. Reading
	// our subscriptions and applying them to another namespace's topic names
	// starred logs this session does not read and is not subscribed to, purely
	// because the names matched.
	mine := map[string]bool{}
	if own, e, err := pigeon.Self(); err == nil && ns.Is(own) {
		for _, t := range e.Subscriptions {
			mine[t] = true
		}
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	// A row per topic, marked when this session subscribes. A global topic is
	// listed once with no namespace of its own, because it does not have one.
	row := func(space string, t pigeon.TopicInfo) {
		mark := " "
		if mine[t.Name] {
			mark = "*"
		}
		if t.Global {
			space = "-"
		}
		if *allNS {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d\n", mark, space, t.Name, t.Subscribers)
		} else {
			fmt.Fprintf(tw, "%s\t%s\t%d\n", mark, t.Name, t.Subscribers)
		}
	}

	if !*allNS {
		fmt.Fprintln(tw, "\tTOPIC\tSUBSCRIBERS")
		topics, err := ns.ListTopics()
		if err != nil {
			return err
		}
		for _, t := range topics {
			row(ns.String(), t)
		}
		return tw.Flush()
	}

	fmt.Fprintln(tw, "\tNAMESPACE\tTOPIC\tSUBSCRIBERS")
	spaces, err := pigeon.ListNamespaces()
	if err != nil {
		return err
	}
	seenGlobal := map[string]bool{}
	for _, info := range spaces {
		space, err := pigeon.ParseNamespace(info.Name)
		if err != nil {
			continue
		}
		topics, err := space.ListTopics()
		if err != nil {
			continue
		}
		for _, t := range topics {
			if t.Global {
				if seenGlobal[t.Name] {
					continue
				}
				seenGlobal[t.Name] = true
			}
			row(info.Name, t)
		}
	}
	return tw.Flush()
}

func cmdNamespaces(args []string, w, stderr io.Writer) error {
	fs := flags("namespaces", stderr)
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	spaces, err := pigeon.ListNamespaces()
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(w, spaces)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "\tNAMESPACE\tLIVE\tDEAF")
	for _, info := range spaces {
		mark := " "
		if info.Current {
			mark = "*"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\n", mark, info.Name, info.Live, info.Deaf)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(w, "\n* the namespace this shell uses (pigeon namespace <name> to change it)")
	return nil
}

// cmdNamespace is get-or-set, like `pigeon name`. Setting it records a
// preference for shell invocations; it does not move a running session, whose
// namespace was fixed when its monitor armed and whose lock and topics all live
// in that namespace's directory.
func cmdNamespace(args []string, w, stderr io.Writer) error {
	fs := flags("namespace", stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		ns, origin := pigeon.ResolveNamespace()
		// The name alone on stdout so `$(pigeon namespace)` is usable; where it
		// came from on stderr, because a namespace you did not expect is the
		// only interesting thing this command can tell you.
		fmt.Fprintln(w, ns)
		fmt.Fprintf(stderr, "(from %s)\n", origin)
		return nil
	}
	if len(rest) != 1 {
		return fmt.Errorf("usage: pigeon namespace [<name>]")
	}
	ns, err := pigeon.ParseNamespace(rest[0])
	if err != nil {
		return err
	}
	if err := pigeon.SetCLINamespace(ns); err != nil {
		return err
	}
	fmt.Fprintf(w, "namespace set to %q for shell invocations\n", ns)

	// Setting a preference that something already outranks is a silent no-op
	// otherwise, and the user would go on wondering why ls looks the same.
	if effective, origin := pigeon.ResolveNamespace(); !effective.Is(ns) {
		fmt.Fprintf(stderr, "note: %s still applies here, from %s\n", effective, origin)
	}
	fmt.Fprintln(w, "running sessions keep the namespace they armed with; restart one to move it")
	return nil
}

// cmdAs is get-or-set for the standing shell identity, mirroring cmdNamespace. A
// resolved identity gives shell sends and publishes a real reply address --
// though only while an inbox of that name is actually listening.
func cmdAs(args []string, w, stderr io.Writer) error {
	fs := flags("as", stderr)
	clearIt := fs.Bool("clear", false, "forget the standing identity")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()

	if *clearIt {
		if len(rest) > 0 {
			return fmt.Errorf("give either a name or --clear, not both")
		}
		if err := pigeon.SetCLIIdentity(""); err != nil {
			return err
		}
		fmt.Fprintln(w, "standing identity cleared; shell posts are stamped as a plain shell again")
		return nil
	}

	if len(rest) == 0 {
		name, origin := pigeon.ActingName("")
		if name == "" {
			// A dash on stdout keeps `$(pigeon as)` usable; the why on stderr.
			fmt.Fprintln(w, "-")
			fmt.Fprintln(stderr, "(no acting identity; posts are stamped as a plain shell with no reply address)")
			return nil
		}
		fmt.Fprintln(w, name)
		fmt.Fprintf(stderr, "(from %s)\n", origin)
		// The reply address is only real while an inbox is holding it open; say so
		// when nothing is, rather than let a standing preference look effective.
		if pigeon.ActingSender("").Kind != "session" {
			fmt.Fprintf(stderr, "note: no inbox named %q is listening, so posts still go out as a plain shell.\n", name)
			fmt.Fprintln(stderr, "open it with: pigeon listen")
		}
		return nil
	}
	if len(rest) != 1 {
		return fmt.Errorf("usage: pigeon as [<name>]")
	}
	name := rest[0]
	if err := pigeon.ValidName(name); err != nil {
		return err
	}
	if err := pigeon.SetCLIIdentity(name); err != nil {
		return err
	}
	fmt.Fprintf(w, "acting identity set to %q for shell invocations\n", name)
	fmt.Fprintln(w, "open the inbox with: pigeon listen")
	// Setting a preference that something already outranks is otherwise silent.
	if eff, origin := pigeon.ActingName(""); eff != name {
		fmt.Fprintf(stderr, "note: %q still applies here, from %s\n", eff, origin)
	}
	return nil
}

func cmdWhoami(w io.Writer) error {
	sid := pigeon.CurrentSessionID()
	if sid == "" {
		// Outside a real session, a shell may still be acting as an ephemeral
		// inbox. ActingSender only returns a session stamp when that inbox is
		// actually listening, so its Kind is the honest test of "can I be replied
		// to right now".
		name, origin := pigeon.ActingName("")
		s := pigeon.ActingSender("")
		if s.Kind == "session" {
			fmt.Fprintf(w, "acting as %q (from %s)\n", s.Name, origin)
			fmt.Fprintf(w, "others reach you at:  pigeon send %s\n", s.Addr())
			return nil
		}
		if name != "" {
			fmt.Fprintf(w, "acting identity %q is set (from %s), but no inbox of that name is\n", name, origin)
			fmt.Fprintf(w, "listening, so posts go out as %s with no reply address.\n", pigeon.ShellIdentity())
			fmt.Fprintln(w, "open the inbox with: pigeon listen")
			return nil
		}
		fmt.Fprintf(w, "not inside a Claude Code session; sending as %s\n", pigeon.ShellIdentity())
		return nil
	}
	_, e, err := pigeon.Self()
	if err != nil {
		fmt.Fprintf(w, "session:  %s\n", sid)
		fmt.Fprintln(w, "not registered -- is the pigeon plugin installed and the session restarted?")
		return nil
	}
	fmt.Fprintf(w, "session:      %s\n", e.SessionID)
	fmt.Fprintf(w, "namespace:    %s\n", e.Namespace)
	fmt.Fprintf(w, "pid:          %s\n", pidCol(e.PID))
	fmt.Fprintf(w, "name:         %s\n", dash(e.Name))
	fmt.Fprintf(w, "claude name:  %s\n", claudeNameCol(e))
	fmt.Fprintf(w, "description:  %s\n", dash(e.Description))
	fmt.Fprintf(w, "cwd:          %s\n", e.Cwd)
	fmt.Fprintf(w, "status:       %s\n", e.Status)
	fmt.Fprintf(w, "topics:       %s\n", dash(strings.Join(e.Subscriptions, ", ")))
	fmt.Fprintf(w, "inbox:        %s\n", pigeon.SpoolPath(e.SessionID))
	if e.Private {
		// The blank cwd and description above are a deliberate policy, not a
		// failure to register properly. Say which.
		fmt.Fprintln(w, "private:      this project publishes no cwd or description")
	}
	fmt.Fprintf(w, "\nothers reach you with:  pigeon send %s \"...\"\n", e.Addr())
	return nil
}

func cmdName(args []string, w, stderr io.Writer) error {
	fs := flags("name", stderr)
	tmpl := fs.String("template", "", "render the name from a Go text/template (see README)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()

	e, err := ownEntry()
	if err != nil {
		return err
	}
	if *tmpl == "" && len(rest) == 0 {
		fmt.Fprintln(w, dash(e.Name))
		return nil
	}

	name := strings.TrimSpace(strings.Join(rest, " "))
	if *tmpl != "" {
		if name != "" {
			return fmt.Errorf("give either a name or --template, not both")
		}
		// Rendered names are validated, never repaired: a template that
		// produces something unusable must not hand this session an address
		// nobody declared.
		if name, err = pigeon.RenderName(*tmpl, e.SessionID, pigeon.CurrentCwd()); err != nil {
			return fmt.Errorf("--template: %w", err)
		}
	}
	if err := pigeon.ValidName(name); err != nil {
		return err
	}
	if pigeon.NameTaken(name, e.SessionID) {
		// Which name collided is the useful part when a template produced it,
		// since the template itself is not what is taken.
		if *tmpl != "" {
			return fmt.Errorf("the template rendered %q, which another live session already uses", name)
		}
		return fmt.Errorf("another live session already uses the name %q", name)
	}
	if err := pigeon.MutateEntry(e.SessionID, func(en *pigeon.Entry) error {
		en.Name = name
		return nil
	}); err != nil {
		return err
	}
	fmt.Fprintf(w, "name set to %q -- others can now use: pigeon send %s \"...\"\n", name, name)
	return nil
}

func cmdDescribe(args []string, w, stderr io.Writer) error {
	fs := flags("describe", stderr)
	tmpl := fs.String("template", "", "render the description from a Go text/template (see README)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()

	e, err := ownEntry()
	if err != nil {
		return err
	}
	if *tmpl == "" && len(rest) == 0 {
		fmt.Fprintln(w, dash(e.Description))
		return nil
	}

	desc := pigeon.Sanitize(strings.Join(rest, " "))
	if *tmpl != "" {
		if desc != "" {
			return fmt.Errorf("give either a description or --template, not both")
		}
		if desc, err = pigeon.RenderDescription(*tmpl, e.SessionID, pigeon.CurrentCwd()); err != nil {
			return fmt.Errorf("--template: %w", err)
		}
	}
	if err := pigeon.MutateEntry(e.SessionID, func(en *pigeon.Entry) error {
		en.Description = desc
		return nil
	}); err != nil {
		return err
	}
	fmt.Fprintln(w, "description updated")
	if e.Private {
		// Saying nothing would leave you believing peers can see it.
		fmt.Fprintln(w, "this project is marked private, so the description is not published to other sessions")
	}
	return nil
}

func cmdDoctor(args []string, w, stderr io.Writer) error {
	fs := flags("doctor", stderr)
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return pigeon.Doctor(w, *asJSON)
}

func cmdPrune(args []string, w, stderr io.Writer) error {
	fs := flags("prune", stderr)
	allNS := fs.Bool("all-namespaces", false, "sweep every namespace, not only this one")
	var nsName string
	nsFlag(fs, &nsName)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *allNS && strings.TrimSpace(nsName) != "" {
		return fmt.Errorf("give either --all-namespaces or -n, not both")
	}
	ns, err := namespaceOf(nsName)
	if err != nil {
		return err
	}

	// A session whose project config changed namespace leaves its old entry
	// behind, in a namespace nothing else looks at. --all-namespaces is what
	// clears that.
	spaces := []pigeon.Namespace{ns}
	if *allNS {
		all, err := pigeon.ListNamespaces()
		if err != nil {
			return err
		}
		spaces = spaces[:0]
		for _, info := range all {
			if got, err := pigeon.ParseNamespace(info.Name); err == nil {
				spaces = append(spaces, got)
			}
		}
	}

	dead, orphans := 0, 0
	var res pigeon.PruneResult
	for _, space := range spaces {
		before, err := space.ListSessions(true, false)
		if err != nil {
			return err
		}
		if _, err := space.ListSessions(true, true); err != nil {
			return err
		}
		after, err := space.ListSessions(true, false)
		if err != nil {
			return err
		}
		dead += len(before) - len(after)
		orphans += space.ReconcileOrphans()

		// Topic logs are append-only, so reclaim the prefix every live subscriber
		// has already read, and drop logs nobody subscribes to.
		got, err := space.PruneTopics()
		if err != nil {
			return err
		}
		res.Add(got)
	}
	// The global logs are swept once, counting subscribers in every namespace:
	// cutting a prefix that only a session next door has yet to read would drop
	// that session's mail.
	shared, err := pigeon.PruneSharedTopics()
	if err != nil {
		return err
	}
	res.Add(shared)

	fmt.Fprintf(w, "pruned %d dead session(s)\n", dead)
	fmt.Fprintf(w, "removed %d orphaned state file(s)\n", orphans)
	fmt.Fprintf(w, "removed %d unsubscribed topic log(s), compacted %d, "+
		"reclaimed %d payload file(s), freed %s\n",
		res.TopicsRemoved, res.TopicsCompacted, res.PayloadsRemoved, humanBytes(res.BytesReclaimed))
	if *allNS {
		fmt.Fprintf(w, "swept %d namespace(s)\n", len(spaces))
	}
	return nil
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func ownEntry() (*pigeon.Entry, error) {
	if pigeon.CurrentSessionID() == "" {
		return nil, fmt.Errorf("not inside a Claude Code session")
	}
	_, e, err := pigeon.Self()
	if err != nil {
		return nil, fmt.Errorf("this session is not registered (install the plugin and restart)")
	}
	return e, nil
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// pidCol renders a pid for the ls table, or a dash when there is none.
func pidCol(pid int) string {
	if pid <= 0 {
		return "-"
	}
	return fmt.Sprintf("%d", pid)
}

// claudeCol renders the CLAUDE column of `pigeon ls`. A shell inbox is not a
// Claude Code session and has no such name, so it is labelled as what it is
// rather than shown as a blank waiting to be filled.
func claudeCol(e *pigeon.Entry) string {
	if e.Ephemeral {
		return "shell"
	}
	return dash(truncate(e.ClaudeName, 24))
}

// isTerminal reports whether w is a terminal, so `pigeon listen` can default to
// NDJSON for a pipe and the human line for a person.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// claudeNameCol renders Claude Code's own session name for whoami, noting how it
// was arrived at so a mostly-cosmetic "derived" name is not read as a chosen one.
func claudeNameCol(e *pigeon.Entry) string {
	if strings.TrimSpace(e.ClaudeName) == "" {
		return "-"
	}
	if e.ClaudeNameSource != "" {
		return fmt.Sprintf("%s (%s)", e.ClaudeName, e.ClaudeNameSource)
	}
	return e.ClaudeName
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func abbrev(p string, n int) string {
	r := []rune(p)
	if len(r) <= n {
		return p
	}
	return "…" + string(r[len(r)-n+1:])
}
