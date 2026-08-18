package pigeon

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// pollInterval is how often a follower checks its source for new bytes. This
// is process-local file polling and costs no tokens; the constraint that
// matters is that the *session* never polls.
const pollInterval = 200 * time.Millisecond

// subCheckInterval is how quickly a subscribe/unsubscribe takes effect in a
// running monitor.
const subCheckInterval = time.Second

// maxPerMinute caps emitted notifications. Claude Code stops monitors that
// produce too many events, so we suppress and report rather than get killed.
const maxPerMinute = 30

// digestInterval is how often a digest or quiet topic's buffered messages are
// collapsed into one notification line. A var, not a const, so a test can
// shrink it rather than wait a real minute for a flush; see withDigestInterval
// in monitor_test.go.
var digestInterval = time.Minute

// overrunGrace is how many polls a follower waits when the log is shorter than
// its own position before deciding this is not a compaction. Compaction leaves
// that window open for one rename plus one small write, so a handful of polls
// is generous; waiting forever would hide a real truncation.
const overrunGrace = 5

// RunMonitor is the process Claude Code arms at session start. Every line it
// writes to stdout becomes a <task_notification> in this session, waking it
// from idle. It follows two kinds of source:
//
//   - the direct inbox spool, for 1:1 messages addressed to this session
//   - one log per subscribed topic, for pub/sub
//
// Subscriptions are re-read continuously, so a session can join or leave a
// topic mid-flight via MCP or the CLI without restarting anything.
func RunMonitor(stdout io.Writer, stderr io.Writer) error {
	logf := func(format string, a ...any) { // not shown to the model
		fmt.Fprintf(stderr, "[pigeon] "+format+"\n", a...)
	}

	rt := CurrentRuntime()

	if !rt.Supported() {
		logf("plugin monitors are not available on this platform; pigeon can send")
		logf("from here, but cannot receive. Standing down.")
		block()
		return nil
	}

	if OptedOut() {
		logf("disabled via %s -- standing down", EnvOptOut)
		block()
		return nil
	}

	// A project can keep its own sessions off the bus, for the same reason a
	// launcher can: not every checkout is one you want peers waking you about.
	// The environment still outranks the file, so PIGEON=1 overrides it.
	if cwd := CurrentCwd(); ProjectDisabled(cwd) {
		logf("disabled by %s -- standing down", ProjectConfigPath(cwd))
		block()
		return nil
	}

	// rt.SessionID validates the value, so an unsubstituted "${...}" literal
	// or anything else unsafe already arrives here as an error, not a guess.
	sid, err := rt.SessionID()
	if err != nil {
		// Fail loudly. Guessing which session we belong to is worse than not
		// running: a wrong guess delivers another session's mail.
		logf("FATAL: %s is unset -- cannot identify this session.", EnvSessionID)
		logf("pigeon needs an interactive Claude Code session (>= 2.1.105).")
		logf("Note: do not interpolate ${%s} in monitors.json; it does not", EnvSessionID)
		logf("substitute. The monitor reads it from its own environment.")
		block()
		return nil
	}

	// The namespace is fixed here, for the same reason a monitor cannot be
	// rebound mid-session: from now on this process holds a lock in that
	// namespace's directory and follows that namespace's topics. Changing where
	// a session lives means restarting it.
	ns, origin := ResolveNamespace()
	logf("namespace %s (from %s)", ns, origin)

	if err := ns.EnsureDirs(); err != nil {
		return err
	}

	// Hold an exclusive lock for our whole lifetime. This does double duty:
	// it stops a second monitor double-delivering, and because the kernel
	// releases it the moment we exit -- cleanly, crashed or SIGKILLed -- other
	// sessions can detect a dead monitor just by trying to take it.
	// Deliberately never unlinked: removing the file would let a second
	// process lock a different inode and both believe they hold it.
	lock, acquired, err := tryExclusive(ns.LockPath(sid))
	if err != nil {
		return err
	}
	if !acquired {
		logf("another monitor already owns session %s -- standing down", Short(sid))
		block()
		return nil
	}
	defer lock.Close()

	if err := register(ns, sid, rt, logf); err != nil {
		logf("registration failed: %v", err)
	}

	spool := ns.SpoolPath(sid)
	if f, err := os.OpenFile(spool, os.O_WRONLY|os.O_CREATE, 0o600); err == nil {
		f.Close()
	}
	logf("armed session=%s namespace=%s spool=%s", sid, ns, spool)

	// Trap before doing anything interruptible, so a signal arriving during
	// startup is not lost, and release the handlers on the way out.
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigc)

	done := make(chan struct{})
	var closeOnce sync.Once
	defer closeOnce.Do(func() { close(done) })

	lines := make(chan followedMessage, 256)

	go heartbeat(ns, sid, rt, done)
	go watchdog(CurrentClaudePID(), sigc, done, logf)

	// Direct inbox: resume from our own cursor so mail queued while no monitor
	// was listening is delivered when one comes back, rather than skipped.
	// The spool is never compacted, so its base is always zero and a logical
	// offset is a file position. followSource re-derives that either way.
	inboxOffset := ns.readCursors(sid)[inboxCursorKey]
	go followSource(spool, inboxOffset, inboxCursorKey, lines, done, logf)

	// Topics: started and stopped as subscriptions change.
	go manageSubscriptions(ns, sid, lines, done, logf)

	// Deregister on the way out, but leave the spool in place: anything queued
	// for this session should survive until a monitor actually reads it.
	defer ns.RemoveEntry(sid)

	_, _, perMinute := rt.Budget()
	emit, emitLine, flushSuppressed, rollWindow := newRateLimiter(stdout, ns, sid, spool, time.Minute, perMinute)
	defer flushSuppressed()

	// persistCursor advances source's monitor cursor to at, and only ever
	// forward: mutateCursors is a plain read-modify-write, so without the
	// guard a message arriving out of order -- or this call racing a seed --
	// could walk the cursor backwards and replay history that was already
	// notified.
	//
	// Called once per message, and only once it has been HANDLED: pushed,
	// dropped as this session's own broadcast, suppressed by the rate
	// limiter, or folded into a digest that has since flushed. See
	// followSource's doc comment for why ownership moved here.
	persistCursor := func(source string, at int64) {
		_ = ns.mutateCursors(sid, func(c map[string]int64) {
			if at > c[source] {
				c[source] = at
			}
		})
	}

	// digests buffers topic messages held for a digest or quiet topic, keyed
	// by source (a topic string). Touched only from this goroutine -- both
	// message arrival and the flush tick run through the select loop below --
	// so it needs no lock. A key's presence in the map, not merely its count,
	// is what advanceCursor below treats as "something on this topic is still
	// unflushed": a topic whose only buffered message was just dropped by a
	// supersede (see dropSuperseded) stays present with a zero count, so the
	// two conditions are no longer always the same thing -- presence is the
	// one that is load-bearing.
	digests := map[string]*digestState{}

	// flushDigests emits one line per topic with anything buffered, and moves
	// that topic's cursor -- the only place a digest topic's cursor moves.
	//
	// persist says whether the cursor may follow the line out. It is false on
	// the way out of the process, and that distinction is the whole point:
	// the dominant shutdown here is Claude Code killing the monitor when a
	// session resumes or exits, so the reader on the other end of stdout is
	// already gone or going. The line lands nowhere. Advancing the cursor over
	// messages whose only notification was that lost line would leave a rearmed
	// monitor resuming past them, and nothing would ever mention them again --
	// exactly the silent loss moving the cursor to handled-time was meant to
	// end. So on shutdown the line is written best-effort and the cursor stays
	// put: a duplicate digest line after a restart is the cheap side of the
	// trade, a message nobody ever hears about is the expensive one.
	flushDigests := func(persist bool) {
		for topic, st := range digests {
			if n := st.count(); n > 0 {
				if !emitLine(renderDigestLine(topic, n, st.senderNames())) {
					// Either the reader is gone or the budget is spent.
					// Nothing was delivered, so nothing may be marked handled:
					// keep the buffer and let the next tick try again.
					continue
				}
			}
			if !persist {
				// Shutdown. The line went out best-effort, but the cursor
				// stays where it is so a rearmed monitor re-reads rather than
				// resumes past what only that line announced.
				delete(digests, topic)
				continue
			}
			// Persisted even when every buffered message was dropped by a
			// supersede: dropping one still counts as handling it (see
			// resolveSupersede).
			persistCursor(topic, st.maxOffset)
			delete(digests, topic)
		}
	}
	defer flushDigests(false)

	// senders remembers which session sent each message id this monitor has
	// itself followed off a log, in a bounded window -- the minimum state
	// resolveSupersede needs to check a "supersedes" claim's sender against
	// the original's, without holding or re-reading a whole log for it (see
	// resolveSupersede's doc comment).
	senders := newSenderMemory()

	// advanceCursor persists a handled message's offset, UNLESS an earlier
	// message on the same topic is still sitting in an unflushed digest
	// buffer. Every message on one topic comes off one append-only log
	// through one follower, so any message reaching here while that topic
	// has a pending buffer necessarily sits past the oldest thing in it --
	// persisting its offset would silently carry the cursor over a message
	// nothing has notified about yet. So the cursor for that topic simply
	// does not move again until the buffer flushes and takes it there itself.
	// A message frozen out this way is not lost: it is exactly where
	// followSource left it, and a restart re-reads it rather than skipping
	// it, which is the redeliver-over-drop rule this whole design follows.
	//
	// This also covers a session's own broadcast on a digest topic: dropping
	// it is still "handled" and still must not jump the cursor past a peer's
	// message still waiting on the same buffer.
	advanceCursor := func(fm followedMessage) {
		// Keyed on fm.source, never on the message's own topic field. Buffers
		// are filed under the source the follower read, so a hand-written line
		// omitting or misnaming its topic would otherwise skip this check
		// entirely and carry that topic's cursor past a peer's message still
		// sitting unflushed in its buffer.
		if _, pending := digests[fm.source]; pending {
			return
		}
		persistCursor(fm.source, fm.offset)
	}

	// markOwnAsRead carries the session's CONSUMPTION cursor over its own
	// broadcast, so publishing does not leave a session with unread mail from
	// itself.
	//
	// The monitor cursor moving is not enough. The two cursors are deliberately
	// separate (see readCursorPrefix): the monitor's says how far notifications
	// have got, this one says how far the session has actually read, and
	// nothing was advancing the second for a message the first deliberately
	// never showed anyone. So a session that published to a topic it was up to
	// date on was told, by its own inbox, that it had mail waiting -- and the
	// mail was its own broadcast, which it wrote.
	//
	// Guarded on start, and this is the whole reason start exists. Jumping the
	// consumption cursor to fm.offset unconditionally would carry it over
	// anything unread sitting in front of this message: publish once to a busy
	// topic and every peer message you had not read yet silently stops being
	// unread. Only a cursor already sitting exactly where this message begins
	// has read everything before it, and only that one may move.
	markOwnAsRead := func(fm followedMessage) {
		key := readCursorKey(fm.source)
		_ = ns.mutateCursors(sid, func(m map[string]int64) {
			if m[key] == fm.start {
				m[key] = fm.offset
			}
		})
	}

	// deliver routes one handled-off-the-log message according to its topic's
	// delivery mode, and is the one place that decides push vs. digest vs.
	// quiet. The direct spool never reaches the mode switch at all: it has no
	// Delivery entry to look up (see Entry.Delivery), so every direct message
	// is push, unconditionally.
	deliver := func(fm followedMessage) {
		m := fm.msg
		// Routed by fm.source throughout, never by m.Topic.
		//
		// followSource stamps source from the log it actually read; m.Topic is
		// a field in a line on disk, and this codebase's standing assumption is
		// that such a line may have been written by hand. Trusting it here
		// corrupts cursors two ways. A line in topic X's log omitting "topic"
		// would take the direct branch below and persist X's cursor
		// unconditionally, carrying it past X's own unflushed digest buffer --
		// the one thing the whole cursor rule forbids. A line claiming to be on
		// topic Y would be buffered under Y and flush Y's cursor to an offset
		// measured in X's log, and since cursors only move forward, Y would
		// skip its own unread messages for good.
		if fm.source == inboxCursorKey {
			emit(fm)
			persistCursor(fm.source, fm.offset)
			return
		}
		topic := fm.source

		mode := DeliveryPush
		self, err := ns.ReadEntry(sid)
		if err == nil && self.Delivery != nil {
			if v, ok := self.Delivery[topic]; ok {
				mode = v
			}
		}

		alert := m.Priority == PriorityAlert
		// Mirrors Render's own "addressed" test: a For list naming everyone
		// (empty) is not a personal mention, only one that actually names
		// this session is.
		namedFor := len(m.For) > 0 && m.IsFor(self)

		// The addressing gate: a broadcast that says who it is for does not
		// interrupt anyone else. It is still delivered -- the monitor cursor
		// crosses it, the consumption cursor does not -- so it is sitting in
		// the inbox for whoever wants to look. It just does not cost a turn.
		//
		// This is the whole point of the For list. On the run that prompted it,
		// one message naming two sessions was pushed into nine, across six
		// repositories, and the seven bystanders each had to spend a turn
		// working out it was none of their business. Nothing failed; the
		// message simply cost seven interruptions to reach two.
		//
		// Addressing beats alert, deliberately. A message urgent enough to
		// escalate is urgent FOR THE SESSIONS IT NAMES; waking everyone else
		// because it matters to somebody else is the exact noise this removes.
		// A sender who means "everyone, now" says so by leaving For empty.
		//
		// Fails open when the entry cannot be read: IsFor(nil) is false for any
		// non-empty For, and treating an unreadable entry as "not addressed"
		// would silently mute a session whose registry file was momentarily
		// unavailable. Redeliver over drop, the same rule the followers use.
		if self != nil && len(m.For) > 0 && !namedFor {
			logf("holding a message on %q for the inbox: it names other sessions", topic)
			advanceCursor(fm)
			return
		}

		switch mode {
		case DeliveryQuiet:
			logf("holding a message on %q for the digest (mode %s)", topic, mode)
			// Absolute, by design: a peer's self-assessed urgency cannot
			// override a session that asked not to be interrupted. Whatever
			// alert or For says, the most it earns is a line at the next
			// digest tick, exactly like everything else on this topic.
			bufferDigest(digests, topic, fm)
		case DeliveryDigest:
			if alert || namedFor {
				emit(fm)
				advanceCursor(fm)
			} else {
				logf("holding a message on %q for the digest (mode %s)", topic, mode)
				bufferDigest(digests, topic, fm)
			}
		default:
			// DeliveryPush, or a value this build does not recognise -- an
			// entry is a file on disk and can be hand-edited -- fails open to
			// the safer behaviour rather than silently swallowing a message.
			emit(fm)
			advanceCursor(fm)
		}
	}

	digestTicker := time.NewTicker(digestInterval)
	defer digestTicker.Stop()

	for {
		select {
		case <-sigc:
			logf("shutting down")
			return nil
		case <-digestTicker.C:
			flushDigests(true)
			// The suppression notice is otherwise written only lazily, from
			// inside the next emit -- and the shape that suppresses an alert
			// is a flood followed by silence, where there is no next emit for
			// hours. This tick is the only thing that reliably comes round.
			rollWindow()
		case fm := <-lines:
			// Recorded before anything else so a later message on the same
			// log -- however it is delivered -- can still find this one's
			// sender to check a "supersedes" claim against.
			senders.remember(fm.msg.ID, fm.msg.From.SessionID)
			resolveSupersede(digests, senders, fm.msg)
			// Never wake a session with its own broadcast, but it has still
			// been handled: advance the cursor or the same message is
			// reconsidered forever.
			if fm.msg.From.SessionID != "" && fm.msg.From.SessionID == sid {
				advanceCursor(fm)
				markOwnAsRead(fm)
				continue
			}
			// Already delivered over the socket, before this line was written
			// (see Message.PushedTo and Namespace.wake). This session has been
			// shown the message; announcing it again would cost a second
			// interruption for one message.
			//
			// The monitor cursor moves and the READ cursor deliberately does
			// not, which is the same split every other delivery here observes:
			// the session was doorbelled, not made to read, so the message is
			// still unread in `pigeon inbox` exactly as it would be after a
			// notification. Conflating the two would let a socket push silently
			// mark mail as read that nobody opened.
			if pushedToSession(fm.msg, sid) {
				logf("already delivered over the socket, not notifying again")
				advanceCursor(fm)
				continue
			}
			deliver(fm)
		}
	}
}

