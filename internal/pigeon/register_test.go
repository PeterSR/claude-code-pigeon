package pigeon

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// hookPayload is what Claude Code writes to a lifecycle hook's stdin.
func hookPayload(event, sid, cwd string) *strings.Reader {
	return strings.NewReader(`{"session_id":"` + sid + `","cwd":"` + cwd +
		`","transcript_path":"/tmp/t.jsonl","hook_event_name":"` + event + `"}`)
}

// withHookEnv contains what the hooks do to the process they run in.
//
// adoptHookCwd sets CLAUDE_PROJECT_DIR with os.Setenv, which is right in
// production -- a hook is a dedicated short-lived process -- and a landmine in a
// test binary, where it would otherwise point every later test in the package at
// a since-deleted temp directory. Claiming the variable with t.Setenv first
// means the restore is registered before anything overwrites it.
//
// A live CLAUDE_PID is claimed for the same reason it is claimed in withHome:
// an entry stamped with the developer's real Claude process is one the socket
// transport can resolve, and a test has no business reaching a real session.
func withHookEnv(t *testing.T) {
	t.Helper()
	withHome(t)
	t.Setenv(EnvProjectDir, "")
	t.Setenv(EnvOptOut, "")
	t.Setenv(EnvSessionID, "")
}

// TestRegisterHookRegistersWithoutAMonitor is the property the whole design
// rests on: a session becomes addressable with no process of pigeon's left
// running afterwards.
func TestRegisterHookRegistersWithoutAMonitor(t *testing.T) {
	withHookEnv(t)

	sid := "hook1111-1111-1111-1111-111111111111"
	cwd := t.TempDir()

	var stderr strings.Builder
	if err := RegisterHook(hookPayload("SessionStart", sid, cwd), &stderr); err != nil {
		t.Fatalf("RegisterHook: %v", err)
	}

	ns := CurrentNamespace()
	e, err := ns.ReadEntry(sid)
	if err != nil {
		t.Fatalf("the session was not registered: %v", err)
	}
	if e.Cwd != cwd {
		t.Errorf("entry cwd = %q, want the payload's %q", e.Cwd, cwd)
	}
	if e.PID <= 0 {
		t.Error("entry has no pid, so nothing could resolve its socket or tell it from a dead session")
	}
	// Deaf, not dead: registered, nothing holding the monitor lock. That is the
	// state AnnotateReach promotes to `socket`, and it is what a machine with no
	// monitor is supposed to look like.
	if got := e.status(ns); got != StatusDeaf {
		t.Errorf("status = %q, want %q", got, StatusDeaf)
	}
	if _, err := os.Stat(ns.SpoolPath(sid)); err != nil {
		t.Errorf("no spool for the registered session: %v", err)
	}
}

// The payload names the session the event is ABOUT; the environment names the
// session this process was spawned from. On a resume they differ, and the event
// is the one that is right.
func TestRegisterHookPrefersThePayloadSessionID(t *testing.T) {
	withHookEnv(t)
	fromEnv := "envaaaaa-1111-1111-1111-111111111111"
	fromPayload := "payloadb-2222-2222-2222-222222222222"
	t.Setenv(EnvSessionID, fromEnv)

	if err := RegisterHook(hookPayload("SessionStart", fromPayload, t.TempDir()), &strings.Builder{}); err != nil {
		t.Fatalf("RegisterHook: %v", err)
	}
	ns := CurrentNamespace()
	if _, err := ns.ReadEntry(fromPayload); err != nil {
		t.Errorf("the payload's session was not registered: %v", err)
	}
	if _, err := ns.ReadEntry(fromEnv); err == nil {
		t.Error("registered the environment's session id as well; one session would hold two entries")
	}
}

// A hook with nothing usable on stdin still has to work, because the
// environment is a perfectly good second source and a session that fails to
// register is a session its peers cannot see.
func TestRegisterHookFallsBackToTheEnvironment(t *testing.T) {
	withHookEnv(t)
	sid := "envonly1-3333-3333-3333-333333333333"
	t.Setenv(EnvSessionID, sid)

	for _, in := range []string{"", "not json at all", "{}"} {
		if err := RegisterHook(strings.NewReader(in), &strings.Builder{}); err != nil {
			t.Fatalf("RegisterHook(%q): %v", in, err)
		}
		if _, err := CurrentNamespace().ReadEntry(sid); err != nil {
			t.Errorf("stdin %q left the session unregistered: %v", in, err)
		}
		CurrentNamespace().RemoveEntry(sid)
	}
}

