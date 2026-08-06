//go:build !windows

// Monitor tests are Unix-only for the same reason MonitorSupported is false on
// Windows: Claude Code arms plugin monitors only on Unix, and shutting one down
// here means signalling this process, which Windows has no equivalent for.

package pigeon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
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

// withDigestInterval shrinks the package's digest flush interval for one
// test. digestInterval is a full minute in production; RunMonitor reads it
// exactly once, early, to build its ticker (see RunMonitor), so setting it
// before startMonitor launches the monitor goroutine -- and restoring it only
// once that goroutine has actually stopped -- never races the read. t.Cleanup
// runs LIFO, and startMonitor registers its own stop cleanup after this
// function returns, so the restore below always runs after the monitor exits.
func withDigestInterval(t *testing.T, d time.Duration) {
	t.Helper()
	orig := digestInterval
	digestInterval = d
	t.Cleanup(func() { digestInterval = orig })
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
	// Everyone joins both public mailboxes at registration: this namespace's,
	// and the machine-wide one, so a broadcast reaches either without anyone
	// configuring anything.
	if got := strings.Join(e.Subscriptions, ","); got != defaultSubs() {
		t.Errorf("Subscriptions = %v, want %s", e.Subscriptions, defaultSubs())
	}
	if e.Namespace != DefaultNamespaceName {
		t.Errorf("Namespace = %q, want %q", e.Namespace, DefaultNamespaceName)
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

// register() reflects Claude Code's own session name into the entry's Label,
// so peers see the /status label without reading Claude Code's internals
// themselves.
func TestMonitorPopulatesLabel(t *testing.T) {
	withHome(t)
	const sid = "mon-claude-name-1"
	// Plant the index register() will read, and point EnvConfigDir at it so the
	// lookup never touches a real ~/.claude.
	writeClaudeIndex(t, os.Getpid(), sid, "chosen-here", "user")
	startMonitor(t, sid)

	var e *Entry
	eventually(t, 5*time.Second, "the entry to carry the label", func() bool {
		var err error
		e, err = ReadEntry(sid)
		return err == nil && e.Label != ""
	})
	if e.Label != "chosen-here" || e.LabelSource != "user" {
		t.Fatalf("label = %q (%q), want chosen-here (user)", e.Label, e.LabelSource)
	}
	if e.Runtime != RuntimeClaudeCode {
		t.Fatalf("runtime = %q, want %q", e.Runtime, RuntimeClaudeCode)
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

// A session hard-killed before its monitor's deferred RemoveEntry runs leaves
// a dead entry behind. The next monitor to register in the same namespace
// sweeps it, so the namespace tidies itself without anyone running
// `pigeon prune` by hand.
func TestMonitorPrunesDeadEntriesOnRegister(t *testing.T) {
	withHome(t)
	if err := WriteEntry(&Entry{SessionID: "mon-dead-leftover", PID: 0}); err != nil {
		t.Fatal(err)
	}

	const sid = "mon-register-prunes-1"
	m := startMonitor(t, sid)

	if _, err := ReadEntry("mon-dead-leftover"); err == nil {
		t.Error("dead entry from an earlier session survived a new monitor's registration")
	}
	if !m.stderr.has("pruned 1 dead session entry") {
		t.Errorf("no prune notice on stderr:\n%s", m.stderr.String())
	}
}

func TestRegisterSkipsPruningADeadLookingSessionWhoseLockIsHeld(t *testing.T) {
	withHome(t)

	ns := DefaultNamespace()
	const heldSID = "mon-held-stale-1"
	const newSID = "mon-register-held-1"

	if err := WriteEntry(&Entry{SessionID: heldSID, PID: 0}); err != nil {
		t.Fatalf("WriteEntry(%s): %v", heldSID, err)
	}
	if err := os.WriteFile(SpoolPath(heldSID), []byte("queued\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", SpoolPath(heldSID), err)
	}
	if err := mutateCursors(heldSID, func(c map[string]int64) { c[inboxCursorKey] = 7 }); err != nil {
		t.Fatalf("mutateCursors(%s): %v", heldSID, err)
	}
	holdSessionLock(t, heldSID)

	if got := ns.pruneDeadEntries(newSID); got != 0 {
		t.Fatalf("pruneDeadEntries() pruned %d entries, want 0 while %s lock is held", got, heldSID)
	}

	t.Setenv(EnvClaudePID, strconv.Itoa(os.Getpid()))
	if err := register(ns, newSID, CurrentRuntime(), func(string, ...any) {}); err != nil {
		t.Fatalf("register(%s): %v", newSID, err)
	}

	for _, path := range []string{
		ns.entryPath(heldSID),
		ns.SpoolPath(heldSID),
		ns.cursorPath(heldSID),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("stat %s: %v; prune deleted files for a session whose lock was held", path, err)
		}
	}
}

// --- direct messages -------------------------------------------------------

func TestMonitorEmitsDirectMessagesButNeverItsOwn(t *testing.T) {
	withHome(t)
	const sid = "mon-direct-1"
	m := startMonitor(t, sid)

	// A message stamped with this session's own id must never be emitted:
	// waking a session with its own broadcast is a loop with a model in it.
	if _, err := Send(mailbox(sid), Draft{Text: "echo of my own voice"}, Sender{Kind: "session", SessionID: sid, Name: "me"}); err != nil {
		t.Fatalf("Send (self): %v", err)
	}
	if _, err := Send(mailbox(sid), Draft{Text: "the build is green"}, peer()); err != nil {
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

// TestOwnBroadcastCarriesTheConsumptionCursorWithIt covers the second cursor.
//
// The monitor already refuses to wake a session with its own broadcast and
// advances the monitor cursor to say so. Nothing advanced the consumption
// cursor, so a session that published to a topic it was up to date on was left
// sitting behind its own message forever: a compaction floor pinned by a line
// the session wrote itself, and a cursor whose meaning ("everything before
// here is dealt with") was false about the one message it could be surest of.
func TestOwnBroadcastCarriesTheConsumptionCursorWithIt(t *testing.T) {
	withHome(t)
	const sid = "mon-selfread-1"
	startMonitor(t, sid)
	if err := Subscribe(sid, "chat"); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "the monitor to pick up the subscription", func() bool {
		_, ok := readCursors(sid)["chat"]
		return ok
	})

	if _, err := Publish("chat", Draft{Text: "my own claim"}, Sender{Kind: "session", SessionID: sid}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	eventually(t, 5*time.Second, "the monitor cursor to cross the session's own message", func() bool {
		return readCursors(sid)["chat"] > 0
	})
	eventually(t, 5*time.Second, "the consumption cursor to follow it", func() bool {
		c := readCursors(sid)
		return c[readCursorKey("chat")] == c["chat"]
	})
}

// TestOwnBroadcastDoesNotSkipAPeersUnreadMessage is the guard on the above.
//
// Advancing the consumption cursor to the end of the session's own message
// unconditionally would carry it over anything unread sitting in front: publish
// once to a busy topic and every peer message not yet pulled silently stops
// being unread. Only a cursor already sitting exactly where the session's own
// message begins has read everything before it.
func TestOwnBroadcastDoesNotSkipAPeersUnreadMessage(t *testing.T) {
	withHome(t)
	const sid = "mon-selfread-2"
	m := startMonitor(t, sid)
	if err := Subscribe(sid, "chat"); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "the monitor to pick up the subscription", func() bool {
		_, ok := readCursors(sid)["chat"]
		return ok
	})

	if _, err := Publish("chat", Draft{Text: "a peer said something first"}, peer()); err != nil {
		t.Fatalf("Publish (peer): %v", err)
	}
	eventually(t, 5*time.Second, "the peer's message to be notified", func() bool {
		return m.stdout.has("a peer said something first")
	})
	// Notified, but never pulled -- so it is still unread, and publishing must
	// not change that.
	if _, err := Publish("chat", Draft{Text: "and then I claimed a file"}, Sender{Kind: "session", SessionID: sid}); err != nil {
		t.Fatalf("Publish (self): %v", err)
	}
	eventually(t, 5*time.Second, "the monitor to handle the session's own message", func() bool {
		return readCursors(sid)["chat"] > 0
	})

	items, _, err := DefaultNamespace().ReadInbox(sid, InboxQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Message.Text != "a peer said something first" {
		t.Fatalf("got %d items (%v), want the peer's message still unread: publishing swallowed it",
			len(items), itemTexts(items))
	}
}

func itemTexts(items []InboxItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Message.Text)
	}
	return out
}

// TestABroadcastNamingOthersDoesNotInterruptYou is the addressing gate.
//
// A For list said who a message was for and then changed nothing about who it
// woke. One broadcast naming two sessions was pushed into nine, and the seven
// bystanders each spent a turn deciding it was none of their business.
func TestABroadcastNamingOthersDoesNotInterruptYou(t *testing.T) {
	withHome(t)
	const sid = "mon-forgate-1"
	m := startMonitor(t, sid)
	if err := Subscribe(sid, "chat"); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "the monitor to pick up the subscription", func() bool {
		_, ok := readCursors(sid)["chat"]
		return ok
	})

	if _, err := Publish("chat", Draft{
		Text: "the seeders are mine, shout if mid-edit",
		For:  []string{"inv-engine", "inv-screens"},
	}, peer()); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// Handled, so the monitor cursor crosses it. That is also what makes the
	// absence below real rather than merely early.
	eventually(t, 5*time.Second, "the monitor to handle the message", func() bool {
		return readCursors(sid)["chat"] > 0
	})
	if m.stdout.has("the seeders are mine") {
		t.Errorf("a broadcast naming other sessions interrupted this one:\n%s", m.stdout.String())
	}

	// Held for the inbox, not dropped: not being interrupted is not the same
	// as not being able to find it.
	items, _, err := DefaultNamespace().ReadInbox(sid, InboxQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d inbox items, want the message held for reading", len(items))
	}
}

func TestABroadcastNamingYouStillInterrupts(t *testing.T) {
	withHome(t)
	const sid = "mon-forgate-2"
	m := startMonitor(t, sid)
	if err := Subscribe(sid, "chat"); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "the monitor to pick up the subscription", func() bool {
		_, ok := readCursors(sid)["chat"]
		return ok
	})

	// By short session id, which is the handle every session has: most never
	// declare a name.
	if _, err := Publish("chat", Draft{
		Text: "this one is yours",
		For:  []string{Short(sid)},
	}, peer()); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	eventually(t, 5*time.Second, "the addressed session to be woken", func() bool {
		return m.stdout.has("this one is yours")
	})
	if !m.stdout.has("-> you") {
		t.Errorf("an addressed message lost its marker:\n%s", m.stdout.String())
	}
}

// TestCheckoutTopicIsTheRepositoryNotTheDirectory: a session started in a
// subdirectory has to land with its peers, not in a room of its own.
func TestCheckoutTopicIsTheRepositoryNotTheDirectory(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "caterflow-inventory")
	sub := filepath.Join(repo, "backend", "app")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	if got := CheckoutTopic(repo); got != "caterflow-inventory" {
		t.Errorf("CheckoutTopic(repo) = %q", got)
	}
	if got := CheckoutTopic(sub); got != "caterflow-inventory" {
		t.Errorf("CheckoutTopic(subdir) = %q, want the repository's room", got)
	}
	// A linked worktree has a .git FILE, not a directory.
	wt := filepath.Join(root, "inventory-wt")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: /elsewhere\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := CheckoutTopic(wt); got != "inventory-wt" {
		t.Errorf("CheckoutTopic(worktree) = %q", got)
	}
	// Outside a repository, the directory itself.
	if got := CheckoutTopic(root); got != topicNameFrom(filepath.Base(root)) {
		t.Errorf("CheckoutTopic(non-repo) = %q", got)
	}

	// Reached by a symlink, it is still the same working tree and so must be
	// the same room: two sessions in a room they disagree about would each
	// believe they had announced themselves to the other.
	link := filepath.Join(root, "link-to-inventory")
	if err := os.Symlink(repo, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if got := CheckoutTopic(link); got != "caterflow-inventory" {
		t.Errorf("CheckoutTopic(symlink) = %q, want the room of the tree it points at", got)
	}
}

func TestCheckoutTopicFoldsNamesIntoTheCharset(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Caterflow Inventory", "caterflow-inventory"},
		{"my.project_v2", "my.project_v2"},
		{"---weird---", "weird"},
		{"Ærø", "r"},
		{"", ""},
		{"...", ""},
	} {
		if got := topicNameFrom(tc.in); got != tc.want {
			t.Errorf("topicNameFrom(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestHereResolvesToTheCheckoutsOwnRoom: the room carrying most of the traffic
// was the only tier with no word for it. It is named after the repository,
// which is what makes it legible to everyone else, and that left a session
// unable to name its own room without looking it up first.
func TestHereResolvesToTheCheckoutsOwnRoom(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "caterflow-inventory")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	if got := ResolveTopicAlias("here", repo); got != "caterflow-inventory" {
		t.Errorf(`ResolveTopicAlias("here") = %q, want the checkout's room`, got)
	}
	if got := ResolveTopicAlias("HERE", repo); got != "caterflow-inventory" {
		t.Errorf("the alias should be case-insensitive, got %q", got)
	}
	// It resolves to the real name before anything is written, so what lands
	// in the log and in a peer's notification says which checkout it was.
	if got := ResolveTopicAlias("deploys", repo); got != "deploys" {
		t.Errorf("an ordinary topic was rewritten: %q", got)
	}
	if got := ResolveTopicAlias("@global", repo); got != "@global" {
		t.Errorf("a global topic was rewritten: %q", got)
	}
	// Nowhere to mean: left alone rather than silently widened to a room the
	// caller did not ask for.
	if got := ResolveTopicAlias("here", ""); got != "here" {
		t.Errorf("outside a checkout the alias should stay put, got %q", got)
	}
}

// The three default rooms are three different logs, and the two well-known
// ones are no longer one string with a prefix.
func TestTheThreeDefaultRoomsAreDistinct(t *testing.T) {
	if PublicTopic != "namespace" {
		t.Errorf("PublicTopic = %q", PublicTopic)
	}
	if GlobalPublicTopic != "@global" {
		t.Errorf("GlobalPublicTopic = %q", GlobalPublicTopic)
	}
	if strings.TrimPrefix(GlobalPublicTopic, GlobalPrefix) == PublicTopic {
		t.Error("the machine room is still the namespace room with a prefix, so they share a name")
	}
	// Nothing is called "all" any more: the name read as "everyone" while
	// meaning "everyone in this namespace".
	for _, n := range []string{PublicTopic, GlobalPublicTopic} {
		if strings.Contains(n, "all") {
			t.Errorf("%q still claims to be everyone", n)
		}
	}
}

// TestPrintedTopicNameCanBeTypedBackIn: every notification says "#chat" and
// typing "#chat" used to fail validation, because "#" is decoration and not
// part of the name. Output that is not valid input only bites whoever copies
// what they were shown.
func TestPrintedTopicNameCanBeTypedBackIn(t *testing.T) {
	printed := TopicLabel("chat")
	if printed != "#chat" {
		t.Fatalf("TopicLabel(chat) = %q", printed)
	}
	ref, err := ParseTopicRef(printed)
	if err != nil {
		t.Fatalf("ParseTopicRef(%q): %v", printed, err)
	}
	if ref.Name != "chat" || ref.Global {
		t.Errorf("ParseTopicRef(%q) = %+v, want the bare namespaced topic", printed, ref)
	}
	// The global form is unchanged: "@" is part of the name and selects a
	// different tree.
	if g, err := ParseTopicRef(TopicLabel("@ops")); err != nil || g.Name != "ops" || !g.Global {
		t.Errorf("ParseTopicRef(@ops) = %+v, %v", g, err)
	}
	// One canonical spelling per tree: "#@ops" is not a way to reach the
	// global log.
	if _, err := ParseTopicRef("#@ops"); err == nil {
		t.Error(`"#@ops" was accepted; it must not resolve to the global tree`)
	}
}

func TestMonitorDeliversMailQueuedBeforeItStarted(t *testing.T) {
	withHome(t)
	const sid = "mon-queued-1"
	// Mail written while nothing was listening: the inbox cursor must resume
	// from where it was rather than skipping to the end of the spool, or a
	// `claude --resume` silently loses everything sent while it was away.
	if _, err := Send(mailbox(sid), Draft{Text: "queued while nobody listened"}, peer()); err != nil {
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
	if _, err := Publish("secrets", Draft{Text: "not for you"}, peer()); err != nil {
		t.Fatalf("Publish (#secrets): %v", err)
	}
	if _, err := Publish(PublicTopic, Draft{Text: "deploying to staging in 5"}, peer()); err != nil {
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

	if _, err := Publish("deploys", Draft{Text: "v2.1 rolled out"}, peer()); err != nil {
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
	if _, err := Publish("deploys", Draft{Text: "first while subscribed"}, peer()); err != nil {
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

	if _, err := Publish("deploys", Draft{Text: "second after unsubscribing"}, peer()); err != nil {
		t.Fatalf("Publish (after): %v", err)
	}
	// Barrier: a direct message sent afterwards proves the monitor is still
	// emitting, so the missing topic line is a real absence.
	if _, err := Send(mailbox(sid), Draft{Text: "still listening"}, peer()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	eventually(t, 6*time.Second, "the barrier direct message", func() bool {
		return m.stdout.has("still listening")
	})
	if m.stdout.has("second after unsubscribing") {
		t.Errorf("monitor kept delivering a topic it had left:\n%s", m.stdout.String())
	}
}

// --- delivery modes ----------------------------------------------------------

// A digest topic must collapse a burst into ONE line per interval, not one
// notification per message -- that collapsing is the entire point.
func TestDigestTopicCollapsesMultipleMessagesIntoOneLine(t *testing.T) {
	withDigestInterval(t, 300*time.Millisecond)
	withHome(t)
	const sid = "mon-digest-collapse-1"
	m := startMonitor(t, sid)

	if err := Subscribe(sid, "deploys"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	eventually(t, 6*time.Second, "the monitor to start following #deploys", func() bool {
		return m.stderr.has(`following topic "deploys"`)
	})
	if err := SetDelivery(sid, "deploys", DeliveryDigest); err != nil {
		t.Fatalf("SetDelivery: %v", err)
	}

	for i := 0; i < 3; i++ {
		if _, err := Publish("deploys", Draft{Text: fmt.Sprintf("build %d", i)}, peer()); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}

	eventually(t, 3*time.Second, "the digest line", func() bool {
		return m.stdout.has("3 waiting on #deploys")
	})
	for i := 0; i < 3; i++ {
		if m.stdout.has(fmt.Sprintf("build %d", i)) {
			t.Errorf("a digest topic pushed an individual message instead of collapsing them:\n%s", m.stdout.String())
		}
	}
	if got := strings.Count(m.stdout.String(), "waiting on #deploys"); got != 1 {
		t.Errorf("the digest line appeared %d time(s), want exactly 1 (one per interval, not one per message)", got)
	}
}

// An alert bypasses a digest topic's buffering entirely: it is scarce by
// construction (see PriorityAlert), and holding it for a minute defeats the
// one thing it exists to do.
func TestAlertOnADigestTopicPushesImmediately(t *testing.T) {
	withHome(t)
	const sid = "mon-digest-alert-1"
	m := startMonitor(t, sid)

	if err := Subscribe(sid, "deploys"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	eventually(t, 6*time.Second, "the monitor to start following #deploys", func() bool {
		return m.stderr.has(`following topic "deploys"`)
	})
	if err := SetDelivery(sid, "deploys", DeliveryDigest); err != nil {
		t.Fatalf("SetDelivery: %v", err)
	}

	if _, err := Publish("deploys", Draft{Text: "prod is down", Priority: PriorityAlert}, peer()); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	eventually(t, 3*time.Second, "the alert to push immediately, without waiting for a digest tick", func() bool {
		return m.stdout.has("prod is down")
	})
}

// quiet is absolute: unlike digest, not even an alert earns an immediate
// push there. A peer's self-assessed urgency cannot override a session that
// asked not to be interrupted at all.
func TestAlertOnAQuietTopicDoesNotPushImmediately(t *testing.T) {
	withHome(t)
	const sid = "mon-quiet-alert-1"
	m := startMonitor(t, sid)

	if err := Subscribe(sid, "deploys"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	eventually(t, 6*time.Second, "the monitor to start following #deploys", func() bool {
		return m.stderr.has(`following topic "deploys"`)
	})
	if err := SetDelivery(sid, "deploys", DeliveryQuiet); err != nil {
		t.Fatalf("SetDelivery: %v", err)
	}

	if _, err := Publish("deploys", Draft{Text: "prod is down", Priority: PriorityAlert}, peer()); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// Barrier: a direct message sent afterwards proves the monitor is alive
	// and processing, so the missing alert text below is a real absence
	// rather than a slow test.
	if _, err := Send(mailbox(sid), Draft{Text: "still listening"}, peer()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	eventually(t, 6*time.Second, "the barrier direct message", func() bool {
		return m.stdout.has("still listening")
	})
	if m.stdout.has("prod is down") {
		t.Errorf("an alert on a quiet topic was pushed immediately; quiet must be absolute:\n%s", m.stdout.String())
	}
}

// A message naming this session in For pushes immediately on a digest topic,
// the same as an alert -- it is not chatter, it is something this session was
// specifically asked to act on.
func TestForNamedMessagePushesOnADigestTopic(t *testing.T) {
	withHome(t)
	const sid = "mon-digest-for-1"
	m := startMonitor(t, sid)

	if err := Subscribe(sid, "deploys"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	eventually(t, 6*time.Second, "the monitor to start following #deploys", func() bool {
		return m.stderr.has(`following topic "deploys"`)
	})
	if err := SetDelivery(sid, "deploys", DeliveryDigest); err != nil {
		t.Fatalf("SetDelivery: %v", err)
	}

	// Short(sid), not a declared name: this session was never given one, and
	// For matches either handle (see Message.IsFor).
	if _, err := Publish("deploys", Draft{Text: "please review this", For: []string{Short(sid)}}, peer()); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	eventually(t, 3*time.Second, "the for-named message to push immediately", func() bool {
		return m.stdout.has("please review this")
	})
}

// The one property Part 1 exists for: a message held in an unflushed digest
// buffer must not have its cursor crossed by something else on the same
// topic, or a monitor that dies before the next flush loses it for good.
//
// The synchronization here is structural, not timing-based: "routine change"
// and the alert that follows it are both on #deploys, read by the one
// goroutine following that log, in that order. So once the alert -- which
// pushes immediately -- is actually on stdout, "routine change" has
// necessarily already been read and folded into the digest buffer by the
// single-threaded delivery loop, whether or not the buffer has flushed yet.
func TestMonitorCursorHoldsAtAnUnflushedDigestMessageThenAdvancesOnFlush(t *testing.T) {
	withDigestInterval(t, 300*time.Millisecond)
	withHome(t)
	const sid = "mon-digest-cursor-1"
	m := startMonitor(t, sid)

	if err := Subscribe(sid, "deploys"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	eventually(t, 6*time.Second, "the monitor to start following #deploys", func() bool {
		return m.stderr.has(`following topic "deploys"`)
	})
	if err := SetDelivery(sid, "deploys", DeliveryDigest); err != nil {
		t.Fatalf("SetDelivery: %v", err)
	}

	if _, err := Publish("deploys", Draft{Text: "routine change"}, peer()); err != nil {
		t.Fatalf("Publish (routine): %v", err)
	}
	if _, err := Publish("deploys", Draft{Text: "urgent followup", Priority: PriorityAlert}, peer()); err != nil {
		t.Fatalf("Publish (alert): %v", err)
	}
	eventually(t, 3*time.Second, "the alert to push past the still-buffered routine message", func() bool {
		return m.stdout.has("urgent followup")
	})

	// "routine change" is confirmed read and buffered by now (see the doc
	// comment above), but the digest has not flushed, so the topic's cursor
	// must still be exactly where it started: untouched.
	if got := readCursors(sid)["deploys"]; got != 0 {
		t.Errorf("cursor for #deploys = %d before any digest flush, want 0 (must not advance past the buffered message)", got)
	}

	eventually(t, 3*time.Second, "the digest to flush", func() bool {
		return m.stdout.has("1 waiting on #deploys")
	})
	eventually(t, 3*time.Second, "the cursor to advance once the digest flushes", func() bool {
		return readCursors(sid)["deploys"] > 0
	})
}

// --- supersedes --------------------------------------------------------------

// impostor is a plausible sender that is neither the session under test nor
// peer() -- used to prove a supersede claim is checked against the exact
// original sender, not merely "some session".
func impostor() Sender {
	return Sender{Kind: "session", SessionID: "cccc4444-5555", Name: "gamma", Cwd: "/home/g/web"}
}

// The one security rule this feature exists to enforce: a peer that did not
// send the original may not supersede it. An impostor's claim must be
// ignored entirely -- the message delivered as ordinary, no correction
// marker anywhere in the line.
func TestMonitorIgnoresASupersedeClaimFromADifferentSender(t *testing.T) {
	withHome(t)
	const sid = "mon-supersede-forge-1"
	m := startMonitor(t, sid)

	original, err := Send(mailbox(sid), Draft{Text: "STOP AND READ, something was destroyed", Priority: PriorityAlert}, peer())
	if err != nil {
		t.Fatalf("Send (original): %v", err)
	}
	eventually(t, 5*time.Second, "the original alert", func() bool {
		return m.stdout.has("STOP AND READ")
	})

	if _, err := Send(mailbox(sid), Draft{Text: "false alarm, ignore that", Supersedes: original.ID}, impostor()); err != nil {
		t.Fatalf("Send (forged supersede): %v", err)
	}
	eventually(t, 5*time.Second, "the forged supersede to be delivered", func() bool {
		return m.stdout.has("false alarm, ignore that")
	})
	if m.stdout.has("correction") {
		t.Errorf("a supersede claim from a different sender than the original was honoured:\n%s", m.stdout.String())
	}
}

// A legitimate correction of a message this monitor already pushed carries
// the marker wherever it is rendered: nothing was buffered to drop, so
// "already emitted" applies.
func TestMonitorRendersTheCorrectionMarkerForAnAlreadyEmittedMessage(t *testing.T) {
	withHome(t)
	const sid = "mon-supersede-correct-1"
	m := startMonitor(t, sid)

	original, err := Send(mailbox(sid), Draft{Text: "STOP AND READ, something was destroyed", Priority: PriorityAlert}, peer())
	if err != nil {
		t.Fatalf("Send (original): %v", err)
	}
	eventually(t, 5*time.Second, "the original alert", func() bool {
		return m.stdout.has("STOP AND READ")
	})

	if _, err := Send(mailbox(sid), Draft{Text: "false alarm, nothing was destroyed", Supersedes: original.ID}, peer()); err != nil {
		t.Fatalf("Send (correction): %v", err)
	}
	eventually(t, 5*time.Second, "the correction to be delivered", func() bool {
		return m.stdout.has("false alarm, nothing was destroyed")
	})
	if !m.stdout.has("↺ correction") {
		t.Errorf("a legitimate correction of an already-emitted message did not carry the marker:\n%s", m.stdout.String())
	}
}

// The scenario this whole feature exists for: an alert lands in a digest
// buffer, and a retraction from the same sender arrives before the buffer
// flushes. The alarm itself must never be shown -- dropped from the buffer
// entirely -- while the retraction goes on to be handled as an ordinary
// message on the same topic. The cursor must still cross both once the
// buffer flushes, or a monitor restart would replay the alarm forever.
func TestSupersedeDropsAStillBufferedMessageAndTheCursorAdvancesPastBoth(t *testing.T) {
	withDigestInterval(t, 300*time.Millisecond)
	withHome(t)
	const sid = "mon-supersede-drop-1"
	m := startMonitor(t, sid)

	if err := Subscribe(sid, "alerts"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	eventually(t, 6*time.Second, "the monitor to start following #alerts", func() bool {
		return m.stderr.has(`following topic "alerts"`)
	})
	if err := SetDelivery(sid, "alerts", DeliveryDigest); err != nil {
		t.Fatalf("SetDelivery: %v", err)
	}

	original, err := Publish("alerts", Draft{Text: "SOMEBODY RAN git reset --hard"}, peer())
	if err != nil {
		t.Fatalf("Publish (original): %v", err)
	}
	// Barrier: a direct message proves the original has already been read
	// and folded into the digest buffer before the retraction is sent.
	if _, err := Send(mailbox(sid), Draft{Text: "still listening 1"}, peer()); err != nil {
		t.Fatalf("Send (barrier 1): %v", err)
	}
	eventually(t, 6*time.Second, "the first barrier", func() bool {
		return m.stdout.has("still listening 1")
	})

	if _, err := Publish("alerts", Draft{Text: "false alarm, nothing was destroyed", Supersedes: original.ID}, peer()); err != nil {
		t.Fatalf("Publish (retraction): %v", err)
	}
	// Barrier 2: proves the retraction itself has been read and processed --
	// dropping the original from the buffer -- before the digest is checked.
	if _, err := Send(mailbox(sid), Draft{Text: "still listening 2"}, peer()); err != nil {
		t.Fatalf("Send (barrier 2): %v", err)
	}
	eventually(t, 6*time.Second, "the second barrier", func() bool {
		return m.stdout.has("still listening 2")
	})

	eventually(t, 3*time.Second, "the digest to flush", func() bool {
		return m.stdout.has("waiting on #alerts")
	})
	// The alarm was dropped; the retraction was not (it is an ordinary
	// message on the topic once its own Supersedes is cleared -- see
	// resolveSupersede). One buffered message survives to be counted, not
	// two and not zero.
	if !m.stdout.has("1 waiting on #alerts") {
		t.Errorf("digest line did not report exactly 1 survivor (the alarm dropped, the retraction kept):\n%s", m.stdout.String())
	}
	if got := strings.Count(m.stdout.String(), "waiting on #alerts"); got != 1 {
		t.Errorf("the digest line appeared %d time(s), want exactly 1", got)
	}

	want := endOffset(CurrentNamespace().TopicPath("alerts"))
	eventually(t, 3*time.Second, "the cursor to advance past both the dropped and the surviving message", func() bool {
		return readCursors(sid)["alerts"] == want
	})
}

// A message the rate limiter suppresses is still HANDLED -- it is deliberately
// not re-notified (see newRateLimiter) -- so the cursor has to cross it same
// as a pushed one, or the same flood is reconsidered forever.
func TestRateLimitSuppressedMessageStillAdvancesTheCursor(t *testing.T) {
	withHome(t)
	const sid = "mon-ratelimit-cursor-1"
	m := startMonitor(t, sid)

	// Enough direct messages to spill past the normal-traffic cap and into
	// suppression (see newRateLimiter's alertReserve), well inside the
	// one-minute window so none of this relies on it rolling over.
	const total = maxPerMinute - alertReserve + 5
	for i := 0; i < total; i++ {
		if _, err := Send(mailbox(sid), Draft{Text: fmt.Sprintf("msg %d", i)}, peer()); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}

	want := endOffset(SpoolPath(sid))
	eventually(t, 6*time.Second, "the inbox cursor to cross every message, suppressed included", func() bool {
		return readCursors(sid)[inboxCursorKey] == want
	})

	// If this test's flood never actually triggered suppression it would not
	// be testing anything: confirm the tail really was held back rather than
	// printed, now that the cursor above proves it was still handled.
	last := fmt.Sprintf("msg %d", total-1)
	if m.stdout.has(last) {
		t.Fatalf("%q was printed rather than suppressed; this test needs a real suppression to prove anything", last)
	}
}

// Shutdown must flush whatever digest is still buffered, the same way it
// already flushes rate-limit suppression notices: a session that stops its
// monitor with an unflushed digest tick pending must still learn what was
// waiting, not lose it silently to a ticker that never got to fire.
func TestMonitorFlushesPendingDigestOnShutdown(t *testing.T) {
	// A long interval: the flush this test checks for must come from
	// shutdown, not from the ticker winning a race against it.
	withDigestInterval(t, time.Hour)
	withHome(t)
	const sid = "mon-digest-shutdown-1"
	m := startMonitor(t, sid)

	if err := Subscribe(sid, "deploys"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	eventually(t, 6*time.Second, "the monitor to start following #deploys", func() bool {
		return m.stderr.has(`following topic "deploys"`)
	})
	if err := SetDelivery(sid, "deploys", DeliveryDigest); err != nil {
		t.Fatalf("SetDelivery: %v", err)
	}
	if _, err := Publish("deploys", Draft{Text: "routine change"}, peer()); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	// Barrier: wait for the monitor to say it has actually buffered the
	// message. A direct message used to stand in for this and could not: the
	// spool and the topic have separate followers, so the direct one being
	// emitted proves nothing about whether the topic one has been read yet.
	eventually(t, 6*time.Second, "the message to be held for the digest", func() bool {
		return m.stderr.has(`holding a message on "deploys"`)
	})

	m.stop(t) // triggers RunMonitor's deferred flushDigests via the SIGTERM path

	if !m.stdout.has("1 waiting on #deploys") {
		t.Errorf("shutdown did not flush the pending digest:\n%s", m.stdout.String())
	}
}

// Push mode is the default and must behave exactly as it did before delivery
// modes existed: the notification line the monitor prints is Render's output,
// verbatim, with nothing else on stdout around it.
func TestPushModeNotificationIsByteIdenticalToRender(t *testing.T) {
	withHome(t)
	const sid = "mon-push-identical-1"
	m := startMonitor(t, sid)

	msg, err := Send(mailbox(sid), Draft{Text: "the build is green", Subject: "ci"}, peer())
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	eventually(t, 5*time.Second, "the notification line", func() bool {
		return m.stdout.has("the build is green")
	})

	self, err := ReadEntry(sid)
	if err != nil {
		t.Fatalf("ReadEntry: %v", err)
	}
	want := DefaultNamespace().Render(msg, self) + "\n"
	if got := m.stdout.String(); got != want {
		t.Errorf("push-mode notification differs from Render's own output:\ngot:  %q\nwant: %q", got, want)
	}
}

// renderDigestLine is peer-controlled input rendered into a notification, the
// same threat model as Render itself: a sender name may carry the structural
// characters Sanitize exists to neutralise, and a burst of senders may not fit
// the notification budget at all.
func TestRenderDigestLineSanitisesSenderNames(t *testing.T) {
	line := renderDigestLine("deploys", 2, []string{"al<pha>", "be]ta["})
	if !strings.HasPrefix(line, "[pigeon] 2 waiting on #deploys from ") {
		t.Errorf("unexpected line shape: %q", line)
	}
	if !strings.HasSuffix(line, "-- read with the inbox tool") {
		t.Errorf("missing trailer: %q", line)
	}
	body := strings.TrimSuffix(strings.TrimPrefix(line, "[pigeon] 2 waiting on #deploys from "), " -- read with the inbox tool")
	if strings.ContainsAny(body, "<>[]") {
		t.Errorf("an unsanitised structural character reached the line: %q", line)
	}
}

func TestRenderDigestLineIsBoundedByRenderBudget(t *testing.T) {
	senders := make([]string, 50)
	for i := range senders {
		senders[i] = strings.Repeat("x", 40)
	}
	line := renderDigestLine("deploys", len(senders), senders)
	if n := len([]rune(line)); n > RenderBudget {
		t.Errorf("digest line is %d runes, want at most RenderBudget (%d)", n, RenderBudget)
	}
}

// --- namespaces ------------------------------------------------------------
//
// The library tests cover which directory each call reads. These cover the one
// thing only a running monitor can show: what actually wakes a session.

// peerFrom is a sender in a named namespace, as a real one always is.
func peerFrom(ns string) Sender {
	s := peer()
	s.Namespace = ns
	return s
}

// The deliberate hole in the isolation: a machine-wide broadcast has to reach
// everybody, whatever namespace they armed in.
func TestMonitorReceivesGlobalBroadcastsFromAnotherNamespace(t *testing.T) {
	withHome(t)
	t.Setenv(EnvNamespace, "acme")
	const sid = "mon-global-1"
	m := startMonitor(t, sid)

	if _, err := DefaultNamespace().Publish(GlobalPublicTopic, Draft{Text: "everyone please stand by"}, peerFrom(DefaultNamespaceName)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	eventually(t, 6*time.Second, "the machine-wide broadcast", func() bool {
		return m.stdout.has("everyone please stand by")
	})
	// Arriving from outside the recipient's boundary is the one thing that
	// changes how the line should be read, so it has to say so.
	if !m.stdout.has("[ns: " + DefaultNamespaceName + "]") {
		t.Errorf("the notification does not name the sender's namespace:\n%s", m.stdout.String())
	}
}

// And the rule it is an exception to: a plain topic is one log per namespace,
// so an identically named one next door must never wake this session.
func TestMonitorIgnoresANamespacedTopicFromAnotherNamespace(t *testing.T) {
	withHome(t)
	t.Setenv(EnvNamespace, "acme")
	const sid = "mon-nsleak-1"
	m := startMonitor(t, sid)

	acme := mustNS(t, "acme")
	if err := acme.Subscribe(sid, "deploys"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	eventually(t, 6*time.Second, "the monitor to follow #deploys", func() bool {
		return m.stderr.has(`following topic "deploys"`)
	})

	if _, err := DefaultNamespace().Publish("deploys", Draft{Text: "not for you"}, peerFrom(DefaultNamespaceName)); err != nil {
		t.Fatalf("Publish (other namespace): %v", err)
	}
	// Barrier: a publish into this namespace's own #deploys proves the follower
	// is running, so the missing line above is a real absence.
	if _, err := acme.Publish("deploys", Draft{Text: "this one is ours"}, peerFrom("acme")); err != nil {
		t.Fatalf("Publish (own namespace): %v", err)
	}
	eventually(t, 6*time.Second, "the barrier message", func() bool {
		return m.stdout.has("this one is ours")
	})
	if m.stdout.has("not for you") {
		t.Errorf("a topic published in another namespace was delivered:\n%s", m.stdout.String())
	}
}

// Sending across is allowed, so the recipient has to be able to reply. A bare
// address would either miss or find a different session answering to the same
// name here.
func TestMonitorDeliversACrossNamespaceDirectMessage(t *testing.T) {
	withHome(t)
	t.Setenv(EnvNamespace, "acme")
	const sid = "mon-crossns-1"
	m := startMonitor(t, sid)

	if _, err := mustNS(t, "acme").Send(mailbox(sid), Draft{Text: "from next door"}, peerFrom(DefaultNamespaceName)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	eventually(t, 6*time.Second, "the cross-namespace message", func() bool {
		return m.stdout.has("from next door")
	})
	if !m.stdout.has("pigeon send -n " + DefaultNamespaceName + " beta") {
		t.Errorf("the reply hint does not name the sender's namespace:\n%s", m.stdout.String())
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

// followSource no longer persists a cursor itself -- see its doc comment --
// so this test now checks the replacement contract: every message carries the
// logical offset immediately after it, and resuming a fresh follower from
// that offset (as the delivery side does once it has actually handled the
// message) picks up only what is new.
func TestFollowSourceStampsOffsetsSoARestartDoesNotReplay(t *testing.T) {
	dir := withHome(t)
	path := filepath.Join(dir, "topic.ndjson")
	appendMessage(t, path, "one")
	appendMessage(t, path, "two")

	out := make(chan followedMessage, 8)
	stop := make(chan struct{})
	go followSource(path, 0, "topic", out, stop, func(string, ...any) {})

	var lastOffset int64
	for _, want := range []string{"one", "two"} {
		select {
		case fm := <-out:
			if fm.msg.Text != want {
				t.Fatalf("got %q, want %q -- the log must be read in order", fm.msg.Text, want)
			}
			if fm.source != "topic" {
				t.Fatalf("source = %q, want %q", fm.source, "topic")
			}
			lastOffset = fm.offset
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for %q", want)
		}
	}
	if lastOffset != endOffset(path) {
		t.Errorf("offset on the last message = %d, want %d (the end of the log)", lastOffset, endOffset(path))
	}
	close(stop)

	// A replay here would re-notify the session with everything it has
	// already seen every time its monitor restarts.
	out2 := make(chan followedMessage, 8)
	stop2 := make(chan struct{})
	defer close(stop2)
	go followSource(path, lastOffset, "topic", out2, stop2, func(string, ...any) {})
	appendMessage(t, path, "three")

	select {
	case fm := <-out2:
		if fm.msg.Text != "three" {
			t.Fatalf("got %q after restart, want only the new line", fm.msg.Text)
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

	out := make(chan followedMessage, 4)
	stop := make(chan struct{})
	defer close(stop)
	go followSource(path, 0, "topic", out, stop, func(string, ...any) {})
	appendMessage(t, path, "after the junk")

	select {
	case fm := <-out:
		if fm.msg.Text != "after the junk" {
			t.Fatalf("got %q, want the message after the junk line", fm.msg.Text)
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
