//go:build !windows

// Listen tests share the monitor_test harness (syncWriter, eventually, peer,
// mailbox, and the process-wide SIGTERM catcher in TestMain), so they carry the
// same Unix-only build tag. They do not signal to stop, though -- every listener
// here exits on --count or --timeout, which is deterministic and needs no signal.

package pigeon

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// listener is a Listen running in the background for one test.
type listener struct {
	stdout *syncWriter
	stderr *syncWriter
	exited chan error
}

func startListen(t *testing.T, opts ListenOptions) *listener {
	t.Helper()
	l := &listener{
		stdout: &syncWriter{},
		stderr: &syncWriter{},
		exited: make(chan error, 1),
	}
	go func() { l.exited <- Listen(opts, l.stdout, l.stderr) }()
	return l
}

// waitReady blocks until the followers are up. Both shapes log a "listening ..."
// line only after their sources are launched (and, for an inbox, after its
// cursors are seeded), so a publish after this cannot be missed for timing.
func (l *listener) waitReady(t *testing.T) {
	t.Helper()
	eventually(t, 5*time.Second, "the listener to start", func() bool {
		return l.stderr.has("listening ")
	})
}

func (l *listener) waitExit(t *testing.T) error {
	t.Helper()
	select {
	case err := <-l.exited:
		return err
	case <-time.After(15 * time.Second):
		t.Fatal("Listen did not return within 15s")
		return nil
	}
}

func TestListenAnonymousTailReceivesPublish(t *testing.T) {
	withHome(t)
	l := startListen(t, ListenOptions{
		Namespace: DefaultNamespace(),
		Topics:    []string{"deploys"},
		Plain:     true,
		Count:     1,
		Timeout:   15 * time.Second,
	})
	l.waitReady(t)

	if _, err := Publish("deploys", Draft{Text: "v2.1 rolled out"}, peer()); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := l.waitExit(t); err != nil {
		t.Errorf("Listen returned %v, want a clean exit", err)
	}
	if !l.stdout.has("v2.1 rolled out") {
		t.Errorf("stdout missing the published body:\n%s", l.stdout.String())
	}
	if !l.stdout.has("[pigeon #deploys]") {
		t.Errorf("human output missing the topic header:\n%s", l.stdout.String())
	}
}

// A default listen delivers only what arrives while it is listening; --replay
// drains what is already on the log.
func TestListenReplayVsFromNow(t *testing.T) {
	t.Run("from now on skips history", func(t *testing.T) {
		withHome(t)
		if _, err := Publish("deploys", Draft{Text: "published before"}, peer()); err != nil {
			t.Fatalf("Publish: %v", err)
		}
		l := startListen(t, ListenOptions{
			Namespace: DefaultNamespace(),
			Topics:    []string{"deploys"},
			Plain:     true,
			Count:     1,
			Timeout:   time.Second, // nothing new arrives, so exit on timeout
		})
		_ = l.waitExit(t)
		if l.stdout.has("published before") {
			t.Errorf("a from-now-on listen replayed history:\n%s", l.stdout.String())
		}
	})

	t.Run("replay drains history", func(t *testing.T) {
		withHome(t)
		if _, err := Publish("deploys", Draft{Text: "published before"}, peer()); err != nil {
			t.Fatalf("Publish: %v", err)
		}
		l := startListen(t, ListenOptions{
			Namespace: DefaultNamespace(),
			Topics:    []string{"deploys"},
			Plain:     true,
			Replay:    true,
			Count:     1,
			Timeout:   15 * time.Second,
		})
		if err := l.waitExit(t); err != nil {
			t.Errorf("Listen returned %v", err)
		}
		if !l.stdout.has("published before") {
			t.Errorf("--replay did not deliver the existing message:\n%s", l.stdout.String())
		}
	})
}

