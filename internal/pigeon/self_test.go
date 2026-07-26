package pigeon

import (
	"strings"
	"testing"
	"time"
)

// cleared registers a session under the id its monitor armed with, then points
// this process's environment at the id Claude Code minted after a clear: the
// exact split this file exists to close.
func cleared(t *testing.T, armedWith, nowKnownAs string) *Entry {
	t.Helper()
	e := armed(t, armedWith, "alpha")
	t.Setenv(EnvSessionID, nowKnownAs)
	clearedSession(t, nowKnownAs, armGrace+time.Hour)
	return e
}

// Self answers with the entry, not with the environment. Everything downstream
// -- who may reply to me, am I reachable, am I already armed -- is decided from
// this one answer, so it is the single place the split gets closed.
func TestSelfResolvesTheEntryAfterAClear(t *testing.T) {
	withHome(t)
	const armedWith = "aaaa1111-2222-3333-4444-555555555555"
	want := cleared(t, armedWith, "ffff9999-8888-7777-6666-555555555555")

	ns, e, err := Self()
	if err != nil {
		t.Fatalf("Self: %v", err)
	}
	if e.SessionID != want.SessionID {
		t.Errorf("Self() = %s, want the id the monitor armed with (%s)", e.SessionID, armedWith)
	}
	if !ns.Is(DefaultNamespace()) {
		t.Errorf("namespace = %s, want default", ns)
	}
	if got := SelfID(); got != armedWith {
		t.Errorf("SelfID() = %q, want %q", got, armedWith)
	}
}

// A plain shell has no session to resolve, and must not be handed somebody
// else's entry just because a claude process happens to be running.
func TestSelfFailsOutsideASession(t *testing.T) {
	withHome(t)
	armed(t, "aaaa1111-2222-3333-4444-555555555555", "alpha")
	t.Setenv(EnvSessionID, "")

	if _, _, err := Self(); err == nil {
		t.Error("Self succeeded outside a session")
	}
	if got := SelfID(); got != "" {
		t.Errorf("SelfID() = %q, want empty outside a session", got)
	}
}

// The reply address is the one field that must not lie. After a clear the
// environment's id names no spool at all, so stamping it would put an
// undeliverable return address on every message this session sends.
func TestCurrentSenderStampsTheAddressThatWorks(t *testing.T) {
	withHome(t)
	const armedWith = "aaaa1111-2222-3333-4444-555555555555"
	cleared(t, armedWith, "ffff9999-8888-7777-6666-555555555555")

	from := CurrentSender()
	if from.SessionID != armedWith {
		t.Errorf("from.SessionID = %s, want %s -- a reply must reach a live spool", from.SessionID, armedWith)
	}
	if from.Name != "alpha" {
		t.Errorf("from.Name = %q, want alpha", from.Name)
	}
	// The address only means anything alongside the namespace holding it.
	if from.Namespace != DefaultNamespace().String() {
		t.Errorf("from.Namespace = %q, want %q", from.Namespace, DefaultNamespace().String())
	}
}

// The stamp is what suppresses a session's own broadcast (see RunMonitor). With
// the environment's id on it, a publish from a shell spawned after a clear no
// longer matched the monitor's own id, so the session woke itself up.
func TestCurrentSenderMatchesTheMonitorsOwnID(t *testing.T) {
	withHome(t)
	const armedWith = "aaaa1111-2222-3333-4444-555555555555"
	cleared(t, armedWith, "ffff9999-8888-7777-6666-555555555555")

	// armedWith is what RunMonitor holds in `sid` for its whole life.
	if from := CurrentSender(); from.SessionID != armedWith {
		t.Errorf("from.SessionID = %s, monitor holds %s: the self-echo guard would miss",
			from.SessionID, armedWith)
	}
}

// Arming twice would put two monitors on one session. Telling a cleared session
// it has no monitor is exactly how that happens.
func TestArmSeesTheMonitorAfterAClear(t *testing.T) {
	withHome(t)
	cleared(t, "aaaa1111-2222-3333-4444-555555555555", "ffff9999-8888-7777-6666-555555555555")

	var b strings.Builder
	if err := Arm(&b); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	if !strings.Contains(b.String(), "Already armed") {
		t.Errorf("Arm said %q, want it to report the live monitor", b.String())
	}
}

// doctor's verdict drives what a user does next. "Not registered" for a
// reachable session sends them restarting a healthy monitor.
func TestDoctorSeesAClearedSessionAsLive(t *testing.T) {
	withHome(t)
	cleared(t, "aaaa1111-2222-3333-4444-555555555555", "ffff9999-8888-7777-6666-555555555555")

	var b strings.Builder
	_ = Doctor(&b, false)
	out := b.String()
	if strings.Contains(out, "not registered") {
		t.Errorf("doctor reported an armed session as unregistered:\n%s", out)
	}
	if !strings.Contains(out, "live, reachable") {
		t.Errorf("doctor did not report the session as live:\n%s", out)
	}
}

// A session whose monitor genuinely never armed must still be told so, by every
// one of these surfaces. The process fallback is an exact match, not a way of
// finding something to say.
func TestSelfStillFailsWhenNothingArmed(t *testing.T) {
	withHome(t)
	sid := "ffff9999-8888-7777-6666-555555555555"
	t.Setenv(EnvSessionID, sid)
	clearedSession(t, sid, armGrace+time.Hour) // a live process, but no entry

	if _, _, err := Self(); err == nil {
		t.Error("Self found an entry where none was registered")
	}
	if from := CurrentSender(); from.SessionID != sid {
		t.Errorf("from.SessionID = %s, want the environment's id when nothing is registered", from.SessionID)
	}
	var b strings.Builder
	if err := Arm(&b); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	if strings.Contains(b.String(), "Already armed") {
		t.Errorf("Arm claimed a monitor that does not exist: %q", b.String())
	}
}
