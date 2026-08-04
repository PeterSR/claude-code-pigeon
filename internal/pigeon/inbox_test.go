package pigeon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// appendRawMessage writes one message straight to a log file, bypassing
// Publish/Send. It exists so a test can pin an exact TS: Publish stamps
// second-resolution timestamps, and several tests here need messages from
// different sources to sort in a specific, deterministic order.
func appendRawMessage(t *testing.T, path string, m *Message) {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
}

// --- Task 1: ReadInbox -------------------------------------------------------

// TestReadInboxUnreadOnlySkipsAlreadyNotifiedHistory guards the "not to zero"
// rule: a session pulling for the first time must resume from its monitor
// cursor, not dump its whole history into context just because it has never
// called ReadInbox before.
func TestReadInboxUnreadOnlySkipsAlreadyNotifiedHistory(t *testing.T) {
	withHome(t)
	sid := "aaaa1111-2222"
	liveEntry(t, sid, "alpha", "/tmp/a")
	if err := Subscribe(sid, "chatter"); err != nil {
		t.Fatal(err)
	}

	from := Sender{Kind: "shell", Name: "sh"}
	for i := 0; i < 2; i++ {
		if _, err := Publish("chatter", Draft{Text: fmt.Sprintf("old-%d", i)}, from); err != nil {
			t.Fatal(err)
		}
	}
	// Simulate a monitor that already ingested (and notified) these two: park
	// the MONITOR cursor at the current end of the log, exactly what
	// followSource does after a read pass.
	notified := endOffset(TopicPath("chatter"))
	if err := mutateCursors(sid, func(m map[string]int64) { m["chatter"] = notified }); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := Publish("chatter", Draft{Text: fmt.Sprintf("new-%d", i)}, from); err != nil {
			t.Fatal(err)
		}
	}

	items, err := DefaultNamespace().ReadInbox(sid, InboxQuery{UnreadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2: the whole 4-message history came back instead of "+
			"just what followed the monitor cursor", len(items))
	}
	for _, it := range items {
		if len(it.Message.Text) >= 4 && it.Message.Text[:4] == "old-" {
			t.Errorf("ReadInbox returned pre-cursor history: %q", it.Message.Text)
		}
	}
}

