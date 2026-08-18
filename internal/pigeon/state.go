// Package pigeon implements message passing between live Claude Code sessions.
package pigeon

import (
	"encoding/json"
	"fmt"
	"io"
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
	// EnvConfigDir is where Claude Code keeps its own state, which is where
	// pigeon reads the session name /status shows. It is deliberately separate
	// from PIGEON_HOME: pigeon's own state can be relocated without moving
	// Claude's, and the two default to siblings under ~/.claude.
	EnvConfigDir = "CLAUDE_CONFIG_DIR"

	// EnvOptOut lets a launcher that drives sessions programmatically
	// (claude-p, pupptyeer, CI) keep them out of the bus.
	EnvOptOut = "PIGEON"
	EnvHome   = "PIGEON_HOME"
	// EnvNamespace outranks every other source, because a launcher states how
	// it started a session and a committed file only states where it was
	// cloned.
	EnvNamespace = "PIGEON_NAMESPACE"
	// EnvAs names the ephemeral identity a plain shell acts as, for the same
	// per-invocation reason PIGEON_NAMESPACE exists: a script states who it is
	// speaking as. It only takes effect outside a real Claude Code session; see
	// ActingIdentity.
	EnvAs = "PIGEON_AS"
	// EnvTransport selects how a recipient is woken -- see Transport. Per
	// invocation, for the same reason as the two above: a script states how it
	// wants its mail delivered, and that outranks a standing preference set
	// weeks ago in the machine config.
	EnvTransport = "PIGEON_TRANSPORT"
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

// --- namespaces --------------------------------------------------------------
//
// A namespace is an isolated group of sessions, and the isolation is
// structural: each one owns a complete state tree, so a session in "acme"
// cannot see "default"'s registry because that is not the directory it reads.
//
// The alternative -- a field on the entry plus a filter -- would put the
// isolation rule in ListSessions, ResolveTarget, NameTaken, ListTopics, the
// publish subscriber count, prune, reconcileOrphans, doctor's peers check and
// the MCP tools. Miss one of those and it leaks another namespace's sessions,
// which is the same class of failure as guessing a session id: it delivers
// somebody else's mail.

// DefaultNamespaceName is where every session lands when nobody says
// otherwise, so anyone who never thinks about namespaces never has to.
const DefaultNamespaceName = "default"

// namespaceRe holds a namespace to the same charset a topic uses, and for the
// same reason: it becomes a directory name, so it may not express a traversal
// or a separator.
var namespaceRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ValidNamespace rejects anything unsafe to interpolate into a file path.
func ValidNamespace(ns string) error {
	if !namespaceRe.MatchString(ns) || strings.Contains(ns, "..") {
		return fmt.Errorf("invalid namespace %q: use lowercase letters, digits, dot, dash or underscore (max 64)", ns)
	}
	return nil
}

// Namespace is a validated namespace name.
//
// It is a struct rather than a string so that nothing outside this package can
// build one that was never checked: every path in the state tree is joined
// from it, and ParseNamespace is the only door in. The zero value is the
// default namespace, so a caller that never mentions namespaces gets the one
// everybody else is in rather than a directory called "".
type Namespace struct{ name string }

// ParseNamespace validates a namespace typed at the CLI, declared in a project
// config, or handed over by an MCP caller.
func ParseNamespace(s string) (Namespace, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Namespace{}, fmt.Errorf("empty namespace")
	}
	if err := ValidNamespace(s); err != nil {
		return Namespace{}, err
	}
	return Namespace{name: s}, nil
}

// DefaultNamespace is the namespace of a session that declares none.
func DefaultNamespace() Namespace { return Namespace{name: DefaultNamespaceName} }

func (n Namespace) String() string {
	if n.name == "" {
		return DefaultNamespaceName
	}
	return n.name
}

// Is reports whether two namespaces are the same one, comparing the normalised
// name so the zero value and an explicit "default" are not two answers.
func (n Namespace) Is(other Namespace) bool { return n.String() == other.String() }

// NamespacesDir holds one complete state tree per namespace.
func NamespacesDir() string { return filepath.Join(Home(), "namespaces") }

