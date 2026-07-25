package pigeon

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"
)

// Listen is the receive half of pigeon for a plain shell -- the same delivery
// machinery the monitor runs, printing to a terminal instead of surfacing a task
// notification inside a session. It blocks until interrupted (or --count /
// --timeout is reached), tailing the logs with the exact followSource loop the
// monitor uses, so it inherits its compaction- and truncation-tolerance for free.
//
// Two shapes, chosen by whether an identity was given:
//
//   - anonymous tail: follow the named topics only, register nothing, stay
//     invisible. This is the focused automation subscriber.
//   - named inbox (As set): register a visible ephemeral session, hold the
//     liveness lock, follow the direct spool plus subscriptions. It shows up in
//     `pigeon ls` as a shell and is addressable as `pigeon send <name>`.
type ListenOptions struct {
	Namespace Namespace     // where to listen; resolved by the caller
	As        string        // acting name; "" means an anonymous tail
	Topics    []string      // topics to follow (validated here)
	JSON      bool          // force NDJSON output
	Plain     bool          // force the human line
	Replay    bool          // start from the beginning of the logs, not "from now on"
	Count     int           // exit after this many messages (0 = unbounded)
	Timeout   time.Duration // exit after this long (0 = no limit)
	// TTY tells Listen whether stdout is a terminal, so that with neither --json
	// nor --plain it can default to NDJSON for a pipe and the human line for a
	// person. The caller detects it, because only it holds the real os.Stdout.
	TTY bool
}

// listenEvent is one line of NDJSON output: the stored message plus the
// namespace it arrived in, and -- when the body overflowed to a payload file --
// the full untruncated text inlined, so a consumer never has to open the file.
type listenEvent struct {
	*Message
	Namespace string `json:"namespace"`
	Body      string `json:"body,omitempty"`
}

// Listen runs the receive loop. stdout carries the messages (parsed by
// automation); stderr carries the human-facing notices, so piping stdout to jq
// does not mix the two.
func Listen(opts ListenOptions, stdout, stderr io.Writer) error {
	logf := func(format string, a ...any) {
		fmt.Fprintf(stderr, "[pigeon] "+format+"\n", a...)
	}

	ns := opts.Namespace
	if err := ns.EnsureDirs(); err != nil {
		return err
	}

	// Validate the topics up front, and refuse a machine-wide one from a private
	// namespace for the same reason a monitor does: a private namespace is sealed
	// against `@` topics in both directions.
	var topics []TopicRef
	for _, t := range opts.Topics {
		ref, err := ParseTopicRef(t)
		if err != nil {
			return err
		}
		if ref.Global && ns.IsPrivate() {
			return fmt.Errorf("namespace %q is private, so it cannot follow the machine-wide topic %s", ns, ref)
		}
		topics = append(topics, ref)
	}

	// NDJSON for a pipe, the human line for a terminal, unless forced either way.
	emitJSON := opts.JSON || (!opts.Plain && !opts.TTY)

	sigc := make(chan os.Signal, 1)
	// os.Interrupt and SIGTERM only: both exist on every platform, so this file
	// needs no build tag. Ctrl-C and `kill` are all a shell listener has to
	// answer to.
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigc)

	done := make(chan struct{})
	closed := false
	closeDone := func() {
		if !closed {
			closed = true
			close(done)
		}
	}
	lines := make(chan *Message, 256)

	var (
		sid  string
		lock io.Closer
	)
	// One cleanup, in a fixed order: stop the followers, vanish from `ls`, take
	// the ephemeral spool and cursors with us, then release the liveness lock.
	// RemoveEntry runs before the state removal, and both mutateCursors and
	// MutateEntry gate on the entry existing, so a follower still winding down
	// cannot re-create what we just cleared.
	defer func() {
		closeDone()
		if sid != "" {
			ns.RemoveEntry(sid)
			removeEphemeralState(ns, sid)
		}
		if lock != nil {
			_ = lock.Close()
		}
	}()

	if opts.As != "" {
		if err := ValidName(opts.As); err != nil {
			return err
		}
		// Deliberately a local, not the outer sid, until we have both the lock and
		// a written entry. The deferred cleanup keys off sid, so setting it before
		// we own this name would make a listener that failed to start -- lost the
		// lock, or found the name taken -- delete the live entry and spool of the
		// listener that already holds it.
		id := syntheticSessionID(opts.As)

		// The liveness lock is the mutual exclusion: a second listener on the same
		// name lands on the same lock and stands down, exactly like a second
		// monitor for one session.
		l, acquired, err := tryExclusive(ns.LockPath(id))
		if err != nil {
			return err
		}
		if !acquired {
			return fmt.Errorf("another listener already holds the inbox %q in namespace %s", opts.As, ns)
		}
		lock = l

		// A name is an address, so it must be unique among live sessions -- a real
		// session that declared the same name would otherwise be shadowed.
		if ns.NameTaken(opts.As, id) {
			return fmt.Errorf("another live session already answers to %q in namespace %s", opts.As, ns)
		}

		if err := registerEphemeral(ns, id, opts.As, topics, opts.Replay); err != nil {
			return err
		}
		// From here we own the name: the entry is written and the lock is ours, so
		// the deferred cleanup may safely tear it all down.
		sid = id

		// Keep the entry fresh so a live inbox is never mistaken for a wedged one.
		go ephemeralHeartbeat(ns, sid, done)

		// Direct inbox: create the spool so the follower has something to stat,
		// then follow it from the end (or the start under --replay).
		spool := ns.SpoolPath(sid)
		if f, err := os.OpenFile(spool, os.O_WRONLY|os.O_CREATE, 0o600); err == nil {
			f.Close()
		}
		var inboxOffset int64
		if !opts.Replay {
			inboxOffset = endOffset(spool) // the spool is never compacted, so base is 0
		}
		go followSource(spool, inboxOffset, lines, done, nil, logf)

		// Topics: reuse the monitor's manager, so a `pigeon subscribe` against this
		// inbox takes effect live, just as it would for a session.
		go manageSubscriptions(ns, sid, lines, done, logf)

		logf("listening as %q (%s) in namespace %s -- reachable at: pigeon send %s",
			opts.As, Short(sid), ns, opts.As)
	} else {
		if len(topics) == 0 {
			return fmt.Errorf("give at least one topic to listen to, or --as <name> to open a named inbox")
		}
		for _, ref := range topics {
			path := ref.path(ns)
			off := readBase(path)
			if !opts.Replay {
				off += endOffset(path)
			}
			go followSource(path, off, lines, done, nil, logf)
		}
		logf("listening on %s in namespace %s", topicList(topics), ns)
	}

	var timeout <-chan time.Time
	if opts.Timeout > 0 {
		t := time.NewTimer(opts.Timeout)
		defer t.Stop()
		timeout = t.C
	}

	enc := json.NewEncoder(stdout)
	received := 0
	for {
		select {
		case <-sigc:
			logf("shutting down")
			return nil
		case <-timeout:
			logf("timeout reached after %s", opts.Timeout)
			return nil
		case m := <-lines:
			// Never echo our own broadcast back to us.
			if sid != "" && m.From.SessionID == sid {
				continue
			}
			if emitJSON {
				ev := listenEvent{Message: m, Namespace: ns.String()}
				if m.Payload != "" {
					if b, err := os.ReadFile(m.Payload); err == nil {
						ev.Body = string(b)
					}
				}
				if err := enc.Encode(ev); err != nil {
					return err
				}
			} else {
				fmt.Fprintln(stdout, ns.Render(m))
			}
			received++
			if opts.Count > 0 && received >= opts.Count {
				logf("received %d message(s), stopping", received)
				return nil
			}
		}
	}
}

