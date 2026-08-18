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
	// StatusSocket: no monitor is listening, but the session's own Claude Code
	// inbox socket answers, so pigeon can still put a message in front of it.
	//
	// This status is never produced by Entry.status, which knows only about the
	// monitor lock. It is a PROMOTION of StatusDeaf, applied by AnnotateReach
	// and only by callers that asked for it -- see that function for why the
	// probe is opt-in rather than folded into every listing.
	//
	// It exists because "deaf" was coined when the monitor was the only way in.
	// A session with a dead monitor and a good socket is the single most common
	// state on a machine that has been running a while (see the README on
	// monitors dying at resume), and reporting it as deaf tells you to restart
	// a session that would have received your message perfectly well.
	StatusSocket Status = "socket"
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

	// Runtime names the agent host that wrote this entry; RuntimeVersion is
	// that host's own version string. RuntimeClaudeCode is the only value
	// pigeon writes today, but the field exists so a host is recorded as a
	// fact rather than assumed -- see doctor.go's minCCVersion/testedCCVersion
	// for the Claude-Code-specific version floor this is not.
	Runtime        string `json:"runtime,omitempty"`
	RuntimeVersion string `json:"runtimeVersion,omitempty"`
	// Deprecated: written by pigeon before the runtime/label rename. Read,
	// never written -- folded into RuntimeVersion when that key is absent (see
	// Entry.migrateLegacy).
	LegacyCCVersion string `json:"ccVersion,omitempty"`

	// Label is a display name the host assigns -- the one /status shows, for
	// Claude Code -- and LabelSource is how the host arrived at it ("derived"
	// from the cwd, or a value marking it user-set). Both are a label pigeon
	// merely reflects, never an address -- Name is what routing uses. They are
	// filled in best-effort from the host's own session state (see
	// claudesession.go for how Claude Code's is read) and refreshed by the
	// heartbeat, so a mid-session rename shows up within about 15s; empty when
	// that state cannot be read. Withheld for private sessions, because a
	// derived label echoes the cwd.
	Label       string `json:"label,omitempty"`
	LabelSource string `json:"labelSource,omitempty"`
	// Deprecated: written by pigeon before the rename. Read, never written --
	// folded into Label/LabelSource when the new keys are absent (see
	// Entry.migrateLegacy). The exposure of keeping the old reader around is
	// small: a heartbeat rewrites a live entry every 15s, so a session whose
	// monitor is from this build self-heals onto the new keys within one tick;
	// one still running an older monitor keeps writing these for its own
	// lifetime, and the worst consequence is a blank column in a listing,
	// because the label is never an address and nothing routes on it.
	LegacyClaudeName       string `json:"claudeName,omitempty"`
	LegacyClaudeNameSource string `json:"claudeNameSource,omitempty"`
	Driven                 bool   `json:"driven,omitempty"`
	// Delivery maps a subscribed topic to how it reaches this session: absent
	// (or "push") notifies per message, "digest" collapses routine traffic
	// into one line a minute (an alert or a message naming this session in
	// `for` still pushes immediately), "quiet" notifies nothing but that one
	// line, ever -- see RunMonitor's delivery switch for the exact rules.
	//
	// The direct spool has no key here and cannot be configured: a message
	// addressed to this session personally is not chatter to be batched, it
	// is mail for exactly one reader, so RunMonitor never even looks this map
	// up for it.
	Delivery map[string]string `json:"delivery,omitempty"`
	// Private sessions publish no cwd and no description. The flag itself is
	// published so this session can be told why its own entry looks bare.
	Private bool `json:"private,omitempty"`
	// Ephemeral marks a session that is not an agent session at all: a plain
	// shell holding an inbox open with `pigeon listen`. Its pid is that shell, it
	// has no Label to reflect, and it vanishes the moment the shell exits, so
	// listings mark it as a shell rather than reporting a blank label as
	// something to fix.
	Ephemeral bool `json:"ephemeral,omitempty"`

	// Derived at read time, never persisted.
	Status Status `json:"-"`
}

// RuntimeClaudeCode is the only Runtime value pigeon writes today.
const RuntimeClaudeCode = "claude-code"

