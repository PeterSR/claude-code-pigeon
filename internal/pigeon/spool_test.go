package pigeon

import (
	"testing"
)

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
