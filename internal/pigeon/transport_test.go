package pigeon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestValidTransportAcceptsOnlyTheThreeNames(t *testing.T) {
	for _, in := range []string{"auto", "socket", "monitor", "AUTO", " socket "} {
		if _, err := ValidTransport(in); err != nil {
			t.Errorf("ValidTransport(%q) = %v, want it accepted", in, err)
		}
	}
	// "spool" is the plausible wrong guess -- it is what the transport is
	// called everywhere else in this codebase -- so it is the one worth
	// asserting is rejected rather than quietly treated as the default.
	for _, in := range []string{"", "spool", "socket2", "auto,socket"} {
		if _, err := ValidTransport(in); err == nil {
			t.Errorf("ValidTransport(%q) was accepted, want an error", in)
		}
	}
}

// The precedence is the point of CurrentTransport, and each layer is asserted
// against the one below it rather than in isolation: a layer that reads
// correctly but never wins is the bug this catches.
func TestCurrentTransportPrecedence(t *testing.T) {
	home := withUserHome(t)
	cfg := filepath.Join(home, ".config", "pigeon", "config.json")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(c UserConfig) {
		b, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cfg, b, 0o600); err != nil {
			t.Fatal(err)
		}
		resetUserConfigForTest()
	}

	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv(EnvTransport, "")
	write(UserConfig{})
	if got, src := CurrentTransport(""); got != TransportAuto || src != "default" {
		t.Errorf("with nothing set = %q from %q, want auto from default", got, src)
	}

	write(UserConfig{Transport: "monitor"})
	if got, _ := CurrentTransport(""); got != TransportMonitor {
		t.Errorf("with only the config set = %q, want monitor", got)
	}

	t.Setenv(EnvTransport, "socket")
	if got, src := CurrentTransport(""); got != TransportSocket || src != EnvTransport {
		t.Errorf("env over config = %q from %q, want socket from %s", got, src, EnvTransport)
	}

	if got, src := CurrentTransport("auto"); got != TransportAuto || src != "--via" {
		t.Errorf("flag over env = %q from %q, want auto from --via", got, src)
	}
}

