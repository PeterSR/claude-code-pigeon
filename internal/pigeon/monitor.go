package pigeon

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/signal"
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

	if !MonitorSupported {
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

	// CurrentSessionID validates the value, so an unsubstituted "${...}"
	// literal or anything else unsafe already arrives here as "".
	sid := CurrentSessionID()
	if sid == "" {
		// Fail loudly. Guessing which session we belong to is worse than not
		// running: a wrong guess delivers another session's mail.
		logf("FATAL: %s is unset -- cannot identify this session.", EnvSessionID)
		logf("pigeon needs an interactive Claude Code session (>= 2.1.105).")
		logf("Note: do not interpolate ${%s} in monitors.json; it does not", EnvSessionID)
		logf("substitute. The monitor reads it from its own environment.")
		block()
		return nil
	}

	if err := EnsureDirs(); err != nil {
		return err
	}

	// Hold an exclusive lock for our whole lifetime. This does double duty:
	// it stops a second monitor double-delivering, and because the kernel
	// releases it the moment we exit -- cleanly, crashed or SIGKILLed -- other
	// sessions can detect a dead monitor just by trying to take it.
	// Deliberately never unlinked: removing the file would let a second
	// process lock a different inode and both believe they hold it.
	lock, acquired, err := tryExclusive(LockPath(sid))
	if err != nil {
		return err
	}
	if !acquired {
		logf("another monitor already owns session %s -- standing down", Short(sid))
		block()
		return nil
	}
	defer lock.Close()

	if err := register(sid); err != nil {
		logf("registration failed: %v", err)
	}

	spool := SpoolPath(sid)
	if f, err := os.OpenFile(spool, os.O_WRONLY|os.O_CREATE, 0o600); err == nil {
		f.Close()
	}
	logf("armed session=%s spool=%s", sid, spool)

	// Trap before doing anything interruptible, so a signal arriving during
	// startup is not lost, and release the handlers on the way out.
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigc)

	done := make(chan struct{})
	var closeOnce sync.Once
	defer closeOnce.Do(func() { close(done) })

	lines := make(chan *Message, 256)

	go heartbeat(sid, done)
	go watchdog(CurrentClaudePID(), sigc, done, logf)

	// Direct inbox: resume from our own cursor so mail queued while no monitor
	// was listening is delivered when one comes back, rather than skipped.
	inboxOffset := readCursors(sid)[inboxCursorKey]
	if inboxOffset > endOffset(spool) {
		inboxOffset = 0
	}
	go followSource(spool, inboxOffset, lines, done,
		func(n int64) { _ = mutateCursors(sid, func(c map[string]int64) { c[inboxCursorKey] = n }) },
		func() int64 { return readCursors(sid)[inboxCursorKey] },
		logf)

	// Topics: started and stopped as subscriptions change.
	go manageSubscriptions(sid, lines, done, logf)

	// Deregister on the way out, but leave the spool in place: anything queued
	// for this session should survive until a monitor actually reads it.
	defer RemoveEntry(sid)

	emit := newRateLimiter(stdout, spool)
	for {
		select {
		case <-sigc:
			logf("shutting down")
			return nil
		case m := <-lines:
			// Never wake a session with its own broadcast.
			if m.From.SessionID != "" && m.From.SessionID == sid {
				continue
			}
			emit(Render(m))
		}
	}
}

