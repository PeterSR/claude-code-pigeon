package pigeon

import (
	"path/filepath"
	"strings"
	"testing"
)

// Pending counts from the monitor's cursor, not from the top of the spool:
// after a monitor has read the file, a later message is the only one waiting.
func TestPendingCountsOnlyPastTheCursor(t *testing.T) {
	withHome(t)
	beta := liveEntry(t, "bbbb2222", "beta", "/tmp/work")
	from := Sender{Kind: "shell", Name: "test"}

	for i := 0; i < 2; i++ {
		if _, err := Send(beta, Draft{Text: "read already"}, from); err != nil {
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

	if _, err := Send(beta, Draft{Text: "arrived after"}, from); err != nil {
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
	if _, err := Send(beta, Draft{Text: "hello"}, Sender{Kind: "shell", Name: "test"}); err != nil {
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

// TestRenderNeverBreaksItsInvariantsUnderALongPayloadPath sweeps the payload
// path length across the point where Render's give-up ladder fails entirely
// and falls through to its last-resort branches. Two things must hold at every
// length, subject or no subject: the line stays inside RenderBudget, and the
// payload pointer survives, because it is the only route to a message whose
// body did not fit. The subject is kept wherever it fits alongside the sender,
// which in this path is best-effort rather than guaranteed -- there is simply
// not always room, and the pointer outranks it.
//
// The sweep stops short of the length at which Render's last-resort branch
// truncates the pointer itself and emits a broken path. That is a pre-existing
// defect rather than anything the subject introduced; see IMPLEMENTATION-LOG.md.
func TestRenderNeverBreaksItsInvariantsUnderALongPayloadPath(t *testing.T) {
	withHome(t)
	ns := CurrentNamespace()
	if err := ns.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	const subject = "THE ONE THING WORTH KNOWING"
	keptSubject := 0
	for pad := 0; pad < 280; pad += 20 {
		m := &Message{
			ID:      "m_deadbeef",
			From:    Sender{Kind: "session", SessionID: "aaaa1111", Name: "peer", Namespace: ns.String()},
			Text:    "a body that may or may not survive the squeeze",
			Subject: subject,
			Payload: filepath.Join(ns.PayloadsDir(), strings.Repeat("d", pad)+".txt"),
		}
		got := ns.Render(m)
		if n := len([]rune(got)); n > RenderBudget {
			t.Fatalf("pad=%d: render exceeded the budget: %d runes", pad, n)
		}
		if !strings.Contains(got, m.Payload) {
			t.Fatalf("pad=%d: the payload pointer was dropped:\n%s", pad, got)
		}
		if strings.Contains(got, subject) {
			keptSubject++
		}
	}
	if keptSubject == 0 {
		t.Fatal("the subject was dropped at every payload length; it should survive the shorter ones")
	}
}