// followedMessage pairs a message read off a source with what it takes to
// advance that source's cursor once the message has been handled: the cursor
// key (inboxCursorKey, or a topic's TopicRef.String()) and the logical offset
// immediately after this message in that source. followSource stamps both, so
// the delivery side never has to ask which follower a message came from.
type followedMessage struct {
	msg    *Message
	source string
	offset int64
	// start is the logical offset immediately BEFORE this message, i.e. where
	// a cursor sitting on it would be. Only markOwnAsRead needs it, to tell
	// "this session had read everything up to its own broadcast" from "it has
	// unread mail sitting in front of it", which are the same fm.offset.
	start int64
}

// digestState buffers what a digest or quiet topic has accumulated since its
// last flush, and how far the cursor may move once this buffer is actually
// flushed -- never before, or a monitor that dies with messages still
// sitting here would resume past them and they would never be seen.
//
// Individual messages are kept, in arrival order, rather than folded straight
// into a running count and sender list: a supersede arriving inside the same
// window has to be able to find and drop the exact entry it names (see
// dropSuperseded), and count/senderNames below are derived from what
// survives that, computed fresh each time rather than kept incrementally, so
// a drop can never leave them out of sync with the messages that back them.
type digestState struct {
	messages []bufferedDigestMessage
	// maxOffset is the furthest cursor position anything ever folded into
	// this buffer reached, dropped entries included: dropping a buffered
	// message still counts as handling it, so its offset must still be
	// crossed once this buffer flushes (see flushDigests).
	maxOffset int64
}

