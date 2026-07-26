package pigeon

import (
	"fmt"
	"strconv"
	"time"

	wb "github.com/PeterSR/claude-code-weaverbird/provider"
)

// pigeon's status widgets are an alarm, not a dashboard.
//
// A healthy session renders nothing at all. There is nothing useful to say:
// a live monitor drains the spool within about a second, so there is never a
// standing unread count to show, and listing which other sessions happen to
// be running fills the line with peers working on unrelated things. A widget
// that is always lit becomes wallpaper, and wallpaper is exactly what you
// stop reading before the one time it mattered.
//
// pigeon.wait renders only when this session cannot receive: deaf, or never
// armed. Those are the states nothing else in the UI reports -- mail
// silently piles up on a spool while the session looks completely normal --
// and they are the only states where the count of waiting messages is real.
// The other two widgets answer questions worth asking for rather than alarms
// worth interrupting for, which is why they are opt-in.

// armGrace is how long after a session starts pigeon stays silent about a
// missing registry entry.
//
// A host renders its status line at session start and caches the result,
// re-running the provider only on a real turn, not on idle time or
// keystrokes. The monitor registers a beat later, so the first render
// usually happens before there is an entry to find. Without this window the
// false "not armed" from that render sticks on an idle session's bar until
// its next turn. Inside it, a missing entry means "still arming"; past it, a
// missing entry is real. Set well above the sub-second-to-a-second a monitor
// normally takes, so a genuine failure still surfaces on the next render
// beyond it.
const armGrace = 10 * time.Second

// BuildWeaverbirdSpec declares pigeon's weaverbird widgets: the default
// alarm (pigeon.wait) plus two opt-in detail widgets (pigeon.monitor,
// pigeon.peers) that only render for a user who has asked their layout for
// them, grouped under pigeon.detail so that asking is one id instead of
// three.
//
// pigeon.wait is one widget, not two: "waiting" (a deaf or dead monitor
// with mail piling up) and "not armed" (no monitor ever attached) are
// mutually exclusive readings of the same question, can this session
// receive mail right now, never shown together. Per weaverbird's
// widget-split rule that is one alarm state, not two independently
// meaningful facts weaverbird could order or drop apart, so it is one
// widget whose text and class vary, not a widget plus a colored span.
//
// pigeon.monitor and pigeon.peers are Default: wb.OptIn() on purpose: they
// answer questions worth asking for, not alarms worth interrupting for, so
// they stay out of the no-layout view and pigeon.wait's implicit default
// group exactly as before. A 15s TTLSec keeps them cheap without a
// file-driven Invalidate of their own -- neither reads a single file whose
// mtime alone settles the question the way the spool does for pigeon.wait.
//
// pigeon.wait's cache invalidation names the spool file for the best
// session id this process can resolve without one on stdin (spec is
// fetched once, with no session piped in, see weaverbird's SPEC.md section
// 2.2): CLAUDE_CODE_SESSION_ID if Claude Code set it in this process's own
// environment. If that is unset, the widget still gets the ttl_sec
// ceiling, just no file-driven invalidation.
func BuildWeaverbirdSpec() wb.Spec {
	cache := &wb.Cache{TTLSec: 30}
	if p := specSpoolPath(); p != "" {
		cache.Invalidate = &wb.Invalidate{File: p}
	}
	return wb.Spec{
		V:        1,
		Provider: "pigeon",
		Icon:     "🕊",
		Widgets: []wb.Widget{
			{ID: "pigeon.wait", Title: "Messages", Priority: 50, Cache: cache},
			{
				ID:       "pigeon.monitor",
				Title:    "Monitor",
				Priority: 20,
				Row:      1,
				Default:  wb.OptIn(),
				Cache:    &wb.Cache{TTLSec: 15},
			},
			{
				ID:       "pigeon.peers",
				Title:    "Peers",
				Priority: 10,
				Row:      1,
				Default:  wb.OptIn(),
				Cache:    &wb.Cache{TTLSec: 15},
			},
		},
		Groups: []wb.Group{
			{
				ID:      "pigeon.detail",
				Title:   "Session detail",
				Widgets: []string{"pigeon.wait", "pigeon.monitor", "pigeon.peers"},
			},
		},
	}
}

// specSpoolPath is the best-effort spool location a spec invocation (no
// session on stdin) can resolve to a concrete file.
func specSpoolPath() string {
	sid := CurrentSessionID()
	if sid == "" {
		return ""
	}
	return CurrentNamespace().SpoolPath(sid)
}

