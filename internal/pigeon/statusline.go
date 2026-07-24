package pigeon

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// The statusline is an alarm, not a dashboard.
//
// A healthy session renders nothing at all. There is nothing useful to say: a
// live monitor drains the spool within about a second, so there is never a
// standing unread count to show, and listing which other sessions happen to be
// running fills the line with peers working on unrelated things. A statusline
// that is always lit becomes wallpaper, and wallpaper is exactly what you stop
// reading before the one time it mattered.
//
// It renders only when this session cannot receive: deaf, or never armed.
// Those are the states nothing else in the UI reports -- mail silently piles up
// on a spool while the session looks completely normal -- and they are the only
// states where the count of waiting messages is real.

// statuslineInput is the JSON Claude Code writes to a statusline command's
// stdin. Only the session id matters here, and it is read leniently: unknown
// fields come and go across versions, and an unparseable payload should fall
// back to the environment rather than blank the line.
type statuslineInput struct {
	SessionID string `json:"session_id"`
}

const (
	ansiWarn  = "\x1b[33m"
	ansiReset = "\x1b[0m"
)

// armGrace is how long after a session starts pigeon stays silent about a
// missing registry entry.
//
// Claude Code renders the statusline at session start and caches the result,
// re-running the command only on a real turn, not on idle time or keystrokes.
// The monitor registers a beat later, so the first render usually happens
// before there is an entry to find. Without this window the false "not armed"
// from that render sticks on an idle session's bar until its next turn. Inside
// it, a missing entry means "still arming"; past it, a missing entry is real.
// Set well above the sub-second-to-a-second a monitor normally takes, so a
// genuine failure still surfaces on the next render beyond it.
const armGrace = 10 * time.Second

// StatuslineOptions controls rendering. The zero value is what Claude Code
// gets: emoji, colour, and silence when all is well.
type StatuslineOptions struct {
	// Plain drops the emoji and ANSI colour, for terminals or wrappers that
	// mangle either.
	Plain bool
}

// Statusline writes this session's alarm line, or nothing.
//
// It never returns an error for an absent or malformed input: a statusline
// command that fails is noise in the user's terminal on every render, and
// there is no state here worth failing over.
func Statusline(stdin io.Reader, w io.Writer, opts StatuslineOptions) error {
	line := statuslineFor(sessionFromStatusline(stdin), opts)
	if line == "" {
		return nil
	}
	_, err := fmt.Fprintln(w, line)
	return err
}

// sessionFromStatusline prefers the id Claude Code hands us over the
// environment. The statusline command is spawned per render and does not
// reliably inherit CLAUDE_CODE_SESSION_ID, so stdin is the authoritative
// source; the env var is the fallback for running it by hand.
func sessionFromStatusline(stdin io.Reader) string {
	if stdin != nil {
		if b, err := io.ReadAll(io.LimitReader(stdin, 1<<20)); err == nil && len(b) > 0 {
			var in statuslineInput
			if json.Unmarshal(b, &in) == nil {
				if id := strings.TrimSpace(in.SessionID); ValidSessionID(id) == nil {
					return id
				}
			}
		}
	}
	return CurrentSessionID()
}

func statuslineFor(sid string, opts StatuslineOptions) string {
	// Nothing to report for a session that is not on the bus by choice, and
	// nothing we could report for one we cannot identify.
	if sid == "" || OptedOut() {
		return ""
	}

	ns, e, err := locateSession(sid)
	if err != nil {
		// Registered is the normal state for any session started after
		// `pigeon install`. Not being registered usually means the monitor
		// never armed, which is silent everywhere else -- but a session that
		// only just started has a monitor still arming, and crying "not armed"
		// then is a false alarm Claude Code caches onto an idle bar. Stay quiet
		// until the session is old enough that a missing entry is real.
		if age, ok := claudeSessionAge(sid, time.Now()); ok && age < armGrace {
			return ""
		}
		return decorate("not armed", opts)
	}

	switch e.Status {
	case StatusLive:
		return ""
	case StatusDeaf, StatusDead:
		// Only now is a count meaningful: the monitor is not draining the
		// spool, so whatever is on it is genuinely waiting.
		if n := ns.Pending(sid); n > 0 {
			return decorate(fmt.Sprintf("deaf · %d waiting", n), opts)
		}
		return decorate("deaf", opts)
	default:
		return ""
	}
}

// locateSession finds a session's entry in this namespace, or failing that in
// any namespace.
//
// The statusline is spawned per render, with an environment and a working
// directory that need not match the ones the monitor armed with, so resolving a
// namespace here can land somewhere the session simply is not. Reporting "not
// armed" for a healthy session is the false alarm this widget exists to avoid:
// one wrong lit line and it becomes wallpaper. Searching by exact session id
// cannot pick the wrong session, so the fallback costs nothing but a few globs
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
	return ns, nil, err
}

func decorate(text string, opts StatuslineOptions) string {
	if opts.Plain {
		return "pigeon " + text
	}
	return ansiWarn + "🕊 " + text + ansiReset
}
