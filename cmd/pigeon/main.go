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
	"os"
	"strings"
	"text/tabwriter"

	"github.com/PeterSR/claude-code-pigeon/internal/pigeon"
)

var version = "0.1.0"

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
  pigeon prune                   forget sessions whose process is gone
  pigeon monitor                 run the inbox monitor (used by the plugin)
  pigeon mcp                     run the MCP server (used by the plugin)
  pigeon version

Targets resolve as: exact session id, declared name, id prefix, cwd basename.`

func main() {
	if len(os.Args) < 2 {
		fmt.Println(usage)
		return
	}
	cmd, args := os.Args[1], os.Args[2:]

	var err error
	switch cmd {
	case "arm":
		err = pigeon.Arm(os.Stdout)
	case "install":
		err = pigeon.Install(version, os.Stdout)
	case "uninstall":
		fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
		purge := fs.Bool("purge", false, "also delete state and queued messages")
		_ = fs.Parse(args)
		err = pigeon.Uninstall(*purge, os.Stdout)
	case "ls", "list":
		err = cmdList(args)
	case "send":
		err = cmdSend(args)
	case "publish", "pub":
		err = cmdPublish(args)
	case "subscribe", "sub":
		err = cmdSubscribe(args)
	case "unsubscribe", "unsub":
		err = cmdUnsubscribe(args)
	case "topics":
		err = cmdTopics()
	case "whoami":
		err = cmdWhoami()
	case "name":
		err = cmdName(args)
	case "describe":
		err = cmdDescribe(args)
	case "prune":
		err = cmdPrune()
	case "monitor":
		err = pigeon.RunMonitor(os.Stdout, os.Stderr)
	case "mcp":
		err = pigeon.RunMCP(os.Stdin, os.Stdout, version)
	case "version", "--version", "-v":
		fmt.Printf("pigeon %s\n", version)
	case "help", "--help", "-h":
		fmt.Println(usage)
	default:
		err = fmt.Errorf("unknown command %q (try: pigeon help)", cmd)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "pigeon: %v\n", err)
		os.Exit(1)
	}
}

func cmdList(args []string) error {
	fs := flag.NewFlagSet("ls", flag.ExitOnError)
	all := fs.Bool("all", false, "include sessions whose process has exited")
	asJSON := fs.Bool("json", false, "machine-readable output")
	_ = fs.Parse(args)

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
		return printJSON(out)
	}
	if len(entries) == 0 {
		fmt.Println("no registered pigeon sessions")
		fmt.Println("(a session registers when its monitor arms at startup;")
		fmt.Println(" run `pigeon install` then restart Claude Code)")
		return nil
	}

	me := pigeon.CurrentSessionID()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "\tSESSION\tNAME\tSTATUS\tCWD\tDESCRIPTION")
	for _, e := range entries {
		mark := " "
		if e.SessionID == me {
			mark = "*"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			mark, pigeon.Short(e.SessionID), dash(e.Name), e.Status,
			abbrev(e.Cwd, 32), dash(truncate(e.Description, 40)))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	for _, e := range entries {
		if e.Status == pigeon.StatusDeaf {
			fmt.Printf("\nwarning: %s is running but its monitor is not listening.\n",
				e.Display())
			fmt.Println("messages queue on its spool, but only a monitor for the same session id")
			fmt.Println("will read them (claude --resume); a new session gets a new id.")
			break
		}
	}
	return nil
}

func cmdSend(args []string) error {
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
	fmt.Printf("sent -> %s (%s)\n", pigeon.Short(to.SessionID), to.Display())
	if msg.Payload != "" {
		fmt.Printf("body exceeded %d chars; full text at %s\n", pigeon.BodyBudget, msg.Payload)
	}
	if to.Status == pigeon.StatusDeaf {
		fmt.Fprintf(os.Stderr,
			"warning: %s has no listening monitor. The message is queued on its spool, but\n"+
				"only a monitor for the same session id will ever read it (claude --resume).\n"+
				"A brand-new session gets a new id and will not see it.\n",
			to.Display())
	}
	return nil
}

func cmdPublish(args []string) error {
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
	fmt.Printf("published to #%s (%d subscriber(s) besides you)\n", topic, n)
	if msg.Payload != "" {
		fmt.Printf("body exceeded %d chars; full text at %s\n", pigeon.BodyBudget, msg.Payload)
	}
	return nil
}

func cmdSubscribe(args []string) error {
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
	fmt.Printf("subscribed to #%s (takes effect within a second, no restart)\n", args[0])
	return nil
}

func cmdUnsubscribe(args []string) error {
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
	fmt.Printf("unsubscribed from #%s\n", args[0])
	return nil
}

func cmdTopics() error {
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
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "\tTOPIC\tSUBSCRIBERS")
	for _, t := range topics {
		mark := " "
		if mine[t.Name] {
			mark = "*"
		}
		fmt.Fprintf(w, "%s\t%s\t%d\n", mark, t.Name, t.Subscribers)
	}
	return w.Flush()
}

func cmdWhoami() error {
	sid := pigeon.CurrentSessionID()
	if sid == "" {
		fmt.Printf("not inside a Claude Code session; sending as %s\n", pigeon.ShellIdentity())
		return nil
	}
	e, err := pigeon.ReadEntry(sid)
	if err != nil {
		fmt.Printf("session:  %s\n", sid)
		fmt.Println("not registered -- is the pigeon plugin installed and the session restarted?")
		return nil
	}
	fmt.Printf("session:      %s\n", e.SessionID)
	fmt.Printf("name:         %s\n", dash(e.Name))
	fmt.Printf("description:  %s\n", dash(e.Description))
	fmt.Printf("cwd:          %s\n", e.Cwd)
	fmt.Printf("status:       %s\n", e.Status)
	fmt.Printf("topics:       %s\n", dash(strings.Join(e.Subscriptions, ", ")))
	fmt.Printf("inbox:        %s\n", pigeon.SpoolPath(e.SessionID))
	fmt.Printf("\nothers reach you with:  pigeon send %s \"...\"\n", e.Addr())
	return nil
}

func cmdName(args []string) error {
	e, err := ownEntry()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		fmt.Println(dash(e.Name))
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
	fmt.Printf("name set to %q -- others can now use: pigeon send %s \"...\"\n", name, name)
	return nil
}

func cmdDescribe(args []string) error {
	e, err := ownEntry()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		fmt.Println(dash(e.Description))
		return nil
	}
	desc := pigeon.Sanitize(strings.Join(args, " "))
	if err := pigeon.MutateEntry(e.SessionID, func(en *pigeon.Entry) error {
		en.Description = desc
		return nil
	}); err != nil {
		return err
	}
	fmt.Println("description updated")
	return nil
}

func cmdPrune() error {
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
	fmt.Printf("pruned %d dead session(s)\n", len(before)-len(after))
	return nil
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

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
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
