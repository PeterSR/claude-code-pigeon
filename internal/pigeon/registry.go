package pigeon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Status of a registered session, from the point of view of another session
// deciding whether it is worth writing to.
type Status string

const (
	// StatusLive: the session is running and its monitor holds the lock, so a
	// message will be delivered within about a second.
	StatusLive Status = "live"
	// StatusDeaf: the claude process is alive but no monitor is listening.
	// Messages still land in the spool but nothing will read them out --
	// monitors cannot be rebound mid-session, so this needs a restart.
	StatusDeaf Status = "deaf"
	// StatusDead: the claude process is gone. The entry is garbage.
	StatusDead Status = "dead"
)

// heartbeatInterval is how often a live monitor refreshes its entry.
const heartbeatInterval = 15 * time.Second

// heartbeatGrace allows for a slow or briefly stalled monitor before we call
// it deaf on heartbeat alone.
const heartbeatGrace = 3 * heartbeatInterval

// Entry is one registered session.
type Entry struct {
	SessionID     string   `json:"sessionId"`
	Name          string   `json:"name,omitempty"`        // self-declared, usable as an address
	Description   string   `json:"description,omitempty"` // self-declared, free text
	PID           int      `json:"pid,omitempty"`
	ProcStart     string   `json:"procStart,omitempty"`
	Cwd           string   `json:"cwd,omitempty"`
	Host          string   `json:"host,omitempty"`
	StartedAt     string   `json:"startedAt,omitempty"`
	HeartbeatAt   string   `json:"heartbeatAt,omitempty"`
	Subscriptions []string `json:"subscriptions,omitempty"`
	CCVersion     string   `json:"ccVersion,omitempty"`
	Driven        bool     `json:"driven,omitempty"`
	// Private sessions publish no cwd and no description. The flag itself is
	// published so this session can be told why its own entry looks bare.
	Private bool `json:"private,omitempty"`

	// Derived at read time, never persisted.
	Status Status `json:"-"`
}

// Display is the best short human handle for this session.
func (e *Entry) Display() string {
	if e.Name != "" {
		return e.Name
	}
	if e.Cwd != "" {
		return filepath.Base(e.Cwd)
	}
	return Short(e.SessionID)
}

// Addr is what another session should type to reach this one.
func (e *Entry) Addr() string {
	if e.Name != "" {
		return e.Name
	}
	return Short(e.SessionID)
}

// Short truncates a session UUID to a git-style prefix.
func Short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func entryPath(sessionID string) string {
	return filepath.Join(SessionsDir(), sessionID+".json")
}

// LockPath is the file a live monitor flocks for its whole lifetime. Holding
// it is the ground truth for "someone is listening".
func LockPath(sessionID string) string {
	return filepath.Join(LocksDir(), sessionID+".lock")
}

func ReadEntry(sessionID string) (*Entry, error) {
	if err := ValidSessionID(sessionID); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(entryPath(sessionID))
	if err != nil {
		return nil, err
	}
	var e Entry
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, err
	}
	e.Status = e.status()
	return &e, nil
}

// WriteEntry persists atomically so a reader never sees a half-written entry.
//
// A private session's entry is written without its cwd or description. That is
// enforced here rather than at registration because every later write lands
// here too -- a heartbeat, a subscribe, a `pigeon describe` -- and any one of
// them would otherwise put back what the project asked to keep off the bus.
func WriteEntry(e *Entry) error {
	if err := ValidSessionID(e.SessionID); err != nil {
		return err
	}
	if err := EnsureDirs(); err != nil {
		return err
	}
	rec := *e
	if rec.Private {
		rec.Cwd, rec.Description = "", ""
	}
	b, err := json.MarshalIndent(&rec, "", "  ")
	if err != nil {
		return err
	}
	// A unique temp file per write. A fixed name lets two concurrent writers
	// interleave into the same file and rename a half-and-half document into
	// place, which ReadEntry then rejects -- silently removing the session
	// from every peer's listing while its monitor sits there looking live.
	tmp, err := os.CreateTemp(SessionsDir(), "entry-*.tmp")
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
	return os.Rename(name, entryPath(e.SessionID))
}

