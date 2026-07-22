package pigeon

import (
	"encoding/json"
	"strings"
	"testing"
)

func call(t *testing.T, method string, params any) (any, *rpcError) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	return dispatch(rpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  method,
		Params:  raw,
	}, "test")
}

func toolText(t *testing.T, name string, args any) string {
	t.Helper()
	res, rerr := call(t, "tools/call", map[string]any{"name": name, "arguments": args})
	if rerr != nil {
		t.Fatalf("tools/call %s: %v", name, rerr.Message)
	}
	m, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("unexpected result shape %T", res)
	}
	content, ok := m["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("no content in result: %+v", m)
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	return text
}

func TestInitializeAdvertisesTools(t *testing.T) {
	res, rerr := call(t, "initialize", map[string]any{})
	if rerr != nil {
		t.Fatalf("initialize: %v", rerr.Message)
	}
	m := res.(map[string]any)
	caps := m["capabilities"].(map[string]any)
	if _, ok := caps["tools"]; !ok {
		t.Error("server does not advertise tool capability")
	}
	if m["protocolVersion"] != protocolVersion {
		t.Errorf("protocolVersion = %v", m["protocolVersion"])
	}
}

func TestToolsListIsComplete(t *testing.T) {
	res, rerr := call(t, "tools/list", map[string]any{})
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr.Message)
	}
	defs := res.(map[string]any)["tools"].([]toolDef)

	want := []string{
		"list_sessions", "send_message", "publish", "subscribe",
		"unsubscribe", "list_topics", "whoami", "set_identity",
	}
	got := map[string]bool{}
	for _, d := range defs {
		got[d.Name] = true
		if d.Description == "" {
			t.Errorf("tool %q has no description", d.Name)
		}
		if d.InputSchema == nil {
			t.Errorf("tool %q has no input schema", d.Name)
		}
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing tool %q", w)
		}
	}
}

func TestUnknownMethodIsAProtocolError(t *testing.T) {
	_, rerr := call(t, "no/such/method", map[string]any{})
	if rerr == nil || rerr.Code != -32601 {
		t.Fatalf("expected method-not-found, got %+v", rerr)
	}
}

func TestNotificationsAreIgnored(t *testing.T) {
	_, rerr := call(t, "notifications/initialized", map[string]any{})
	if rerr != nil {
		t.Fatalf("notification should not error: %v", rerr.Message)
	}
}

func TestToolErrorsAreResultsNotProtocolErrors(t *testing.T) {
	// The model needs to see a tool failure as text it can act on, rather
	// than the transport reporting a fault.
	res, rerr := call(t, "tools/call", map[string]any{"name": "nope", "arguments": map[string]any{}})
	if rerr != nil {
		t.Fatalf("unexpected protocol error: %v", rerr.Message)
	}
	if isErr, _ := res.(map[string]any)["isError"].(bool); !isErr {
		t.Error("unknown tool should be flagged isError")
	}
}

func TestSendMessageRequiresBothFields(t *testing.T) {
	withHome(t)
	if got := toolText(t, "send_message", map[string]any{"to": "x"}); !strings.Contains(got, "required") {
		t.Errorf("expected a validation message, got %q", got)
	}
}

func TestSendMessageDeliversAndWarnsWhenDeaf(t *testing.T) {
	withHome(t)
	t.Setenv(EnvSessionID, "aaaa1111-2222")
	liveEntry(t, "aaaa1111-2222", "alpha", "/tmp/a")
	liveEntry(t, "bbbb2222-3333", "beta", "/tmp/b")

	got := toolText(t, "send_message", map[string]any{"to": "beta", "text": "ping"})
	if !strings.Contains(got, "Delivered to beta") {
		t.Errorf("got %q, want a delivery confirmation", got)
	}
	// No monitor holds beta's lock in tests, so it is deaf and the caller
	// must be told the message will not arrive.
	if !strings.Contains(got, "WARNING") {
		t.Errorf("got %q, want a deaf-session warning", got)
	}
}