// bufferedDigestMessage is one entry in a digest buffer. dropped marks an
// entry a later supersede has removed from what will actually be shown --
// left in place rather than spliced out, so arrival order among whatever
// survives is never disturbed and dropping is idempotent to look up twice.
type bufferedDigestMessage struct {
	fm      followedMessage
	dropped bool
}

// bufferDigest folds fm into topic's digest buffer, creating it on first use.
func bufferDigest(digests map[string]*digestState, topic string, fm followedMessage) {
	st, ok := digests[topic]
	if !ok {
		st = &digestState{}
		digests[topic] = st
	}
	st.messages = append(st.messages, bufferedDigestMessage{fm: fm})
	if fm.offset > st.maxOffset {
		st.maxOffset = fm.offset
	}
}

// count reports how many buffered messages are still due to be shown.
func (st *digestState) count() int {
	n := 0
	for _, bm := range st.messages {
		if !bm.dropped {
			n++
		}
	}
	return n
}

// senderNames lists who sent each surviving buffered message, in the order
// first seen, deduplicated -- recomputed from st.messages every call rather
// than tracked incrementally, so a drop can never leave a name behind whose
// only message is gone.
func (st *digestState) senderNames() []string {
	seen := map[string]bool{}
	var out []string
	for _, bm := range st.messages {
		if bm.dropped {
			continue
		}
		name := bm.fm.msg.From.Display()
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// dropSuperseded removes id's entry from whichever topic's digest buffer
// still holds it un-flushed, wherever it is -- a supersede is not required to
// name a message on its own topic, so every buffer is checked, not just the
// one for m.Topic. It reports whether id was found: a caller that gets false
// has nothing buffered to drop, so the "already emitted" behaviour applies
// instead (see resolveSupersede).
func dropSuperseded(digests map[string]*digestState, id string) bool {
	for _, st := range digests {
		for i := range st.messages {
			if st.messages[i].dropped || st.messages[i].fm.msg.ID != id {
				continue
			}
			st.messages[i].dropped = true
			return true
		}
	}
	return false
}

// supersedeMemory caps how many recent message ids senderMemory remembers a
// sender for. Bounded rather than unbounded because a monitor runs for a
// whole session's life: the two things a "supersedes" claim is ever checked
// against -- a message this monitor already pushed, or one still sitting in
// its own digest buffer -- both concern messages this session has itself
// seen recently, never a log's full history, so remembering forever would
// grow this map for no case that actually needs it.
const supersedeMemory = 512

// senderMemory remembers, for a bounded trailing window of message ids this
// monitor has followed off a log, who sent each one. It is the minimum state
// resolveSupersede needs to check a "supersedes" claim's sender against the
// original's without holding, or re-reading, a whole log to answer one
// question (see RunMonitor's doc comment on why not: the delivery loop sees
// messages once, streaming past, and Send/Publish already reject anything
// that is not shaped like a real id at the point a message is sent -- see
// validateSupersedes -- so what is left to check here is only ever "did the
// same sender send both", which this is enough for).
//
// Eviction only ever weakens a supersede claim, never strengthens one: an id
// that ages out of memory is treated as unverifiable and the claim naming it
// is dropped (see resolveSupersede), the same safe-by-default outcome as a
// claim naming an id this monitor never saw at all.
type senderMemory struct {
	sender map[string]string // message id -> From.SessionID
	order  []string          // insertion order, oldest first, for eviction
}

func newSenderMemory() *senderMemory {
	return &senderMemory{sender: map[string]string{}}
}

// remember records who sent id, the first time it is seen. Touched only from
// RunMonitor's own goroutine, so it needs no lock, the same as digests.
func (s *senderMemory) remember(id, sessionID string) {
	if id == "" {
		return
	}
	if _, exists := s.sender[id]; exists {
		return
	}
	if len(s.order) >= supersedeMemory {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.sender, oldest)
	}
	s.sender[id] = sessionID
	s.order = append(s.order, id)
}

// senderOf reports who sent id, and whether this monitor has seen it at all.
func (s *senderMemory) senderOf(id string) (sessionID string, seen bool) {
	sessionID, seen = s.sender[id]
	return sessionID, seen
}

// resolveSupersede decides, once and for the rest of a message's delivery,
// whether its Supersedes claim is real -- and mutates m in place, because
// everything downstream (Render, bufferDigest, renderDigestLine) simply
// trusts m.Supersedes from this point on and none of it has the history to
// check it again. Real means: the named id is one this monitor has itself
// seen, sent by the exact same session that is sending this message. Render
// has no log to check that against, which is why this runs here, once, at
// the moment a message comes off the log, rather than wherever it ends up
// being shown.
//
// An unverifiable or cross-sender claim is wiped so the message behaves
// exactly like an ordinary one that carries none: accepting it instead is a
// peer silently cancelling, or relabelling, somebody else's message.
//
// A verified claim against a message still sitting in a digest buffer drops
// that entry outright (see dropSuperseded) and is then wiped too: there is
// nothing left in the recipient's view to call this message a correction OF
// -- the original was never shown -- so it is delivered as an ordinary
// message rather than framed as one. A verified claim against anything else
// (already pushed, or already flushed out of a digest) is left alone, so
// Render's correction marker fires wherever this message is actually shown.
func resolveSupersede(digests map[string]*digestState, mem *senderMemory, m *Message) {
	if m.Supersedes == "" {
		return
	}
	origSender, seen := mem.senderOf(m.Supersedes)
	if !seen || origSender == "" || origSender != m.From.SessionID {
		m.Supersedes = ""
		return
	}
	if dropSuperseded(digests, m.Supersedes) {
		m.Supersedes = ""
	}
}

// digestSenderNameLimit bounds one sender's name in a digest line, matching
// the bound Render places on the same field: a name is peer-controlled (see
// Sanitize), so it gets the same defensive truncation here as everywhere else
// it is rendered.
const digestSenderNameLimit = 40

// renderDigestLine is the one line a digest or quiet topic produces per
// flush, naming the topic, how many messages piled up, and who sent them --
// e.g. "[pigeon] 6 waiting on #inventory-chain from indkoeb-ui, ad-hoc,
// project-overview -- read with the inbox tool". Bounded by RenderBudget like
// every other notification line, for the same reason: Claude Code clips a
// longer one and there is no payload pointer here to lose.
func renderDigestLine(topic string, count int, senders []string) string {
	names := make([]string, len(senders))
	for i, s := range senders {
		names[i] = truncate(Sanitize(s), digestSenderNameLimit)
	}
	line := fmt.Sprintf("[pigeon] %d waiting on %s from %s -- read with the inbox tool",
		count, TopicLabel(topic), strings.Join(names, ", "))
	return truncate(line, RenderBudget)
}

// manageSubscriptions starts a follower per subscribed topic and stops it when
// the session unsubscribes, re-reading the registry entry as the source of
// truth so MCP and CLI changes both take effect without a restart.
func manageSubscriptions(ns Namespace, sid string, out chan<- followedMessage, done <-chan struct{}, logf func(string, ...any)) {
	type follower struct{ stop chan struct{} }
	active := map[string]*follower{}

	stopAll := func() {
		for _, f := range active {
			close(f.stop)
		}
	}
	defer stopAll()

	t := time.NewTicker(subCheckInterval)
	defer t.Stop()

	for {
		select {
		case <-done:
			return
		case <-t.C:
		}

		e, err := ns.ReadEntry(sid)
		if err != nil {
			continue
		}
		want := map[string]bool{}
		for _, topic := range e.Subscriptions {
			// The entry is a file on disk, so a subscription is not necessarily
			// something Subscribe validated. Following an unchecked name would
			// let a planted entry point this session's reader at any file.
			ref, err := ParseTopicRef(topic)
			if err != nil {
				continue
			}
			// A private namespace is sealed against machine-wide topics, so a
			// subscription to one is not honoured however it got into the entry.
			if ref.Global && ns.IsPrivate() {
				continue
			}
			want[ref.String()] = true
		}

		for topic := range want {
			if _, running := active[topic]; running {
				continue
			}
			stop := make(chan struct{})
			active[topic] = &follower{stop: stop}
			path := ns.TopicPath(topic)
			// Resume from our own cursor so each subscriber reads the shared
			// log independently and nobody consumes anyone else's messages.
			cur := ns.readCursors(sid)
			off, ok := cur[topic]
			if !ok {
				off = readBase(path) + endOffset(path)
			}
			tp := topic
			logf("following topic %q from offset %d", topic, off)
			go followSource(path, off, tp, out, stop, logf)
		}

		for topic, f := range active {
			if !want[topic] {
				logf("unfollowing topic %q", topic)
				close(f.stop)
				delete(active, topic)
			}
		}
	}
}

// tryMonitorLock attempts to take the session's liveness lock without waiting.
// A held lock means a monitor is registering or already running, so anything
// pruning state for that session must stand down.
func (n Namespace) tryMonitorLock(sessionID string) (io.Closer, bool, error) {
	return tryExclusive(n.LockPath(sessionID))
}

func register(ns Namespace, sid string, rt Runtime, logf func(string, ...any)) error {
	// A session hard-killed before its monitor's deferred RemoveEntry runs
	// leaves a dead entry behind -- otherwise only `pigeon prune` clears it.
	// Sweeping here means the namespace tidies itself as sessions come and go,
	// rather than accumulating garbage until someone runs prune by hand.
	if pruned := ns.pruneDeadEntries(sid); pruned > 0 {
		logf("pruned %d dead session entry/entries left by earlier sessions", pruned)
	}

	// The sweep above only finds sessions that still have an entry, i.e. ones
	// killed before they could deregister. A session that exits *cleanly*
	// removes its own entry and orphans its spool and cursor, which nothing
	// then searches by -- so the tidy path leaked and the messy one did not.
	//
	// Guarded, because an entry can also be missing for a session that is very
	// much alive; see orphanGrace and ownerAlive. Excludes this session by name
	// on top of that: its own entry does not exist yet -- it is written further
	// down -- so without this it would qualify as an orphan and delete the very
	// spool it is about to start following, three lines before recreating it
	// empty. A monitor rearming after a gap is exactly the case that has mail
	// waiting.
	if swept := ns.reconcileOrphans(orphanGrace, sid); swept > 0 {
		logf("swept %d orphaned state file(s) from sessions that are gone", swept)
	}
	// This session is back, so whatever its last exit recorded is no longer
	// true. Left in place it would outlive the entry it describes and answer
	// for a pid that has since been recycled onto some unrelated process.
	ns.clearTombstone(sid)

	pid := CurrentClaudePID()
	cwd := CurrentCwd()

	// The config is read at every registration, but only *seeds* identity at
	// the first one. Reading it every time is what keeps `private` honest: it
	// is the project's standing rule about what may be published, not a
	// session's declaration, so a monitor restart must not start publishing a
	// directory the checkout asked to keep off the bus.
	cfg, problems, cerr := LoadProjectConfig(cwd)
	for _, p := range problems {
		logf("project config: %s", p)
	}
	if cerr != nil {
		logf("project config: %v", cerr)
	}

	// Preserve identity, subscriptions and delivery modes declared earlier in
	// this session.
	//
	// Delivery belongs in that list for a sharper reason than the rest. A
	// monitor being killed and rearmed is not an edge case here, it is the
	// documented dominant lifecycle event -- Claude Code does it on every
	// resume -- and WriteEntry replaces the whole entry, so a field left out
	// here is not merely stale, it is erased. Dropping it turned every digest
	// and quiet topic back into push on resume, silently, which is the one
	// outcome set_delivery exists to prevent: a session that asked not to be
	// interrupted goes back to being interrupted and is never told. It also
	// moves under askAudience, which excludes quiet sessions from a question's
	// denominator.
	var name, desc string
	var subs []string
	var delivery map[string]string
	prev, err := ns.ReadEntry(sid)
	if err == nil {
		name, desc, subs = prev.Name, prev.Description, prev.Subscriptions
		delivery = prev.Delivery
	} else {
		// Only a session's *first* registration takes identity from the
		// config. After that the session's own declarations are authoritative,
		// so a `pigeon name` or `pigeon unsubscribe` is not quietly undone the
		// next time a monitor starts for the same id.
		name, desc, subs = applyProjectConfig(ns, sid, cwd, cfg, logf)
	}

	// Everyone gets both public mailboxes by default: this namespace's, and the
	// machine-wide one. `@all` crossing the boundary is the deliberate hole in
	// the isolation -- a broadcast meant for everybody has to reach everybody --
	// and `pigeon unsubscribe @all` closes it for a session that would rather
	// not hear it.
	if subs == nil {
		subs = defaultSubscriptions(ns, cwd, cfg != nil && cfg.Private)
	}
	now := nowRFC3339()
	labelName, labelSource := rt.Label(pid, sid)
	if err := ns.WriteEntry(&Entry{
		SessionID:      sid,
		Namespace:      ns.String(),
		Name:           name,
		Description:    desc,
		PID:            pid,
		ProcStart:      ProcStart(pid),
		Cwd:            cwd,
		Host:           hostname(),
		StartedAt:      now,
		HeartbeatAt:    now,
		Subscriptions:  subs,
		Delivery:       delivery,
		Runtime:        rt.Name(),
		RuntimeVersion: rt.Version(),
		Label:          labelName,
		LabelSource:    labelSource,
		Driven:         os.Getenv(EnvChild) == "1",
		Private:        cfg != nil && cfg.Private,
	}); err != nil {
		return err
	}

	// Cursors are seeded after the entry exists, because taking a session's
	// lock requires the session to be registered -- that is what stops a
	// mistyped namespace from being conjured into existence by a read. The
	// order is otherwise immaterial: a follower that starts before its cursor
	// is seeded begins at the end of the log, which is where seeding would
	// have put it.
	//
	// Only topics never followed before are seeded. Re-seeding an existing one
	// would discard the session's read position on every monitor restart,
	// silently skipping whatever was published while it was down.
	existing := ns.readCursors(sid)
	for _, t := range subs {
		if _, seen := existing[t]; seen {
			continue
		}
		if ref, err := ParseTopicRef(t); err == nil {
			_, _ = ns.seedCursor(sid, ref, "")
		}
	}

	// The direct spool's consumption cursor is seeded here rather than in
	// seedCursor, because the spool is not a topic and has no TopicRef. It
	// takes the monitor's own position, which is 0 for a new session and the
	// resumed offset for one coming back under the same id -- either way, the
	// point this session begins reading from, not wherever notifications
	// happen to have reached by the time it first asks.
	if _, seen := existing[readCursorKey(inboxCursorKey)]; !seen {
		_ = ns.mutateCursors(sid, func(m map[string]int64) {
			m[readCursorKey(inboxCursorKey)] = m[inboxCursorKey]
		})
	}
	return nil
}

// defaultSubscriptions is what a session comes up listening to before any
// config or command has a say.
//
// Three rooms, widest last: the checkout this session started in, the
// namespace, the machine. The checkout one is the addition, and it is an
// ordinary namespaced topic whose name happens to be derived rather than
// typed -- no new tree, no new prefix, nothing that cuts across namespaces.
//
// It exists because the narrow room has to be the one you are already in. A
// project topic was available to the run that prompted this and two of the
// three sessions in that checkout had joined it; the third had not, and
// broadcast to the whole machine instead. Joining was a deliberate act nobody
// prompted, which is the same reason nobody set a delivery preference and
// nobody used the tool built for asking a question. Defaults are the only
// instructions a session reliably follows.
func defaultSubscriptions(ns Namespace, cwd string, private bool) []string {
	subs := []string{PublicTopic}
	// A private namespace joins its own mailbox only. @all is the one place
	// isolation is deliberately not absolute, and a namespace declared private
	// is precisely the one that opted out of that.
	if !ns.IsPrivate() {
		subs = append(subs, GlobalPublicTopic)
	}
	// Not for a private checkout, and this is the one place the checkout topic
	// has to be suppressed rather than merely scoped.
	//
	// The name IS the directory basename, and a subscription list is published
	// in the entry every peer reads. WriteEntry already blanks Cwd, Label and
	// Description for a private session for exactly this reason, in a comment
	// that names the hazard: "a derived one is the cwd basename, so publishing
	// it would leak exactly the directory Private is meant to keep off the
	// bus." Subscriptions are not blanked, so joining this room would put that
	// basename back on the bus by another route -- and publishing into it would
	// hang the hidden directory's name in `list_topics` for the whole
	// namespace.
	//
	// A private NAMESPACE is a different matter: its topics are namespace-local
	// and nobody outside can see them, so the room is safe there. It is the
	// per-project `private: true` that has to opt out.
	if !private {
		if t := CheckoutTopic(cwd); t != "" && t != PublicTopic {
			subs = append(subs, t)
		}
	}
	sort.Strings(subs)
	return subs
}

// CheckoutTopic is the topic name a session in dir joins by default: the
// basename of the git repository it is in, or of dir itself when it is not in
// one, folded to what ValidTopic accepts. "" when nothing usable survives.
//
// The repository root rather than the directory, so that a session started in
// a subdirectory lands with its peers rather than in a room of its own. Two
// worktrees of the same repository sit at different roots and so get different
// rooms, which is right when they are different lines of work and wrong when
// they are the same one; the basename gets the common case and does not
// attempt the rest.
func CheckoutTopic(dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return ""
	}
	// Resolved first, so two sessions reaching one checkout by different paths
	// land in the same room. A symlinked route and the physical one are the
	// same working tree, and a room they disagree about is worse than no room:
	// each would believe it had announced itself to the other.
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	if root := repoRoot(dir); root != "" {
		dir = root
	}
	return topicNameFrom(filepath.Base(dir))
}

