package pigeon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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
	SessionID string `json:"sessionId"`
	// Namespace is the group this session belongs to. It is published so a
	// consumer of `pigeon ls --json` or list_sessions does not have to infer
	// it, and it is always filled in from the directory the entry was read
	// from rather than from the file, so a planted entry cannot claim to live
	// somewhere it does not.
	Namespace     string   `json:"namespace,omitempty"`
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
	// ClaudeName is Claude Code's own session name, the one /status shows, and
	// ClaudeNameSource is how Claude Code arrived at it ("derived" from the cwd,
	// or user-set). Both are a host label pigeon merely reflects, never an
	// address -- Name is what routing uses. They are filled in best-effort from
	// Claude Code's session index and refreshed by the heartbeat, so a mid-session
	// rename shows up within about 15s; empty when the index cannot be read.
	// Withheld for private sessions, because a derived name echoes the cwd.
	ClaudeName       string `json:"claudeName,omitempty"`
	ClaudeNameSource string `json:"claudeNameSource,omitempty"`
	Driven           bool   `json:"driven,omitempty"`
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

// NS is the namespace this entry was read from.
func (e *Entry) NS() Namespace {
	ns, err := ParseNamespace(e.Namespace)
	if err != nil {
		return DefaultNamespace()
	}
	return ns
}

// Short truncates a session UUID to a git-style prefix.
func Short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func (n Namespace) entryPath(sessionID string) string {
	return filepath.Join(n.SessionsDir(), sessionID+".json")
}

// LockPath is the file a live monitor flocks for its whole lifetime. Holding
// it is the ground truth for "someone is listening".
func (n Namespace) LockPath(sessionID string) string {
	return filepath.Join(n.LocksDir(), sessionID+".lock")
}

func ReadEntry(sessionID string) (*Entry, error) {
	return CurrentNamespace().ReadEntry(sessionID)
}

func (n Namespace) ReadEntry(sessionID string) (*Entry, error) {
	if err := ValidSessionID(sessionID); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(n.entryPath(sessionID))
	if err != nil {
		return nil, err
	}
	var e Entry
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, err
	}
	// The directory is the truth about which namespace this session is in; the
	// field is a convenience for whoever reads the JSON afterwards.
	e.Namespace = n.String()
	e.Status = e.status(n)
	return &e, nil
}

// WriteEntry persists atomically so a reader never sees a half-written entry.
//
// A private session's entry is written without its cwd or description. That is
// enforced here rather than at registration because every later write lands
// here too -- a heartbeat, a subscribe, a `pigeon describe` -- and any one of
// them would otherwise put back what the project asked to keep off the bus.

func (n Namespace) WriteEntry(e *Entry) error {
	if err := ValidSessionID(e.SessionID); err != nil {
		return err
	}
	if err := n.EnsureDirs(); err != nil {
		return err
	}
	rec := *e
	// Stamped from the directory being written, never from the caller's copy: a
	// mismatch between the two would make a session appear to be somewhere it
	// cannot receive mail.
	rec.Namespace = n.String()
	if rec.Private {
		// The Claude name goes too: a derived one is the cwd basename with a
		// suffix, so publishing it would leak exactly the directory Private is
		// meant to keep off the bus.
		rec.Cwd, rec.Description = "", ""
		rec.ClaudeName, rec.ClaudeNameSource = "", ""
	}
	b, err := json.MarshalIndent(&rec, "", "  ")
	if err != nil {
		return err
	}
	// A unique temp file per write. A fixed name lets two concurrent writers
	// interleave into the same file and rename a half-and-half document into
	// place, which ReadEntry then rejects -- silently removing the session
	// from every peer's listing while its monitor sits there looking live.
	tmp, err := os.CreateTemp(n.SessionsDir(), "entry-*.tmp")
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
	return os.Rename(name, n.entryPath(e.SessionID))
}

func RemoveEntry(sessionID string) { CurrentNamespace().RemoveEntry(sessionID) }

func (n Namespace) RemoveEntry(sessionID string) {
	if ValidSessionID(sessionID) != nil {
		return
	}
	_ = os.Remove(n.entryPath(sessionID))
}

// monitorListening reports whether a monitor currently holds the session lock.
//
// This is the cheap, race-free death signal: the monitor holds an exclusive
// flock for its entire lifetime, and the kernel drops it the instant that
// process exits -- crash, SIGKILL or clean shutdown alike. So if we can take
// the lock, nobody is listening.
func (n Namespace) monitorListening(sessionID string) bool {
	return !lockIsFree(n.LockPath(sessionID))
}