func RemoveEntry(sessionID string) {
	if ValidSessionID(sessionID) != nil {
		return
	}
	_ = os.Remove(entryPath(sessionID))
}

// monitorListening reports whether a monitor currently holds the session lock.
//
// This is the cheap, race-free death signal: the monitor holds an exclusive
// flock for its entire lifetime, and the kernel drops it the instant that
// process exits -- crash, SIGKILL or clean shutdown alike. So if we can take
// the lock, nobody is listening.
func monitorListening(sessionID string) bool {
	return !lockIsFree(LockPath(sessionID))
}

func (e *Entry) status() Status {
	if !ProcessAlive(e.PID, e.ProcStart) {
		return StatusDead
	}
	if !monitorListening(e.SessionID) {
		return StatusDeaf
	}
	// Lock held but heartbeat stale means a wedged monitor: still counts as
	// not delivering, so report it the same way.
	if e.HeartbeatAt != "" {
		if t, err := time.Parse(time.RFC3339, e.HeartbeatAt); err == nil {
			if time.Since(t) > heartbeatGrace {
				return StatusDeaf
			}
		}
	}
	return StatusLive
}

// ProcessAlive checks the pid exists and, when we have one, that its start
// time still matches -- guarding against pid reuse handing us a stranger.
func ProcessAlive(pid int, wantStart string) bool {
	if pid <= 0 {
		return false
	}
	if !processExists(pid) {
		return false
	}
	if wantStart != "" {
		if got := ProcStart(pid); got != "" && got != wantStart {
			return false
		}
	}
	return true
}

// ListSessions returns registered sessions with Status filled in. Dead entries
// are hidden unless includeDead, and removed from disk when prune is set.
func ListSessions(includeDead, prune bool) ([]*Entry, error) {
	if err := EnsureDirs(); err != nil {
		return nil, err
	}
	paths, err := filepath.Glob(filepath.Join(SessionsDir(), "*.json"))
	if err != nil {
		return nil, err
	}
	var out []*Entry
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var e Entry
		if err := json.Unmarshal(b, &e); err != nil {
			continue
		}
		// A planted entry file must not be able to steer prune at a path
		// outside the state tree.
		if ValidSessionID(e.SessionID) != nil ||
			filepath.Base(p) != e.SessionID+".json" {
			continue
		}
		e.Status = e.status()
		if e.Status == StatusDead {
			if prune {
				removeSessionFiles(e.SessionID, p)
			}
			if !includeDead {
				continue
			}
		}
		out = append(out, &e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt < out[j].StartedAt })
	return out, nil
}

// removeSessionFiles clears everything a dead session leaves behind. Missing
// any of these means `pigeon prune` slowly litters the state directory.
func removeSessionFiles(sessionID, entryFile string) {
	_ = os.Remove(entryFile)
	_ = os.Remove(SpoolPath(sessionID))
	_ = os.Remove(LockPath(sessionID))
	_ = os.Remove(filepath.Join(LocksDir(), sessionID+".entry.lock"))
	_ = os.Remove(cursorPath(sessionID))
}

// reconcileOrphans clears state belonging to sessions with no registry entry.
//
// A monitor deregisters on the way out, which removes the entry that prune
// searches by -- so every other file that session owned became unreachable and
// accumulated. Sweep the state directories directly instead.
// ReconcileOrphans is the exported entry point for the sweep.
func ReconcileOrphans() int { return reconcileOrphans() }

func reconcileOrphans() int {
	known := map[string]bool{}
	entries, err := filepath.Glob(filepath.Join(SessionsDir(), "*.json"))
	if err == nil {
		for _, p := range entries {
			known[strings.TrimSuffix(filepath.Base(p), ".json")] = true
		}
	}

	removed := 0
	sweep := func(dir, suffix string) {
		paths, err := filepath.Glob(filepath.Join(dir, "*"+suffix))
		if err != nil {
			return
		}
		for _, p := range paths {
			id := strings.TrimSuffix(filepath.Base(p), suffix)
			if id == "" || known[id] || ValidSessionID(id) != nil {
				continue
			}
			if os.Remove(p) == nil {
				removed++
			}
		}
	}
	sweep(InboxDir(), ".ndjson")
	sweep(CursorsDir(), ".json")
	sweep(LocksDir(), ".lock")
	sweep(LocksDir(), ".entry.lock")
	return removed
}

