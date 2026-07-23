package pigeon

import (
	"strings"
	"testing"
)

// armed registers a session and holds its monitor lock for the rest of the
// test, which is exactly what makes a session "live" -- there is no flag for
// it, the lock is the signal.
func armed(t *testing.T, id, name string) *Entry {
	t.Helper()
	e := liveEntry(t, id, name, "/tmp/work")
	e.HeartbeatAt = nowRFC3339()
	if err := WriteEntry(e); err != nil {
		t.Fatalf("WriteEntry: %v", err)
	}
	lock, acquired, err := tryExclusive(LockPath(id))
	if err != nil || !acquired {
		t.Fatalf("take monitor lock: acquired=%v err=%v", acquired, err)
	}
	t.Cleanup(func() { lock.Close() })
	return e
}

func statusline(t *testing.T, stdin string, opts StatuslineOptions) string {
	t.Helper()
	var b strings.Builder
	if err := Statusline(strings.NewReader(stdin), &b, opts); err != nil {
		t.Fatalf("Statusline: %v", err)
	}
	return b.String()
}

// The whole design rests on this: a session that can receive gets no line at
// all. If a healthy session ever renders something, the widget becomes
// wallpaper and stops being read.
func TestStatuslineIsSilentWhenLive(t *testing.T) {
	withHome(t)
	armed(t, "aaaa1111", "alpha")
	t.Setenv(EnvSessionID, "aaaa1111")

	if got := statusline(t, "", StatuslineOptions{}); got != "" {
		t.Errorf("a live session rendered %q, want nothing", got)
	}
}

func TestStatuslineReportsDeafWithWaitingCount(t *testing.T) {
	withHome(t)
	beta := liveEntry(t, "bbbb2222", "beta", "/tmp/work")
	t.Setenv(EnvSessionID, "bbbb2222")

	for i := 0; i < 3; i++ {
		if _, err := Send(beta, "queued message", Sender{Kind: "shell", Name: "test"}, ""); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}

	got := statusline(t, "", StatuslineOptions{Plain: true})
	if !strings.Contains(got, "deaf") || !strings.Contains(got, "3 waiting") {
		t.Errorf("got %q, want deaf with 3 waiting", got)
	}
}

func TestStatuslineReportsDeafWithNoCountWhenSpoolIsEmpty(t *testing.T) {
	withHome(t)
	liveEntry(t, "bbbb2222", "beta", "/tmp/work")
	t.Setenv(EnvSessionID, "bbbb2222")

	got := strings.TrimSpace(statusline(t, "", StatuslineOptions{Plain: true}))
	if got != "pigeon deaf" {
		t.Errorf("got %q, want %q", got, "pigeon deaf")
	}
}

// An unregistered session is silently unreachable: the monitor never armed,
// and nothing else in the UI says so.
func TestStatuslineReportsNotArmed(t *testing.T) {
	withHome(t)
	t.Setenv(EnvSessionID, "cccc3333")

	got := statusline(t, "", StatuslineOptions{Plain: true})
	if !strings.Contains(got, "not armed") {
		t.Errorf("got %q, want not armed", got)
	}
}

func TestStatuslineIsSilentWhenOptedOut(t *testing.T) {
	withHome(t)
	liveEntry(t, "bbbb2222", "beta", "/tmp/work")
	t.Setenv(EnvSessionID, "bbbb2222")
	t.Setenv(EnvOptOut, "0")

	if got := statusline(t, "", StatuslineOptions{}); got != "" {
		t.Errorf("opted out rendered %q, want nothing", got)
	}
}

func TestStatuslineIsSilentWithNoSession(t *testing.T) {
	withHome(t)
	t.Setenv(EnvSessionID, "")

	if got := statusline(t, "", StatuslineOptions{}); got != "" {
		t.Errorf("got %q, want nothing", got)
	}
}

// Claude Code spawns the statusline per render without reliably passing
// CLAUDE_CODE_SESSION_ID, so the id on stdin is authoritative.
func TestStatuslineStdinBeatsEnvironment(t *testing.T) {
	withHome(t)
	armed(t, "aaaa1111", "alpha")
	liveEntry(t, "bbbb2222", "beta", "/tmp/work")
	t.Setenv(EnvSessionID, "aaaa1111") // live, would render nothing

	got := statusline(t, `{"session_id":"bbbb2222"}`, StatuslineOptions{Plain: true})
	if !strings.Contains(got, "deaf") {
		t.Errorf("got %q, want the stdin session's deaf line", got)
	}
}

