package pigeon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// maxClaudeName bounds the host label before it is stored or shown. It is a
// name a user could set to anything in Claude Code's own UI, and it lands in a
// `pigeon ls` column and a JSON field, so it is flattened and clipped like any
// other free text pigeon surfaces.
const maxClaudeName = 64

// ClaudeSession is the slice of Claude Code's own per-session index that pigeon
// surfaces: the name shown by /status and whether Claude Code derived it or a
// user set it. Both are empty when the index cannot be read, which is the
// normal degraded state rather than an error.
type ClaudeSession struct {
	// Name is the session name /status displays. It is a host label, not a
	// pigeon address: routing keys on Entry.Name, never on this.
	Name string
	// Source is Claude Code's nameSource -- "derived" (from the working
	// directory) or a value that marks it as user-chosen. A derived name mostly
	// duplicates the cwd basename; a non-derived one is the case worth having.
	Source string
}

// claudeConfigDir is where Claude Code keeps its own state, honouring
// CLAUDE_CONFIG_DIR and otherwise ~/.claude. It is resolved independently of
// Home() so that PIGEON_HOME relocating pigeon's state does not drag the lookup
// of Claude's own files along with it.
func claudeConfigDir() string {
	if v := strings.TrimSpace(os.Getenv(EnvConfigDir)); v != "" {
		return v
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(h, ".claude")
}

// LookupClaudeSession reads the name Claude Code shows in /status for one
// session, from its per-session index at <config>/sessions/<pid>.json.
//
// This is another observed-not-promised Claude Code internal, the same class of
// dependency as CLAUDE_CODE_SESSION_ID, so it fails soft: any missing file,
// unreadable JSON, or mismatch yields an empty result and the caller falls back
// to what pigeon already knows. The index is keyed by the claude process pid,
// so the read is verified against the session id before it is trusted -- a
// recycled pid pointing at a different session's index is ignored rather than
// used to mislabel this one. `pigeon doctor` reports whether this lookup works.
func LookupClaudeSession(pid int, sessionID string) ClaudeSession {
	if pid <= 0 || ValidSessionID(sessionID) != nil {
		return ClaudeSession{}
	}
	dir := claudeConfigDir()
	if dir == "" {
		return ClaudeSession{}
	}
	b, err := os.ReadFile(filepath.Join(dir, "sessions", strconv.Itoa(pid)+".json"))
	if err != nil {
		return ClaudeSession{}
	}
	var rec struct {
		SessionID  string `json:"sessionId"`
		Name       string `json:"name"`
		NameSource string `json:"nameSource"`
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		return ClaudeSession{}
	}
	if rec.SessionID != sessionID {
		return ClaudeSession{}
	}
	return ClaudeSession{
		Name:   truncateRunes(Sanitize(rec.Name), maxClaudeName),
		Source: truncateRunes(Sanitize(rec.NameSource), maxClaudeName),
	}
}

// claudeSessionAge reports how long ago a session started, read from Claude
// Code's session index, and whether that is known. The index is keyed by pid,
// so this scans it for the matching session id -- the statusline holds the id
// but not the pid.
//
// It exists so the statusline can tell a monitor that is still arming (session
// a second old, no entry yet) from one that never armed (session long up, still
// no entry). Best-effort like everything that reads Claude Code's internals: a
// missing file, unreadable JSON, or absent startedAt yields ok=false, and the
// caller keeps its prior behaviour.
func claudeSessionAge(sessionID string, now time.Time) (age time.Duration, ok bool) {
	if ValidSessionID(sessionID) != nil {
		return 0, false
	}
	dir := claudeConfigDir()
	if dir == "" {
		return 0, false
	}
	paths, err := filepath.Glob(filepath.Join(dir, "sessions", "*.json"))
	if err != nil {
		return 0, false
	}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var rec struct {
			SessionID string `json:"sessionId"`
			StartedAt int64  `json:"startedAt"`
		}
		if json.Unmarshal(b, &rec) != nil || rec.SessionID != sessionID {
			continue
		}
		if rec.StartedAt <= 0 {
			return 0, false
		}
		return now.Sub(time.UnixMilli(rec.StartedAt)), true
	}
	return 0, false
}