// repoRoot walks up from dir looking for a .git entry, and returns "" if it
// reaches the filesystem root without finding one. A file rather than a
// directory counts: that is what a linked worktree has.
func repoRoot(dir string) string {
	for {
		if isGitDir(filepath.Join(dir, ".git")) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// isGitDir reports whether path is git's marker for a working tree, applying
// the same test git itself does rather than merely checking the name exists.
//
// The distinction is not pedantic. An empty directory called .git is not a
// checkout to git and must not be one to pigeon: a stray /tmp/.git, which is
// easy to create by accident and which this machine actually had, would
// otherwise make every session under /tmp announce itself into a room called
// "tmp" and see strangers there. The room a session joins is derived from this
// answer, so a false positive here silently merges unrelated work.
//
// Two shapes are valid. A linked worktree has a .git FILE holding a gitdir
// pointer, and its mere existence is the marker. A primary checkout has a .git
// DIRECTORY, which git considers a repository only once it contains HEAD.
func isGitDir(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil {
		return false
	}
	if !fi.IsDir() {
		return true
	}
	_, err = os.Lstat(filepath.Join(path, "HEAD"))
	return err == nil
}

// topicNameFrom folds a directory name into a valid topic name, or "" if it
// cannot. Anything outside the charset becomes a dash, runs collapse, and the
// result is trimmed to a leading alphanumeric because that is what topicRe
// demands of the first character.
func topicNameFrom(s string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_':
			b.WriteRune(r)
			lastDash = false
		case !lastDash:
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-._")
	if len(out) > 64 {
		out = strings.Trim(out[:64], "-._")
	}
	if ValidTopic(out) != nil {
		return ""
	}
	return out
}

// applyProjectConfig seeds a brand-new session's identity from the project it
// started in. The config's shape was validated when it was loaded; what is
// decided here is what a template actually produces for *this* session, and
// what to do when the config asks for something this machine cannot give it --
// most often a name another live session already answers to.
func applyProjectConfig(ns Namespace, sid, cwd string, cfg *ProjectConfig, logf func(string, ...any)) (name, desc string, subs []string) {
	subs = defaultSubscriptions(ns, cwd, cfg != nil && cfg.Private)
	if cfg == nil {
		return "", "", subs
	}

	// Resolve reports rather than repairs: a template that renders to a name
	// this session may not have leaves it unnamed and says why. That is the
	// honest outcome -- the session is still reachable by id, and no reply is
	// misrouted.
	res := cfg.Resolve(ns, sid, cwd)
	for _, p := range res.Problems {
		logf("project config: %s", p)
	}

	subs = append(subs, res.Topics...)
	sort.Strings(subs)

	logf("project config: %s applied (name=%q topics=%v private=%v)",
		ProjectConfigPath(cwd), res.Name, subs, res.Private)
	return res.Name, res.Description, subs
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

// heartbeat refreshes the entry so a wedged monitor -- still holding the lock
// but no longer working -- is still reported as not delivering.
func heartbeat(ns Namespace, sid string, rt Runtime, done <-chan struct{}) {
	t := time.NewTicker(heartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			// Through MutateEntry, not ReadEntry+WriteEntry: this ticks every
			// 15s against a file the CLI and MCP also write, and an unlocked
			// read-modify-write here is exactly how the entry gets shredded.
			_ = ns.MutateEntry(sid, func(e *Entry) error {
				e.HeartbeatAt = nowRFC3339()
				// Refresh the host label too, so a session renamed in Claude
				// Code mid-run is reflected within a heartbeat. Only overwrite
				// on a successful read: a transient miss must not blank a name
				// peers can already see. WriteEntry re-blanks it for a private
				// session, so this cannot resurrect a withheld one.
				if name, source := rt.Label(e.PID, sid); name != "" {
					e.Label, e.LabelSource = name, source
				}
				return nil
			})
		}
	}
}

// watchdog exits when the owning claude process goes away, so a hard-killed
// session does not leave an orphaned monitor behind. (Never blanket-pkill
// these: you would kill other sessions' monitors too.)
func watchdog(pid int, sigc chan<- os.Signal, done <-chan struct{}, logf func(string, ...any)) {
	if pid <= 0 {
		return
	}
	want := ProcStart(pid)
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
		}
		if !ProcessAlive(pid, want) {
			logf("claude pid %d is gone -- exiting", pid)
			// Non-blocking: if the shutdown path is already running, nobody
			// is left to receive this and we must not wedge here.
			select {
			case sigc <- syscall.SIGTERM:
			default:
			}
			return
		}
	}
}

