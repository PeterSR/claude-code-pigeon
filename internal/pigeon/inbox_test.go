package pigeon

import (
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
func TestReadInboxTreatsEverythingSinceSubscribeAsUnread(t *testing.T) {
	withHome(t)
	sid := "aaaa1111-2222"
	liveEntry(t, sid, "alpha", "/tmp/a")
	if err := Subscribe(sid, "chatter"); err != nil {
		t.Fatal(err)
	}

	from := Sender{Kind: "shell", Name: "sh"}
	for i := 0; i < 4; i++ {
		if _, err := Publish("chatter", Draft{Text: fmt.Sprintf("msg-%d", i)}, from); err != nil {
			t.Fatal(err)
		}
	}
	// The monitor ingests and notifies all four, moving its own cursor to the
	// end. That must not consume them on the session's behalf: being told about
	// a message is not the same as having read it.
	if err := mutateCursors(sid, func(m map[string]int64) {
		m["chatter"] = endOffset(TopicPath("chatter"))
	}); err != nil {
		t.Fatal(err)
	}

	items, _, err := DefaultNamespace().ReadInbox(sid, InboxQuery{UnreadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 4 {
		t.Fatalf("got %d items, want 4: the monitor's ingest consumed messages the session never read", len(items))
	}
}

// A session registered before the consumption cursors existed has none, and
// must not have its entire backlog dumped into context on the first pull. With
// the key absent the monitor's own position is the only honest estimate of what
// it has already been shown.
func TestReadInboxFallsBackToTheMonitorCursorForALegacySession(t *testing.T) {
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
	notified := endOffset(TopicPath("chatter"))
	if err := mutateCursors(sid, func(m map[string]int64) {
		m["chatter"] = notified
		// Predate this feature: no consumption cursor was ever written.
		delete(m, readCursorKey("chatter"))
		delete(m, readAtCursorKey("chatter"))
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := Publish("chatter", Draft{Text: fmt.Sprintf("new-%d", i)}, from); err != nil {
			t.Fatal(err)
		}
	}

	items, _, err := DefaultNamespace().ReadInbox(sid, InboxQuery{UnreadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2: a legacy session got its whole history instead of what followed the monitor cursor", len(items))
	}
	for _, it := range items {
		if strings.HasPrefix(it.Message.Text, "old-") {
			t.Errorf("ReadInbox returned pre-cursor history: %q", it.Message.Text)
		}
	}
}

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

	items, _, err := DefaultNamespace().ReadInbox(sid, InboxQuery{UnreadOnly: true, MarkRead: true})
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

	items, _, err := DefaultNamespace().ReadInbox(sid, InboxQuery{})
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
	// A session registered before consumption cursors existed: only the
	// monitor cursor is on disk. A naive fix that trusted the missing key's
	// zero value would collapse the cut to 0 and stop compaction fleet-wide.
	if err := mutateCursors(sid, func(m map[string]int64) {
		m["busy"] = full.Size()
		delete(m, readCursorKey("busy"))
		delete(m, readAtCursorKey("busy"))
	}); err != nil {
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
// A big burst is not an abandoned session. An earlier rule cut once the gap
// passed a byte threshold, which destroyed the case the cursor exists for: the
// session pulls on time, a peer publishes a megabyte, prune runs a minute later
// and the whole burst is gone before the session ever asks for it.
func TestPruneTopicsKeepsAFreshCursorThroughALargeBurst(t *testing.T) {
	requireRenameOverOpenFile(t)
	withHome(t)
	sid := "ffff8888-9999"
	liveEntry(t, sid, "fresh", "/tmp/f")
	if err := Subscribe(sid, "burst"); err != nil {
		t.Fatal(err)
	}
	from := Sender{Kind: "shell", Name: "sh"}
	body := strings.Repeat("x", 250)
	for i := 0; i < 6000; i++ {
		if _, err := Publish("burst", Draft{Text: body}, from); err != nil {
			t.Fatal(err)
		}
	}
	full, err := os.Stat(TopicPath("burst"))
	if err != nil {
		t.Fatal(err)
	}
	if full.Size() < 1<<20 {
		t.Skipf("log is only %d bytes; this test needs more than a megabyte of gap", full.Size())
	}
	// The session read the first message a moment ago and has not caught up.
	if err := mutateCursors(sid, func(m map[string]int64) {
		m["burst"] = full.Size()
		m[readCursorKey("burst")] = 0
		m[readAtCursorKey("burst")] = time.Now().Unix()
	}); err != nil {
		t.Fatal(err)
	}

	res, err := PruneTopics()
	if err != nil {
		t.Fatal(err)
	}
	if res.TopicsCompacted != 0 {
		t.Fatalf("compaction cut past a cursor pulled seconds ago; a live reader lost %d bytes", res.BytesReclaimed)
	}
	after, err := os.Stat(TopicPath("burst"))
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != full.Size() {
		t.Fatalf("log shrank from %d to %d despite a fresh consumption cursor", full.Size(), after.Size())
	}
}

func TestReadInboxStillSeesWhatTheMonitorHasAlreadyIngested(t *testing.T) {
	withHome(t)
	me := armed(t, "aaaa1111", "me")
	ns := CurrentNamespace()
	if err := ns.Subscribe(me.SessionID, "chatter"); err != nil {
		t.Fatal(err)
	}
	ref, err := ParseTopicRef("chatter")
	if err != nil {
		t.Fatal(err)
	}
	if err := ns.seedCursor(me.SessionID, ref); err != nil {
		t.Fatal(err)
	}

	topic := ns.TopicPath("chatter")
	appendRawMessage(t, topic, &Message{
		ID: "m_seen", TS: nowRFC3339(), Topic: "chatter",
		From: Sender{Kind: "session", SessionID: "bbbb2222", Name: "peer", Namespace: ns.String()},
		Text: "published, then ingested by the monitor before anyone asked",
	})

	// Simulate the monitor ingesting it: its cursor moves to the end of the log.
	if err := ns.mutateCursors(me.SessionID, func(m map[string]int64) {
		m["chatter"] = readBase(topic) + endOffset(topic)
	}); err != nil {
		t.Fatal(err)
	}

	got, _, err := ns.ReadInbox(me.SessionID, InboxQuery{UnreadOnly: true, MarkRead: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("the monitor's ingest hid the message from the pull path; got %d items", len(got))
	}
	if got[0].Message.ID != "m_seen" {
		t.Fatalf("unexpected message: %+v", got[0].Message)
	}
}

// The day-one state now that Subscribe seeds a consumption cursor: the key
// exists but readat does not, because nobody has pulled. That must not hold the
// log open -- a session that only ever takes notifications would otherwise pin
// every topic it subscribes to for the whole staleness window. Pulling once is
// what opts a session into that protection.
func TestPruneTopicsCompactsForASeededButNeverPulledCursor(t *testing.T) {
	requireRenameOverOpenFile(t)
	withHome(t)
	sid := "eeee5555-7777"
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
	if err := mutateCursors(sid, func(m map[string]int64) { m["busy"] = full.Size() }); err != nil {
		t.Fatal(err)
	}
	cur := readCursors(sid)
	if _, ok := cur[readCursorKey("busy")]; !ok {
		t.Fatal("Subscribe no longer seeds a consumption cursor; this test is checking nothing")
	}
	if _, ok := cur[readAtCursorKey("busy")]; ok {
		t.Fatal("a seeded cursor must not carry a readat -- its absence is what marks it never-pulled")
	}

	res, err := PruneTopics()
	if err != nil {
		t.Fatal(err)
	}
	if res.TopicsCompacted != 1 {
		t.Fatalf("TopicsCompacted = %d, want 1: a never-pulled cursor held the log open", res.TopicsCompacted)
	}
}

// The invariant that matters for the pull path, and the one two earlier tests
// were too specific to catch: across repeated unread pulls every message is
// returned exactly once, whatever the Limit and however many sources there are.
//
// Taking the NEWEST Limit instead of the oldest satisfies every per-call
// assertion and still deadlocks here: the dropped and kept messages come from
// the same source, so no cursor may advance, and the head of the backlog
// becomes permanently unreachable while its tail is redelivered forever.
func TestReadInboxDrainsEveryMessageExactlyOnce(t *testing.T) {
	withHome(t)
	me := armed(t, "aaaa1111", "me")
	ns := CurrentNamespace()
	if err := ns.Subscribe(me.SessionID, "busy"); err != nil {
		t.Fatal(err)
	}
	peer := Sender{Kind: "session", SessionID: "bbbb2222", Name: "p", Namespace: ns.String()}

	// Interleave two sources so the Limit cut lands inside a source's run.
	want := map[string]bool{}
	for i := 0; i < 9; i++ {
		id := fmt.Sprintf("m_t%02d", i)
		appendRawMessage(t, ns.TopicPath("busy"), &Message{
			ID: id, TS: fmt.Sprintf("2026-08-04T10:%02d:00Z", i*2), Topic: "busy", From: peer,
			Text: id,
		})
		want[id] = true
		id = fmt.Sprintf("m_d%02d", i)
		appendRawMessage(t, ns.SpoolPath(me.SessionID), &Message{
			ID: id, TS: fmt.Sprintf("2026-08-04T10:%02d:30Z", i*2), From: peer, Text: id,
		})
		want[id] = true
	}

	seen := map[string]int{}
	for pull := 0; pull < 12; pull++ {
		got, more, err := ns.ReadInbox(me.SessionID, InboxQuery{Limit: 4, UnreadOnly: true, MarkRead: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == 0 {
			if more != 0 {
				t.Fatalf("pull %d returned nothing but claims %d more unread", pull, more)
			}
			break
		}
		for _, it := range got {
			seen[it.Message.ID]++
		}
	}

	for id := range want {
		switch seen[id] {
		case 1:
		case 0:
			t.Errorf("%s was never returned by any pull -- unreachable", id)
		default:
			t.Errorf("%s was returned %d times -- redelivered", id, seen[id])
		}
	}
	for id := range seen {
		if !want[id] {
			t.Errorf("unexpected message %s", id)
		}
	}
}

// A browse is not a consume. Marking read on "show me recent history" would
// make the same flag mean different things depending on how much history
// happened to exist, and would let a glance discard unread mail.
func TestReadInboxBrowseDoesNotConsume(t *testing.T) {
	withHome(t)
	me := armed(t, "aaaa1111", "me")
	ns := CurrentNamespace()
	peer := Sender{Kind: "session", SessionID: "bbbb2222", Name: "p", Namespace: ns.String()}
	appendRawMessage(t, ns.SpoolPath(me.SessionID), &Message{
		ID: "m_one", TS: nowRFC3339(), From: peer, Text: "still unread afterwards",
	})

	if _, _, err := ns.ReadInbox(me.SessionID, InboxQuery{UnreadOnly: false, MarkRead: true}); err != nil {
		t.Fatal(err)
	}
	got, _, err := ns.ReadInbox(me.SessionID, InboxQuery{UnreadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("a browse consumed unread mail; %d items still unread, want 1", len(got))
	}
}
