package pigeon

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/signal"
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

	// A project can keep its own sessions off the bus, for the same reason a
	// launcher can: not every checkout is one you want peers waking you about.
	// The environment still outranks the file, so PIGEON=1 overrides it.
	if cwd := CurrentCwd(); ProjectDisabled(cwd) {
		logf("disabled by %s -- standing down", ProjectConfigPath(cwd))
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

	if err := register(ns, sid, logf); err != nil {
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

	lines := make(chan *Message, 256)

	go heartbeat(ns, sid, done)
	go watchdog(CurrentClaudePID(), sigc, done, logf)

	// Direct inbox: resume from our own cursor so mail queued while no monitor
	// was listening is delivered when one comes back, rather than skipped.
	// The spool is never compacted, so its base is always zero and a logical
	// offset is a file position. followSource re-derives that either way.
	inboxOffset := ns.readCursors(sid)[inboxCursorKey]
	go followSource(spool, inboxOffset, lines, done,
		func(off int64) { _ = ns.mutateCursors(sid, func(c map[string]int64) { c[inboxCursorKey] = off }) },
		logf)

	// Topics: started and stopped as subscriptions change.
	go manageSubscriptions(ns, sid, lines, done, logf)

	// Deregister on the way out, but leave the spool in place: anything queued
	// for this session should survive until a monitor actually reads it.
	defer ns.RemoveEntry(sid)

	emit, flushSuppressed := newRateLimiter(stdout, ns, spool, time.Minute)
	defer flushSuppressed()
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
			emit(m)
		}
	}
}

// manageSubscriptions starts a follower per subscribed topic and stops it when
// the session unsubscribes, re-reading the registry entry as the source of
// truth so MCP and CLI changes both take effect without a restart.
func manageSubscriptions(ns Namespace, sid string, out chan<- *Message, done <-chan struct{}, logf func(string, ...any)) {
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
			persist := func(at int64) {
				_ = ns.mutateCursors(sid, func(c map[string]int64) { c[tp] = at })
			}
			logf("following topic %q from offset %d", topic, off)
			go followSource(path, off, out, stop, persist, logf)
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

func register(ns Namespace, sid string, logf func(string, ...any)) error {
	// A session hard-killed before its monitor's deferred RemoveEntry runs
	// leaves a dead entry behind -- otherwise only `pigeon prune` clears it.
	// Sweeping here means the namespace tidies itself as sessions come and go,
	// rather than accumulating garbage until someone runs prune by hand.
	if pruned := ns.pruneDeadEntries(sid); pruned > 0 {
		logf("pruned %d dead session entry/entries left by earlier sessions", pruned)
	}

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

	// Preserve identity and subscriptions declared earlier in this session.
	var name, desc string
	var subs []string
	prev, err := ns.ReadEntry(sid)
	if err == nil {
		name, desc, subs = prev.Name, prev.Description, prev.Subscriptions
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
		subs = defaultSubscriptions(ns)
	}
	now := nowRFC3339()
	claude := LookupClaudeSession(pid, sid)
	if err := ns.WriteEntry(&Entry{
		SessionID:        sid,
		Namespace:        ns.String(),
		Name:             name,
		Description:      desc,
		PID:              pid,
		ProcStart:        ProcStart(pid),
		Cwd:              cwd,
		Host:             hostname(),
		StartedAt:        now,
		HeartbeatAt:      now,
		Subscriptions:    subs,
		CCVersion:        os.Getenv(EnvVersion),
		ClaudeName:       claude.Name,
		ClaudeNameSource: claude.Source,
		Driven:           os.Getenv(EnvChild) == "1",
		Private:          cfg != nil && cfg.Private,
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
			_ = ns.seedCursor(sid, ref)
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
func defaultSubscriptions(ns Namespace) []string {
	// A private namespace joins its own mailbox only. @all is the one place
	// isolation is deliberately not absolute, and a namespace declared private
	// is precisely the one that opted out of that.
	if ns.IsPrivate() {
		return []string{PublicTopic}
	}
	subs := []string{PublicTopic, GlobalPublicTopic}
	sort.Strings(subs)
	return subs
}

// applyProjectConfig seeds a brand-new session's identity from the project it
// started in. The config's shape was validated when it was loaded; what is
// decided here is what a template actually produces for *this* session, and
// what to do when the config asks for something this machine cannot give it --
// most often a name another live session already answers to.
func applyProjectConfig(ns Namespace, sid, cwd string, cfg *ProjectConfig, logf func(string, ...any)) (name, desc string, subs []string) {
	subs = defaultSubscriptions(ns)
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
func heartbeat(ns Namespace, sid string, done <-chan struct{}) {
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
				if claude := LookupClaudeSession(e.PID, sid); claude.Name != "" {
					e.ClaudeName, e.ClaudeNameSource = claude.Name, claude.Source
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
// compaction, truncation, and a file that does not exist yet. persist, when
// non-nil, records the logical offset so a restart resumes rather than
// replays.
//
// Working in logical offsets is what makes this safe against a concurrent
// compaction. The physical position is derived from the base on every pass, so
// a compaction that lands mid-poll changes where we read, never what we have
// read: there is no shared number for the compactor and this loop to race
// over, and no window in which one of them sees the file in one era and the
// cursor in another.
func followSource(path string, offset int64, out chan<- *Message, stop <-chan struct{},
	persist func(int64), logf func(string, ...any)) {

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
func newRateLimiter(w io.Writer, ns Namespace, spool string, window time.Duration) (emit func(*Message), flush func()) {
	windowStart := time.Now()
	count := 0
	suppressed := map[string]int{}

	flush = func() {
		for _, src := range sortedKeys(suppressed) {
			fmt.Fprintf(w, "[pigeon] %d further message(s) suppressed by rate limit; they are in %s\n",
				suppressed[src], src)
		}
		clear(suppressed)
	}

	emit = func(m *Message) {
		if time.Since(windowStart) >= window {
			flush()
			windowStart = time.Now()
			count = 0
		}
		if count >= maxPerMinute {
			src := spool
			if m.Topic != "" {
				if p := ns.TopicPath(m.Topic); p != "" {
					src = p
				}
			}
			suppressed[src]++
			return
		}
		count++
		fmt.Fprintln(w, ns.Render(m))
	}
	return emit, flush
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