func endOffset(path string) int64 {
	if fi, err := os.Stat(path); err == nil {
		return fi.Size()
	}
	return 0
}

// followSource tails an NDJSON log from a LOGICAL offset, tolerating
// compaction, truncation, and a file that does not exist yet. source is the
// cursor key this log is filed under (inboxCursorKey, or a topic's
// TopicRef.String()); it is stamped onto every message this loop emits so the
// delivery side, not this one, can decide when a cursor is safe to move.
//
// It deliberately does not persist a cursor itself. That used to happen here,
// once per read pass, right after the pass's messages were pushed into the
// channel and before any of them had been rendered, folded into a digest, or
// even looked at -- so the cursor recorded ingestion, not delivery. A digest
// can hold a message for up to a minute inside a process known to die on
// session resume without always being rearmed; if the cursor had already
// moved past a message still sitting in that buffer, a restarted follower
// would resume beyond it and the message would never be seen again, by
// monitor or digest alike. So cursor ownership
// belongs to whoever decides a message has actually been handled -- pushed,
// dropped, suppressed, or flushed out of a digest -- which is never this loop.
//
// Working in logical offsets is what makes this safe against a concurrent
// compaction. The physical position is derived from the base on every pass, so
// a compaction that lands mid-poll changes where we read, never what we have
// read: there is no shared number for the compactor and this loop to race
// over, and no window in which one of them sees the file in one era and the
// cursor in another.
func followSource(path string, offset int64, source string, out chan<- followedMessage, stop <-chan struct{},
	logf func(string, ...any)) {

	// Consecutive passes seeing a file shorter than our position. Compaction
	// produces exactly one or two of these; anything sustained is not one.
	overrun := 0

	for {
		select {
		case <-stop:
			return
		default:
		}

		fi, err := os.Stat(path)
		if err != nil {
			time.Sleep(pollInterval)
			continue
		}
		// Convert to a file position every pass, never once at the top. The
		// base is what a compaction changes, so re-reading it here is what
		// makes a compaction invisible to this loop.
		base := readBase(path)
		physical := offset - base

		if physical < 0 {
			// Compaction cut past where we had read. It computes the cut as
			// the minimum over live subscribers, so this means we were not
			// counted as one -- unsubscribed and resubscribed, or our entry
			// was unreadable when prune ran. The messages are already gone;
			// resume at the start of what survives rather than sit on an
			// offset that can never be reached.
			logf("%s was compacted past this session's position; %d bytes were missed",
				path, -physical)
			offset = base
			physical = 0
		}

		if physical > fi.Size() {
			// Compaction renames the log and then writes the base, so for a
			// moment the file is short while the base still says otherwise.
			// That resolves within a poll. Anything that persists is not a
			// compaction -- an external truncation, or a log deleted and
			// recreated -- and must not be waited on forever.
			overrun++
			if overrun <= overrunGrace {
				time.Sleep(pollInterval)
				continue
			}
			// Not a compaction, so this is not the file we were reading:
			// truncated, or replaced with unrelated content. Read it from the
			// start rather than skipping to the end. Which of its bytes we
			// have already seen is unknowable, and re-delivering a message is
			// recoverable in a way that never delivering one is not.
			logf("%s is shorter than this session's position and stayed that way; "+
				"re-reading it from the start (%d bytes)", path, fi.Size())
			offset = base
			physical = 0
		}
		overrun = 0

		if physical == fi.Size() {
			time.Sleep(pollInterval)
			continue
		}

		f, err := os.Open(path)
		if err != nil {
			time.Sleep(pollInterval)
			continue
		}
		if _, err := f.Seek(physical, io.SeekStart); err != nil {
			f.Close()
			offset = base
			continue
		}

		r := bufio.NewReader(f)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				// Partial trailing line: leave the offset before it so the
				// next pass picks it up whole.
				break
			}
			start := offset
			offset += int64(len(line))
			s := strings.TrimSpace(line)
			if s == "" {
				continue
			}
			m, perr := ParseMessage(s)
			if perr != nil {
				continue
			}
			select {
			case out <- followedMessage{msg: m, source: source, offset: offset, start: start}:
			case <-stop:
				f.Close()
				return
			}
		}
		f.Close()
	}
}