// An inbox registers a visible ephemeral session, receives both a direct message
// and a subscribed topic, and vanishes on a clean exit.
func TestListenInboxRegistersReceivesAndDeregisters(t *testing.T) {
	withHome(t)
	const name = "inbox"
	sid := syntheticSessionID(name)

	l := startListen(t, ListenOptions{
		Namespace: DefaultNamespace(),
		As:        name,
		Plain:     true,
		Count:     2, // one direct, one topic
		Timeout:   15 * time.Second,
	})
	l.waitReady(t)

	var e *Entry
	eventually(t, 5*time.Second, "the inbox entry to be written", func() bool {
		var err error
		e, err = ReadEntry(sid)
		return err == nil
	})
	if !e.Ephemeral {
		t.Error("inbox entry is not marked Ephemeral")
	}
	if e.Status != StatusLive {
		t.Errorf("Status = %q, want %q while the listener holds the lock", e.Status, StatusLive)
	}
	if e.PID != os.Getpid() {
		t.Errorf("PID = %d, want this process %d", e.PID, os.Getpid())
	}
	if got, err := ResolveTarget(name); err != nil || got.SessionID != sid {
		t.Errorf("ResolveTarget(%q) = %v, %v; want the inbox %s", name, got, err, sid)
	}

	if _, err := Send(mailbox(sid), Draft{Text: "direct-hello"}, peer()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := Publish(PublicTopic, Draft{Text: "topic-hello"}, peer()); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	if err := l.waitExit(t); err != nil {
		t.Errorf("Listen returned %v", err)
	}
	if !l.stdout.has("direct-hello") {
		t.Errorf("stdout missing the direct message:\n%s", l.stdout.String())
	}
	if !l.stdout.has("topic-hello") {
		t.Errorf("stdout missing the topic message:\n%s", l.stdout.String())
	}
	// Ephemeral: the entry, spool and cursors are taken down on a clean exit.
	if _, err := ReadEntry(sid); err == nil {
		t.Error("the inbox entry survived a clean exit; it should be ephemeral")
	}
	if _, err := os.Stat(DefaultNamespace().SpoolPath(sid)); err == nil {
		t.Error("the inbox spool survived a clean exit")
	}
}

func TestListenJSONOutput(t *testing.T) {
	withHome(t)
	l := startListen(t, ListenOptions{
		Namespace: DefaultNamespace(),
		Topics:    []string{"deploys"},
		JSON:      true,
		Count:     1,
		Timeout:   15 * time.Second,
	})
	l.waitReady(t)

	if _, err := Publish("deploys", Draft{Text: "json-body"}, peer()); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := l.waitExit(t); err != nil {
		t.Errorf("Listen returned %v", err)
	}

	line := strings.TrimSpace(l.stdout.String())
	var ev struct {
		Text      string `json:"text"`
		Topic     string `json:"topic"`
		Namespace string `json:"namespace"`
		From      Sender `json:"from"`
	}
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		t.Fatalf("output is not one JSON object per line: %v\n%s", err, line)
	}
	if ev.Text != "json-body" {
		t.Errorf("text = %q, want %q", ev.Text, "json-body")
	}
	if ev.Topic != "deploys" {
		t.Errorf("topic = %q, want %q", ev.Topic, "deploys")
	}
	if ev.Namespace != DefaultNamespaceName {
		t.Errorf("namespace = %q, want %q", ev.Namespace, DefaultNamespaceName)
	}
}

// With neither --json nor --plain, a non-terminal stdout (a pipe) gets NDJSON.
func TestListenAutoNDJSONWhenNotTTY(t *testing.T) {
	withHome(t)
	l := startListen(t, ListenOptions{
		Namespace: DefaultNamespace(),
		Topics:    []string{"deploys"},
		TTY:       false, // a pipe
		Count:     1,
		Timeout:   15 * time.Second,
	})
	l.waitReady(t)

	if _, err := Publish("deploys", Draft{Text: "auto-json"}, peer()); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := l.waitExit(t); err != nil {
		t.Errorf("Listen returned %v", err)
	}
	line := strings.TrimSpace(l.stdout.String())
	if !strings.HasPrefix(line, "{") {
		t.Errorf("a piped listen did not default to NDJSON:\n%s", line)
	}
}

func TestListenAnonymousRequiresTopic(t *testing.T) {
	withHome(t)
	err := Listen(ListenOptions{Namespace: DefaultNamespace()}, io.Discard, io.Discard)
	if err == nil {
		t.Error("an anonymous tail with no topics should be an error")
	}
}

// A second listener on the same name loses the lock, and -- critically -- must
// not tear down the first listener's live entry or spool on its way out.
func TestListenSecondInboxSameNameFails(t *testing.T) {
	withHome(t)
	const name = "dup"
	sid := syntheticSessionID(name)

	first := startListen(t, ListenOptions{
		Namespace: DefaultNamespace(),
		As:        name,
		Plain:     true,
		Count:     1,
		Timeout:   15 * time.Second,
	})
	first.waitReady(t)
	eventually(t, 5*time.Second, "the first inbox to register", func() bool {
		_, err := ReadEntry(sid)
		return err == nil
	})

	if err := Listen(ListenOptions{
		Namespace: DefaultNamespace(),
		As:        name,
		Count:     1,
		Timeout:   time.Second,
	}, io.Discard, io.Discard); err == nil {
		t.Error("a second listener took an inbox name already held")
	}

	// The failed second listener must not have removed the first's state.
	if _, err := ReadEntry(sid); err != nil {
		t.Errorf("the first inbox's entry was removed by the failed second listener: %v", err)
	}

	// Stop the first cleanly by delivering its one message.
	if _, err := Send(mailbox(sid), Draft{Text: "bye"}, peer()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := first.waitExit(t); err != nil {
		t.Errorf("first listener returned %v", err)
	}
}