// Opting out has to mean not appearing at all. Registering anyway would put an
// address on the bus for a session that asked to be off it.
func TestRegisterHookHonoursOptOut(t *testing.T) {
	withHookEnv(t)
	t.Setenv(EnvOptOut, "0")
	sid := "optout11-4444-4444-4444-444444444444"

	if err := RegisterHook(hookPayload("SessionStart", sid, t.TempDir()), &strings.Builder{}); err != nil {
		t.Fatalf("RegisterHook: %v", err)
	}
	if _, err := CurrentNamespace().ReadEntry(sid); err == nil {
		t.Error("an opted-out session was registered anyway")
	}
}

// A project that keeps its sessions off the bus keeps them off it here too. The
// hook is a new way in, and a new way in is exactly how a rule like this gets
// quietly lost.
func TestRegisterHookHonoursAProjectOptOut(t *testing.T) {
	withHookEnv(t)
	dir := writeProjectConfig(t, `{"enabled": false}`)
	sid := "projoff1-5555-5555-5555-555555555555"

	if err := RegisterHook(hookPayload("SessionStart", sid, dir), &strings.Builder{}); err != nil {
		t.Fatalf("RegisterHook: %v", err)
	}
	if _, err := CurrentNamespace().ReadEntry(sid); err == nil {
		t.Error("a session in a disabled checkout was registered anyway")
	}
}

