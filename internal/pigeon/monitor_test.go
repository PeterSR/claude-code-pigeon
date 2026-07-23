//go:build !windows

// Monitor tests are Unix-only for the same reason MonitorSupported is false on
// Windows: Claude Code arms plugin monitors only on Unix, and shutting one down
// here means signalling this process, which Windows has no equivalent for.

package pigeon

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The monitor only shuts down on a signal, and the only way to deliver one to
// it from a test is to signal this process. Registering a permanent catcher
// before any test runs means a stray or early SIGTERM is dropped rather than
// killing the test binary, which is otherwise the default action.
func TestMain(m *testing.M) {
	signal.Notify(make(chan os.Signal, 8), syscall.SIGTERM)
	os.Exit(m.Run())
}

// --- harness ---------------------------------------------------------------

// syncWriter is an io.Writer the monitor goroutine can write to while the test
// goroutine reads. A bytes.Buffer is not safe for concurrent use, and the
// monitor writes from several goroutines' worth of work, so the race detector
// would flag an unguarded one immediately.
type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func (w *syncWriter) has(s string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return bytes.Contains(w.buf.Bytes(), []byte(s))
}

// eventually polls cond until it holds or the deadline passes. Everything in
// the monitor is driven by a 200ms file poll and a 1s subscription re-check,
// so tests wait for an observable effect instead of sleeping a fixed time and
// hoping. Every wait has a deadline, so no test can hang.
func eventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// monitor is a RunMonitor running in the background for one test.
//
// Only one may run at a time, because shutdown is a process-wide SIGTERM that
// every running monitor would see. Tests in this file are therefore
// deliberately not parallel.
type monitor struct {
	sid     string
	stdout  *syncWriter
	stderr  *syncWriter
	exited  chan error
	stopped bool
}

// startMonitor arms a monitor for sid and shuts it down when the test ends.
func startMonitor(t *testing.T, sid string) *monitor {
	t.Helper()
	t.Setenv(EnvSessionID, sid)
	t.Setenv(EnvOptOut, "")
	// A real pid keeps the watchdog quiet and makes the registered entry look
	// alive; this process is the closest stand-in for the claude process.
	t.Setenv(EnvClaudePID, strconv.Itoa(os.Getpid()))

	m := &monitor{
		sid:    sid,
		stdout: &syncWriter{},
		stderr: &syncWriter{},
		exited: make(chan error, 1),
	}
	go func() { m.exited <- RunMonitor(m.stdout, m.stderr) }()

	// "armed" is logged once the lock is held and the session is registered,
	// which is the first moment the monitor can actually deliver anything.
	eventually(t, 5*time.Second, "the monitor to arm", func() bool {
		return m.stderr.has("armed session=" + sid)
	})
	t.Cleanup(func() { m.stop(t) })
	return m
}

// stop signals the monitor and waits for RunMonitor to return.
func (m *monitor) stop(t *testing.T) {
	t.Helper()
	if m.stopped {
		return
	}
	m.stopped = true

	// signal.Notify is installed just after "armed" is logged, so the very
	// first SIGTERM can be dropped. Keep signalling until the monitor actually
	// returns rather than assuming one lands.
	deadline := time.After(10 * time.Second)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
			t.Errorf("kill(self, SIGTERM): %v", err)
			return
		}
		select {
		case err := <-m.exited:
			if err != nil {
				t.Errorf("RunMonitor returned %v, want a clean shutdown", err)
			}
			// Followers wake at most one pollInterval after shutdown and may
			// persist a cursor on the way out. Let them finish before the temp
			// home is torn down, or the cleanup races a recreated directory.
			time.Sleep(pollInterval + 100*time.Millisecond)
			return
		case <-tick.C:
		case <-deadline:
			t.Error("monitor did not shut down within 10s of SIGTERM")
			return
		}
	}
}

// peer is a plausible sender that is not the session under test.
func peer() Sender {
	return Sender{Kind: "session", SessionID: "bbbb2222-3333", Name: "beta", Cwd: "/home/p/web"}
}

