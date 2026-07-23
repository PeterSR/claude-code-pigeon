package pigeon

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
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

	e, err := ReadEntry(sid)
	if err != nil {
		// Registered is the normal state for any session started after
		// `pigeon install`. Not being registered means the monitor never
		// armed, which is silent everywhere else.
		return decorate("not armed", opts)
	}

	switch e.Status {
	case StatusLive:
		return ""
	case StatusDeaf, StatusDead:
		// Only now is a count meaningful: the monitor is not draining the
		// spool, so whatever is on it is genuinely waiting.
		if n := Pending(sid); n > 0 {
			return decorate(fmt.Sprintf("deaf · %d waiting", n), opts)
		}
		return decorate("deaf", opts)
	default:
		return ""
	}
}

func decorate(text string, opts StatuslineOptions) string {
	if opts.Plain {
		return "pigeon " + text
	}
	return ansiWarn + "🕊 " + text + ansiReset
}
