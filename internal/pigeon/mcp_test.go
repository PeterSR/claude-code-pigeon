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
		"unsubscribe", "inbox", "list_topics", "list_namespaces", "whoami", "set_identity",
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

func TestSetIdentityRendersTemplates(t *testing.T) {
	withHome(t)
	t.Setenv(EnvSessionID, "bbbb2222-3333")
	t.Setenv(EnvProjectDir, "/home/p/api")
	liveEntry(t, "aaaa1111-2222", "api", "/home/p/api")
	liveEntry(t, "bbbb2222-3333", "", "/home/p/api")

	toolText(t, "set_identity", map[string]any{
		"nameTemplate":        "{{.Dir}}-{{.Seq}}",
		"descriptionTemplate": "second session in {{.Dir}}",
	})
	e, err := ReadEntry("bbbb2222-3333")
	if err != nil {
		t.Fatal(err)
	}
	if e.Name != "api-2" {
		t.Errorf("name = %q, want api-2", e.Name)
	}
	if e.Description != "second session in api" {
		t.Errorf("description = %q", e.Description)
	}
}

// A template and a literal for the same field is an ambiguity the caller has
// to settle: picking either would give the session a name nobody asked for.
func TestSetIdentityRefusesATemplateAndALiteralTogether(t *testing.T) {
	withHome(t)
	t.Setenv(EnvSessionID, "aaaa1111-2222")
	liveEntry(t, "aaaa1111-2222", "", "/home/p/api")

	for _, args := range []map[string]any{
		{"name": "alpha", "nameTemplate": "{{.Dir}}"},
		{"description": "work", "descriptionTemplate": "{{.Dir}}"},
	} {
		if got := toolText(t, "set_identity", args); !strings.Contains(got, "not both") {
			t.Errorf("%v: got %q, want a refusal", args, got)
		}
	}
}