// registerEphemeral writes the visible entry for a shell inbox and seeds its
// cursors. It is a lean cousin of the monitor's register: no Claude session
// lookup, no project-config identity seeding -- a shell inbox names itself. It
// still respects a private project, blanking the cwd through the same WriteEntry
// path a private session uses.
func registerEphemeral(ns Namespace, sid, name string, topics []TopicRef, replay bool) error {
	cwd := CurrentCwd()
	private := false
	if cfg, _, err := LoadProjectConfig(cwd); err == nil && cfg != nil && cfg.Private {
		private = true
	}

	subs := defaultSubscriptions(ns)
	for _, ref := range topics {
		subs = append(subs, ref.String())
	}
	subs = dedupeSortedStrings(subs)

	pid := os.Getpid()
	now := nowRFC3339()
	if err := ns.WriteEntry(&Entry{
		SessionID:     sid,
		Namespace:     ns.String(),
		Name:          name,
		PID:           pid,
		ProcStart:     ProcStart(pid),
		Cwd:           cwd,
		Host:          hostname(),
		StartedAt:     now,
		HeartbeatAt:   now,
		Subscriptions: subs,
		Ephemeral:     true,
		Private:       private,
	}); err != nil {
		return err
	}

	// Seeded after the entry exists, because mutateCursors takes the session lock
	// and the lock requires the session to be registered. Default to the end so
	// opening an inbox is not a replay; --replay starts at the base of what
	// survives on disk.
	for _, t := range subs {
		ref, err := ParseTopicRef(t)
		if err != nil {
			continue
		}
		if replay {
			base := readBase(ref.path(ns))
			_ = ns.mutateCursors(sid, func(m map[string]int64) { m[ref.String()] = base })
		} else {
			_ = ns.seedCursor(sid, ref)
		}
	}
	return nil
}

// ephemeralHeartbeat keeps a shell inbox's entry fresh so status() reads it as
// live rather than deaf once the heartbeat grace elapses. It is deliberately
// simpler than the monitor's heartbeat: a shell has no Claude Code session name
// to reflect.
func ephemeralHeartbeat(ns Namespace, sid string, done <-chan struct{}) {
	t := time.NewTicker(heartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			_ = ns.MutateEntry(sid, func(e *Entry) error {
				e.HeartbeatAt = nowRFC3339()
				return nil
			})
		}
	}
}

// removeEphemeralState takes a shell inbox's spool and cursors with it on a clean
// exit. A shell listener that is gone is gone -- there is no `claude --resume`
// that could bring the same synthetic id back to read a queued spool -- so unlike
// a real session's spool, this one is not worth keeping.
func removeEphemeralState(ns Namespace, sid string) {
	_ = os.Remove(ns.SpoolPath(sid))
	_ = os.Remove(ns.cursorPath(sid))
}

func dedupeSortedStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func topicList(refs []TopicRef) string {
	labels := make([]string, 0, len(refs))
	for _, r := range refs {
		labels = append(labels, TopicLabel(r.String()))
	}
	if len(labels) == 0 {
		return "(no topics)"
	}
	sort.Strings(labels)
	return joinComma(labels)
}

func joinComma(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += ", "
		}
		out += v
	}
	return out
}