// mailbox addresses a session by id. Send only needs the id, so this avoids
// registering a second entry that would then show up in ListSessions.
func mailbox(sid string) *Entry { return &Entry{SessionID: sid} }

// --- lifecycle -------------------------------------------------------------

func TestMonitorRegistersEntryAndHoldsTheLock(t *testing.T) {
	withHome(t)
	const sid = "mon-register-1"
	startMonitor(t, sid)

	var e *Entry
	eventually(t, 5*time.Second, "the session entry to be written", func() bool {
		var err error
		e, err = ReadEntry(sid)
		return err == nil
	})

	if e.PID != os.Getpid() {
		t.Errorf("PID = %d, want %d (the owning claude process)", e.PID, os.Getpid())
	}
	if e.Cwd == "" {
		t.Error("Cwd is empty; other sessions address by cwd basename")
	}
	if h, _ := os.Hostname(); e.Host != h {
		t.Errorf("Host = %q, want %q", e.Host, h)
	}
	for _, f := range []struct{ name, val string }{
		{"StartedAt", e.StartedAt},
		{"HeartbeatAt", e.HeartbeatAt},
	} {
		if _, err := time.Parse(time.RFC3339, f.val); err != nil {
			t.Errorf("%s = %q, not RFC3339: %v", f.name, f.val, err)
		}
	}
	// Everyone joins the public mailbox at registration, so a broadcast
	// reaches the machine without anyone configuring anything.
	if len(e.Subscriptions) != 1 || e.Subscriptions[0] != PublicTopic {
		t.Errorf("Subscriptions = %v, want [%s]", e.Subscriptions, PublicTopic)
	}
	if e.Status != StatusLive {
		t.Errorf("Status = %q, want %q while a monitor is running", e.Status, StatusLive)
	}

	// The lock is what stops a second monitor double-delivering. Take it the
	// way a second monitor would and assert we are refused. (Running a second
	// RunMonitor for real is not testable: it parks in block() forever.)
	c, acquired, err := tryExclusive(LockPath(sid))
	if err != nil {
		t.Fatalf("tryExclusive: %v", err)
	}
	if acquired {
		_ = c.Close()
		t.Error("a second monitor could take the session lock; both would deliver every message")
	}
	if !monitorListening(sid) {
		t.Error("monitorListening() = false while a monitor holds the lock")
	}
}

func TestMonitorDeregistersOnShutdown(t *testing.T) {
	withHome(t)
	const sid = "mon-shutdown-1"
	m := startMonitor(t, sid)
	eventually(t, 5*time.Second, "the session entry to be written", func() bool {
		_, err := ReadEntry(sid)
		return err == nil
	})

	m.stop(t)

	if _, err := ReadEntry(sid); err == nil {
		t.Error("entry survived shutdown; peers would keep addressing a session nothing listens to")
	}
	if monitorListening(sid) {
		t.Error("the lock is still held after shutdown, so this session still looks live")
	}
	if !m.stderr.has("shutting down") {
		t.Errorf("no shutdown notice on stderr:\n%s", m.stderr.String())
	}
	// The spool is deliberately left behind: anything queued for this session
	// must survive until a monitor actually reads it.
	if _, err := os.Stat(SpoolPath(sid)); err != nil {
		t.Errorf("spool removed on shutdown, so queued mail was lost: %v", err)
	}
}

// --- direct messages -------------------------------------------------------

