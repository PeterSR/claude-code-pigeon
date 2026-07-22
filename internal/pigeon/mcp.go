package pigeon

import (
	"bufio"
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

func tools() []toolDef {
	return []toolDef{
		{
			Name: "list_sessions",
			Description: "List live Claude Code sessions reachable via pigeon, with their " +
				"name, description, working directory and status. Status 'deaf' means the " +
				"session is running but not listening, so messages to it will not arrive.",
			InputSchema: obj(map[string]any{
				"type":       "object",
				"properties": obj(map[string]any{}),
			}),
		},
		{
			Name: "send_message",
			Description: "Send a message to another Claude Code session. The recipient is " +
				"woken even if idle. Your own identity is attached automatically so they can " +
				"reply. Keep it under ~300 characters; longer text is written to a file and " +
				"the recipient gets a path instead.",
			InputSchema: obj(map[string]any{
				"type": "object",
				"properties": obj(map[string]any{
					"to": obj(map[string]any{
						"type": "string",
						"description": "Target session: its declared name, session id (or a " +
							"prefix), or the basename of its working directory.",
					}),
					"text": obj(map[string]any{
						"type":        "string",
						"description": "Message body.",
					}),
				}),
				"required": []string{"to", "text"},
			}),
		},
		{
			Name: "publish",
			Description: "Publish a message to a topic. Every session subscribed to that " +
				"topic is woken, even if idle. Every session subscribes to 'all' by default, " +
				"so publishing to 'all' broadcasts to the whole machine.",
			InputSchema: obj(map[string]any{
				"type": "object",
				"properties": obj(map[string]any{
					"topic": obj(map[string]any{
						"type":        "string",
						"description": "Topic name: lowercase letters, digits, dot, dash or underscore.",
					}),
					"text": obj(map[string]any{"type": "string", "description": "Message body."}),
				}),
				"required": []string{"topic", "text"},
			}),
		},
		{
			Name: "subscribe",
			Description: "Start receiving a topic in this session. Takes effect within about " +
				"a second, without restarting. Only messages published from now on arrive; " +
				"history is not replayed.",
			InputSchema: obj(map[string]any{
				"type": "object",
				"properties": obj(map[string]any{
					"topic": obj(map[string]any{"type": "string", "description": "Topic to join."}),
				}),
				"required": []string{"topic"},
			}),
		},
		{
			Name:        "unsubscribe",
			Description: "Stop receiving a topic in this session.",
			InputSchema: obj(map[string]any{
				"type": "object",
				"properties": obj(map[string]any{
					"topic": obj(map[string]any{"type": "string", "description": "Topic to leave."}),
				}),
				"required": []string{"topic"},
			}),
		},
		{
			Name:        "list_topics",
			Description: "List known topics and how many live sessions subscribe to each.",
			InputSchema: obj(map[string]any{"type": "object", "properties": obj(map[string]any{})}),
		},
		{
			Name: "whoami",
			Description: "Show this session's pigeon identity: session id, declared name, " +
				"description, and the address other sessions use to reach it.",
			InputSchema: obj(map[string]any{
				"type":       "object",
				"properties": obj(map[string]any{}),
			}),
		},
		{
			Name: "set_identity",
			Description: "Declare this session's name and/or description so other sessions " +
				"can find and address it. The name must be a single word and unique among " +
				"live sessions; it then works as an address.",
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
				}),
			}),
		},
	}
}