// SharedDir holds what deliberately crosses namespaces: the logs of global
// topics and the payload files their overflowing messages spill to. Nothing
// here is namespaced, which is the point, and nothing else lives here.
func SharedDir() string         { return filepath.Join(Home(), "shared") }
func SharedTopicsDir() string   { return filepath.Join(SharedDir(), "topics") }
func SharedPayloadsDir() string { return filepath.Join(SharedDir(), "payloads") }

// SharedAsksDir holds the question and answer files for an ask published on a
// machine-wide "@" topic. It has to live outside every namespace for the same
// reason SharedTopicsDir does: an ask's audience can span namespaces, and a
// peer answering from one of them has to find the record without knowing
// which namespace the asker happened to be in.
func SharedAsksDir() string { return filepath.Join(SharedDir(), "asks") }

// sharedLocksDir guards the global topic logs. The lock has to be outside every
// namespace too: two namespaces compacting one shared log under their own locks
// would rewrite it from under each other.
func sharedLocksDir() string { return filepath.Join(SharedDir(), "locks") }

// Root is everything this namespace can see.
func (n Namespace) Root() string        { return filepath.Join(NamespacesDir(), n.String()) }
func (n Namespace) SessionsDir() string { return filepath.Join(n.Root(), "sessions") }
func (n Namespace) InboxDir() string    { return filepath.Join(n.Root(), "inbox") }
func (n Namespace) PayloadsDir() string { return filepath.Join(n.Root(), "payloads") }
func (n Namespace) LocksDir() string    { return filepath.Join(n.Root(), "locks") }
func (n Namespace) TopicsDir() string   { return filepath.Join(n.Root(), "topics") }
func (n Namespace) CursorsDir() string  { return filepath.Join(n.Root(), "cursors") }

// AsksDir holds this namespace's ask records and answer logs: asks/<id>.json
// for the question, asker and audience snapshot, asks/<id>.ndjson for the
// answers appended to it. A namespaced topic's ask lives here; a machine-wide
// "@" topic's lives in SharedAsksDir instead, mirroring TopicsDir/SharedTopicsDir.
func (n Namespace) AsksDir() string { return filepath.Join(n.Root(), "asks") }

// The package-level forms address the caller's own namespace, which is what
// almost every caller means. Each resolves the namespace once and hands it
// down; nothing below this layer resolves it again.

// ensureHome creates the state root and migrates an old layout into it, and
// stops there.
//
// Reads go through this rather than EnsureDirs so that listing a namespace does
// not create it: `pigeon ls -n acmee` is a typo, and a typo that leaves a
// permanent empty namespace behind would show up in every later listing as
// something real.
func ensureHome() error {
	if err := os.MkdirAll(Home(), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", Home(), err)
	}
	_ = os.Chmod(Home(), 0o700)
	// Before anything creates a directory in the new shape, since the old shape
	// is recognised by its absence.
	return migrateFlatLayout(stderrLogf)
}

// EnsureDirs creates the state tree with owner-only permissions. The spool is
// an injection surface into a live agent, so it is deliberately not shared.
func EnsureDirs() error { return CurrentNamespace().EnsureDirs() }

func (n Namespace) EnsureDirs() error {
	if err := ensureHome(); err != nil {
		return err
	}
	for _, d := range []string{
		NamespacesDir(), n.Root(),
		n.SessionsDir(), n.InboxDir(), n.PayloadsDir(), n.LocksDir(), n.TopicsDir(), n.CursorsDir(), n.AsksDir(),
		SharedDir(), SharedTopicsDir(), SharedPayloadsDir(), sharedLocksDir(), SharedAsksDir(),
	} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", d, err)
		}
		_ = os.Chmod(d, 0o700)
	}
	return nil
}