func TestSetIdentityRejectsMultiWordNames(t *testing.T) {
	withHome(t)
	t.Setenv(EnvSessionID, "aaaa1111-2222")
	liveEntry(t, "aaaa1111-2222", "", "/tmp/a")

	got := toolText(t, "set_identity", map[string]any{"name": "two words"})
	if !strings.Contains(got, "invalid name") {
		t.Errorf("got %q, want a name-validation error", got)
	}
}

func TestSetIdentityRejectsDuplicateNames(t *testing.T) {
	withHome(t)
	t.Setenv(EnvSessionID, "bbbb2222-3333")
	liveEntry(t, "aaaa1111-2222", "alpha", "/tmp/a")
	liveEntry(t, "bbbb2222-3333", "", "/tmp/b")

	got := toolText(t, "set_identity", map[string]any{"name": "alpha"})
	if !strings.Contains(got, "already uses") {
		t.Errorf("got %q, want a name-clash error", got)
	}
}

func TestSetIdentityUpdatesNameAndDescription(t *testing.T) {
	withHome(t)
	t.Setenv(EnvSessionID, "aaaa1111-2222")
	liveEntry(t, "aaaa1111-2222", "", "/tmp/a")

	toolText(t, "set_identity", map[string]any{"name": "alpha", "description": "parser work"})
	e, err := ReadEntry("aaaa1111-2222")
	if err != nil {
		t.Fatal(err)
	}
	if e.Name != "alpha" || e.Description != "parser work" {
		t.Errorf("entry not updated: %+v", e)
	}
}

func TestSubscribeAndListTopicsViaMCP(t *testing.T) {
	withHome(t)
	t.Setenv(EnvSessionID, "aaaa1111-2222")
	liveEntry(t, "aaaa1111-2222", "alpha", "/tmp/a")

	if got := toolText(t, "subscribe", map[string]any{"topic": "deploys"}); !strings.Contains(got, "deploys") {
		t.Errorf("subscribe returned %q", got)
	}
	if got := toolText(t, "list_topics", map[string]any{}); !strings.Contains(got, "#deploys") {
		t.Errorf("list_topics returned %q", got)
	}
	if got := toolText(t, "unsubscribe", map[string]any{"topic": "deploys"}); !strings.Contains(got, "Unsubscribed") {
		t.Errorf("unsubscribe returned %q", got)
	}
}

func TestPublishReportsNobodyListening(t *testing.T) {
	withHome(t)
	t.Setenv(EnvSessionID, "aaaa1111-2222")
	liveEntry(t, "aaaa1111-2222", "alpha", "/tmp/a")

	got := toolText(t, "publish", map[string]any{"topic": "quiet", "text": "anyone?"})
	if !strings.Contains(got, "Nobody is listening") {
		t.Errorf("got %q, want an explicit no-subscribers note", got)
	}
}

func TestWhoamiOutsideASession(t *testing.T) {
	withHome(t)
	t.Setenv(EnvSessionID, "")
	if got := toolText(t, "whoami", map[string]any{}); !strings.Contains(got, "Not running inside") {
		t.Errorf("got %q", got)
	}
}

func TestListSessionsMarksSelfAndFlagsDeafPeers(t *testing.T) {
	withHome(t)
	t.Setenv(EnvSessionID, "aaaa1111-2222")
	liveEntry(t, "aaaa1111-2222", "alpha", "/home/p/api")
	beta := liveEntry(t, "bbbb2222-3333", "beta", "/home/p/web")
	beta.Description = "refactoring the parser"
	if err := WriteEntry(beta); err != nil {
		t.Fatalf("WriteEntry: %v", err)
	}

	got := toolText(t, "list_sessions", map[string]any{})
	for _, want := range []string{"alpha", "beta", "refactoring the parser", "* = this session"} {
		if !strings.Contains(got, want) {
			t.Errorf("list_sessions is missing %q:\n%s", want, got)
		}
	}
	// No monitor holds any lock in tests, so every session is deaf. The model
	// has to be told that plainly, or it will assume its message landed.
	if !strings.Contains(got, "not listening") {
		t.Errorf("deaf sessions are not flagged:\n%s", got)
	}
	// The caller needs to know which line is itself before it starts messaging.
	if !strings.Contains(got, "* aaaa1111") {
		t.Errorf("this session is not marked:\n%s", got)
	}
}