// ResolveTarget finds a session by exact id, self-declared name, id prefix, or
// cwd basename -- in that order. Dead sessions are never resolved; deaf ones
// are, so the caller can warn rather than silently fail.
func ResolveTarget(token string) (*Entry, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("empty target")
	}
	all, err := ListSessions(false, false)
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("no live pigeon sessions registered")
	}

	var byName, byPrefix, byCwd []*Entry
	for _, e := range all {
		if e.SessionID == token {
			return e, nil
		}
		if e.Name != "" && strings.EqualFold(e.Name, token) {
			byName = append(byName, e)
		}
		if strings.HasPrefix(e.SessionID, token) {
			byPrefix = append(byPrefix, e)
		}
		if e.Cwd != "" && filepath.Base(e.Cwd) == token {
			byCwd = append(byCwd, e)
		}
	}
	for _, set := range [][]*Entry{byName, byPrefix, byCwd} {
		switch len(set) {
		case 1:
			return set[0], nil
		case 0:
			continue
		default:
			var ids []string
			for _, e := range set {
				ids = append(ids, fmt.Sprintf("%s (%s)", Short(e.SessionID), e.Display()))
			}
			return nil, fmt.Errorf("%q is ambiguous: %s", token, strings.Join(ids, ", "))
		}
	}
	if strings.HasPrefix(strings.ToLower(token), "shell:") {
		return nil, fmt.Errorf("%q is a shell, not a session: it has no inbox and cannot be replied to", token)
	}
	return nil, fmt.Errorf("no live session matching %q", token)
}

// nameRe constrains self-declared names. A name is rendered into other
// sessions' notifications and used as an address, so it may not carry
// structural characters a sender could use to forge a directive.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`)

// ValidName rejects names that are unsafe to render or ambiguous to address.
func ValidName(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("invalid name %q: use 1-32 characters, letters, digits, dot, dash or underscore", name)
	}
	return nil
}

// MutateEntry applies fn to a session's entry under an exclusive lock, so
// concurrent updates (a name change racing a subscribe, say) cannot lose each
// other. Read-modify-write on a JSON file is otherwise last-writer-wins.
//
// The lock is separate from the monitor's liveness lock: taking that one would
// make the session look deaf for the duration.
func MutateEntry(sessionID string, fn func(*Entry) error) error {
	unlock, err := lockSession(sessionID)
	if err != nil {
		return err
	}
	defer unlock()

	e, err := ReadEntry(sessionID)
	if err != nil {
		return fmt.Errorf("session not registered")
	}
	if err := fn(e); err != nil {
		return err
	}
	return WriteEntry(e)
}

// lockSession takes the per-session mutation lock, held for the duration of a
// read-modify-write on that session's entry or cursors.
//
// This is deliberately not the monitor's liveness lock: taking that one would
// make the session look deaf for as long as we held it.
func lockSession(sessionID string) (func(), error) {
	if err := ValidSessionID(sessionID); err != nil {
		return nil, err
	}
	if err := EnsureDirs(); err != nil {
		return nil, err
	}
	c, err := blockingExclusive(filepath.Join(LocksDir(), sessionID+".entry.lock"))
	if err != nil {
		return nil, err
	}
	return func() { _ = c.Close() }, nil
}

// NameTaken reports whether another live session already claims a name.
func NameTaken(name, exceptSessionID string) bool {
	all, err := ListSessions(false, false)
	if err != nil {
		return false
	}
	for _, e := range all {
		if e.SessionID != exceptSessionID && strings.EqualFold(e.Name, name) {
			return true
		}
	}
	return false
}