func TestMonitorEmitsDirectMessagesButNeverItsOwn(t *testing.T) {
	withHome(t)
	const sid = "mon-direct-1"
	m := startMonitor(t, sid)

	// A message stamped with this session's own id must never be emitted:
	// waking a session with its own broadcast is a loop with a model in it.
	if _, err := Send(mailbox(sid), "echo of my own voice",
		Sender{Kind: "session", SessionID: sid, Name: "me"}, ""); err != nil {
		t.Fatalf("Send (self): %v", err)
	}
	if _, err := Send(mailbox(sid), "the build is green", peer(), ""); err != nil {
		t.Fatalf("Send (peer): %v", err)
	}

	eventually(t, 5*time.Second, "the peer's message on stdout", func() bool {
		return m.stdout.has("the build is green")
	})
	// Both lines were spooled before either could be read, and the follower
	// reads in order, so once the second is out the first has been through the
	// filter. That makes this a real absence rather than a slow test.
	if m.stdout.has("echo of my own voice") {
		t.Errorf("monitor emitted the session's own message:\n%s", m.stdout.String())
	}
	if !m.stdout.has("[pigeon] message from beta (web)") {
		t.Errorf("stdout does not carry a rendered notification:\n%s", m.stdout.String())
	}
}

func TestMonitorDeliversMailQueuedBeforeItStarted(t *testing.T) {
	withHome(t)
	const sid = "mon-queued-1"
	// Mail written while nothing was listening: the inbox cursor must resume
	// from where it was rather than skipping to the end of the spool, or a
	// `claude --resume` silently loses everything sent while it was away.
	if _, err := Send(mailbox(sid), "queued while nobody listened", peer(), ""); err != nil {
		t.Fatalf("Send: %v", err)
	}

	m := startMonitor(t, sid)
	eventually(t, 5*time.Second, "the queued message to be delivered", func() bool {
		return m.stdout.has("queued while nobody listened")
	})
	// And the cursor must advance, or the same message is redelivered forever.
	eventually(t, 5*time.Second, "the inbox cursor to advance", func() bool {
		return readCursors(sid)[inboxCursorKey] > 0
	})
}

// --- topics ----------------------------------------------------------------

func TestMonitorDeliversSubscribedTopicsOnly(t *testing.T) {
	withHome(t)
	const sid = "mon-topics-1"
	m := startMonitor(t, sid)

	// Nothing subscribes this session to #secrets, so it must stay silent even
	// though the log sits in the same state directory and is trivially
	// readable. Publishing it first means the barrier below is meaningful.
	if _, err := Publish("secrets", "not for you", peer()); err != nil {
		t.Fatalf("Publish (#secrets): %v", err)
	}
	if _, err := Publish(PublicTopic, "deploying to staging in 5", peer()); err != nil {
		t.Fatalf("Publish (#%s): %v", PublicTopic, err)
	}

	// The subscription manager re-checks once a second, so the follower for
	// #all starts shortly after registration and then resumes from offset 0.
	eventually(t, 6*time.Second, "the #"+PublicTopic+" message", func() bool {
		return m.stdout.has("deploying to staging in 5")
	})
	if m.stdout.has("not for you") {
		t.Errorf("monitor delivered a topic it never subscribed to:\n%s", m.stdout.String())
	}
	if !m.stdout.has("[pigeon #" + PublicTopic + "]") {
		t.Errorf("topic marker missing from the notification:\n%s", m.stdout.String())
	}
}