// A config file is read on every listing, so a typo in it must cost the
// preference and not the command. The env var is held to the same rule.
func TestUnusableTransportDegradesToAuto(t *testing.T) {
	home := withUserHome(t)
	cfg := filepath.Join(home, ".config", "pigeon", "config.json")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte(`{"transport":"carrier-pigeon"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	resetUserConfigForTest()
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv(EnvTransport, "")

	if c := LoadUserConfig(); c.Transport != "" {
		t.Errorf("LoadUserConfig kept %q, want an unusable transport dropped", c.Transport)
	}
	if got, _ := CurrentTransport(""); got != TransportAuto {
		t.Errorf("CurrentTransport = %q, want auto", got)
	}

	t.Setenv(EnvTransport, "carrier-pigeon")
	got, src := CurrentTransport("")
	if got != TransportAuto {
		t.Errorf("CurrentTransport with a bad env = %q, want auto", got)
	}
	if src == EnvTransport {
		t.Errorf("source = %q, want it to say the value was ignored", src)
	}
}

func TestPushedToSession(t *testing.T) {
	m := &Message{PushedTo: []string{"aaaa1111-2222", "bbbb3333-4444"}}
	if !pushedToSession(m, "bbbb3333-4444") {
		t.Error("a listed session was not recognised")
	}
	if pushedToSession(m, "cccc5555-6666") {
		t.Error("an unlisted session was recognised")
	}
	// An empty id must never match, or a message with an empty string in the
	// list would suppress notification for a session that has no id yet.
	if pushedToSession(&Message{PushedTo: []string{""}}, "") {
		t.Error("an empty session id matched")
	}
	if pushedToSession(nil, "aaaa1111-2222") {
		t.Error("a nil message matched")
	}
}

// publishTargets applies, from the sending side, the same gates RunMonitor's
// deliver applies from inside the recipient. This is the matrix that has to
// stay in step with that switch; if deliver changes, this test is the one that
// should start disagreeing.
func TestPublishTargetsAppliesTheMonitorsGates(t *testing.T) {
	withHome(t)
	ns := DefaultNamespace()

	sender := liveEntry(t, "5555aaaa-0000", "sender", "/tmp/sender")
	plain := liveEntry(t, "1111aaaa-0000", "plain", "/tmp/plain")
	digest := liveEntry(t, "2222aaaa-0000", "digester", "/tmp/digest")
	quiet := liveEntry(t, "3333aaaa-0000", "quieter", "/tmp/quiet")
	named := liveEntry(t, "4444aaaa-0000", "named", "/tmp/named")

	digest.Delivery = map[string]string{"ops": DeliveryDigest}
	quiet.Delivery = map[string]string{"ops": DeliveryQuiet}
	for _, e := range []*Entry{sender, plain, digest, quiet, named} {
		e.Subscriptions = []string{"ops"}
		if err := ns.WriteEntry(e); err != nil {
			t.Fatal(err)
		}
	}

	all := []*Entry{sender, plain, digest, quiet, named}
	names := func(entries []*Entry) []string {
		out := []string{}
		for _, e := range entries {
			out = append(out, e.Name)
		}
		return out
	}

	// A broadcast naming nobody: everyone but the two muted modes and the
	// sender itself.
	got := names(ns.publishTargets(&Message{Topic: "ops"}, all, sender.SessionID))
	if len(got) != 2 || got[0] != "plain" || got[1] != "named" {
		t.Errorf("plain broadcast targeted %v, want [plain named]", got)
	}

	// Addressed: only the session named, even though the others are on push.
	got = names(ns.publishTargets(&Message{Topic: "ops", For: []string{"named"}}, all, sender.SessionID))
	if len(got) != 1 || got[0] != "named" {
		t.Errorf("addressed broadcast targeted %v, want [named]", got)
	}

	// An alert does NOT buy a push to a digest or quiet subscriber. The monitor
	// may still choose to interrupt them; that decision is the monitor's,
	// because only it can withdraw the message again if it is superseded.
	got = names(ns.publishTargets(&Message{Topic: "ops", Priority: PriorityAlert}, all, sender.SessionID))
	if len(got) != 2 {
		t.Errorf("alert broadcast targeted %v, want the same two as a plain one", got)
	}
}

// TransportMonitor must not open a socket at all: it is the escape hatch for a
// caller that wants the monitor's rate limiting and notification budget, and a
// push behind its back would defeat the point of asking for it.
func TestWakeDoesNothingOnTheMonitorTransport(t *testing.T) {
	withHome(t)
	ns := DefaultNamespace()
	e := liveEntry(t, "1111bbbb-0000", "peer", "/tmp/peer")

	pushed, problems := ns.wake(TransportMonitor, &Message{ID: "m_1", Text: "hi"}, []*Entry{e})
	if len(pushed) != 0 || len(problems) != 0 {
		t.Errorf("wake on the monitor transport pushed %v / %v, want nothing", pushed, problems)
	}
}

// An unreachable recipient under auto is not an error: the message still lands
// on the spool and the monitor announces it, which is the whole fallback. Under
// --via socket the same failure IS reported, because the caller asked to be
// told rather than to be quietly downgraded.
func TestWakeFallsBackUnderAutoAndReportsUnderSocket(t *testing.T) {
	withHome(t)
	// No Claude Code registry here, so nothing this test addresses can be
	// resolved to a socket.
	t.Setenv(EnvConfigDir, t.TempDir())
	ns := DefaultNamespace()
	e := liveEntry(t, "1111cccc-0000", "peer", "/tmp/peer")
	msg := &Message{ID: "m_1", Text: "hi", To: e.SessionID}

	pushed, problems := ns.wake(TransportAuto, msg, []*Entry{e})
	if len(pushed) != 0 {
		t.Errorf("auto pushed %v, want nothing", pushed)
	}
	if len(problems) != 0 {
		t.Errorf("auto reported %v, want silence so the monitor can carry it", problems)
	}

	if _, problems = ns.wake(TransportSocket, msg, []*Entry{e}); len(problems) != 1 {
		t.Errorf("--via socket reported %v, want exactly one problem", problems)
	}
}

// The record is what makes a pigeon message a message rather than an
// interruption, so it must be complete whichever transport carried it.
func TestSendRecordsTheMessageWhicheverTransportIsUsed(t *testing.T) {
	withHome(t)
	t.Setenv(EnvConfigDir, t.TempDir())
	ns := DefaultNamespace()
	to := liveEntry(t, "1111dddd-0000", "peer", "/tmp/peer")

	msg, err := ns.Send(to, Draft{Text: "the build is green", Subject: "green"}, Sender{Kind: "shell"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(msg.PushedTo) != 0 {
		t.Errorf("PushedTo = %v, want empty when nothing could be pushed", msg.PushedTo)
	}
	b, err := os.ReadFile(ns.SpoolPath(to.SessionID))
	if err != nil {
		t.Fatalf("reading the spool: %v", err)
	}
	got, err := ParseMessage(string(b))
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	if got.ID != msg.ID || got.Text != "the build is green" {
		t.Errorf("spool holds %+v, want the message that was sent", got)
	}
}

// AnnotateReach must only ever promote deaf. Promoting a dead entry would hide
// a session whose process is gone behind a status that reads as reachable.
func TestAnnotateReachOnlyPromotesDeaf(t *testing.T) {
	withHome(t)
	t.Setenv(EnvConfigDir, t.TempDir())
	entries := []*Entry{
		{SessionID: "1111eeee-0000", Status: StatusLive},
		{SessionID: "2222eeee-0000", Status: StatusDead},
		{SessionID: "3333eeee-0000", Status: StatusDeaf},
	}
	AnnotateReach(entries)
	if entries[0].Status != StatusLive {
		t.Errorf("live became %q", entries[0].Status)
	}
	if entries[1].Status != StatusDead {
		t.Errorf("dead became %q", entries[1].Status)
	}
	// Unreachable here, so it stays deaf; the promotion itself is exercised
	// end to end rather than against a fabricated socket.
	if entries[2].Status != StatusDeaf {
		t.Errorf("deaf became %q with no socket to reach", entries[2].Status)
	}
}