// TestReadInboxMarkReadAdvancesOnlyTheConsumptionCursor guards the two cursor
// families staying separate: a pull must never move the cursor the monitor
// uses to decide what to notify.
func TestReadInboxMarkReadAdvancesOnlyTheConsumptionCursor(t *testing.T) {
	withHome(t)
	sid := "bbbb2222-3333"
	liveEntry(t, sid, "beta", "/tmp/b")
	if err := Subscribe(sid, "updates"); err != nil {
		t.Fatal(err)
	}
	if _, err := Publish("updates", Draft{Text: "hello"}, Sender{Kind: "shell", Name: "sh"}); err != nil {
		t.Fatal(err)
	}
	monitorBefore := readCursors(sid)["updates"]

	items, err := DefaultNamespace().ReadInbox(sid, InboxQuery{MarkRead: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}

	got := readCursors(sid)
	if got["updates"] != monitorBefore {
		t.Errorf("MarkRead moved the monitor cursor from %d to %d", monitorBefore, got["updates"])
	}
	if got["read:updates"] == 0 {
		t.Error("MarkRead did not advance the consumption cursor read:updates")
	}
	if got["readat:updates"] == 0 {
		t.Error("MarkRead did not stamp readat:updates")
	}
}

// TestReadInboxLimitTruncationLeavesACrowdedOutSourceForTheNextPull covers a
// Limit that truncates one source's contribution to zero because a busier
// source's newer messages fill every slot. The crowded-out source's
// consumption cursor must not move -- it contributed nothing -- so its
// message surfaces on the very next pull once the busy source stops
// competing for the same Limit.
func TestReadInboxLimitTruncationLeavesACrowdedOutSourceForTheNextPull(t *testing.T) {
	withHome(t)
	sid := "cccc3333-4444"
	liveEntry(t, sid, "gamma", "/tmp/c")
	if err := Subscribe(sid, "busy"); err != nil {
		t.Fatal(err)
	}

	from := Sender{Kind: "shell", Name: "sh"}
	// The direct spool holds one message, older than anything on the topic.
	appendRawMessage(t, SpoolPath(sid), &Message{
		ID: "m_direct", TS: "2030-01-01T00:00:00Z", From: from, Text: "direct-old",
	})
	// The topic holds three messages, all newer -- more than the Limit of 3
	// leaves room for once the direct spool's one message is in the mix too.
	for i, ts := range []string{"2030-01-01T00:00:01Z", "2030-01-01T00:00:02Z", "2030-01-01T00:00:03Z"} {
		appendRawMessage(t, TopicPath("busy"), &Message{
			ID: fmt.Sprintf("m_busy%d", i), TS: ts, From: from, Topic: "busy",
			Text: fmt.Sprintf("busy-%d", i),
		})
	}

	items, err := DefaultNamespace().ReadInbox(sid, InboxQuery{Limit: 3, UnreadOnly: true, MarkRead: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("first pull got %d items, want 3", len(items))
	}
	for _, it := range items {
		if it.Source != "busy" {
			t.Errorf("first pull returned a non-topic item %+v; the direct message should "+
				"have lost the Limit cut entirely, not partially", it)
		}
	}

	if _, ok := readCursors(sid)["read::inbox"]; ok {
		t.Error("the direct spool contributed nothing to the first pull, so its consumption " +
			"cursor must not have moved")
	}

	// Second pull: the topic is now fully consumed, so its messages no longer
	// compete for the Limit, and the direct spool's message -- excluded, not
	// lost, by the first call -- is what comes back.
	items, err = DefaultNamespace().ReadInbox(sid, InboxQuery{Limit: 3, UnreadOnly: true, MarkRead: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("second pull got %d items, want 1 (the direct message left over from the first call)",
			len(items))
	}
	if items[0].Message.Text != "direct-old" {
		t.Errorf("second pull returned %q, want the direct spool's message", items[0].Message.Text)
	}
}

// TestReadInboxSkipsItsOwnPublishedMessage guards the same rule
// RunMonitor applies to pushed notifications (monitor.go:162-165): a session
// never receives its own broadcast back, on the pull path either.
func TestReadInboxSkipsItsOwnPublishedMessage(t *testing.T) {
	withHome(t)
	sid := "dddd4444-5555"
	liveEntry(t, sid, "delta", "/tmp/d")
	if err := Subscribe(sid, "chat"); err != nil {
		t.Fatal(err)
	}
	if _, err := Publish("chat", Draft{Text: "from myself"}, Sender{Kind: "session", SessionID: sid}); err != nil {
		t.Fatal(err)
	}
	if _, err := Publish("chat", Draft{Text: "from someone else"}, Sender{Kind: "shell", Name: "sh"}); err != nil {
		t.Fatal(err)
	}

	items, err := DefaultNamespace().ReadInbox(sid, InboxQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1: this session's own broadcast must not come back", len(items))
	}
	if items[0].Message.Text != "from someone else" {
		t.Errorf("got %q", items[0].Message.Text)
	}
}

// --- Task 2: compaction respects consumption cursors -------------------------

// TestPruneTopicsCompactsWhenNoSessionHasEverPulled is the regression the
// brief calls out by name: readCursors returns a plain map, so a missing
// consumption cursor reads back as 0 if it is trusted directly. That would
// collapse `slowest` to 0 and stop compaction fleet-wide -- exactly the state
// of every session on the day this shipped, since none has a consumption
// cursor yet.
func TestPruneTopicsCompactsWhenNoSessionHasEverPulled(t *testing.T) {
	requireRenameOverOpenFile(t)
	withHome(t)
	sid := "eeee5555-6666"
	liveEntry(t, sid, "echo", "/tmp/e")
	if err := Subscribe(sid, "busy"); err != nil {
		t.Fatal(err)
	}
	from := Sender{Kind: "shell", Name: "sh"}
	body := strings.Repeat("x", 250)
	for i := 0; i < 600; i++ {
		if _, err := Publish("busy", Draft{Text: body}, from); err != nil {
			t.Fatal(err)
		}
	}
	full, err := os.Stat(TopicPath("busy"))
	if err != nil {
		t.Fatal(err)
	}
	if full.Size() < minCompactBytes {
		t.Skipf("log is only %d bytes, below the %d compaction threshold", full.Size(), minCompactBytes)
	}
	// Only the monitor cursor exists here -- no session has ever called
	// ReadInbox, which is the fleet-wide state on day one.
	if err := mutateCursors(sid, func(m map[string]int64) { m["busy"] = full.Size() }); err != nil {
		t.Fatal(err)
	}
	if _, ok := readCursors(sid)["read:busy"]; ok {
		t.Fatal("test setup accidentally created a consumption cursor")
	}

	res, err := PruneTopics()
	if err != nil {
		t.Fatal(err)
	}
	if res.TopicsCompacted != 1 {
		t.Fatalf("TopicsCompacted = %d, want 1: a naive fix that trusts a missing consumption "+
			"cursor's zero-value read would stop compaction here", res.TopicsCompacted)
	}
	if res.AbandonedCursors != 0 {
		t.Errorf("AbandonedCursors = %d, want 0: there was no consumption cursor to abandon", res.AbandonedCursors)
	}
	after, err := os.Stat(TopicPath("busy"))
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != 0 {
		t.Errorf("log is %d bytes after every subscriber's monitor read all of it, want 0", after.Size())
	}
}

// TestPruneTopicsDoesNotCutPastAFreshConsumptionCursor is the flip side: a
// consumption cursor genuinely lags the monitor cursor, and is recent, so
// compaction must stop at it rather than at the monitor's more advanced
// position.
func TestPruneTopicsDoesNotCutPastAFreshConsumptionCursor(t *testing.T) {
	requireRenameOverOpenFile(t)
	withHome(t)
	sid := "ffff6666-7777"
	liveEntry(t, sid, "foxtrot", "/tmp/f")
	if err := Subscribe(sid, "busy"); err != nil {
		t.Fatal(err)
	}
	from := Sender{Kind: "shell", Name: "sh"}
	body := strings.Repeat("x", 250)
	for i := 0; i < 600; i++ {
		if _, err := Publish("busy", Draft{Text: body}, from); err != nil {
			t.Fatal(err)
		}
	}
	full, err := os.Stat(TopicPath("busy"))
	if err != nil {
		t.Fatal(err)
	}
	if full.Size() < 2*minCompactBytes {
		t.Skipf("log is only %d bytes, too small to carve a read cursor comfortably below it", full.Size())
	}
	readPos := full.Size() / 2

	// The monitor has ingested (and notified) the whole log, but this session
	// has only actually pulled half of it, and did so a moment ago.
	if err := mutateCursors(sid, func(m map[string]int64) {
		m["busy"] = full.Size()
		m["read:busy"] = readPos
		m["readat:busy"] = time.Now().Unix()
	}); err != nil {
		t.Fatal(err)
	}

	res, err := PruneTopics()
	if err != nil {
		t.Fatal(err)
	}
	if res.AbandonedCursors != 0 {
		t.Errorf("AbandonedCursors = %d, want 0: the cursor is fresh", res.AbandonedCursors)
	}
	after, err := os.Stat(TopicPath("busy"))
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != full.Size()-readPos {
		t.Errorf("log is %d bytes after compaction, want %d: it must cut to the consumption "+
			"cursor, not the further-ahead monitor cursor", after.Size(), full.Size()-readPos)
	}
}

// TestPruneTopicsCutsPastAConsumptionCursorAbandonedByAge covers a session
// that pulled once, a long time ago, and never came back. Pinning the log
// open for it forever would be worse than the message loss the cursor exists
// to prevent.
func TestPruneTopicsCutsPastAConsumptionCursorAbandonedByAge(t *testing.T) {
	requireRenameOverOpenFile(t)
	withHome(t)
	sid := "gggg7777-8888"
	liveEntry(t, sid, "golf", "/tmp/g")
	if err := Subscribe(sid, "busy"); err != nil {
		t.Fatal(err)
	}
	from := Sender{Kind: "shell", Name: "sh"}
	body := strings.Repeat("x", 250)
	for i := 0; i < 600; i++ {
		if _, err := Publish("busy", Draft{Text: body}, from); err != nil {
			t.Fatal(err)
		}
	}
	full, err := os.Stat(TopicPath("busy"))
	if err != nil {
		t.Fatal(err)
	}
	if full.Size() < minCompactBytes {
		t.Skipf("log is only %d bytes, below the %d compaction threshold", full.Size(), minCompactBytes)
	}

	stale := time.Now().Add(-(maxUnreadAge + time.Hour)).Unix()
	if err := mutateCursors(sid, func(m map[string]int64) {
		m["busy"] = full.Size()
		m["read:busy"] = full.Size() / 4 // far short of caught up
		m["readat:busy"] = stale
	}); err != nil {
		t.Fatal(err)
	}

	res, err := PruneTopics()
	if err != nil {
		t.Fatal(err)
	}
	if res.AbandonedCursors != 1 {
		t.Errorf("AbandonedCursors = %d, want 1", res.AbandonedCursors)
	}
	after, err := os.Stat(TopicPath("busy"))
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != 0 {
		t.Errorf("log is %d bytes after an age-abandoned cursor, want 0: it should have fallen "+
			"back to the monitor cursor", after.Size())
	}
}

// TestPruneTopicsCutsPastAConsumptionCursorAbandonedByBytes covers a session
// whose consumption cursor is fresh by the clock but has fallen far enough
// behind the monitor cursor that waiting on it is no longer worth it.
func TestPruneTopicsCutsPastAConsumptionCursorAbandonedByBytes(t *testing.T) {
	requireRenameOverOpenFile(t)
	withHome(t)
	sid := "hhhh8888-9999"
	liveEntry(t, sid, "hotel", "/tmp/h")
	if err := Subscribe(sid, "bulk"); err != nil {
		t.Fatal(err)
	}

	// The content does not need to be valid messages: pruneTopicDir cuts on
	// byte position alone. A single big write is far cheaper than publishing
	// enough real messages to cross 1 MiB.
	size := int64(maxUnreadBytes + 2*minCompactBytes)
	if err := os.WriteFile(TopicPath("bulk"), bytes.Repeat([]byte("x"), int(size)), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := mutateCursors(sid, func(m map[string]int64) {
		m["bulk"] = size                     // the monitor has "seen" the whole thing
		m["read:bulk"] = 0                   // this session has pulled none of it
		m["readat:bulk"] = time.Now().Unix() // ...but just now, so age is not why
	}); err != nil {
		t.Fatal(err)
	}

	res, err := PruneTopics()
	if err != nil {
		t.Fatal(err)
	}
	if res.AbandonedCursors != 1 {
		t.Errorf("AbandonedCursors = %d, want 1", res.AbandonedCursors)
	}
	after, err := os.Stat(TopicPath("bulk"))
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != 0 {
		t.Errorf("log is %d bytes after a byte-abandoned cursor, want 0: it should have fallen "+
			"back to the monitor cursor", after.Size())
	}
}

// The Limit cut is taken across sources ordered by time, so it can land in the
// middle of one source's run: the newest message from a quiet source survives
// while an older one from the same source does not. Advancing that source's
// cursor to the newest returned message would step over the older one, which
// nobody has seen and nothing would show again.
func TestReadInboxDoesNotAdvancePastAGapInsideOneSource(t *testing.T) {
	withHome(t)
	me := armed(t, "aaaa1111", "me")
	ns := CurrentNamespace()
	if err := ns.Subscribe(me.SessionID, "chatter"); err != nil {
		t.Fatal(err)
	}

	spool := ns.SpoolPath(me.SessionID)
	topic := ns.TopicPath("chatter")
	peer := func(id, name string) Sender {
		return Sender{Kind: "session", SessionID: id, Name: name, Namespace: ns.String()}
	}

	// The direct spool is the quiet source: one old message, one new one. The
	// topic is busy in between and will win the Limit cut on its own.
	appendRawMessage(t, spool, &Message{
		ID: "m_old", TS: "2026-08-04T10:00:00Z", From: peer("bbbb2222", "quiet"), Text: "A-old",
	})
	for i, ts := range []string{
		"2026-08-04T10:01:00Z", "2026-08-04T10:02:00Z",
		"2026-08-04T10:03:00Z", "2026-08-04T10:04:00Z",
	} {
		appendRawMessage(t, topic, &Message{
			ID:    fmt.Sprintf("m_b%d", i),
			TS:    ts,
			From:  peer("cccc3333", "busy"),
			Topic: "chatter",
			Text:  fmt.Sprintf("B-%d", i),
		})
	}
	appendRawMessage(t, spool, &Message{
		ID: "m_new", TS: "2026-08-04T10:05:00Z", From: peer("bbbb2222", "quiet"), Text: "A-new",
	})

	got, err := ns.ReadInbox(me.SessionID, InboxQuery{Limit: 3, UnreadOnly: true, MarkRead: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 items, got %d", len(got))
	}

	rest, err := ns.ReadInbox(me.SessionID, InboxQuery{Limit: 50, UnreadOnly: true, MarkRead: true})
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, it := range rest {
		texts = append(texts, it.Message.Text)
	}
	for _, want := range []string{"A-old"} {
		found := false
		for _, g := range texts {
			if g == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s was skipped by the cursor and is now unreachable; second pull returned %v", want, texts)
		}
	}
}
