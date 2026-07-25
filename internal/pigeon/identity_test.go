package pigeon

import (
	"os"
	"testing"
)

func TestSyntheticSessionIDIsValidAndDistinct(t *testing.T) {
	sid := syntheticSessionID("inbox")
	if err := ValidSessionID(sid); err != nil {
		t.Errorf("syntheticSessionID(%q) = %q, which is not a valid session id: %v", "inbox", sid, err)
	}
	if !isEphemeralID(sid) {
		t.Errorf("isEphemeralID(%q) = false, want true", sid)
	}
	if got := ephemeralName(sid); got != "inbox" {
		t.Errorf("ephemeralName(%q) = %q, want %q", sid, got, "inbox")
	}
	// A real Claude Code session id (a UUID) must never be read as ephemeral.
	if isEphemeralID("6fd76a6f-32ac-48cf-9315-ce3de2cee988") {
		t.Error("a UUID was misread as an ephemeral id")
	}
}

func TestActingIdentityPrecedence(t *testing.T) {
	withHome(t)
	t.Setenv(EnvSessionID, "")
	t.Setenv(EnvAs, "")

	// Nothing set: a plain shell, no acting identity.
	if sid, _ := ActingIdentity(""); sid != "" {
		t.Errorf("no identity: sid = %q, want empty", sid)
	}

	// The standing preference is the lowest layer.
	if err := SetCLIIdentity("cfgname"); err != nil {
		t.Fatalf("SetCLIIdentity: %v", err)
	}
	if sid, origin := ActingIdentity(""); sid != syntheticSessionID("cfgname") || origin != CLIConfigPath() {
		t.Errorf("config layer: sid = %q origin = %q, want %q from %q", sid, origin, syntheticSessionID("cfgname"), CLIConfigPath())
	}

	// PIGEON_AS outranks the standing preference.
	t.Setenv(EnvAs, "envname")
	if sid, origin := ActingIdentity(""); sid != syntheticSessionID("envname") || origin != EnvAs {
		t.Errorf("env layer: sid = %q origin = %q, want the env value", sid, origin)
	}

	// A real Claude Code session outranks every ambient layer.
	t.Setenv(EnvSessionID, "real-session-1")
	if sid, origin := ActingIdentity(""); sid != "real-session-1" || origin != EnvSessionID {
		t.Errorf("real session: sid = %q origin = %q, want the real session", sid, origin)
	}

	// An explicit --as outranks even a real session.
	if sid, origin := ActingIdentity("flagname"); sid != syntheticSessionID("flagname") || origin != "--as" {
		t.Errorf("--as flag: sid = %q origin = %q, want the flag to win", sid, origin)
	}
}

func TestActingIdentityRejectsBadFlag(t *testing.T) {
	withHome(t)
	t.Setenv(EnvSessionID, "real-session-x")
	// A bad --as does not silently fall through to the real session: that would
	// stamp a message as an identity the caller did not ask for.
	if sid, _ := ActingIdentity("bad name"); sid != "" {
		t.Errorf("a bad --as resolved to %q, want empty", sid)
	}
}

// ActingName is the ephemeral-only view: it never adopts a real session, because
// a real session is not an inbox to open with `pigeon listen`.
func TestActingNameSkipsRealSession(t *testing.T) {
	withHome(t)
	t.Setenv(EnvSessionID, "real-session-2")
	t.Setenv(EnvAs, "")

	if err := SetCLIIdentity("cfgname"); err != nil {
		t.Fatalf("SetCLIIdentity: %v", err)
	}
	if name, _ := ActingName(""); name != "cfgname" {
		t.Errorf("ActingName ignored the standing preference in a real session: %q", name)
	}
	t.Setenv(EnvAs, "envname")
	if name, _ := ActingName(""); name != "envname" {
		t.Errorf("env should outrank config: %q", name)
	}
	if name, _ := ActingName("flagname"); name != "flagname" {
		t.Errorf("--as should win: %q", name)
	}
}

