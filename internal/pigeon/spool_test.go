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

// --- priority ----------------------------------------------------------

// Anything other than the empty string or PriorityAlert must be rejected at
// send time, on both paths: a typo or a made-up level must never reach a
// spool silently downgraded to "whatever the sender typed".
func TestPriorityRejectsAnythingButNormalOrAlert(t *testing.T) {
	withHome(t)
	beta := liveEntry(t, "bbbb2222", "beta", "/tmp/work")
	from := Sender{Kind: "shell", Name: "test"}

	for _, bad := range []string{"URGENT", "high", "ALERT", "Alert", " alert"} {
		if _, err := Send(beta, Draft{Text: "hi", Priority: bad}, from); err == nil {
			t.Errorf("Send with priority %q should have been rejected", bad)
		}
		if _, err := Publish("deploys", Draft{Text: "hi", Priority: bad}, from); err == nil {
			t.Errorf("Publish with priority %q should have been rejected", bad)
		}
	}
	for _, ok := range []string{"", PriorityAlert} {
		if _, err := Send(beta, Draft{Text: "hi", Priority: ok}, from); err != nil {
			t.Errorf("Send with priority %q was rejected: %v", ok, err)
		}
		if _, err := Publish("deploys", Draft{Text: "hi", Priority: ok}, from); err != nil {
			t.Errorf("Publish with priority %q was rejected: %v", ok, err)
		}
	}
}

// A message with no priority must render exactly as it did before Priority
// existed: nothing new appears in the line, and nothing is echoed for a field
// that carries no information.
func TestRenderNormalMessageIsByteIdenticalToBeforePriority(t *testing.T) {
	withHome(t)
	ns := CurrentNamespace()
	from := Sender{Kind: "session", SessionID: "aaaa1111", Name: "peer", Namespace: ns.String()}

	plain := &Message{ID: "m_test1", From: from, Text: "the build is green"}
	if got, want := ns.Render(plain), "[pigeon] message from peer :: the build is green [reply: pigeon send peer]"; got != want {
		t.Errorf("Render(plain) = %q, want %q", got, want)
	}

	topic := &Message{ID: "m_test2", From: from, Topic: "deploys", Text: "v2 rolled out"}
	want := "[pigeon #deploys] from peer :: v2 rolled out [reply: pigeon send peer] [topic: pigeon publish deploys]"
	if got := ns.Render(topic); got != want {
		t.Errorf("Render(topic) = %q, want %q", got, want)
	}

	global := &Message{ID: "m_test3", From: from, Topic: "@ops", Text: "everyone please stand by"}
	want = "[pigeon @ops] from peer [ns: default] :: everyone please stand by [reply: pigeon send peer] [topic: pigeon publish @ops]"
	if got := ns.Render(global); got != want {
		t.Errorf("Render(global) = %q, want %q", got, want)
	}
}

// An alert carries the "!" marker in every position a topic marker can
// appear, and nowhere else in the line.
func TestRenderAlertCarriesTheMarker(t *testing.T) {
	withHome(t)
	ns := CurrentNamespace()
	from := Sender{Kind: "session", SessionID: "aaaa1111", Name: "peer", Namespace: ns.String()}

	plain := &Message{ID: "m_a1", From: from, Text: "stop the deploy", Priority: PriorityAlert}
	if got, want := ns.Render(plain), "[pigeon !] message from peer :: stop the deploy [reply: pigeon send peer]"; got != want {
		t.Errorf("Render(plain alert) = %q, want %q", got, want)
	}

	topic := &Message{ID: "m_a2", From: from, Topic: "deploys", Text: "roll it back now", Priority: PriorityAlert}
	want := "[pigeon !#deploys] from peer :: roll it back now [reply: pigeon send peer] [topic: pigeon publish deploys]"
	if got := ns.Render(topic); got != want {
		t.Errorf("Render(topic alert) = %q, want %q", got, want)
	}

	global := &Message{ID: "m_a3", From: from, Topic: "@ops", Text: "everyone stop now", Priority: PriorityAlert}
	want = "[pigeon !@ops] from peer [ns: default] :: everyone stop now [reply: pigeon send peer] [topic: pigeon publish @ops]"
	if got := ns.Render(global); got != want {
		t.Errorf("Render(global alert) = %q, want %q", got, want)
	}
}

// A spool line is not always written through Send: it can be hand-edited or
// come from a version of pigeon that used a different vocabulary. Render must
// treat anything but the exact sentinel as normal rather than trusting it, and
// must never echo the raw value into the line -- a hostile or garbled value
// could otherwise forge its own marker.
func TestRenderTreatsAnUnrecognisedPriorityAsNormal(t *testing.T) {
	withHome(t)
	ns := CurrentNamespace()
	m := &Message{
		ID:       "m_bad1",
		From:     Sender{Kind: "session", SessionID: "aaaa1111", Name: "peer", Namespace: ns.String()},
		Text:     "just a normal update",
		Priority: "URGENT!!",
	}
	got := ns.Render(m)
	want := "[pigeon] message from peer :: just a normal update [reply: pigeon send peer]"
	if got != want {
		t.Errorf("Render(bogus priority) = %q, want %q", got, want)
	}
	if strings.Contains(got, "URGENT") {
		t.Errorf("the raw priority value was echoed into the line: %q", got)
	}
}