func TestListSessionsWhenNobodyIsRegistered(t *testing.T) {
	withHome(t)
	t.Setenv(EnvSessionID, "aaaa1111-2222")
	// An empty registry is the normal state before anyone restarts, so it must
	// read as an answer rather than as an empty table.
	if got := toolText(t, "list_sessions", map[string]any{}); !strings.Contains(got, "No other Claude Code sessions") {
		t.Errorf("got %q, want an explicit empty-registry answer", got)
	}
}

func TestWhoamiReportsIdentityAndDashesTheBlanks(t *testing.T) {
	withHome(t)
	t.Setenv(EnvSessionID, "aaaa1111-2222")
	liveEntry(t, "aaaa1111-2222", "", "/home/p/api")

	got := toolText(t, "whoami", map[string]any{})
	if !strings.Contains(got, "session:     aaaa1111-2222") {
		t.Errorf("whoami does not report the session id:\n%s", got)
	}
	// An undeclared name must render as a dash rather than a blank, so the
	// model can tell "not set" from "failed to read".
	if !strings.Contains(got, "name:        -") {
		t.Errorf("an undeclared name did not render as a dash:\n%s", got)
	}
	// Without a name the address is the short id, and whoami is where a
	// session learns what to tell a peer.
	if !strings.Contains(got, "pigeon send aaaa1111") {
		t.Errorf("whoami does not report a usable address:\n%s", got)
	}
}

func TestWhoamiForAnUnregisteredSession(t *testing.T) {
	withHome(t)
	t.Setenv(EnvSessionID, "cccc3333-4444")
	// Inside a session but with no entry means the monitor never started: the
	// answer has to point at that, not just report nothing.
	got := toolText(t, "whoami", map[string]any{})
	if !strings.Contains(got, "not registered") {
		t.Errorf("got %q, want an explanation that the session never registered", got)
	}
}

func TestSubscriptionToolsRequireASession(t *testing.T) {
	withHome(t)
	t.Setenv(EnvSessionID, "")
	// Subscribing needs an identity to attach the subscription to, so from a
	// plain shell it must fail rather than guess.
	if got := toolText(t, "subscribe", map[string]any{"topic": "deploys"}); !strings.Contains(got, "not running inside") {
		t.Errorf("got %q, want a not-in-a-session error", got)
	}
	t.Setenv(EnvSessionID, "aaaa1111-2222")
	liveEntry(t, "aaaa1111-2222", "alpha", "/tmp/a")
	if got := toolText(t, "subscribe", map[string]any{}); !strings.Contains(got, "required") {
		t.Errorf("got %q, want a missing-topic error", got)
	}
}

func TestPublishRequiresBothFields(t *testing.T) {
	withHome(t)
	if got := toolText(t, "publish", map[string]any{"topic": "deploys"}); !strings.Contains(got, "required") {
		t.Errorf("got %q, want a validation message", got)
	}
}

func TestRunMCPHandlesAStream(t *testing.T) {
	withHome(t)
	in := strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}` + "\n")
	var out strings.Builder
	if err := RunMCP(in, &out, "test"); err != nil {
		t.Fatalf("RunMCP: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d responses, want 2 (the notification must not get one)", len(lines))
	}
	for _, l := range lines {
		var resp rpcResponse
		if err := json.Unmarshal([]byte(l), &resp); err != nil {
			t.Fatalf("invalid JSON response: %v", err)
		}
		if resp.Error != nil {
			t.Errorf("unexpected error response: %v", resp.Error.Message)
		}
	}
}