// RunMCP serves MCP on stdio until the stream closes.
func RunMCP(in io.Reader, out io.Writer, version string) error {
	dec := json.NewDecoder(bufio.NewReader(in))
	enc := json.NewEncoder(out)

	for {
		var req rpcRequest
		if err := dec.Decode(&req); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
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

func callTool(name string, raw json.RawMessage) (string, error) {
	switch name {
	case "list_sessions":
		return mcpList()
	case "send_message":
		var a struct {
			To   string `json:"to"`
			Text string `json:"text"`
		}
		_ = json.Unmarshal(raw, &a)
		return mcpSend(a.To, a.Text)
	case "publish":
		var a struct {
			Topic string `json:"topic"`
			Text  string `json:"text"`
		}
		_ = json.Unmarshal(raw, &a)
		return mcpPublish(a.Topic, a.Text)
	case "subscribe", "unsubscribe":
		var a struct {
			Topic string `json:"topic"`
		}
		_ = json.Unmarshal(raw, &a)
		return mcpSubscription(name, a.Topic)
	case "list_topics":
		return mcpTopics()
	case "whoami":
		return mcpWhoami()
	case "set_identity":
		var a struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		_ = json.Unmarshal(raw, &a)
		return mcpSetIdentity(a.Name, a.Description)
	}
	return "", fmt.Errorf("unknown tool %q", name)
}

func mcpList() (string, error) {
	entries, err := ListSessions(false, false)
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "No other Claude Code sessions are registered with pigeon.", nil
	}
	me := CurrentSessionID()
	var b strings.Builder
	for _, e := range entries {
		if e.SessionID == me {
			b.WriteString("* ")
		} else {
			b.WriteString("  ")
		}
		fmt.Fprintf(&b, "%s  addr=%s  status=%s  cwd=%s",
			Short(e.SessionID), e.Addr(), e.Status, e.Cwd)
		if e.Description != "" {
			fmt.Fprintf(&b, "\n      %s", Sanitize(e.Description))
		}
		if e.Status == StatusDeaf {
			b.WriteString("\n      (running but not listening; messages will not arrive)")
		}
		b.WriteString("\n")
	}
	b.WriteString("\n* = this session")
	return b.String(), nil
}

func mcpSend(to, text string) (string, error) {
	if strings.TrimSpace(to) == "" || strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("both 'to' and 'text' are required")
	}
	target, err := ResolveTarget(to)
	if err != nil {
		return "", err
	}
	msg, err := Send(target, text, CurrentSender(), "")
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Delivered to %s (%s).", target.Display(), Short(target.SessionID))
	if msg.Payload != "" {
		fmt.Fprintf(&b, " Body exceeded %d chars, so the full text was written to %s and the recipient received a pointer.",
			BodyBudget, msg.Payload)
	}
	if target.Status == StatusDeaf {
		b.WriteString(" WARNING: that session is running but has no listening monitor. The message is queued on its spool, but only a monitor for the same session id will ever read it; a newly started session gets a new id and will not see it.")
	}
	return b.String(), nil
}

func mcpPublish(topic, text string) (string, error) {
	if strings.TrimSpace(topic) == "" || strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("both 'topic' and 'text' are required")
	}
	msg, err := Publish(topic, text, CurrentSender())
	if err != nil {
		return "", err
	}
	me := CurrentSessionID()
	n := 0
	if entries, e := ListSessions(false, false); e == nil {
		for _, en := range entries {
			for _, t := range en.Subscriptions {
				if t == topic && en.SessionID != me {
					n++
				}
			}
		}
	}
	out := fmt.Sprintf("Published to #%s. %d other live session(s) subscribe to it.", topic, n)
	if n == 0 {
		out += " Nobody is listening right now, but the message is on the log for anyone who subscribes later."
	}
	if msg.Payload != "" {
		out += fmt.Sprintf(" Body exceeded %d chars, so subscribers got a pointer to %s.", BodyBudget, msg.Payload)
	}
	return out, nil
}

func mcpSubscription(action, topic string) (string, error) {
	sid := CurrentSessionID()
	if sid == "" {
		return "", fmt.Errorf("not running inside a Claude Code session")
	}
	if strings.TrimSpace(topic) == "" {
		return "", fmt.Errorf("'topic' is required")
	}
	if action == "subscribe" {
		if err := Subscribe(sid, topic); err != nil {
			return "", err
		}
		return fmt.Sprintf("Subscribed to #%s. Messages published from now on will arrive in this session, even while idle.", topic), nil
	}
	if err := Unsubscribe(sid, topic); err != nil {
		return "", err
	}
	return fmt.Sprintf("Unsubscribed from #%s.", topic), nil
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
		fmt.Fprintf(&b, "%s#%s  (%d subscriber(s))\n", mark, t.Name, t.Subscribers)
	}
	b.WriteString("\n* = this session subscribes")
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
	fmt.Fprintf(&b, "name:        %s\n", orDash(e.Name))
	fmt.Fprintf(&b, "description: %s\n", orDash(e.Description))
	fmt.Fprintf(&b, "cwd:         %s\n", e.Cwd)
	fmt.Fprintf(&b, "status:      %s\n", e.Status)
	fmt.Fprintf(&b, "topics:      %s\n", orDash(strings.Join(e.Subscriptions, ", ")))
	fmt.Fprintf(&b, "\nOther sessions reach this one with: pigeon send %s \"...\"", e.Addr())
	return b.String(), nil
}

func mcpSetIdentity(name, desc string) (string, error) {
	sid := CurrentSessionID()
	if sid == "" {
		return "", fmt.Errorf("not running inside a Claude Code session")
	}
	e, err := ReadEntry(sid)
	if err != nil {
		return "", fmt.Errorf("this session is not registered; install the plugin and restart")
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
	return fmt.Sprintf("Identity updated. Other sessions can now reach this one with: pigeon send %s \"...\"", e.Addr()), nil
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