func (e *Entry) status(n Namespace) Status {
	if !ProcessAlive(e.PID, e.ProcStart) {
		return StatusDead
	}
	if !n.monitorListening(e.SessionID) {
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
	return CurrentNamespace().ListSessions(includeDead, prune)
}

func (n Namespace) ListSessions(includeDead, prune bool) ([]*Entry, error) {
	if err := ensureHome(); err != nil {
		return nil, err
	}
	paths, err := filepath.Glob(filepath.Join(n.SessionsDir(), "*.json"))
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
		e.Namespace = n.String()
		e.Status = e.status(n)
		if e.Status == StatusDead {
			if prune {
				n.removeSessionFiles(e.SessionID, p)
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

// ListAllSessions returns every registered session on the machine, namespace by
// namespace. Ordinary listing is per namespace on purpose; this is what
// `--all-namespaces` asks for.
func ListAllSessions(includeDead, prune bool) ([]*Entry, error) {
	spaces, err := ListNamespaces()
	if err != nil {
		return nil, err
	}
	var out []*Entry
	for _, info := range spaces {
		ns, err := ParseNamespace(info.Name)
		if err != nil {
			continue
		}
		got, err := ns.ListSessions(includeDead, prune)
		if err != nil {
			continue
		}
		out = append(out, got...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].StartedAt < out[j].StartedAt
	})
	return out, nil
}

// NamespaceInfo is one row of `pigeon namespaces`.
type NamespaceInfo struct {
	Name    string `json:"name"`
	Live    int    `json:"live"`
	Deaf    int    `json:"deaf"`
	Current bool   `json:"current,omitempty"`
}

// ListNamespaces reports every namespace that exists on disk, plus the caller's
// own and the default one, so a fresh install still answers the question.
func ListNamespaces() ([]NamespaceInfo, error) {
	if err := ensureHome(); err != nil {
		return nil, err
	}
	current := CurrentNamespace()
	seen := map[string]bool{DefaultNamespaceName: true, current.String(): true}

	if ents, err := os.ReadDir(NamespacesDir()); err == nil {
		for _, ent := range ents {
			// A directory whose name is not a valid namespace was not put there
			// by pigeon, and listing it would suggest it can be addressed.
			if ent.IsDir() && ValidNamespace(ent.Name()) == nil {
				seen[ent.Name()] = true
			}
		}
	}

	out := make([]NamespaceInfo, 0, len(seen))
	for name := range seen {
		ns, err := ParseNamespace(name)
		if err != nil {
			continue
		}
		// A private namespace does not exist as far as anything inside a
		// Claude Code session is concerned, unless it is the one asking.
		if checkPrivateAccess(ns) != nil {
			continue
		}
		info := NamespaceInfo{Name: name, Current: ns.Is(current)}
		entries, err := ns.ListSessions(false, false)
		if err == nil {
			for _, e := range entries {
				switch e.Status {
				case StatusLive:
					info.Live++
				case StatusDeaf:
					info.Deaf++
				}
			}
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// removeSessionFiles clears everything a dead session leaves behind. Missing
// any of these means `pigeon prune` slowly litters the state directory.
//
// Locks are the deliberate exception: unlinking one lets a second process lock
// a different inode while both believe they hold it. A dead session's lock is
// an empty file nobody holds, and leaving it costs a few bytes.
func (n Namespace) removeSessionFiles(sessionID, entryFile string) {
	_ = os.Remove(entryFile)
	_ = os.Remove(n.SpoolPath(sessionID))
	_ = os.Remove(n.cursorPath(sessionID))
}

// reclaimPayloads removes payload files no surviving log still points at.
//
// A message whose body overflows the notification budget spills to a file and
// carries a pointer instead, so the file has to outlive the message in the log.
// Nothing deleted them: not removeSessionFiles, not reconcileOrphans, not the
// topic retention pass. `pigeon uninstall --purge` was the only thing that ever
// did, which for a long-running machine is not a retention policy.
//
// The reference set is built by scanning what remains, which is exact rather
// than an age heuristic: a payload for a message still sitting unread on a deaf
// session's spool must survive however old it is, and one whose message was
// compacted away is garbage the moment it goes.
func (n Namespace) reclaimPayloads() (removed int, bytes int64) {
	return reclaimPayloadsIn(n.PayloadsDir(), n.referencedPayloads())
}

// referencedPayloads collects every payload path still named by a message in
// this namespace's spools or topic logs.
func (n Namespace) referencedPayloads() map[string]bool {
	refs := map[string]bool{}
	for _, glob := range []string{
		filepath.Join(n.InboxDir(), "*.ndjson"),
		filepath.Join(n.TopicsDir(), "*.ndjson"),
	} {
		paths, _ := filepath.Glob(glob)
		for _, p := range paths {
			collectPayloadRefs(p, refs)
		}
	}
	return refs
}

func collectPayloadRefs(logPath string, into map[string]bool) {
	f, err := os.Open(logPath)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		m, err := ParseMessage(strings.TrimSpace(sc.Text()))
		if err != nil || m.Payload == "" {
			continue
		}
		into[m.Payload] = true
	}
}

func reclaimPayloadsIn(dir string, referenced map[string]bool) (removed int, bytes int64) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.txt"))
	if err != nil {
		return 0, 0
	}
	for _, p := range paths {
		if referenced[p] {
			continue
		}
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if os.Remove(p) == nil {
			removed++
			bytes += fi.Size()
		}
	}
	return removed, bytes
}

// reconcileOrphans clears state belonging to sessions with no registry entry.
//
// A monitor deregisters on the way out, which removes the entry that prune
// searches by -- so every other file that session owned became unreachable and
// accumulated. Sweep the state directories directly instead.
// ReconcileOrphans is the exported entry point for the sweep. It covers one
// namespace; a session whose project config changed namespace leaves its old
// entry behind, and only sweeping every namespace clears that.
func ReconcileOrphans() int { return CurrentNamespace().ReconcileOrphans() }

func (n Namespace) ReconcileOrphans() int { return n.reconcileOrphans() }

func (n Namespace) reconcileOrphans() int {
	known := map[string]bool{}
	entries, err := filepath.Glob(filepath.Join(n.SessionsDir(), "*.json"))
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
	sweep(n.InboxDir(), ".ndjson")
	sweep(n.CursorsDir(), ".json")
	// Locks are deliberately NOT swept, and adding them back would be a bug.
	// Unlinking a lock file is how two processes end up holding "the same"
	// lock on different inodes, which is the failure sys_unix.go avoids by
	// never unlinking one.
	//
	// The sweep did include them, and it was worse than useless: it trimmed
	// the suffix and treated the rest as a session id, so "<sid>.entry.lock"
	// became "<sid>.entry" and "topic-deploys.lock" became "topic-deploys".
	// Neither is a registered session, so both were deleted, for live sessions
	// and active topics alike. A publisher holding a topic lock had it removed
	// underneath it; the compaction later in the same `pigeon prune` then took
	// a fresh inode instead of blocking, and the line the publisher had
	// already reported as sent went to the replaced file and was lost.
	//
	// An abandoned lock file is a zero-byte inode. Leaving it costs nothing.
	return removed
}

// ResolveTarget finds a session by exact id, exact pid, self-declared name, id
// prefix, or cwd basename -- in that order. Dead sessions are never resolved;
// deaf ones are, so the caller can warn rather than silently fail.
//
// A pid is exact and unique among live sessions, so it ranks with the session
// id above the fuzzier name/prefix/basename tiers. A numeric token is also a
// valid hex id-prefix, so the pid tier wins that overlap deliberately: someone
// typing a pid means the process, not a UUID that happens to start with those
// digits.
//
// It searches one namespace, which is what makes namespaces isolation rather
// than decoration: a name that is taken next door is not taken here, and a
// token that matches nothing here is not quietly answered by a stranger.
func ResolveTarget(token string) (*Entry, error) {
	return CurrentNamespace().ResolveTarget(token)
}

func (n Namespace) ResolveTarget(token string) (*Entry, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("empty target")
	}
	all, err := n.ListSessions(false, false)
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("no live pigeon sessions registered in namespace %q", n)
	}

	var byPid, byName, byPrefix, byCwd []*Entry
	for _, e := range all {
		if e.SessionID == token {
			return e, nil
		}
		if e.PID > 0 && strconv.Itoa(e.PID) == token {
			byPid = append(byPid, e)
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
	for _, set := range [][]*Entry{byPid, byName, byPrefix, byCwd} {
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
	return nil, fmt.Errorf("no live session matching %q in namespace %q", token, n)
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
	return CurrentNamespace().MutateEntry(sessionID, fn)
}

func (n Namespace) MutateEntry(sessionID string, fn func(*Entry) error) error {
	unlock, err := n.lockSession(sessionID)
	if err != nil {
		return err
	}
	defer unlock()

	e, err := n.ReadEntry(sessionID)
	if err != nil {
		return fmt.Errorf("session not registered in namespace %q", n)
	}
	if err := fn(e); err != nil {
		return err
	}
	return n.WriteEntry(e)
}

// lockSession takes the per-session mutation lock, held for the duration of a
// read-modify-write on that session's entry or cursors.
//
// This is deliberately not the monitor's liveness lock: taking that one would
// make the session look deaf for as long as we held it.
func (n Namespace) lockSession(sessionID string) (func(), error) {
	if err := ValidSessionID(sessionID); err != nil {
		return nil, err
	}
	// The session has to already be here. Calling EnsureDirs instead would
	// build a whole namespace tree before discovering there is nobody in it, so
	// `subscribe --namespace defualt` -- a typo -- left "defualt" in every
	// later `pigeon namespaces` as if it were a real place.
	if _, err := os.Stat(n.entryPath(sessionID)); err != nil {
		return nil, fmt.Errorf("session %s is not registered in namespace %q", Short(sessionID), n)
	}
	if err := os.MkdirAll(n.LocksDir(), 0o700); err != nil {
		return nil, err
	}
	c, err := blockingExclusive(filepath.Join(n.LocksDir(), sessionID+".entry.lock"))
	if err != nil {
		return nil, err
	}
	return func() { _ = c.Close() }, nil
}

// NameTaken reports whether another live session already claims a name.
//
// Only within this namespace: a name is an address, and two sessions in
// separate namespaces answering to "api" cannot misroute anything, because
// nothing addresses across a boundary without saying so.
func NameTaken(name, exceptSessionID string) bool {
	return CurrentNamespace().NameTaken(name, exceptSessionID)
}

func (n Namespace) NameTaken(name, exceptSessionID string) bool {
	all, err := n.ListSessions(false, false)
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