// migrateLegacy folds an entry's pre-rename keys into their replacements
// whenever the new key is absent, so an entry an older pigeon wrote reads the
// same as one this build wrote. It never overwrites a new key that is already
// set: that value is what wrote the entry most recently, and a legacy key
// sitting next to it must not win back over a change made under this key.
//
// Called once, right after every raw JSON entry is unmarshalled -- see
// ReadEntry, ListSessions and pruneDeadEntries -- so every reader downstream
// of those sees the new shape regardless of which shape is actually on disk.
func (e *Entry) migrateLegacy() {
	if e.Label == "" {
		e.Label = e.LegacyClaudeName
	}
	if e.LabelSource == "" {
		e.LabelSource = e.LegacyClaudeNameSource
	}
	if e.RuntimeVersion == "" {
		e.RuntimeVersion = e.LegacyCCVersion
	}
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
	e.migrateLegacy()
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
		// The label goes too: a derived one is the cwd basename with a
		// suffix, so publishing it would leak exactly the directory Private is
		// meant to keep off the bus.
		rec.Cwd, rec.Description = "", ""
		rec.Label, rec.LabelSource = "", ""
	}
	// Legacy keys are read, never written: whatever put them on rec -- a
	// migrateLegacy fold-in from ReadEntry, or a hand-edited file -- must not
	// be carried forward by a write this build makes.
	rec.LegacyClaudeName, rec.LegacyClaudeNameSource, rec.LegacyCCVersion = "", "", ""
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
	// Leave a tombstone naming the process that owned this session, so the
	// unattended sweep can tell "the monitor exited" from "the session is
	// gone". Those are not the same thing and the difference is a mailbox:
	// a monitor can die and rearm while its claude process runs on, and the
	// spool it deliberately leaves behind is that session's unread mail.
	//
	// Written before the entry is removed, from the entry itself, because the
	// entry is the only place the pid lives. Best effort: a failure here costs
	// the sweep its exact signal and falls back to the age guard, which is the
	// behaviour orphans from older builds get anyway.
	if e, err := n.ReadEntry(sessionID); err == nil && e.PID > 0 {
		if b, err := json.Marshal(tombstone{PID: e.PID, ProcStart: e.ProcStart}); err == nil {
			_ = os.WriteFile(n.tombstonePath(sessionID), b, 0o600)
		}
	}
	_ = os.Remove(n.entryPath(sessionID))
}

// tombstone records who owned a session whose entry has been removed. It exists
// so the sweep has the one fact an orphan otherwise cannot supply: whether the
// process that owned this state is still running.
type tombstone struct {
	PID       int    `json:"pid"`
	ProcStart string `json:"procStart,omitempty"`
}

// tombstoneSuffix is deliberately not ".json": the sessions directory is
// globbed for "*.json" by both pruneDeadEntries and reconcileOrphans, and a
// tombstone must never be mistaken for an entry.
const tombstoneSuffix = ".gone"

func (n Namespace) tombstonePath(sessionID string) string {
	return filepath.Join(n.SessionsDir(), sessionID+tombstoneSuffix)
}

// clearTombstone drops the record left by a previous exit of this session.
func (n Namespace) clearTombstone(sessionID string) {
	if ValidSessionID(sessionID) != nil {
		return
	}
	_ = os.Remove(n.tombstonePath(sessionID))
}