// The tidy exit the monitor's deferred RemoveEntry used to be. Without it every
// session that ever ran would leave an entry for the next registration to sweep.
func TestDeregisterHookRemovesTheEntryAndKeepsTheSpool(t *testing.T) {
	withHookEnv(t)
	sid := "bye11111-6666-6666-6666-666666666666"
	cwd := t.TempDir()

	if err := RegisterHook(hookPayload("SessionStart", sid, cwd), &strings.Builder{}); err != nil {
		t.Fatalf("RegisterHook: %v", err)
	}
	ns := CurrentNamespace()
	if _, err := ns.Send(mailbox(sid), Draft{Text: "queued while it lived"}, Sender{Kind: "shell", Name: "sh"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if err := DeregisterHook(hookPayload("SessionEnd", sid, cwd), &strings.Builder{}); err != nil {
		t.Fatalf("DeregisterHook: %v", err)
	}
	if _, err := ns.ReadEntry(sid); err == nil {
		t.Error("the entry survived deregistration")
	}
	// The spool outlives the session on purpose: the same id can come back, and
	// mail queued for it should still be there when it does.
	b, err := os.ReadFile(ns.SpoolPath(sid))
	if err != nil {
		t.Fatalf("the spool was removed with the entry: %v", err)
	}
	if !strings.Contains(string(b), "queued while it lived") {
		t.Error("the spool was emptied by deregistration")
	}
}

// Deregistering something that was never registered must be silent rather than
// an error: SessionEnd fires for every session, including ones that opted out.
func TestDeregisterHookOnAnUnregisteredSession(t *testing.T) {
	withHookEnv(t)
	if err := DeregisterHook(hookPayload("SessionEnd", "ghost111-7777-7777-7777-777777777777", t.TempDir()), &strings.Builder{}); err != nil {
		t.Errorf("DeregisterHook on an unknown session: %v", err)
	}
}

// A SessionStart hook's stdout is fed back into the session as context, so
// anything printed there is tokens spent in every session for as long as the
// plugin is installed. What actually enforces that here is the signature --
// RegisterHook is handed no stdout writer at all -- and a test cannot prove the
// absence of an os.Stdout write, so this does not pretend to. What it checks is
// the half that is checkable and that a refactor could break: that the
// diagnostics exist, and that they go to the writer the model never sees.
func TestRegisterHookDiagnosticsGoToStderr(t *testing.T) {
	withHookEnv(t)
	sid := "quiet111-8888-8888-8888-888888888888"

	var stderr strings.Builder
	if err := RegisterHook(hookPayload("SessionStart", sid, t.TempDir()), &stderr); err != nil {
		t.Fatalf("RegisterHook: %v", err)
	}
	if !strings.Contains(stderr.String(), "registered") {
		t.Errorf("nothing logged to stderr, so a failed registration would be undiagnosable:\n%s", stderr.String())
	}
}

// TestPickClaudePIDPrefersALiveEnvironmentPID pins the ordering that a stale
// record on disk got wrong.
//
// Claude Code's session registry returns a single match WITHOUT checking that it
// is running, and those files outlive their processes. So a session killed hard
// and then resumed has a leftover record carrying its old pid, and preferring
// the registry wrote that dead pid into the entry -- leaving the resumed session
// hidden, unresolvable and due to be swept, which is the exact failure the hook
// exists to prevent.
func TestPickClaudePIDPrefersALiveEnvironmentPID(t *testing.T) {
	live := os.Getpid()
	dead := deadPID(t)
	const sid = "pick1111-1111-1111-1111-111111111111"

	stale := func(string) (int, string, bool) { return dead, "999999", true }
	if got := pickClaudePID(live, sid, stale); got != live {
		t.Errorf("with a live env pid and a stale record = %d, want the live pid %d", got, live)
	}

	// The registry is still the fallback when the environment has nothing.
	liveRecord := func(string) (int, string, bool) { return live, ProcStart(live), true }
	if got := pickClaudePID(0, sid, liveRecord); got != live {
		t.Errorf("with no env pid and a live record = %d, want %d", got, live)
	}

	// A record for a process that is gone decides nothing. Writing that pid
	// would produce an entry that looks answerable and is not.
	if got := pickClaudePID(0, sid, stale); got != 0 {
		t.Errorf("a dead record was accepted (%d); an entry with a dead pid is worse than one with none", got)
	}
	// Same for an environment variable naming a process that has gone.
	none := func(string) (int, string, bool) { return 0, "", false }
	if got := pickClaudePID(dead, sid, none); got != 0 {
		t.Errorf("a dead env pid was accepted (%d)", got)
	}
}

// A session whose process cannot be identified is registered anyway, but never
// quietly: everything downstream decides against that pid, so the entry exists
// and looks dead at the same time.
func TestRegisterHookSaysSoWhenItCannotIdentifyTheProcess(t *testing.T) {
	withHookEnv(t)
	t.Setenv(EnvClaudePID, strconv.Itoa(deadPID(t)))
	sid := "nopid111-aaaa-aaaa-aaaa-aaaaaaaaaaaa"

	var stderr strings.Builder
	if err := RegisterHook(hookPayload("SessionStart", sid, t.TempDir()), &stderr); err != nil {
		t.Fatalf("RegisterHook: %v", err)
	}
	if !strings.Contains(stderr.String(), "WARNING") {
		t.Errorf("registered with no usable pid and said nothing about it:\n%s", stderr.String())
	}
	e, err := CurrentNamespace().ReadEntry(sid)
	if err != nil {
		t.Fatalf("the session was not registered at all: %v", err)
	}
	if e.PID > 0 {
		t.Errorf("entry pid = %d, want 0 rather than a process that is gone", e.PID)
	}
}

// A hook is spawned by Claude Code, so it can be told which process the session
// is; the entry is worthless if it points anywhere else, because liveness,
// pruning and socket resolution are all decided against that pid.
func TestRegisterHookRecordsTheClaudeProcess(t *testing.T) {
	withHookEnv(t)
	t.Setenv(EnvClaudePID, strconv.Itoa(os.Getpid()))
	sid := "pid11111-9999-9999-9999-999999999999"

	if err := RegisterHook(hookPayload("SessionStart", sid, t.TempDir()), &strings.Builder{}); err != nil {
		t.Fatalf("RegisterHook: %v", err)
	}
	e, err := CurrentNamespace().ReadEntry(sid)
	if err != nil {
		t.Fatalf("ReadEntry: %v", err)
	}
	if e.PID != os.Getpid() {
		t.Errorf("entry pid = %d, want %d", e.PID, os.Getpid())
	}
	if e.ProcStart != ProcStart(os.Getpid()) {
		t.Error("entry has no start token, so a recycled pid would answer for this session")
	}
}