func TestSubscribingWhileTheMonitorRunsTakesEffect(t *testing.T) {
	withHome(t)
	const sid = "mon-subscribe-1"
	m := startMonitor(t, sid)

	// The whole point of re-reading the entry every subCheckInterval: joining
	// a topic must not need a restart, because monitors cannot be rebound.
	if err := Subscribe(sid, "deploys"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	eventually(t, 6*time.Second, "the monitor to start following #deploys", func() bool {
		return m.stderr.has(`following topic "deploys"`)
	})

	if _, err := Publish("deploys", "v2.1 rolled out", peer()); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	eventually(t, 6*time.Second, "the #deploys message", func() bool {
		return m.stdout.has("v2.1 rolled out")
	})
}

func TestUnsubscribingWhileTheMonitorRunsStopsDelivery(t *testing.T) {
	withHome(t)
	const sid = "mon-unsubscribe-1"
	m := startMonitor(t, sid)

	if err := Subscribe(sid, "deploys"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	eventually(t, 6*time.Second, "the monitor to start following #deploys", func() bool {
		return m.stderr.has(`following topic "deploys"`)
	})
	if _, err := Publish("deploys", "first while subscribed", peer()); err != nil {
		t.Fatalf("Publish (before): %v", err)
	}
	eventually(t, 6*time.Second, "the first #deploys message", func() bool {
		return m.stdout.has("first while subscribed")
	})

	if err := Unsubscribe(sid, "deploys"); err != nil {
		t.Fatalf("Unsubscribe: %v", err)
	}
	eventually(t, 6*time.Second, "the monitor to drop #deploys", func() bool {
		return m.stderr.has(`unfollowing topic "deploys"`)
	})

	if _, err := Publish("deploys", "second after unsubscribing", peer()); err != nil {
		t.Fatalf("Publish (after): %v", err)
	}
	// Barrier: a direct message sent afterwards proves the monitor is still
	// emitting, so the missing topic line is a real absence.
	if _, err := Send(mailbox(sid), "still listening", peer(), ""); err != nil {
		t.Fatalf("Send: %v", err)
	}
	eventually(t, 6*time.Second, "the barrier direct message", func() bool {
		return m.stdout.has("still listening")
	})
	if m.stdout.has("second after unsubscribing") {
		t.Errorf("monitor kept delivering a topic it had left:\n%s", m.stdout.String())
	}
}

// --- standing down ---------------------------------------------------------
//
// In both cases below RunMonitor deliberately parks in block() forever:
// exiting would make Claude Code report the monitor as failed on every single
// session, which is far noisier than idling. So these tests assert the
// diagnostics and then leave the goroutine sleeping. It holds no lock, has
// registered nothing and touches no state afterwards, so it cannot affect any
// later test -- but it does mean RunMonitor must never be called this way
// without a deadline on the assertion.

func TestMonitorStandsDownWhenOptedOut(t *testing.T) {
	withHome(t)
	const sid = "mon-optout-1"
	t.Setenv(EnvSessionID, sid)
	t.Setenv(EnvOptOut, "0")

	stderr := &syncWriter{}
	go func() { _ = RunMonitor(&syncWriter{}, stderr) }()

	eventually(t, 5*time.Second, "the opt-out notice", func() bool {
		return stderr.has("disabled via " + EnvOptOut)
	})
	// An opted-out session must not appear as a reachable peer either.
	if _, err := ReadEntry(sid); err == nil {
		t.Error("an opted-out session registered itself, so peers would try to message it")
	}
}

// A project can take its own sessions off the bus, and it has to work the same
// way PIGEON=0 does: no entry, so nothing tries to message a session that
// deliberately is not there.
func TestMonitorStandsDownWhenTheProjectIsDisabled(t *testing.T) {
	withHome(t)
	const sid = "mon-disabled-1"
	dir := writeProjectConfig(t, `{"name": "api", "enabled": false}`)
	t.Setenv(EnvSessionID, sid)
	t.Setenv(EnvOptOut, "")
	t.Setenv(EnvProjectDir, dir)

	stderr := &syncWriter{}
	go func() { _ = RunMonitor(&syncWriter{}, stderr) }()

	eventually(t, 5*time.Second, "the project opt-out notice", func() bool {
		return stderr.has("disabled by " + ProjectConfigPath(dir))
	})
	if _, err := ReadEntry(sid); err == nil {
		t.Error("a disabled project registered a session, so peers would try to message it")
	}
}

// The launcher knows how it started the session; the config arrived with a
// clone. So an explicit PIGEON=1 keeps this session on the bus anyway.
func TestEnvOptInOverridesADisabledProject(t *testing.T) {
	withHome(t)
	const sid = "mon-override-1"
	dir := writeProjectConfig(t, `{"name": "api", "enabled": false}`)
	t.Setenv(EnvSessionID, sid)
	t.Setenv(EnvProjectDir, dir)
	t.Setenv(EnvClaudePID, strconv.Itoa(os.Getpid()))
	// Deliberately not startMonitor: it clears the opt-out variable, which is
	// the whole subject of this test.
	t.Setenv(EnvOptOut, "1")

	m := &monitor{sid: sid, stdout: &syncWriter{}, stderr: &syncWriter{}, exited: make(chan error, 1)}
	go func() { m.exited <- RunMonitor(m.stdout, m.stderr) }()
	t.Cleanup(func() { m.stop(t) })

	eventually(t, 5*time.Second, "the session to register anyway", func() bool {
		_, err := ReadEntry(sid)
		return err == nil
	})
	if m.stderr.has("standing down") {
		t.Errorf("the monitor stood down despite %s=1:\n%s", EnvOptOut, m.stderr.String())
	}
}

func TestSecondMonitorStandsDownRatherThanDoubleDeliver(t *testing.T) {
	withHome(t)
	const sid = "mon-second-1"
	t.Setenv(EnvSessionID, sid)
	t.Setenv(EnvOptOut, "")
	// Take the lock the way a first monitor would, then start a second one.
	// Two monitors on one session would deliver every message twice.
	holdSessionLock(t, sid)

	stdout, stderr := &syncWriter{}, &syncWriter{}
	go func() { _ = RunMonitor(stdout, stderr) }()

	eventually(t, 5*time.Second, "the stand-down notice", func() bool {
		return stderr.has("another monitor already owns session " + Short(sid))
	})
	if got := stdout.String(); got != "" {
		t.Errorf("the second monitor delivered %q; the session would see everything twice", got)
	}
}

func TestMonitorRefusesToGuessTheSession(t *testing.T) {
	withHome(t)
	// No CLAUDE_CODE_SESSION_ID: guessing which session we belong to would
	// deliver another session's mail, so the monitor must fail loudly instead.
	t.Setenv(EnvSessionID, "")

	stdout, stderr := &syncWriter{}, &syncWriter{}
	go func() { _ = RunMonitor(stdout, stderr) }()

	eventually(t, 5*time.Second, "the fatal diagnostic", func() bool {
		return stderr.has("FATAL: " + EnvSessionID + " is unset")
	})
	// The advice matters as much as the failure: interpolating the variable in
	// monitors.json is the mistake that leaves other projects' monitors idle.
	if !stderr.has("do not interpolate") {
		t.Errorf("stderr does not warn against interpolating the session id:\n%s", stderr.String())
	}
	// Nothing may reach the session's stdout, which is the model's context.
	if got := stdout.String(); got != "" {
		t.Errorf("stdout = %q, want nothing delivered to the session", got)
	}
}

// --- followers -------------------------------------------------------------

func TestFollowSourcePersistsOffsetSoARestartDoesNotReplay(t *testing.T) {
	dir := withHome(t)
	path := filepath.Join(dir, "topic.ndjson")
	appendMessage(t, path, "one")
	appendMessage(t, path, "two")

	var mu sync.Mutex
	var saved int64
	persist := func(n int64) {
		mu.Lock()
		defer mu.Unlock()
		saved = n
	}
	readSaved := func() int64 {
		mu.Lock()
		defer mu.Unlock()
		return saved
	}

	out := make(chan *Message, 8)
	stop := make(chan struct{})
	go followSource(path, 0, out, stop, persist, nil, func(string, ...any) {})

	for _, want := range []string{"one", "two"} {
		select {
		case m := <-out:
			if m.Text != want {
				t.Fatalf("got %q, want %q -- the log must be read in order", m.Text, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for %q", want)
		}
	}
	eventually(t, 5*time.Second, "the offset to be persisted", func() bool {
		return readSaved() == endOffset(path)
	})
	close(stop)

	// Restarting from the persisted offset must pick up only what is new. A
	// replay here would re-notify the session with everything it has already
	// seen every time its monitor restarts.
	out2 := make(chan *Message, 8)
	stop2 := make(chan struct{})
	defer close(stop2)
	go followSource(path, readSaved(), out2, stop2, nil, nil, func(string, ...any) {})
	appendMessage(t, path, "three")

	select {
	case m := <-out2:
		if m.Text != "three" {
			t.Fatalf("got %q after restart, want only the new line", m.Text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the line appended after the restart")
	}
}

func TestFollowSourceSkipsUnparseableLines(t *testing.T) {
	dir := withHome(t)
	path := filepath.Join(dir, "junk.ndjson")
	// A corrupt or half-written line must not stall the follower: everything
	// after it still has to reach the session.
	if err := os.WriteFile(path, []byte("not json at all\n\n"), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}

	out := make(chan *Message, 4)
	stop := make(chan struct{})
	defer close(stop)
	go followSource(path, 0, out, stop, nil, nil, func(string, ...any) {})
	appendMessage(t, path, "after the junk")

	select {
	case m := <-out:
		if m.Text != "after the junk" {
			t.Fatalf("got %q, want the message after the junk line", m.Text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("follower stalled on an unparseable line")
	}
}

// --- watchdog --------------------------------------------------------------

func TestWatchdogSignalsWhenTheOwningProcessIsGone(t *testing.T) {
	// A session that is hard-killed must not leave its monitor behind: an
	// orphan keeps the lock, so the dead session goes on looking live to
	// everyone else. The watchdog ticks every 5s and that interval is not
	// injectable, so this test is slow by construction rather than by
	// sloppiness. It is the only one worth skipping in a hurry.
	if testing.Short() {
		t.Skip("waits for the watchdog's 5s tick")
	}
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start a throwaway process: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}
	// Reaped, so the pid is genuinely gone rather than a zombie.
	if ProcessAlive(pid, "") {
		t.Skipf("pid %d was recycled before the test could use it", pid)
	}

	sigc := make(chan os.Signal, 1)
	stop := make(chan struct{})
	defer close(stop)
	go watchdog(pid, sigc, stop, func(string, ...any) {})
	select {
	case sig := <-sigc:
		if sig != syscall.SIGTERM {
			t.Errorf("watchdog raised %v, want SIGTERM", sig)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("watchdog did not notice that the owning process had gone")
	}
}

func TestWatchdogStandsDownWithoutAPID(t *testing.T) {
	// Manually armed monitors have no CLAUDE_PID, and a watchdog that treated
	// "unknown" as "gone" would shut them down immediately.
	sigc := make(chan os.Signal, 1)
	stop := make(chan struct{})
	defer close(stop)
	done := make(chan struct{})
	go func() { watchdog(0, sigc, stop, func(string, ...any) {}); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watchdog(0) did not return")
	}
	select {
	case sig := <-sigc:
		t.Errorf("watchdog(0) raised %v with no pid to watch", sig)
	default:
	}
}

func TestEndOffsetOfAMissingFileIsZero(t *testing.T) {
	dir := withHome(t)
	// A session with no spool yet must start at 0, not fail: the file is
	// created lazily by the first sender.
	if got := endOffset(filepath.Join(dir, "nothing-here.ndjson")); got != 0 {
		t.Errorf("endOffset() = %d for a missing file, want 0", got)
	}
}

func appendMessage(t *testing.T, path, text string) {
	t.Helper()
	line, err := json.Marshal(&Message{ID: newMessageID(), Text: text, From: peer()})
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		t.Fatalf("append to %s: %v", path, err)
	}
}

// TestWatchdogStopsWhenTheMonitorDoes guards a leak: the watchdog used to loop
// on its ticker forever, so every RunMonitor call left a goroutine behind.
func TestWatchdogStopsWhenTheMonitorDoes(t *testing.T) {
	sigc := make(chan os.Signal, 1)
	stop := make(chan struct{})
	finished := make(chan struct{})
	// A live pid, so the watchdog has no reason of its own to return.
	go func() { watchdog(os.Getpid(), sigc, stop, func(string, ...any) {}); close(finished) }()

	close(stop)
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("watchdog outlived its stop signal (goroutine leak)")
	}
}