// ResolveNamespace reports this process's namespace and where it came from.
//
// Highest first: the environment, because a launcher knows how it started a
// session; then the project config, because a checkout knows what it is; then
// the CLI default set by `pigeon namespace`, which is a standing preference
// rather than a statement about this session; then "default".
//
// The origin is not decoration. A session that quietly landed somewhere it
// cannot see its peers is the whole failure mode of this feature, and doctor
// and `pigeon namespace` both report the answer this returns.
func ResolveNamespace() (ns Namespace, origin string) {
	if raw := strings.TrimSpace(os.Getenv(EnvNamespace)); raw != "" {
		if ns, err := ParseNamespace(raw); err == nil {
			return ns, EnvNamespace
		}
		// A value that cannot be a directory name must not steer one. Falling
		// back is safe; doing it silently would not be.
		return DefaultNamespace(), EnvNamespace + " is not a usable namespace, so it was ignored"
	}
	cwd := CurrentCwd()
	if cfg, _, err := LoadProjectConfig(cwd); err == nil && cfg != nil && cfg.Namespace != "" {
		// LoadProjectConfig validated it, so this cannot fail.
		if ns, err := ParseNamespace(cfg.Namespace); err == nil {
			return ns, ProjectConfigPath(cwd)
		}
	}
	if raw := readCLIConfig().Namespace; raw != "" {
		if ns, err := ParseNamespace(raw); err == nil {
			return ns, CLIConfigPath()
		}
	}
	return DefaultNamespace(), "the built-in default"
}

// CurrentNamespace is the namespace this process reads and writes.
func CurrentNamespace() Namespace {
	ns, _ := ResolveNamespace()
	return ns
}

// --- the CLI's own default ---------------------------------------------------

// CLIConfigPath records the namespace shell invocations should use, the way
// `kubectl config set-context` records a namespace. It is deliberately not a
// live move: a running session keeps the namespace its monitor armed with.
func CLIConfigPath() string { return filepath.Join(Home(), "cli.json") }

// maxCLIConfigBytes bounds the read. This file is written by pigeon and holds
// one field; anything larger is not it.
const maxCLIConfigBytes = 8 << 10

type cliConfig struct {
	Namespace string `json:"namespace,omitempty"`
	// As is the ephemeral identity a plain shell defaults to, the standing form
	// of PIGEON_AS. Validated as a name on read, so a hand-edited value cannot
	// steer a synthetic session id at anything unsafe.
	As string `json:"as,omitempty"`
}

// readCLIConfig degrades to "nothing set" for a missing or unreadable file. A
// corrupt preference must not cost a session its mail.
func readCLIConfig() cliConfig {
	var c cliConfig
	f, err := os.Open(CLIConfigPath())
	if err != nil {
		return c
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxCLIConfigBytes))
	if err != nil {
		return c
	}
	_ = json.Unmarshal(b, &c)
	if ValidNamespace(c.Namespace) != nil {
		c.Namespace = ""
	}
	if ValidName(c.As) != nil {
		c.As = ""
	}
	return c
}

// SetCLINamespace persists the namespace shell invocations default to.
func SetCLINamespace(ns Namespace) error {
	c := readCLIConfig()
	c.Namespace = ns.String()
	return writeCLIConfig(c)
}

// SetCLIIdentity persists the ephemeral identity shell invocations default to,
// or clears it when name is empty. Kept separate from the namespace so setting
// one never disturbs the other.
func SetCLIIdentity(name string) error {
	c := readCLIConfig()
	c.As = name
	return writeCLIConfig(c)
}

// writeCLIConfig persists the whole preference file. Callers read-modify-write
// so one field's setter cannot wipe another's value.
func writeCLIConfig(c cliConfig) error {
	if err := os.MkdirAll(Home(), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", Home(), err)
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	// Written by rename like every other state file, so a reader never sees a
	// half-written preference.
	tmp, err := os.CreateTemp(Home(), "cli-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Rename(name, CLIConfigPath())
}

// --- migration ---------------------------------------------------------------

// flatLayoutDirs are the six state directories that used to sit directly under
// Home(), before namespaces put a tree under each one.
var flatLayoutDirs = []string{"sessions", "inbox", "payloads", "locks", "topics", "cursors"}

// stderrLogf is where a one-time on-disk change announces itself. There is no
// logger to hand in EnsureDirs, and moving a live session's spool without
// saying so is exactly the kind of silence this is meant to avoid.
func stderrLogf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "[pigeon] "+format+"\n", a...)
}

