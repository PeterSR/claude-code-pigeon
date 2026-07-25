package pigeon

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// A minimal MCP server over stdio (JSON-RPC 2.0). Stdlib only.
//
// Claude Code injects CLAUDE_CODE_SESSION_ID into the MCP servers it spawns,
// so this process knows which session it belongs to without being told. That
// is what lets send_message stamp the sender automatically: a session never
// has to know or state its own address for replies to work.

const protocolVersion = "2025-06-18"

// maxRPCLine bounds one request. Requests carry a message body, which is
// already far smaller than this; the cap is here so a peer cannot make the
// scanner buffer without limit.
const maxRPCLine = 1 << 20

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type toolDef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
}

func obj(m map[string]any) map[string]any { return m }

// namespaceArg is the optional input shared by every tool that can address
// another namespace. Sessions are isolated per namespace, so leaving it out
// means "mine", which is what a session almost always wants.
func namespaceArg(what string) map[string]any {
	return obj(map[string]any{
		"type": "string",
		"description": "Namespace to " + what + ". Sessions in different namespaces cannot " +
			"see each other. Omit for this session's own namespace, which is nearly " +
			"always what you want; use list_namespaces to see the others.",
	})
}

func tools() []toolDef {
	return []toolDef{
		{
			Name: "list_sessions",
			Description: "List live Claude Code sessions reachable via pigeon, with their " +
				"name, description, working directory, namespace, status, pid, and Claude " +
				"Code's own session name (the one /status shows, shown as claude=). Status " +
				"'deaf' means the session is running but not listening, so messages to it " +
				"will not arrive. A row marked [shell inbox] is a plain shell holding an " +
				"inbox open with `pigeon listen`, not a Claude Code session, but is " +
				"addressable the same way. Only this session's namespace is listed unless one is named.",
			InputSchema: obj(map[string]any{
				"type": "object",
				"properties": obj(map[string]any{
					"namespace": namespaceArg("list"),
				}),
			}),
		},
		{
			Name: "send_message",
			Description: "Send a message to another Claude Code session. The recipient is " +
				"woken even if idle. Your own identity is attached automatically so they can " +
				"reply. Keep it under ~300 characters; longer text is written to a file and " +
				"the recipient gets a path instead. The target is resolved inside one " +
				"namespace, so a name that means nothing here may still exist elsewhere.",
			InputSchema: obj(map[string]any{
				"type": "object",
				"properties": obj(map[string]any{
					"to": obj(map[string]any{
						"type": "string",
						"description": "Target session: its declared name, session id (or a " +
							"prefix), its pid, or the basename of its working directory. " +
							"Claude Code's own session name is not a target; use the declared " +
							"name or pid from list_sessions.",
					}),
					"text": obj(map[string]any{
						"type":        "string",
						"description": "Message body.",
					}),
					"namespace": namespaceArg("resolve the target in"),
				}),
				"required": []string{"to", "text"},
			}),
		},
		{
			Name: "publish",
			Description: "Publish a message to a topic. Every session subscribed to that " +
				"topic is woken, even if idle. A plain topic name resolves inside one " +
				"namespace; a name starting with '@' is machine-wide and reaches every " +
				"namespace. Every session subscribes to 'all' and '@all' by default, so " +
				"'all' broadcasts to this namespace and '@all' to the whole machine.",
			InputSchema: obj(map[string]any{
				"type": "object",
				"properties": obj(map[string]any{
					"topic": obj(map[string]any{
						"type": "string",
						"description": "Topic name: lowercase letters, digits, dot, dash or " +
							"underscore. Prefix with '@' for the machine-wide topic of that name.",
					}),
					"text":      obj(map[string]any{"type": "string", "description": "Message body."}),
					"namespace": namespaceArg("publish into"),
				}),
				"required": []string{"topic", "text"},
			}),
		},
		{
			Name: "subscribe",
			Description: "Start receiving a topic in this session. Takes effect within about " +
				"a second, without restarting. Only messages published from now on arrive; " +
				"history is not replayed. Prefix the name with '@' for the machine-wide topic.",
			InputSchema: obj(map[string]any{
				"type": "object",
				"properties": obj(map[string]any{
					"topic": obj(map[string]any{
						"type":        "string",
						"description": "Topic to join. '@name' joins the machine-wide one.",
					}),
					"namespace": namespaceArg("find this session's registration in"),
				}),
				"required": []string{"topic"},
			}),
		},
		{
			Name:        "unsubscribe",
			Description: "Stop receiving a topic in this session. '@all' opts out of machine-wide broadcasts.",
			InputSchema: obj(map[string]any{
				"type": "object",
				"properties": obj(map[string]any{
					"topic": obj(map[string]any{
						"type":        "string",
						"description": "Topic to leave. '@name' leaves the machine-wide one.",
					}),
					"namespace": namespaceArg("find this session's registration in"),
				}),
				"required": []string{"topic"},
			}),
		},
		{
			Name: "list_topics",
			Description: "List the topics reachable from this session -- its namespace's own, " +
				"plus the machine-wide '@' ones -- and how many live sessions subscribe to each.",
			InputSchema: obj(map[string]any{"type": "object", "properties": obj(map[string]any{})}),
		},
		{
			Name: "list_namespaces",
			Description: "List every namespace on this machine with how many of its sessions " +
				"are live and how many are deaf. Namespaces are isolated: a session only sees, " +
				"resolves and broadcasts to its own, apart from '@' topics, which are " +
				"machine-wide. A session's namespace is fixed when it starts.",
			InputSchema: obj(map[string]any{"type": "object", "properties": obj(map[string]any{})}),
		},
		{
			Name: "whoami",
			Description: "Show this session's pigeon identity: session id, namespace, pid, " +
				"declared name, Claude Code's own session name, description, and the address " +
				"other sessions use to reach it.",
			InputSchema: obj(map[string]any{
				"type":       "object",
				"properties": obj(map[string]any{}),
			}),
		},
		{
			Name: "set_identity",
			Description: "Declare this session's name and/or description so other sessions " +
				"can find and address it. The name must be a single word and unique among " +
				"live sessions; it then works as an address. Instead of a literal, either " +
				"field can be given as a Go text/template rendered against this session: " +
				"{{.Dir}} the working directory's basename, {{.Cwd}} its full path, " +
				"{{.Branch}} the checked-out git branch, {{.Host}}, {{.User}}, {{.Session}}, " +
				"{{.Short}} the 8-character session id, {{.Seq}}, which counts this " +
				"session among those already in the same directory, and {{.ClaudeName}} " +
				"(alias {{.Label}}), Claude Code's own session name from /status. Functions: " +
				"snake, kebab, lower, upper, trunc N, default \"fallback\".",
			InputSchema: obj(map[string]any{
				"type": "object",
				"properties": obj(map[string]any{
					"name": obj(map[string]any{
						"type":        "string",
						"description": "Single-word name, usable as an address.",
					}),
					"description": obj(map[string]any{
						"type":        "string",
						"description": "What this session is working on.",
					}),
					"nameTemplate": obj(map[string]any{
						"type": "string",
						"description": "Template for the name, e.g. \"{{.Dir}}-{{.Seq}}\". " +
							"Rejected if it renders to something that is not a valid name. " +
							"Give this or 'name', not both.",
					}),
					"descriptionTemplate": obj(map[string]any{
						"type": "string",
						"description": "Template for the description, e.g. " +
							"\"{{.Dir}} on {{.Branch | default \\\"no branch\\\"}}\". " +
							"Give this or 'description', not both.",
					}),
				}),
			}),
		},
	}
}

