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
  pigeon topics                  list topics and subscriber counts
  pigeon whoami                  show this session's identity and address
  pigeon name <name>             declare this session's name (usable as address)
  pigeon describe <text>         declare what this session is working on
  pigeon doctor [--json]         check whether this session can receive mail
  pigeon statusline [--plain]    one-line status for a Claude Code statusline
  pigeon prune                   forget dead sessions and reclaim topic logs
  pigeon monitor                 run the inbox monitor (used by the plugin)
  pigeon mcp                     run the MCP server (used by the plugin)
  pigeon version

Targets resolve as: exact session id, declared name, id prefix, cwd basename.`

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
		err = cmdPublish(rest, stdout)
	case "subscribe", "sub":
		err = cmdSubscribe(rest, stdout)
	case "unsubscribe", "unsub":
		err = cmdUnsubscribe(rest, stdout)
	case "topics":
		err = cmdTopics(stdout)
	case "whoami":
		err = cmdWhoami(stdout)
	case "name":
		err = cmdName(rest, stdout)
	case "describe":
		err = cmdDescribe(rest, stdout)
	case "doctor":
		err = cmdDoctor(rest, stdout, stderr)
	case "statusline":
		err = cmdStatusline(rest, stdin, stdout, stderr)
	case "prune":
		err = cmdPrune(stdout)
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
	if err := fs.Parse(args); err != nil {
		return err
	}

	entries, err := pigeon.ListSessions(*all, false)
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
		fmt.Fprintln(w, "no registered pigeon sessions")
		fmt.Fprintln(w, "(a session registers when its monitor arms at startup;")
		fmt.Fprintln(w, " run `pigeon install` then restart Claude Code)")
		return nil
	}

	me := pigeon.CurrentSessionID()
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "\tSESSION\tNAME\tSTATUS\tCWD\tDESCRIPTION")
	for _, e := range entries {
		mark := " "
		if e.SessionID == me {
			mark = "*"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			mark, pigeon.Short(e.SessionID), dash(e.Name), e.Status,
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
	return nil
}

func cmdSend(args []string, w, stderr io.Writer) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: pigeon send <target> <text>")
	}
	target, text := args[0], strings.Join(args[1:], " ")

	to, err := pigeon.ResolveTarget(target)
	if err != nil {
		return err
	}
	msg, err := pigeon.Send(to, text, pigeon.CurrentSender(), "")
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "sent -> %s (%s)\n", pigeon.Short(to.SessionID), to.Display())
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
	return nil
}

func cmdPublish(args []string, w io.Writer) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: pigeon publish <topic> <text>")
	}
	topic, text := args[0], strings.Join(args[1:], " ")
	msg, err := pigeon.Publish(topic, text, pigeon.CurrentSender())
	if err != nil {
		return err
	}
	n := 0
	if entries, e := pigeon.ListSessions(false, false); e == nil {
		me := pigeon.CurrentSessionID()
		for _, en := range entries {
			for _, t := range en.Subscriptions {
				if t == topic && en.SessionID != me {
					n++
				}
			}
		}
	}
	fmt.Fprintf(w, "published to #%s (%d subscriber(s) besides you)\n", topic, n)
	if msg.Payload != "" {
		fmt.Fprintf(w, "body exceeded %d chars; full text at %s\n", pigeon.BodyBudget, msg.Payload)
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
	fmt.Fprintf(w, "subscribed to #%s (takes effect within a second, no restart)\n", args[0])
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
	fmt.Fprintf(w, "unsubscribed from #%s\n", args[0])
	return nil
}

func cmdTopics(w io.Writer) error {
	topics, err := pigeon.ListTopics()
	if err != nil {
		return err
	}
	mine := map[string]bool{}
	if sid := pigeon.CurrentSessionID(); sid != "" {
		if e, err := pigeon.ReadEntry(sid); err == nil {
			for _, t := range e.Subscriptions {
				mine[t] = true
			}
		}
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "\tTOPIC\tSUBSCRIBERS")
	for _, t := range topics {
		mark := " "
		if mine[t.Name] {
			mark = "*"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\n", mark, t.Name, t.Subscribers)
	}
	return tw.Flush()
}

func cmdWhoami(w io.Writer) error {
	sid := pigeon.CurrentSessionID()
	if sid == "" {
		fmt.Fprintf(w, "not inside a Claude Code session; sending as %s\n", pigeon.ShellIdentity())
		return nil
	}
	e, err := pigeon.ReadEntry(sid)
	if err != nil {
		fmt.Fprintf(w, "session:  %s\n", sid)
		fmt.Fprintln(w, "not registered -- is the pigeon plugin installed and the session restarted?")
		return nil
	}
	fmt.Fprintf(w, "session:      %s\n", e.SessionID)
	fmt.Fprintf(w, "name:         %s\n", dash(e.Name))
	fmt.Fprintf(w, "description:  %s\n", dash(e.Description))
	fmt.Fprintf(w, "cwd:          %s\n", e.Cwd)
	fmt.Fprintf(w, "status:       %s\n", e.Status)
	fmt.Fprintf(w, "topics:       %s\n", dash(strings.Join(e.Subscriptions, ", ")))
	fmt.Fprintf(w, "inbox:        %s\n", pigeon.SpoolPath(e.SessionID))
	fmt.Fprintf(w, "\nothers reach you with:  pigeon send %s \"...\"\n", e.Addr())
	return nil
}

func cmdName(args []string, w io.Writer) error {
	e, err := ownEntry()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		fmt.Fprintln(w, dash(e.Name))
		return nil
	}
	name := strings.TrimSpace(strings.Join(args, " "))
	if err := pigeon.ValidName(name); err != nil {
		return err
	}
	if pigeon.NameTaken(name, e.SessionID) {
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

func cmdDescribe(args []string, w io.Writer) error {
	e, err := ownEntry()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		fmt.Fprintln(w, dash(e.Description))
		return nil
	}
	desc := pigeon.Sanitize(strings.Join(args, " "))
	if err := pigeon.MutateEntry(e.SessionID, func(en *pigeon.Entry) error {
		en.Description = desc
		return nil
	}); err != nil {
		return err
	}
	fmt.Fprintln(w, "description updated")
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

func cmdStatusline(args []string, stdin io.Reader, w, stderr io.Writer) error {
	fs := flags("statusline", stderr)
	plain := fs.Bool("plain", false, "no emoji or colour")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return pigeon.Statusline(statuslineStdin(stdin), w, pigeon.StatuslineOptions{Plain: *plain})
}

// statuslineStdin hands over stdin only when something is actually piped in.
// Claude Code pipes a JSON payload; a human running `pigeon statusline` at a
// prompt is not going to type one, and reading from the terminal would just
// hang with no indication why.
func statuslineStdin(stdin io.Reader) io.Reader {
	f, isFile := stdin.(*os.File)
	if !isFile {
		return stdin
	}
	fi, err := f.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice != 0 {
		return nil
	}
	return f
}

func cmdPrune(w io.Writer) error {
	before, err := pigeon.ListSessions(true, false)
	if err != nil {
		return err
	}
	if _, err := pigeon.ListSessions(true, true); err != nil {
		return err
	}
	after, err := pigeon.ListSessions(true, false)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "pruned %d dead session(s)\n", len(before)-len(after))

	// Topic logs are append-only, so reclaim the prefix every live subscriber
	// has already read, and drop logs nobody subscribes to.
	orphans := pigeon.ReconcileOrphans()
	res, err := pigeon.PruneTopics()
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "removed %d orphaned state file(s)\n", orphans)
	fmt.Fprintf(w, "removed %d unsubscribed topic log(s), compacted %d, reclaimed %s\n",
		res.TopicsRemoved, res.TopicsCompacted, humanBytes(res.BytesReclaimed))
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
	sid := pigeon.CurrentSessionID()
	if sid == "" {
		return nil, fmt.Errorf("not inside a Claude Code session")
	}
	e, err := pigeon.ReadEntry(sid)
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
