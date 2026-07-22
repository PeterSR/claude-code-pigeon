package pigeon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- state locations -------------------------------------------------------

func TestHomeFallsBackToTheClaudeDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	// Everything below depends on this path, so getting the fallback wrong
	// silently splits a machine into two buses that cannot see each other.
	for _, unset := range []string{"", "   "} {
		t.Setenv(EnvHome, unset)
		want := filepath.Join(dir, ".claude", "pigeon")
		if got := Home(); got != want {
			t.Errorf("Home() with %s=%q = %q, want %q", EnvHome, unset, got, want)
		}
	}
	t.Setenv(EnvHome, "/somewhere/else")
	if got := Home(); got != "/somewhere/else" {
		t.Errorf("Home() = %q, want the %s override to win", got, EnvHome)
	}
}

func TestEnsureDirsIsOwnerOnly(t *testing.T) {
	withHome(t)
	// The spool is an injection surface into a live agent: anyone who can
	// write it can put text in someone's context, so it is never shared.
	for _, d := range []string{Home(), SessionsDir(), InboxDir(), PayloadsDir(), LocksDir(), TopicsDir(), CursorsDir()} {
		fi, err := os.Stat(d)
		if err != nil {
			t.Errorf("stat %s: %v", d, err)
			continue
		}
		if perm := fi.Mode().Perm(); perm != 0o700 {
			t.Errorf("%s has mode %04o, want 0700", d, perm)
		}
	}
}

// EnsureDirs is called from every write path, so a home that cannot be created
// has to surface as an error rather than a panic or a silent no-op.
func TestEnsureDirsReportsAnUnusableHome(t *testing.T) {
	dir := t.TempDir()
	blocked := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	t.Setenv(EnvHome, filepath.Join(blocked, "pigeon"))
	if err := EnsureDirs(); err == nil {
		t.Error("EnsureDirs() succeeded with a file in the way of the state tree")
	}
}

// --- environment ------------------------------------------------------------

func TestCurrentClaudePID(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want int
	}{
		{"unset", "", 0},
		{"a pid", "4242", 4242},
		{"padded", "  4242  ", 4242},
		// A garbage value must degrade to "no watchdog" rather than to pid 0,
		// which ProcessAlive would then have to special-case.
		{"not a number", "nonsense", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(EnvClaudePID, c.env)
			if got := CurrentClaudePID(); got != c.want {
				t.Errorf("CurrentClaudePID() = %d, want %d", got, c.want)
			}
		})
	}
}

func TestCurrentCwdPrefersTheProjectDir(t *testing.T) {
	// For the CLI the process cwd can be anywhere under the project, but the
	// session is addressed by its project basename, so the reported dir has to
	// be the one Claude Code declares.
	t.Setenv(EnvProjectDir, "/home/p/api-server")
	if got := CurrentCwd(); got != "/home/p/api-server" {
		t.Errorf("CurrentCwd() = %q, want the declared project dir", got)
	}
	t.Setenv(EnvProjectDir, "")
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if got := CurrentCwd(); got != wd {
		t.Errorf("CurrentCwd() = %q, want the process cwd %q", got, wd)
	}
}

func TestShellIdentityIsAddressless(t *testing.T) {
	id := ShellIdentity()
	if !strings.HasPrefix(id, "shell:") || !strings.Contains(id, "@") {
		t.Errorf("ShellIdentity() = %q, want shell:<user>@<host>", id)
	}
}

// --- message identity -------------------------------------------------------