// RunMCP serves MCP on stdio until the stream closes.
func RunMCP(in io.Reader, out io.Writer, version string) error {
	// One message per line, unmarshalled per line rather than streamed.
	//
	// A json.Decoder cannot skip a malformed message: it does not resync after
	// a syntax error, so the offending bytes stay buffered and the next Decode
	// fails on exactly the same input. Answering "parse error" and continuing,
	// which is what this loop used to do, therefore spins on the first bad
	// byte forever -- never serving another request, never exiting, and either
	// burning a core or blocking once the pipe to Claude Code fills. That is
	// strictly worse than returning the error, which at least ends cleanly.
	//
	// MCP stdio framing is newline-delimited, so reading a line at a time
	// gives the recovery the one property it was missing: the bad input is
	// consumed, and the next request is a fresh line.
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), maxRPCLine)
	enc := json.NewEncoder(out)

	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}

		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			// A single malformed message must not take the server down: the
			// session would lose every pigeon tool for the rest of its life.
			_ = enc.Encode(rpcResponse{
				JSONRPC: "2.0",
				Error:   &rpcError{Code: -32700, Message: "parse error"},
			})
			continue
		}

		result, rerr := dispatch(req, version)

		// Notifications (no id) get no response.
		if len(req.ID) == 0 {
			continue
		}
		resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
		if rerr != nil {
			resp.Error = rerr
		} else {
			resp.Result = result
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	// A line past the buffer cap is not something to recover from silently:
	// the caller is framing wrongly, and pretending the stream ended hides it.
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read request: %w", err)
	}
	return nil
}

