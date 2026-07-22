// Package pigeon implements message passing between live Claude Code sessions.
package pigeon

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Session ids become path components under PIGEON_HOME, so they are validated
// rather than trusted. Claude Code issues UUIDs; this is deliberately a little
// wider than that but still cannot express a traversal or a separator.
var sessionIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ValidSessionID rejects anything unsafe to interpolate into a file path.
func ValidSessionID(id string) error {
	if !sessionIDRe.MatchString(id) || strings.Contains(id, "..") {
		return fmt.Errorf("invalid session id %q", id)
	}
	return nil
}

// Claude Code injects CLAUDE_CODE_SESSION_ID into the processes it spawns,
// including plugin monitors and MCP servers. Note that CLAUDE_SESSION_ID (no
// _CODE_) also appears in the binary but is vestigial and never set; reading
// that one is the bug that leaves xhluca/agent-talk's monitor idling forever.
// See docs/ALTERNATIVES.md.
const (
	EnvSessionID  = "CLAUDE_CODE_SESSION_ID"
	EnvClaudePID  = "CLAUDE_PID"
	EnvProjectDir = "CLAUDE_PROJECT_DIR"
	EnvVersion    = "CLAUDE_CODE_VERSION"
	EnvChild      = "CLAUDE_CODE_CHILD_SESSION"

	// EnvOptOut lets a launcher that drives sessions programmatically
	// (claude-p, pupptyeer, CI) keep them out of the bus.
	EnvOptOut = "PIGEON"
	EnvHome   = "PIGEON_HOME"
)

// Home is the state directory. Everything pigeon knows lives here.
func Home() string {
	if v := strings.TrimSpace(os.Getenv(EnvHome)); v != "" {
		return v
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "pigeon")
	}
	return filepath.Join(h, ".claude", "pigeon")
}

func SessionsDir() string { return filepath.Join(Home(), "sessions") }
func InboxDir() string    { return filepath.Join(Home(), "inbox") }
func PayloadsDir() string { return filepath.Join(Home(), "payloads") }
func LocksDir() string    { return filepath.Join(Home(), "locks") }

// EnsureDirs creates the state tree with owner-only permissions. The spool is
// an injection surface into a live agent, so it is deliberately not shared.
func EnsureDirs() error {
	for _, d := range []string{Home(), SessionsDir(), InboxDir(), PayloadsDir(), LocksDir(), TopicsDir(), CursorsDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", d, err)
		}
		_ = os.Chmod(d, 0o700)
	}
	return nil
}

// CurrentSessionID returns this session's UUID, or "" when not running inside
// Claude Code.
//
// It deliberately does not fall back to walking the process tree. Bloodhound
// showed that start-time heuristics misidentify idle sessions confidently:
// session A starts idle at 10:00, B starts at 10:05, you type in A at 10:30 --
// A's process is older but its first activity is newer, so nearest-match picks
// the wrong session. Failing loudly beats guessing wrongly.
func CurrentSessionID() string {
	id := strings.TrimSpace(os.Getenv(EnvSessionID))
	if ValidSessionID(id) != nil {
		// A hostile or malformed value must not reach a path join.
		return ""
	}
	return id
}

// CurrentClaudePID is the pid of the claude process that owns this subprocess.
func CurrentClaudePID() int {
	v := strings.TrimSpace(os.Getenv(EnvClaudePID))
	if v == "" {
		return 0
	}
	pid, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return pid
}

// OptedOut reports whether this session should stay off the bus.
func OptedOut() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvOptOut))) {
	case "0", "no", "off", "false":
		return true
	}
	return false
}

// CurrentCwd prefers the project dir Claude Code reports over the process cwd,
// which for a monitor is the session cwd anyway but may differ for the CLI.
func CurrentCwd() string {
	if v := strings.TrimSpace(os.Getenv(EnvProjectDir)); v != "" {
		return v
	}
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return wd
}

// ShellIdentity describes a sender that is not a Claude Code session.
func ShellIdentity() string {
	name := "unknown"
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	host, err := os.Hostname()
	if err != nil {
		host = "localhost"
	}
	return fmt.Sprintf("shell:%s@%s", name, host)
}