// A template that renders to something unusable is reported, not repaired.
func TestSetIdentityRejectsAnUnusableRenderedName(t *testing.T) {
	withHome(t)
	t.Setenv(EnvSessionID, "aaaa1111-2222")
	t.Setenv(EnvProjectDir, "/home/p/api")
	liveEntry(t, "aaaa1111-2222", "", "/home/p/api")

	if got := toolText(t, "set_identity", map[string]any{"nameTemplate": "{{.Cwd}}"}); !strings.Contains(got, "invalid name") {
		t.Errorf("got %q, want a name-validation error", got)
	}
	if got := toolText(t, "set_identity", map[string]any{"nameTemplate": "{{.Dir"}); !strings.Contains(got, "nameTemplate") {
		t.Errorf("got %q, want the broken template named", got)
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

// A deaf-only subscriber must not read as "reached": the live count has to
// exclude it, the deaf note has to name it, and the no-one's-listening
// wording has to be the accurate one rather than the reassuring one.
func TestPublishNotesADeafOnlySubscriber(t *testing.T) {
	withHome(t)
	t.Setenv(EnvSessionID, "aaaa1111-2222")
	liveEntry(t, "aaaa1111-2222", "alpha", "/tmp/a")
	liveEntry(t, "bbbb2222-3333", "beta", "/tmp/b") // deaf: no monitor lock held
	if err := Subscribe("bbbb2222-3333", "deploys"); err != nil {
		t.Fatal(err)
	}

	got := toolText(t, "publish", map[string]any{"topic": "deploys", "text": "shipping"})
	if !strings.Contains(got, "0 other live session(s)") {
		t.Errorf("got %q, want the deaf subscriber excluded from the live count", got)
	}
	if !strings.Contains(got, "NOTE: 1 subscriber(s) are deaf") {
		t.Errorf("got %q, want the deaf subscriber called out", got)
	}
	if !strings.Contains(got, "protects nothing") {
		t.Errorf("got %q, want the accurate no-one's-listening wording, not the reassuring one", got)
	}
}

// A mix of live and deaf subscribers: the live count and the deaf note both
// have to show up, and the "nobody is listening" wording must not, since
// someone genuinely is.
func TestPublishNotesDeafSubscribersAlongsideLiveOnes(t *testing.T) {
	withHome(t)
	t.Setenv(EnvSessionID, "aaaa1111-2222")
	liveEntry(t, "aaaa1111-2222", "alpha", "/tmp/a")
	armed(t, "bbbb2222-3333", "beta")
	liveEntry(t, "cccc3333-4444", "gamma", "/tmp/c") // deaf: no monitor lock held
	if err := Subscribe("bbbb2222-3333", "deploys"); err != nil {
		t.Fatal(err)
	}
	if err := Subscribe("cccc3333-4444", "deploys"); err != nil {
		t.Fatal(err)
	}

	got := toolText(t, "publish", map[string]any{"topic": "deploys", "text": "shipping"})
	if !strings.Contains(got, "1 other live session(s)") {
		t.Errorf("got %q, want the live subscriber counted", got)
	}
	if !strings.Contains(got, "NOTE: 1 subscriber(s) are deaf") {
		t.Errorf("got %q, want the deaf subscriber called out even though a live one exists", got)
	}
	if strings.Contains(got, "Nobody is listening") {
		t.Errorf("got %q, want no reassurance when a live subscriber exists", got)
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

// --- namespaces --------------------------------------------------------------

// The model sees one namespace at a time, so the listing has to say what it is
// not showing; concluding the machine is empty when it is only isolated is the
// mistake this text exists to prevent.
func TestListSessionsIsPerNamespaceAndSaysSo(t *testing.T) {
	withHome(t)
	t.Setenv(EnvNamespace, "acme")
	t.Setenv(EnvSessionID, "aaaa1111-2222")
	liveEntryIn(t, mustNS(t, "acme"), "aaaa1111-2222", "alpha", "/home/p/api")
	liveEntryIn(t, mustNS(t, "other"), "bbbb2222-3333", "beta", "/home/p/web")

	got := toolText(t, "list_sessions", map[string]any{})
	if strings.Contains(got, "beta") {
		t.Errorf("list_sessions leaked a session from another namespace:\n%s", got)
	}
	if !strings.Contains(got, "ns=acme") {
		t.Errorf("list_sessions does not report the namespace:\n%s", got)
	}
	if !strings.Contains(got, "1 further session(s) are in 1 other namespace(s)") {
		t.Errorf("list_sessions does not say what it is hiding:\n%s", got)
	}

	// Naming one is how a session looks over the fence deliberately.
	got = toolText(t, "list_sessions", map[string]any{"namespace": "other"})
	if !strings.Contains(got, "beta") {
		t.Errorf("an explicit namespace was not listed:\n%s", got)
	}
	// A namespace that cannot be a directory name is refused rather than
	// replaced: acting on "default" instead would message the wrong people.
	if got := toolText(t, "list_sessions", map[string]any{"namespace": "../escape"}); !strings.Contains(got, "invalid namespace") {
		t.Errorf("got %q, want the bad namespace refused", got)
	}
}

func TestSendMessageAcrossNamespaces(t *testing.T) {
	withHome(t)
	t.Setenv(EnvNamespace, "acme")
	t.Setenv(EnvSessionID, "aaaa1111-2222")
	liveEntryIn(t, mustNS(t, "acme"), "aaaa1111-2222", "alpha", "/home/p/api")
	liveEntryIn(t, mustNS(t, "other"), "bbbb2222-3333", "beta", "/home/p/web")

	if got := toolText(t, "send_message", map[string]any{"to": "beta", "text": "ping"}); !strings.Contains(got, "no live session") {
		t.Errorf("got %q; a session next door must not resolve by name alone", got)
	}
	got := toolText(t, "send_message", map[string]any{"to": "beta", "text": "ping", "namespace": "other"})
	if !strings.Contains(got, "Delivered to beta") || !strings.Contains(got, `namespace "other"`) {
		t.Errorf("got %q, want a delivery into the named namespace", got)
	}
	if n := mustNS(t, "other").Pending("bbbb2222-3333"); n != 1 {
		t.Errorf("recipient has %d message(s) waiting, want 1", n)
	}
}

func TestPublishToAGlobalTopicViaMCP(t *testing.T) {
	withHome(t)
	t.Setenv(EnvNamespace, "acme")
	t.Setenv(EnvSessionID, "aaaa1111-2222")
	liveEntryIn(t, mustNS(t, "acme"), "aaaa1111-2222", "alpha", "/home/p/api")
	other := mustNS(t, "other")
	armedIn(t, other, "bbbb2222-3333", "beta")
	if err := other.Subscribe("bbbb2222-3333", "@ops"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	got := toolText(t, "publish", map[string]any{"topic": "@ops", "text": "all hands"})
	if !strings.Contains(got, "Published to @ops") {
		t.Errorf("got %q, want the global marker kept", got)
	}
	if !strings.Contains(got, "machine-wide") {
		t.Errorf("got %q, want it to say the message crossed namespaces", got)
	}
	if !strings.Contains(got, "1 other live session(s)") {
		t.Errorf("got %q, want the subscriber next door counted", got)
	}
}

func TestListNamespacesViaMCP(t *testing.T) {
	withHome(t)
	t.Setenv(EnvNamespace, "acme")
	liveEntryIn(t, mustNS(t, "other"), "bbbb2222-3333", "beta", "/home/p/web")

	got := toolText(t, "list_namespaces", map[string]any{})
	for _, want := range []string{"* acme", "other", "deaf=1", "fixed when it starts"} {
		if !strings.Contains(got, want) {
			t.Errorf("list_namespaces is missing %q:\n%s", want, got)
		}
	}
}

// --- inbox (Task 1 / Task 2) ------------------------------------------------

func TestInboxToolReturnsFullBodyPastTheNotificationClip(t *testing.T) {
	withHome(t)
	sid := "aaaa1111-2222"
	t.Setenv(EnvSessionID, sid)
	me := liveEntry(t, sid, "me", "/tmp/me")

	long := strings.Repeat("x", BodyBudget+50)
	if _, err := DefaultNamespace().Send(me, Draft{Text: long, Subject: "big one"}, Sender{Kind: "shell", Name: "sh"}); err != nil {
		t.Fatal(err)
	}

	got := toolText(t, "inbox", map[string]any{})
	if !strings.Contains(got, long) {
		t.Errorf("inbox did not return the full %d-char body, which a notification would have clipped at %d:\n%s",
			len(long), BodyBudget, got)
	}
	if !strings.Contains(got, "SUBJECT: big one") {
		t.Errorf("inbox is missing the subject line:\n%s", got)
	}
}

func TestInboxToolMarkReadFalseLeavesTheCursorAlone(t *testing.T) {
	withHome(t)
	sid := "bbbb2222-3333"
	t.Setenv(EnvSessionID, sid)
	me := liveEntry(t, sid, "me", "/tmp/me")
	if _, err := DefaultNamespace().Send(me, Draft{Text: "hello"}, Sender{Kind: "shell", Name: "sh"}); err != nil {
		t.Fatal(err)
	}

	got := toolText(t, "inbox", map[string]any{"mark_read": false})
	if !strings.Contains(got, "hello") {
		t.Fatalf("expected the message body, got %q", got)
	}
	if _, ok := readCursors(sid)[readCursorKey(inboxCursorKey)]; ok {
		t.Error("mark_read: false advanced the consumption cursor")
	}

	// Nothing was consumed by the peek, so a second, default pull must still
	// see the same message.
	got2 := toolText(t, "inbox", map[string]any{})
	if !strings.Contains(got2, "hello") {
		t.Errorf("second pull did not see the message the peek left unread:\n%s", got2)
	}
}

func TestInboxToolDetailSubjectOmitsBody(t *testing.T) {
	withHome(t)
	sid := "cccc3333-4444"
	t.Setenv(EnvSessionID, sid)
	me := liveEntry(t, sid, "me", "/tmp/me")
	if _, err := DefaultNamespace().Send(me, Draft{Text: "the body text", Subject: "the subject"}, Sender{Kind: "shell", Name: "sh"}); err != nil {
		t.Fatal(err)
	}

	got := toolText(t, "inbox", map[string]any{"detail": "subject"})
	if !strings.Contains(got, "SUBJECT: the subject") {
		t.Errorf("missing subject line:\n%s", got)
	}
	if strings.Contains(got, "the body text") {
		t.Errorf("detail: subject leaked the body:\n%s", got)
	}
}

func TestInboxToolRejectsAnUnknownDetailValue(t *testing.T) {
	withHome(t)
	sid := "iiii9999-0000"
	t.Setenv(EnvSessionID, sid)
	liveEntry(t, sid, "me", "/tmp/me")

	res, rerr := call(t, "tools/call", map[string]any{"name": "inbox", "arguments": map[string]any{"detail": "everything"}})
	if rerr != nil {
		t.Fatalf("unexpected protocol error: %v", rerr.Message)
	}
	if isErr, _ := res.(map[string]any)["isError"].(bool); !isErr {
		t.Error("an unrecognised detail value should be an error result")
	}
}

func TestInboxToolEmptyCaseReadsSensibly(t *testing.T) {
	withHome(t)
	sid := "dddd4444-5555"
	t.Setenv(EnvSessionID, sid)
	liveEntry(t, sid, "me", "/tmp/me")

	got := toolText(t, "inbox", map[string]any{})
	if !strings.Contains(got, "No unread messages") {
		t.Errorf("got %q, want the empty-inbox message", got)
	}
	if !strings.Contains(got, "unread_only: false") {
		t.Errorf("got %q, want a hint pointing at unread_only: false", got)
	}

	got2 := toolText(t, "inbox", map[string]any{"unread_only": false})
	if got2 != "No messages." {
		t.Errorf("got %q, want the plain no-messages line when unread_only is false", got2)
	}
}

// The MCP process holds whatever session id CLAUDE_CODE_SESSION_ID has right
// now, which after a /clear is a fresh id the monitor and its spool never
// adopt. inbox must resolve identity through Self, the same fallback
// CurrentSender uses, not through CurrentSessionID -- guessing here would
// read, and mark read, a cursor file belonging to a session that never asked.
func TestInboxToolResolvesIdentityThroughSelfAfterAClear(t *testing.T) {
	withHome(t)
	const armedWith = "aaaa1111-2222-3333-4444-555555555555"
	want := cleared(t, armedWith, "ffff9999-8888-7777-6666-555555555555")

	if _, err := DefaultNamespace().Send(want, Draft{Text: "still reachable"}, Sender{Kind: "shell", Name: "sh"}); err != nil {
		t.Fatal(err)
	}
	if CurrentSessionID() == armedWith {
		t.Fatal("test setup did not actually diverge the environment's id from the armed one")
	}

	got := toolText(t, "inbox", map[string]any{})
	if !strings.Contains(got, "still reachable") {
		t.Errorf("inbox did not resolve identity through Self after a clear:\n%s", got)
	}
}

// A guessed session id would silently corrupt somebody else's read cursor, so
// the failure has to be a legible error, not a fallback to a plain-shell
// identity nobody asked for.
func TestInboxToolFailsClearlyWithNoSessionToResolve(t *testing.T) {
	withHome(t)
	t.Setenv(EnvSessionID, "")

	res, rerr := call(t, "tools/call", map[string]any{"name": "inbox", "arguments": map[string]any{}})
	if rerr != nil {
		t.Fatalf("unexpected protocol error: %v", rerr.Message)
	}
	m := res.(map[string]any)
	if isErr, _ := m["isError"].(bool); !isErr {
		t.Error("inbox with no session to resolve should be an error result")
	}
	content := m["content"].([]any)
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	if !strings.Contains(text, "identity") {
		t.Errorf("got %q, want a message about not being able to resolve identity", text)
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