// WeaverbirdValue answers for every widget BuildWeaverbirdSpec declares.
// The session id comes from the parsed weaverbird Session first, falling
// back to CLAUDE_CODE_SESSION_ID.
//
// It answers for all three widgets in one call -- the one locateSession
// this session's own entry costs is shared between pigeon.wait and
// pigeon.monitor rather than paid twice -- and, like the pre-existing
// pigeon.wait path, ignores requested and simply returns whatever it can
// answer; per ValueFunc's own contract weaverbird filters down to what a
// given render actually needs, and requested can be empty as often as not
// (an empty slice reads as "all of them").
func WeaverbirdValue(sess wb.Session, _ []string) ([]wb.Value, error) {
	sid := sessionIDFromWeaverbird(sess)
	if sid == "" || OptedOut() {
		return nil, nil
	}

	var vals []wb.Value

	ns, e, err := locateSession(sid)
	if err != nil {
		if age, ok := claudeSessionAge(sid, time.Now()); ok && age < armGrace {
			// Still arming: a monitor that has not registered yet must not
			// read as a real "not armed" alarm on either widget.
			// pigeon.peers does not depend on this session's own
			// registration, so it is still collected below.
		} else {
			// Not armed: no namespace found this session in, so there is no
			// monitor to have drained or counted anything, and none to
			// report a status for either -- pigeon.monitor omits itself the
			// same way, per its own doc comment. This is deliberately not
			// the "mail is piling up" alarm waitingValue reports: nothing
			// ever armed here, so nothing is counting mail at all.
			vals = append(vals, notArmedValue())
		}
	} else {
		switch e.Status {
		case StatusLive:
			vals = append(vals, monitorValue(e))
		case StatusDeaf, StatusDead:
			vals = append(vals, waitingValue(ns.Pending(sid)), monitorValue(e))
		}
	}

	if pv := peersValue(); pv != nil {
		vals = append(vals, *pv)
	}

	return vals, nil
}

// sessionIDFromWeaverbird prefers the id weaverbird's Session carries over
// the environment: the process is spawned per render and does not reliably
// inherit CLAUDE_CODE_SESSION_ID. The id reaches a file path, so it is
// validated rather than trusted, exactly as everywhere else.
func sessionIDFromWeaverbird(sess wb.Session) string {
	if id := sess.SessionID; id != "" && ValidSessionID(id) == nil {
		return id
	}
	return CurrentSessionID()
}

// waitingValue renders the one value record this widget ever emits. n <= 0
// covers both a genuinely empty spool and a pending count this process
// could not determine (no namespace located): either way there is nothing
// specific to count, so the text says "waiting" without a number rather
// than a misleading "0 waiting".
func waitingValue(n int) wb.Value {
	if n <= 0 {
		return wb.Value{ID: "pigeon.wait", Text: "waiting", Short: "waiting", Class: wb.ClassWarn}
	}
	return wb.Value{
		ID:    "pigeon.wait",
		Text:  fmt.Sprintf("%d waiting", n),
		Short: strconv.Itoa(n),
		Class: wb.ClassWarn,
	}
}

// notArmedValue renders the never-armed alarm: no monitor ever attached to
// this session, so there is no spool being drained and no count to give.
// Deliberately not the "waiting" wording waitingValue uses for a deaf or
// dead monitor with mail piling up -- the two states cannot receive for
// different reasons, and the fix for each is different.
func notArmedValue() wb.Value {
	return wb.Value{ID: "pigeon.wait", Text: "not armed", Short: "not armed", Class: wb.ClassWarn}
}

// monitorValue renders pigeon.monitor's one value record from a resolved
// entry's Status.
//
// Unlike pigeon.wait, which stays silent on purpose whenever a session can
// receive, this widget's whole job is showing the wiring, so a healthy
// StatusLive session is exactly the case it does report -- "monitor
// live"/ok -- alongside the deaf/dead states pigeon.wait already alarms
// on. It is only ever called once WeaverbirdValue has a resolved entry in
// hand; a session not found or not armed omits this widget entirely rather
// than guessing, which is WeaverbirdValue's job, not this one's.
func monitorValue(e *Entry) wb.Value {
	switch e.Status {
	case StatusLive:
		return wb.Value{ID: "pigeon.monitor", Text: "monitor live", Short: "live", Class: wb.ClassOK}
	case StatusDeaf:
		return wb.Value{ID: "pigeon.monitor", Text: "monitor deaf", Short: "deaf", Class: wb.ClassWarn}
	default: // StatusDead
		return wb.Value{ID: "pigeon.monitor", Text: "monitor dead", Short: "dead", Class: wb.ClassDanger}
	}
}