// alertReserve is how many of the per-minute cap's slots stay off-limits to
// normal traffic, so that once ordinary messages have used the rest of the
// window only a PriorityAlert message may spend what remains.
//
// Without this a flood of routine traffic can fill the whole cap, and the one
// message meant to interrupt gets suppressed exactly like everything else --
// it is still in its log, but nothing wakes the session to say so. Alerts
// still cannot exceed the cap; the reserve narrows who may use the tail of
// the window, it does not raise the ceiling the host enforces.
const alertReserve = 10

// newRateLimiter returns an emit function that caps output per minute and
// reports suppression instead of silently dropping.
//
// Claude Code stops a monitor that produces too many events, so exceeding the
// cap is not an option and something has to give. What gives is the
// notification, never the message: a suppressed message is still in its log,
// and the notice says which log, so it can be read. It will not be re-notified,
// because the follower has already passed it.
//
// The notice is per source. It used to name the direct spool for everything,
// so a suppressed topic message pointed the recipient at a file it had never
// been in -- the one recovery hint they get, aimed somewhere useless.
// sid is this session's own id, re-read into an *Entry on every emitted line
// rather than captured once, so a For marker reflects a name declared (or
// changed) via set_identity partway through the session's life instead of
// whatever it was when the monitor armed.
// newRateLimiter returns the three things the delivery loop needs to stay
// inside the host's event budget.
//
// emit renders and writes one message. emitLine writes an already-rendered line
// -- a digest summary -- and it exists because those lines used to bypass the
// cap entirely: Claude Code kills a monitor that produces too many events, so a
// session with fifteen digest topics could put forty-five uncounted lines a
// minute on top of the thirty counted ones and lose its monitor altogether,
// which is strictly worse than any amount of suppression. A digest line spends
// from the alert reserve, since one of them stands in for many messages and is
// the opposite of chatter. tick rolls the window from outside, so a suppression
// notice does not wait on traffic that may never come.
//
// perMinute is the host's own tolerance -- CurrentRuntime().Budget()'s third
// value in production -- rather than a package constant read from inside this
// function, so the cap has exactly one source (Budget) and this is a
// consumer of it like any other caller, not a second place the number lives.
func newRateLimiter(w io.Writer, ns Namespace, sid, spool string, window time.Duration, perMinute int) (emit func(followedMessage), emitLine func(string) bool, flush func(), tick func()) {
	windowStart := time.Now()
	count := 0
	// Suppression is tracked separately for alerts and normal traffic, per
	// source, because a suppressed alert is a materially worse event than a
	// suppressed routine message and the notice is the only signal the
	// recipient gets of either.
	suppressedNormal := map[string]int{}
	suppressedAlert := map[string]int{}

	flush = func() {
		// Alerts first: it is the worse of the two events, so it leads.
		for _, src := range sortedKeys(suppressedAlert) {
			fmt.Fprintf(w, "[pigeon] %d further ALERT message(s) suppressed by rate limit; they are in %s\n",
				suppressedAlert[src], src)
		}
		for _, src := range sortedKeys(suppressedNormal) {
			fmt.Fprintf(w, "[pigeon] %d further message(s) suppressed by rate limit; they are in %s\n",
				suppressedNormal[src], src)
		}
		clear(suppressedAlert)
		clear(suppressedNormal)
	}

	roll := func() {
		if time.Since(windowStart) >= window {
			flush()
			windowStart = time.Now()
			count = 0
		}
	}
	tick = roll

	emitLine = func(line string) bool {
		roll()
		if count >= perMinute {
			return false
		}
		count++
		if _, err := fmt.Fprintln(w, line); err != nil {
			return false
		}
		return true
	}

	emit = func(fm followedMessage) {
		m := fm.msg
		roll()
		alert := m.Priority == PriorityAlert
		suppress := func(dest map[string]int) {
			// Named from the source the follower actually read, not from the
			// message's own topic field: the notice's whole job is to tell the
			// recipient which log to go and read, and a hand-written line
			// could otherwise send them to the wrong one.
			src := spool
			if fm.source != inboxCursorKey {
				if p := ns.TopicPath(fm.source); p != "" {
					src = p
				}
			}
			dest[src]++
		}
		// The last alertReserve slots of the window are alert-only: normal
		// traffic is cut off early so it can never crowd out the reserve, and
		// an alert is cut off only at the true ceiling.
		if !alert && count >= perMinute-alertReserve {
			suppress(suppressedNormal)
			return
		}
		if count >= perMinute {
			suppress(suppressedAlert)
			return
		}
		count++
		self, _ := ns.ReadEntry(sid)
		fmt.Fprintln(w, ns.Render(m, self))
	}
	return emit, emitLine, flush, tick
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// block parks forever. Exiting would make Claude Code show the monitor as
// failed on every session; idling quietly is less noisy for an opt-out.
//
// It sleeps rather than blocking on a channel: with nothing else running,
// `select {}` trips Go's deadlock detector and the process dies with
// "all goroutines are asleep" instead of idling. That is invisible in a cgo
// build and fatal in the CGO_ENABLED=0 release binaries.
func block() {
	for {
		time.Sleep(time.Hour)
	}
}