func dispatch(req rpcRequest, version string) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		return obj(map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    obj(map[string]any{"tools": obj(map[string]any{})}),
			"serverInfo":      obj(map[string]any{"name": "pigeon", "version": version}),
		}), nil

	case "tools/list":
		return obj(map[string]any{"tools": tools()}), nil

	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		text, err := callTool(p.Name, p.Arguments)
		if err != nil {
			// Tool errors are results, not protocol errors, so the model sees them.
			return toolResult(fmt.Sprintf("error: %v", err), true), nil
		}
		return toolResult(text, false), nil

	case "ping":
		return obj(map[string]any{}), nil
	}

	if strings.HasPrefix(req.Method, "notifications/") {
		return nil, nil
	}
	return nil, &rpcError{Code: -32601, Message: "method not found: " + req.Method}
}

func toolResult(text string, isErr bool) any {
	return obj(map[string]any{
		"content": []any{obj(map[string]any{"type": "text", "text": text})},
		"isError": isErr,
	})
}

// mcpNamespace resolves an optional namespace argument. An empty one is the
// caller's own, which is what a session means unless it says otherwise; a bad
// one is refused rather than quietly replaced, because the difference between
// two namespaces is who reads the message.
func mcpNamespace(arg string) (Namespace, error) {
	if strings.TrimSpace(arg) == "" {
		return CurrentNamespace(), nil
	}
	ns, err := ParseNamespace(arg)
	if err != nil {
		return ns, err
	}
	// The MCP server is a model's hands, so a private namespace is refused here
	// whatever the environment says. The CLI's check is conditional because a
	// human runs it too; nothing but a model calls this.
	if ns.IsPrivate() && !CurrentNamespace().Is(ns) {
		return ns, &PrivacyError{NS: ns}
	}
	return ns, nil
}