// ownerAlive reports whether the process that owned sessionID is still running,
// and whether a tombstone was found to ask at all.
func (n Namespace) ownerAlive(sessionID string) (alive, known bool) {
	b, err := os.ReadFile(n.tombstonePath(sessionID))
	if err != nil {
		return false, false
	}
	var t tombstone
	if err := json.Unmarshal(b, &t); err != nil || t.PID <= 0 {
		return false, false
	}
	return ProcessAlive(t.PID, t.ProcStart), true
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
		e.migrateLegacy()
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

// pruneDeadEntries removes registry entries whose owning process is gone,
// except exceptSID -- its caller is registering that session right now, so by
// definition it is not dead however its own status computes (a pid that
// cannot be resolved yet reads as StatusDead, and re-registering must not
// have that race away the very entry it is about to preserve).
func (n Namespace) pruneDeadEntries(exceptSID string) int {
	paths, err := filepath.Glob(filepath.Join(n.SessionsDir(), "*.json"))
	if err != nil {
		return 0
	}
	count := 0
	for _, p := range paths {
		id := strings.TrimSuffix(filepath.Base(p), ".json")
		if id == exceptSID {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var e Entry
		if err := json.Unmarshal(b, &e); err != nil {
			continue
		}
		e.migrateLegacy()
		// A planted entry file must not be able to steer this at a path
		// outside the state tree, same guard as ListSessions.
		if ValidSessionID(e.SessionID) != nil || filepath.Base(p) != e.SessionID+".json" {
			continue
		}
		if e.status(n) != StatusDead {
			continue
		}
		lock, acquired, err := n.tryMonitorLock(e.SessionID)
		if err != nil || !acquired {
			continue
		}
		n.removeSessionFiles(e.SessionID, p)
		_ = lock.Close()
		count++
	}
	return count
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
	_ = os.Remove(n.tombstonePath(sessionID))
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

// collectPayloadRefs records every payload path a log's messages still point
// at -- the body-overflow file named by Payload, and every file named by
// Attach. Both spill to the same payload directory (see attachFiles), so both
// have to be counted here or the second one is exactly what a live message
// still points at that this scan would otherwise call garbage.
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
		if err != nil {
			continue
		}
		if m.Payload != "" {
			into[m.Payload] = true
		}
		for _, p := range m.Attach {
			if p != "" {
				into[p] = true
			}
		}
	}
}

func reclaimPayloadsIn(dir string, referenced map[string]bool) (removed int, bytes int64) {
	// Every file in the directory, not just *.txt. Bodies spill as
	// "<id>.txt", but an attachment keeps its own extension, so a .patch or a
	// .go copied in here was invisible to reclamation and leaked forever. The
	// referenced-set check below is what decides safety; the glob only decides
	// what gets considered.
	paths, err := filepath.Glob(filepath.Join(dir, "*"))
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
//
// This form sweeps every orphan regardless of age, which is what `pigeon prune`
// asks for: the person typing it is present and has decided. The automatic
// sweep uses reconcileOrphans(orphanGrace) instead -- see the hazard there.
func ReconcileOrphans() int { return CurrentNamespace().ReconcileOrphans() }

func (n Namespace) ReconcileOrphans() int { return n.reconcileOrphans(0, "") }

// orphanGrace bounds how long leftover state survives when nothing can say who
// owned it. It is the FALLBACK, not the rule: see ownerAlive.
//
// "No entry" does not mean "gone". A monitor removes the entry on its way out
// but deliberately leaves the spool, so mail queued while nothing was listening
// survives until a monitor comes back for it, and monitors do die and rearm
// without the claude process ever exiting. Deleting such a session's state
// costs it every message published while it was away: its cursors are re-seeded
// at the END of each log on the next registration, so the gap is not
// redelivered, it is skipped.
//
// Which is why an mtime is not good enough on its own, and this originally
// shipped believing it was. A cursor file is written when messages flow, not
// while a session merely lives, so a quiet session's cursors can be older than
// any grace period while the session is running perfectly well. The tombstone
// RemoveEntry leaves is the exact signal; age only decides orphans that predate
// it, which by definition come from a build that never wrote one and are
// therefore genuinely old.
const orphanGrace = 24 * time.Hour

// reconcileOrphans clears state whose session has no registry entry. exceptSID
// is never touched: its caller may be registering that session right now, and
// its entry does not exist yet.
func (n Namespace) reconcileOrphans(minAge time.Duration, exceptSID string) int {
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
			if id == exceptSID {
				continue
			}
			if minAge > 0 {
				// The tombstone answers exactly, so age is never consulted when
				// there is one: a session whose monitor exited an hour ago but
				// whose claude process is still running keeps its mail, and one
				// whose process is gone is collected immediately rather than
				// after a day.
				if alive, known := n.ownerAlive(id); known {
					if alive {
						continue
					}
				} else {
					fi, err := os.Stat(p)
					if err != nil || time.Since(fi.ModTime()) < minAge {
						continue
					}
				}
			}
			if os.Remove(p) == nil {
				removed++
			}
			_ = os.Remove(n.tombstonePath(id))
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