// manageSubscriptions starts a follower per subscribed topic and stops it when
// the session unsubscribes, re-reading the registry entry as the source of
// truth so MCP and CLI changes both take effect without a restart.
func manageSubscriptions(sid string, out chan<- *Message, done <-chan struct{}, logf func(string, ...any)) {
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

		e, err := ReadEntry(sid)
		if err != nil {
			continue
		}
		want := map[string]bool{}
		for _, topic := range e.Subscriptions {
			want[topic] = true
		}

		for topic := range want {
			if _, running := active[topic]; running {
				continue
			}
			stop := make(chan struct{})
			active[topic] = &follower{stop: stop}
			path := TopicPath(topic)
			// Resume from our own cursor so each subscriber reads the shared
			// log independently and nobody consumes anyone else's messages.
			cur := readCursors(sid)
			off, ok := cur[topic]
			if !ok {
				off = endOffset(path)
			}
			tp := topic
			persist := func(n int64) {
				_ = mutateCursors(sid, func(c map[string]int64) { c[tp] = n })
			}
			reload := func() int64 { return readCursors(sid)[tp] }
			logf("following topic %q from offset %d", topic, off)
			go followSource(path, off, out, stop, persist, reload, logf)
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

func register(sid string) error {
	pid := CurrentClaudePID()
	// Preserve identity and subscriptions declared earlier in this session.
	var name, desc string
	var subs []string
	if prev, err := ReadEntry(sid); err == nil {
		name, desc, subs = prev.Name, prev.Description, prev.Subscriptions
	}
	// Everyone gets the public mailbox by default, so a broadcast reaches the
	// machine without anyone configuring anything.
	if subs == nil {
		subs = []string{PublicTopic}
		_ = seedCursor(sid, PublicTopic)
	}
	now := nowRFC3339()
	return WriteEntry(&Entry{
		SessionID:     sid,
		Name:          name,
		Description:   desc,
		PID:           pid,
		ProcStart:     ProcStart(pid),
		Cwd:           CurrentCwd(),
		Host:          hostname(),
		StartedAt:     now,
		HeartbeatAt:   now,
		Subscriptions: subs,
		CCVersion:     os.Getenv(EnvVersion),
		Driven:        os.Getenv(EnvChild) == "1",
	})
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
func heartbeat(sid string, done <-chan struct{}) {
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
			_ = MutateEntry(sid, func(e *Entry) error {
				e.HeartbeatAt = nowRFC3339()
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

// followSource tails an NDJSON log from an offset, tolerating truncation and a
// file that does not exist yet. persist, when non-nil, records the read offset
// so a restart resumes rather than replays.
func followSource(path string, offset int64, out chan<- *Message, stop <-chan struct{},
	persist func(int64), reload func() int64, logf func(string, ...any)) {

	// Track file identity, not just size. Compaction replaces the log via
	// rename, so the inode changes while the size may land anywhere -- a
	// shrink check alone misses a rewrite that happens to end up the same
	// length or longer, and the follower then stalls or reads garbage.
	var seen os.FileInfo
	if fi, err := os.Stat(path); err == nil {
		seen = fi
	}

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
		replaced := seen != nil && !os.SameFile(seen, fi)
		seen = fi

		// Identity alone is not enough: the log may have been replaced before
		// this follower ever saw the original. Every valid offset in an NDJSON
		// log is either 0 or one byte past a newline, so a misaligned offset
		// is proof the file underneath changed.
		if !replaced && offset > 0 && offset <= fi.Size() && !atRecordBoundary(path, offset) {
			replaced = true
		}

		if replaced || fi.Size() < offset {
			// The file shrank. That is either a compaction, which rewound our
			// persisted cursor by the amount it cut, or a genuine truncation.
			// Trusting the persisted cursor covers both; resetting to zero
			// would replay the whole log as duplicate notifications.
			next := int64(0)
			if reload != nil {
				next = reload()
			}
			if next > fi.Size() {
				next = 0
			}
			logf("%s was replaced or truncated (%d -> %d bytes); resuming at %d",
				path, offset, fi.Size(), next)
			offset = next
		}
		if fi.Size() == offset {
			time.Sleep(pollInterval)
			continue
		}

		f, err := os.Open(path)
		if err != nil {
			time.Sleep(pollInterval)
			continue
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			f.Close()
			offset = 0
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
			case out <- m:
			case <-stop:
				f.Close()
				return
			}
		}
		f.Close()
		if persist != nil {
			persist(offset)
		}
	}
}

// atRecordBoundary reports whether offset sits immediately after a newline,
// which is the only place a reader of an append-only NDJSON log may resume.
func atRecordBoundary(path string, offset int64) bool {
	f, err := os.Open(path)
	if err != nil {
		return true // cannot tell; do not thrash
	}
	defer f.Close()
	var b [1]byte
	if _, err := f.ReadAt(b[:], offset-1); err != nil {
		return true
	}
	return b[0] == '\n'
}

// newRateLimiter returns an emit function that caps output per minute and
// reports suppression instead of silently dropping.
func newRateLimiter(w io.Writer, spool string) func(string) {
	windowStart := time.Now()
	count, suppressed := 0, 0
	return func(line string) {
		if time.Since(windowStart) >= time.Minute {
			if suppressed > 0 {
				fmt.Fprintf(w, "[pigeon] %d further message(s) suppressed by rate limit; full log: %s\n",
					suppressed, spool)
				suppressed = 0
			}
			windowStart = time.Now()
			count = 0
		}
		if count >= maxPerMinute {
			suppressed++
			return
		}
		count++
		fmt.Fprintln(w, line)
	}
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