func TestSenderDisplay(t *testing.T) {
	cases := []struct {
		name string
		s    Sender
		want string
	}{
		{"named", Sender{Kind: "session", SessionID: "aaaa1111-2222", Name: "alpha"}, "alpha"},
		{"unnamed session falls back to the short id", Sender{Kind: "session", SessionID: "aaaa1111-2222"}, "aaaa1111"},
		{"named shell", Sender{Kind: "shell", Name: "shell:p@h"}, "shell:p@h"},
		// Nothing to go on at all: still has to render as something, since the
		// display name goes straight into a notification line.
		{"empty", Sender{}, "unknown"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.s.Display(); got != c.want {
				t.Errorf("Display() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestParseMessageRejectsGarbage(t *testing.T) {
	// A corrupt spool line must be an error the follower can skip, not a panic.
	for _, bad := range []string{"", "not json", "{", `["array"]`} {
		if _, err := ParseMessage(bad); err == nil {
			t.Errorf("ParseMessage(%q) succeeded, want an error", bad)
		}
	}
}

// --- registry edges ---------------------------------------------------------

func TestWedgedMonitorIsReportedDeaf(t *testing.T) {
	withHome(t)
	const sid = "wedged-1111"
	e := liveEntry(t, sid, "alpha", "/tmp/a")
	// Lock held but the heartbeat has stopped: the monitor process is alive
	// and looks live by the lock alone, but it is no longer delivering, so it
	// must be reported the same as one that never started.
	e.HeartbeatAt = time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
	if err := WriteEntry(e); err != nil {
		t.Fatalf("WriteEntry: %v", err)
	}
	holdSessionLock(t, sid)

	got, err := ReadEntry(sid)
	if err != nil {
		t.Fatalf("ReadEntry: %v", err)
	}
	if got.Status != StatusDeaf {
		t.Errorf("Status = %q, want %q for a monitor holding the lock with a stale heartbeat",
			got.Status, StatusDeaf)
	}
}

func TestReadEntryRejectsUnsafeAndCorruptEntries(t *testing.T) {
	withHome(t)
	if _, err := ReadEntry("../../etc/passwd"); err == nil {
		t.Error("ReadEntry accepted a traversing session id")
	}
	if err := os.WriteFile(filepath.Join(SessionsDir(), "broken.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write entry: %v", err)
	}
	if _, err := ReadEntry("broken"); err == nil {
		t.Error("ReadEntry accepted a corrupt entry file")
	}
}

func TestWriteEntryRejectsAnUnsafeSessionID(t *testing.T) {
	withHome(t)
	// A session id becomes a path component, so this is the check that stops a
	// hostile value writing outside the state tree.
	if err := WriteEntry(&Entry{SessionID: "../escape"}); err == nil {
		t.Error("WriteEntry accepted a traversing session id")
	}
}

func TestMutateEntryOnAnUnregisteredSession(t *testing.T) {
	withHome(t)
	err := MutateEntry("ghost-1111", func(*Entry) error { return nil })
	if err == nil {
		t.Fatal("MutateEntry succeeded for a session that was never registered")
	}
	if !strings.Contains(err.Error(), "not registered") {
		t.Errorf("MutateEntry error = %q, want it to say the session is not registered", err)
	}
}

func TestProcStartOfAnImpossiblePID(t *testing.T) {
	// Used on every status check, including for entries whose pid is long gone.
	if got := ProcStart(-1); got != "" {
		t.Errorf("ProcStart(-1) = %q, want the empty string", got)
	}
	if ProcessAlive(-1, "") {
		t.Error("ProcessAlive(-1) = true")
	}
}

// --- topics edges -----------------------------------------------------------

func TestPublishSpillsOversizeBodyToPayload(t *testing.T) {
	withHome(t)
	long := strings.Repeat("b", BodyBudget*2)
	msg, err := Publish("deploys", long, Sender{Kind: "shell", Name: "sh"})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if msg.Payload == "" {
		t.Fatal("expected an overflow payload path for an oversize publish")
	}
	body, err := os.ReadFile(msg.Payload)
	if err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if string(body) != long {
		t.Error("payload file does not hold the full published text")
	}
	// Subscribers get a pointer, and the pointer must survive the clip.
	if !strings.Contains(Render(msg), msg.Payload) {
		t.Errorf("rendered line does not point at the payload:\n%s", Render(msg))
	}
}

func TestSubscribeRejectsAnUnsafeTopic(t *testing.T) {
	withHome(t)
	liveEntry(t, "aaaa1111-2222", "alpha", "/tmp/a")
	// Topic names become filenames, so this must fail before any cursor is
	// seeded or any entry is touched.
	if err := Subscribe("aaaa1111-2222", "../escape"); err == nil {
		t.Fatal("Subscribe accepted a traversing topic")
	}
	e, err := ReadEntry("aaaa1111-2222")
	if err != nil {
		t.Fatalf("ReadEntry: %v", err)
	}
	if len(e.Subscriptions) != 0 {
		t.Errorf("Subscriptions = %v after a rejected subscribe, want none", e.Subscriptions)
	}
}

func TestListTopicsCountsOnlyLiveSubscribers(t *testing.T) {
	withHome(t)
	// A log on disk with nobody listening still has to be listed, or a topic
	// somebody published to yesterday becomes invisible.
	if _, err := Publish("orphaned", "anyone?", Sender{Kind: "shell", Name: "sh"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	// A dead session's subscriptions must not inflate the count: it is exactly
	// the number that tells a publisher whether anyone will hear them.
	if err := WriteEntry(&Entry{SessionID: "dead-1111", PID: 0, Subscriptions: []string{"orphaned"}}); err != nil {
		t.Fatalf("WriteEntry: %v", err)
	}

	topics, err := ListTopics()
	if err != nil {
		t.Fatalf("ListTopics: %v", err)
	}
	byName := map[string]int{}
	for _, tp := range topics {
		byName[tp.Name] = tp.Subscribers
	}
	if n, ok := byName["orphaned"]; !ok {
		t.Errorf("a topic with a log but no subscribers was dropped: %+v", topics)
	} else if n != 0 {
		t.Errorf("#orphaned has %d subscribers, want 0 -- a dead session was counted", n)
	}
	if _, ok := byName[PublicTopic]; !ok {
		t.Errorf("%q is missing; it must always be listed", PublicTopic)
	}
	// Sorted output keeps `pigeon topics` stable between runs.
	for i := 1; i < len(topics); i++ {
		if topics[i-1].Name > topics[i].Name {
			t.Errorf("topics are not sorted: %+v", topics)
			break
		}
	}
}

func TestCursorsSurviveAnUnreadableFile(t *testing.T) {
	withHome(t)
	// A corrupt cursor file must degrade to "start from the beginning" rather
	// than take the monitor down on startup.
	if err := os.WriteFile(cursorPath("aaaa1111-2222"), []byte("{{{"), 0o600); err != nil {
		t.Fatalf("write cursor: %v", err)
	}
	if got := readCursors("aaaa1111-2222"); len(got) != 0 {
		t.Errorf("readCursors() = %v for a corrupt file, want an empty map", got)
	}
}