func callTool(name string, raw json.RawMessage) (string, error) {
	switch name {
	case "list_sessions":
		var a struct {
			Namespace string `json:"namespace"`
		}
		_ = json.Unmarshal(raw, &a)
		ns, err := mcpNamespace(a.Namespace)
		if err != nil {
			return "", err
		}
		return mcpList(ns)
	case "send_message":
		var a struct {
			To        string `json:"to"`
			Text      string `json:"text"`
			Namespace string `json:"namespace"`
		}
		_ = json.Unmarshal(raw, &a)
		ns, err := mcpNamespace(a.Namespace)
		if err != nil {
			return "", err
		}
		return mcpSend(ns, a.To, a.Text)
	case "publish":
		var a struct {
			Topic     string `json:"topic"`
			Text      string `json:"text"`
			Namespace string `json:"namespace"`
		}
		_ = json.Unmarshal(raw, &a)
		ns, err := mcpNamespace(a.Namespace)
		if err != nil {
			return "", err
		}
		return mcpPublish(ns, a.Topic, a.Text)
	case "subscribe", "unsubscribe":
		var a struct {
			Topic     string `json:"topic"`
			Namespace string `json:"namespace"`
		}
		_ = json.Unmarshal(raw, &a)
		ns, err := mcpNamespace(a.Namespace)
		if err != nil {
			return "", err
		}
		return mcpSubscription(ns, name, a.Topic)
	case "list_topics":
		return mcpTopics()
	case "list_namespaces":
		return mcpNamespaces()
	case "whoami":
		return mcpWhoami()
	case "set_identity":
		var a struct {
			Name                string `json:"name"`
			Description         string `json:"description"`
			NameTemplate        string `json:"nameTemplate"`
			DescriptionTemplate string `json:"descriptionTemplate"`
		}
		_ = json.Unmarshal(raw, &a)
		return mcpSetIdentity(a.Name, a.Description, a.NameTemplate, a.DescriptionTemplate)
	}
	return "", fmt.Errorf("unknown tool %q", name)
}

func mcpList(ns Namespace) (string, error) {
	entries, err := ns.ListSessions(false, false)
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return fmt.Sprintf("No other Claude Code sessions are registered with pigeon in namespace %q.%s",
			ns, elsewhereNote(ns)), nil
	}
	me := CurrentSessionID()
	var b strings.Builder
	for _, e := range entries {
		if e.SessionID == me {
			b.WriteString("* ")
		} else {
			b.WriteString("  ")
		}
		fmt.Fprintf(&b, "%s  addr=%s", Short(e.SessionID), e.Addr())
		if e.PID > 0 {
			// A pid is also a valid send target, so surface it next to addr.
			fmt.Fprintf(&b, "  pid=%d", e.PID)
		}
		fmt.Fprintf(&b, "  status=%s  ns=%s  cwd=%s", e.Status, e.Namespace, e.Cwd)
		if e.ClaudeName != "" {
			// Claude Code's own /status name. Informational, not an address.
			fmt.Fprintf(&b, "  claude=%s", e.ClaudeName)
		}
		if e.Ephemeral {
			// A shell holding an inbox open with `pigeon listen`, not a Claude
			// Code session. Addressable all the same.
			b.WriteString("  [shell inbox]")
		}
		if e.Description != "" {
			fmt.Fprintf(&b, "\n      %s", Sanitize(e.Description))
		}
		if e.Status == StatusDeaf {
			b.WriteString("\n      (running but not listening; messages will not arrive)")
		}
		b.WriteString("\n")
	}
	self := ""
	for _, e := range entries {
		if e.SessionID == me {
			self = e.Addr()
			break
		}
	}
	switch {
	case self != "":
		fmt.Fprintf(&b, "\n* = this session. Others reach you as %q.", self)
	case me != "":
		fmt.Fprintf(&b, "\nThis session (%s) is not in the list: it has no listening monitor, "+
			"so other sessions cannot reach it.", Short(me))
	}
	b.WriteString(elsewhereNote(ns))
	return b.String(), nil
}

// elsewhereNote says what the listing deliberately left out. Without it a
// session concludes the machine is empty when it is only alone.
func elsewhereNote(ns Namespace) string {
	sessions, spaces := peersElsewhere(ns)
	if sessions == 0 {
		return ""
	}
	return fmt.Sprintf("\n%d further session(s) are in %d other namespace(s) and cannot see this one; "+
		"list_namespaces names them.", sessions, spaces)
}