// A malformed payload must not blank the line: falling back to the
// environment is strictly better than reporting nothing at all.
func TestStatuslineFallsBackToEnvOnBadInput(t *testing.T) {
	withHome(t)
	liveEntry(t, "bbbb2222", "beta", "/tmp/work")
	t.Setenv(EnvSessionID, "bbbb2222")

	for _, in := range []string{"not json", `{"session_id":123}`, `{"session_id":"../escape"}`, `{}`} {
		got := statusline(t, in, StatuslineOptions{Plain: true})
		if !strings.Contains(got, "deaf") {
			t.Errorf("input %q rendered %q, want the env session's line", in, got)
		}
	}
}

// A session id from stdin reaches a file path, so it is validated rather than
// trusted, exactly as everywhere else.
func TestStatuslineRejectsUnsafeSessionIDFromStdin(t *testing.T) {
	withHome(t)
	t.Setenv(EnvSessionID, "")

	if got := statusline(t, `{"session_id":"../../etc/passwd"}`, StatuslineOptions{}); got != "" {
		t.Errorf("got %q, want nothing", got)
	}
}

func TestStatuslinePlainDropsEmojiAndColour(t *testing.T) {
	withHome(t)
	liveEntry(t, "bbbb2222", "beta", "/tmp/work")
	t.Setenv(EnvSessionID, "bbbb2222")

	plain := statusline(t, "", StatuslineOptions{Plain: true})
	if strings.Contains(plain, "\x1b[") || strings.Contains(plain, "🕊") {
		t.Errorf("plain output is decorated: %q", plain)
	}
	fancy := statusline(t, "", StatuslineOptions{})
	if !strings.Contains(fancy, "\x1b[") || !strings.Contains(fancy, "🕊") {
		t.Errorf("default output is undecorated: %q", fancy)
	}
}

// --- pending ---------------------------------------------------------------

// Pending counts from the monitor's cursor, not from the top of the spool:
// after a monitor has read the file, a later message is the only one waiting.
func TestPendingCountsOnlyPastTheCursor(t *testing.T) {
	withHome(t)
	beta := liveEntry(t, "bbbb2222", "beta", "/tmp/work")
	from := Sender{Kind: "shell", Name: "test"}

	for i := 0; i < 2; i++ {
		if _, err := Send(beta, "read already", from, ""); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	// Simulate a monitor having consumed everything written so far.
	if err := mutateCursors("bbbb2222", func(c map[string]int64) {
		c[inboxCursorKey] = endOffset(SpoolPath("bbbb2222"))
	}); err != nil {
		t.Fatalf("mutateCursors: %v", err)
	}
	if got := Pending("bbbb2222"); got != 0 {
		t.Errorf("Pending after a full read = %d, want 0", got)
	}

	if _, err := Send(beta, "arrived after", from, ""); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := Pending("bbbb2222"); got != 1 {
		t.Errorf("Pending = %d, want 1", got)
	}
}

// A cursor beyond the end means the spool was truncated or replaced. Counting
// from zero over-reports; reporting nothing waiting would hide real mail.
func TestPendingRecoversFromAStaleCursor(t *testing.T) {
	withHome(t)
	beta := liveEntry(t, "bbbb2222", "beta", "/tmp/work")
	if _, err := Send(beta, "hello", Sender{Kind: "shell", Name: "test"}, ""); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := mutateCursors("bbbb2222", func(c map[string]int64) {
		c[inboxCursorKey] = 1 << 30
	}); err != nil {
		t.Fatalf("mutateCursors: %v", err)
	}
	if got := Pending("bbbb2222"); got != 1 {
		t.Errorf("Pending = %d, want 1", got)
	}
}

func TestPendingIsZeroForUnknownOrUnsafeSessions(t *testing.T) {
	withHome(t)
	for _, id := range []string{"", "../escape", "never-existed"} {
		if got := Pending(id); got != 0 {
			t.Errorf("Pending(%q) = %d, want 0", id, got)
		}
	}
}