func TestActingSenderStampsPeerOnlyWhenLive(t *testing.T) {
	withHome(t)
	t.Setenv(EnvSessionID, "") // a plain shell
	t.Setenv(EnvAs, "")
	if err := SetCLIIdentity("ghost"); err != nil {
		t.Fatalf("SetCLIIdentity: %v", err)
	}

	// Identity set, but no inbox exists at all: a plain shell stamp.
	if s := ActingSender(""); s.Kind != "shell" {
		t.Errorf("no inbox: Kind = %q, want shell", s.Kind)
	}

	// A registered but deaf inbox (nobody holds the lock) is still not something
	// to promise a reply to.
	ns := DefaultNamespace()
	sid := syntheticSessionID("ghost")
	pid := os.Getpid()
	if err := ns.WriteEntry(&Entry{
		SessionID: sid, Name: "ghost", PID: pid, ProcStart: ProcStart(pid),
		StartedAt: nowRFC3339(), Ephemeral: true,
	}); err != nil {
		t.Fatalf("WriteEntry: %v", err)
	}
	if s := ActingSender(""); s.Kind != "shell" {
		t.Errorf("deaf inbox: Kind = %q, want shell", s.Kind)
	}

	// Hold the lock: now the inbox is live, and the stamp carries its address.
	lock, acquired, err := tryExclusive(ns.LockPath(sid))
	if err != nil || !acquired {
		t.Fatalf("hold lock: acquired = %v, err = %v", acquired, err)
	}
	defer lock.Close()

	s := ActingSender("")
	if s.Kind != "session" || s.Name != "ghost" || s.SessionID != sid {
		t.Errorf("live inbox: got %+v, want a session stamp for ghost", s)
	}
	if s.Addr() != "ghost" {
		t.Errorf("reply address = %q, want %q", s.Addr(), "ghost")
	}
}

// Inside a real Claude Code session, a bare send/publish stays the session, but
// an explicit --as still overrides it and stamps a live inbox.
func TestActingSenderFlagOverridesRealSession(t *testing.T) {
	withHome(t)
	t.Setenv(EnvSessionID, "real-session-9")
	t.Setenv(EnvAs, "")

	ns := DefaultNamespace()
	sid := syntheticSessionID("flagbot")
	pid := os.Getpid()
	if err := ns.WriteEntry(&Entry{
		SessionID: sid, Name: "flagbot", PID: pid, ProcStart: ProcStart(pid),
		StartedAt: nowRFC3339(), Ephemeral: true,
	}); err != nil {
		t.Fatalf("WriteEntry: %v", err)
	}
	lock, acquired, err := tryExclusive(ns.LockPath(sid))
	if err != nil || !acquired {
		t.Fatalf("hold lock: acquired = %v, err = %v", acquired, err)
	}
	defer lock.Close()

	// No flag: the real session wins, so its own id is stamped.
	if s := ActingSender(""); s.SessionID != "real-session-9" {
		t.Errorf("no flag: SessionID = %q, want the real session to win", s.SessionID)
	}
	// --as overrides even the real session and stamps the live inbox.
	if s := ActingSender("flagbot"); s.Kind != "session" || s.Name != "flagbot" || s.SessionID != sid {
		t.Errorf("--as over a real session: got %+v, want the flagbot inbox", s)
	}
}

// Setting one shell preference must never wipe the other: they share cli.json.
func TestSetCLIIdentityPreservesNamespace(t *testing.T) {
	withHome(t)
	if err := SetCLINamespace(mustNS(t, "acme")); err != nil {
		t.Fatalf("SetCLINamespace: %v", err)
	}
	if err := SetCLIIdentity("bot"); err != nil {
		t.Fatalf("SetCLIIdentity: %v", err)
	}
	if c := readCLIConfig(); c.Namespace != "acme" || c.As != "bot" {
		t.Errorf("cli.json = %+v, want namespace=acme as=bot", c)
	}
	// Clearing the identity leaves the namespace alone.
	if err := SetCLIIdentity(""); err != nil {
		t.Fatalf("SetCLIIdentity(clear): %v", err)
	}
	if c := readCLIConfig(); c.Namespace != "acme" || c.As != "" {
		t.Errorf("after clear: cli.json = %+v, want namespace=acme as=empty", c)
	}
}