func mcpSend(ns Namespace, to, text string) (string, error) {
	if strings.TrimSpace(to) == "" || strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("both 'to' and 'text' are required")
	}
	target, err := ns.ResolveTarget(to)
	if err != nil {
		return "", err
	}
	msg, err := ns.Send(target, text, CurrentSender(), "")
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Delivered to %s (%s) in namespace %q.", target.Display(), Short(target.SessionID), ns)
	if msg.Payload != "" {
		fmt.Fprintf(&b, " Body exceeded %d chars, so the full text was written to %s and the recipient received a pointer.",
			BodyBudget, msg.Payload)
	}
	if target.Status == StatusDeaf {
		b.WriteString(" WARNING: that session is running but has no listening monitor. The message is queued on its spool, but only a monitor for the same session id will ever read it; a newly started session gets a new id and will not see it.")
	}
	return b.String(), nil
}

func mcpPublish(ns Namespace, topic, text string) (string, error) {
	if strings.TrimSpace(topic) == "" || strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("both 'topic' and 'text' are required")
	}
	msg, err := ns.Publish(topic, text, CurrentSender())
	if err != nil {
		return "", err
	}
	num := ns.SubscriberCount(topic, CurrentSessionID())
	out := fmt.Sprintf("Published to %s. %d other live session(s) subscribe to it.",
		TopicLabel(msg.Topic), num)
	if strings.HasPrefix(msg.Topic, GlobalPrefix) {
		out += " That topic is machine-wide, so subscribers in every namespace received it."
	}
	if num == 0 {
		out += " Nobody is listening right now, but the message is on the log for anyone who subscribes later."
	}
	if msg.Payload != "" {
		out += fmt.Sprintf(" Body exceeded %d chars, so subscribers got a pointer to %s.", BodyBudget, msg.Payload)
	}
	return out, nil
}

func mcpSubscription(ns Namespace, action, topic string) (string, error) {
	sid := CurrentSessionID()
	if sid == "" {
		return "", fmt.Errorf("not running inside a Claude Code session")
	}
	if strings.TrimSpace(topic) == "" {
		return "", fmt.Errorf("'topic' is required")
	}
	if action == "subscribe" {
		if err := ns.Subscribe(sid, topic); err != nil {
			return "", err
		}
		return fmt.Sprintf("Subscribed to %s. Messages published from now on will arrive in this session, even while idle.",
			TopicLabel(topic)), nil
	}
	if err := ns.Unsubscribe(sid, topic); err != nil {
		return "", err
	}
	return fmt.Sprintf("Unsubscribed from %s.", TopicLabel(topic)), nil
}

func mcpTopics() (string, error) {
	topics, err := ListTopics()
	if err != nil {
		return "", err
	}
	mine := map[string]bool{}
	if sid := CurrentSessionID(); sid != "" {
		if e, err := ReadEntry(sid); err == nil {
			for _, t := range e.Subscriptions {
				mine[t] = true
			}
		}
	}
	var b strings.Builder
	for _, t := range topics {
		mark := "  "
		if mine[t.Name] {
			mark = "* "
		}
		scope := ""
		if t.Global {
			scope = "  [machine-wide]"
		}
		fmt.Fprintf(&b, "%s%s  (%d subscriber(s))%s\n", mark, TopicLabel(t.Name), t.Subscribers, scope)
	}
	b.WriteString("\n* = this session subscribes")
	return b.String(), nil
}

func mcpNamespaces() (string, error) {
	spaces, err := ListNamespaces()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, info := range spaces {
		mark := "  "
		if info.Current {
			mark = "* "
		}
		fmt.Fprintf(&b, "%s%s  live=%d  deaf=%d\n", mark, info.Name, info.Live, info.Deaf)
	}
	b.WriteString("\n* = this session's namespace. Sessions in other namespaces cannot be listed " +
		"or addressed without naming theirs, and a session cannot move: its namespace is fixed " +
		"when it starts. '@' topics reach every namespace.")
	return b.String(), nil
}