// peersValue renders pigeon.peers' one value record: how many other live
// sessions share this process's own resolved namespace right now, or nil
// when that count is zero -- an opt-in widget saying "you are alone" is
// still wallpaper, just quieter wallpaper.
//
// This deliberately answers for CurrentNamespace() only, one glob-and-read
// pass over its sessions directory, the same cost locateSession already
// pays for one entry. It does not chase the session actually found in
// (which locateSession's cross-namespace fallback can land in a different
// namespace than this process's own), and it does not walk every namespace
// on the machine the way ListNamespaces does for `pigeon namespaces` --
// both would turn one cheap glob into several. "Current namespace" is
// this process's own answer to the question, exactly as specSpoolPath and
// the rest of this file already use CurrentNamespace() for everything
// else.
func peersValue() *wb.Value {
	sessions, err := CurrentNamespace().ListSessions(false, false)
	if err != nil {
		return nil
	}
	live := 0
	for _, e := range sessions {
		if e.Status == StatusLive {
			live++
		}
	}
	// Subtract this session itself: if it's one of the live entries just
	// counted, the number should read as peers, not headcount.
	n := live - 1
	if n <= 0 {
		return nil
	}
	return &wb.Value{
		ID:    "pigeon.peers",
		Text:  fmt.Sprintf("%d peers", n),
		Short: strconv.Itoa(n),
		Class: wb.ClassNeutral,
	}
}

// locateSession finds a session's entry in this namespace, failing that in any
// namespace, and failing that under whatever id its monitor armed with.
//
// The provider is spawned per render, with an environment and a working
// directory that need not match the ones the monitor armed with, so resolving a
// namespace here can land somewhere the session simply is not. Reporting "not
// armed" for a healthy session is the false alarm this widget exists to avoid:
// one wrong lit line and it becomes wallpaper. Searching by exact session id
// cannot pick the wrong session, so the fallbacks cost nothing but a few globs
// in the case that is already an alarm.
func locateSession(sid string) (Namespace, *Entry, error) {
	ns := CurrentNamespace()
	e, err := ns.ReadEntry(sid)
	if err == nil {
		return ns, e, nil
	}
	spaces, lerr := ListNamespaces()
	if lerr != nil {
		return ns, nil, err
	}
	for _, info := range spaces {
		other, perr := ParseNamespace(info.Name)
		if perr != nil || other.Is(ns) {
			continue
		}
		if e, perr := other.ReadEntry(sid); perr == nil {
			return other, e, nil
		}
	}
	if other, e, ok := sessionOfSameProcess(sid, spaces); ok {
		return other, e, nil
	}
	return ns, nil, err
}

// sessionOfSameProcess finds the entry belonging to this session's claude
// process, for a session whose id has changed underneath its monitor.
//
// Clearing a session mints a fresh session id inside the running claude
// process. The monitor is spawned once, when that process starts, so it holds
// the id it armed with for the life of the process -- and so do the registry
// entry, the spool and the lock it owns. The widget, spawned per render, is
// handed the host's *current* id instead, which no entry is filed under. The
// arming grace cannot cover this: Claude Code keeps the original startedAt
// across the change, so the session reads as hours old and the miss looks
// real. Without this the widget cries "not armed" at a session that is alive,
// heartbeating, and draining its mail perfectly well.
//
// The match is on the process rather than the id, since the process is the one
// thing the two ids agree on. A pid plus its start time names exactly one
// process and cannot be recycled into another, which is the same test
// ProcessAlive makes before trusting any entry at all.
func sessionOfSameProcess(sid string, spaces []NamespaceInfo) (Namespace, *Entry, bool) {
	rec, ok := findClaudeSession(sid)
	if !ok || rec.PID <= 0 {
		return Namespace{}, nil, false
	}
	for _, info := range spaces {
		ns, err := ParseNamespace(info.Name)
		if err != nil {
			continue
		}
		// Dead entries are excluded: they name a process that is gone, which
		// is never the live one asking.
		entries, err := ns.ListSessions(false, false)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.PID == rec.PID && sameProcess(e.ProcStart, rec.ProcStart) {
				return ns, e, true
			}
		}
	}
	return Namespace{}, nil, false
}

// sameProcess compares two recorded process start times the way ProcessAlive
// does. An empty value on either side means that platform could not read one,
// which is a reason not to tell two processes apart rather than grounds to
// call them different.
func sameProcess(a, b string) bool { return a == "" || b == "" || a == b }