// migrateFlatLayout moves a pre-namespace state tree into namespaces/default.
//
// Somebody has live sessions when this ships, and their spools, cursors and
// liveness locks are all addressed by path. A session that silently vanished
// from `pigeon ls` on upgrade -- or worse, one whose queued mail was left in a
// directory nothing reads any more -- would be a poor introduction to a feature
// whose whole promise is that mail arrives.
//
// It runs under a lock and moves each directory only when the destination does
// not already exist, so two processes racing on the same upgrade, or a run that
// died halfway through, both end up in the same place.
func migrateFlatLayout(logf func(string, ...any)) error {
	home := Home()
	// The marker records that this home has been converted, and it is what
	// keeps the usual case to a single stat. Without it the test was "are any
	// old directories present", which a process still running the old binary
	// makes true again simply by recreating them -- so every later command
	// re-took the migration lock, re-walked six directories, and re-logged
	// that it had declined to move each one. The steady state was six lines of
	// stderr on every single invocation, forever, describing a real problem in
	// a tone that read like a note.
	marker := filepath.Join(NamespacesDir(), ".migrated")
	if _, err := os.Stat(marker); err == nil {
		reportStrandedState(home, logf)
		return nil
	}
	if len(flatDirsPresent(home)) == 0 {
		// Nothing to convert, which is the answer for every fresh install.
		// Record it, so this is one stat from here on rather than seven.
		if err := os.MkdirAll(NamespacesDir(), 0o700); err == nil {
			_ = os.WriteFile(marker, []byte(nowRFC3339()+"\n"), 0o600)
		}
		return nil
	}
	lock, err := blockingExclusive(filepath.Join(home, "migrate.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()

	// Re-check under the lock: another process may have done it while we waited.
	present := flatDirsPresent(home)
	if len(present) == 0 {
		return nil
	}
	dst := filepath.Join(NamespacesDir(), DefaultNamespaceName)
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	moved := make([]string, 0, len(present))
	for _, d := range present {
		target := filepath.Join(dst, d)
		if _, err := os.Stat(target); err == nil {
			// A half-finished earlier run already placed this one. Leave both
			// alone rather than merging two directories of live state; the
			// stranded copy is reported once, below, not once per directory
			// on every command from here on.
			continue
		}
		if err := os.Rename(filepath.Join(home, d), target); err != nil {
			return fmt.Errorf("move %s into %s: %w", d, dst, err)
		}
		moved = append(moved, d)
	}
	if len(moved) > 0 {
		logf("moved %s into %s: state is now per-namespace and this one is %q",
			strings.Join(moved, ", "), dst, DefaultNamespaceName)
	}
	// Written last, so a run that dies partway is retried rather than recorded
	// as complete.
	if err := os.WriteFile(marker, []byte(nowRFC3339()+"\n"), 0o600); err != nil {
		return fmt.Errorf("record migration: %w", err)
	}
	reportStrandedState(home, logf)
	return nil
}

// reportStrandedState warns when old-layout directories exist after the
// migration has already run.
//
// That means a process still using the old paths recreated them, and its
// sessions are registered somewhere nothing reads: they cannot receive, and
// they are invisible to every other session. Nothing reconciles this, and
// nothing can -- merging two directories of live state is not something to
// attempt underneath a running monitor. So it is reported as the problem it
// is, with the one action that fixes it, rather than as a note about a rename
// that did not happen.
func reportStrandedState(home string, logf func(string, ...any)) {
	stranded := flatDirsPresent(home)
	if len(stranded) == 0 {
		return
	}
	sessions := 0
	if paths, err := filepath.Glob(filepath.Join(home, "sessions", "*.json")); err == nil {
		sessions = len(paths)
	}
	logf("WARNING: %s still holds pre-namespace state (%s).",
		home, strings.Join(stranded, ", "))
	if sessions > 0 {
		logf("WARNING: %d session(s) are registered there. They cannot receive mail and no", sessions)
		logf("WARNING: other session can see them. Restart them to re-register.")
	} else {
		logf("WARNING: a process on the old layout recreated these. Restart it, then remove them.")
	}
}

// flatDirsPresent lists the old-layout directories still sitting under Home().
func flatDirsPresent(home string) []string {
	var out []string
	for _, d := range flatLayoutDirs {
		if fi, err := os.Stat(filepath.Join(home, d)); err == nil && fi.IsDir() {
			out = append(out, d)
		}
	}
	return out
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

// optOutSet reports whether the opt-out variable is set at all, either way.
// That is how an explicit PIGEON=1 outranks a project config that would
// otherwise take this session off the bus.
func optOutSet() bool { return strings.TrimSpace(os.Getenv(EnvOptOut)) != "" }

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