func mcpWhoami() (string, error) {
	sid := CurrentSessionID()
	if sid == "" {
		return "Not running inside a Claude Code session.", nil
	}
	e, err := ReadEntry(sid)
	if err != nil {
		return fmt.Sprintf("Session %s is not registered with pigeon. Is the plugin installed and the session restarted?", sid), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "session:     %s\n", e.SessionID)
	fmt.Fprintf(&b, "namespace:   %s\n", e.Namespace)
	if e.PID > 0 {
		fmt.Fprintf(&b, "pid:         %d\n", e.PID)
	}
	fmt.Fprintf(&b, "name:        %s\n", orDash(e.Name))
	fmt.Fprintf(&b, "claude name: %s\n", claudeNameLine(e))
	fmt.Fprintf(&b, "description: %s\n", orDash(e.Description))
	fmt.Fprintf(&b, "cwd:         %s\n", e.Cwd)
	fmt.Fprintf(&b, "status:      %s\n", e.Status)
	fmt.Fprintf(&b, "topics:      %s\n", orDash(strings.Join(e.Subscriptions, ", ")))
	if e.Private {
		// The blank cwd and description above are a deliberate policy rather
		// than a half-finished registration. Say which, or the model will try
		// to fix it.
		b.WriteString("private:     this project publishes no cwd or description\n")
	}
	fmt.Fprintf(&b, "\nOther sessions reach this one with: pigeon send %s \"...\"", e.Addr())
	return b.String(), nil
}

func mcpSetIdentity(name, desc, nameTmpl, descTmpl string) (string, error) {
	sid := CurrentSessionID()
	if sid == "" {
		return "", fmt.Errorf("not running inside a Claude Code session")
	}
	e, err := ReadEntry(sid)
	if err != nil {
		return "", fmt.Errorf("this session is not registered; install the plugin and restart")
	}

	// A template and a literal for the same field is an ambiguity the caller
	// has to settle, not one to guess at: picking either would give a name
	// nobody asked for.
	cwd := CurrentCwd()
	if nameTmpl != "" {
		if name != "" {
			return "", fmt.Errorf("give either 'name' or 'nameTemplate', not both")
		}
		if name, err = RenderName(nameTmpl, sid, cwd); err != nil {
			return "", fmt.Errorf("nameTemplate: %w", err)
		}
	}
	if descTmpl != "" {
		if desc != "" {
			return "", fmt.Errorf("give either 'description' or 'descriptionTemplate', not both")
		}
		if desc, err = RenderDescription(descTmpl, sid, cwd); err != nil {
			return "", fmt.Errorf("descriptionTemplate: %w", err)
		}
	}

	if name != "" {
		name = strings.TrimSpace(name)
		if err := ValidName(name); err != nil {
			return "", err
		}
		if NameTaken(name, sid) {
			return "", fmt.Errorf("another live session already uses the name %q", name)
		}
	}
	if err := MutateEntry(sid, func(e *Entry) error {
		if name != "" {
			e.Name = name
		}
		if desc != "" {
			e.Description = Sanitize(desc)
		}
		return nil
	}); err != nil {
		return "", err
	}
	e, err = ReadEntry(sid)
	if err != nil {
		return "", err
	}
	out := fmt.Sprintf("Identity updated. Other sessions can now reach this one with: pigeon send %s \"...\"", e.Addr())
	if e.Private && desc != "" {
		// Silently dropping it would leave the model believing peers can see
		// what this session is working on.
		out += " This project is marked private, so the description is not published to other sessions."
	}
	return out, nil
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// claudeNameLine renders Claude Code's own session name with its source, so a
// model reading whoami can tell a derived name (cwd echo) from a chosen one.
func claudeNameLine(e *Entry) string {
	if strings.TrimSpace(e.ClaudeName) == "" {
		return "-"
	}
	if e.ClaudeNameSource != "" {
		return e.ClaudeName + " (" + e.ClaudeNameSource + ")"
	}
	return e.ClaudeName
}
