package pigeon

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// PublicTopic is the mailbox every session joins by default, so a broadcast
// reaches the whole namespace without anyone configuring anything.
const PublicTopic = "all"

// GlobalPrefix marks a topic that lives outside every namespace, so `@ops` is
// one log the whole machine shares while `ops` is one log per namespace.
//
// "@" rather than the obvious alternatives because a topic is typed at a shell:
// `*` globs, `!` history-expands, and `~` gets tilde-expanded, so `pigeon
// publish *ops` would fail in a way that has nothing to do with pigeon. "@" is
// shell-safe and reads differently from the "#" a namespaced topic renders with.
const GlobalPrefix = "@"

// GlobalPublicTopic is the machine-wide mailbox. Every session subscribes to it
// as well as to its own namespace's `all`: this is the one place isolation is
// deliberately not absolute, because a broadcast meant for everyone on the
// machine has to reach everyone on the machine. `pigeon unsubscribe @all`
// opts out.
const GlobalPublicTopic = GlobalPrefix + PublicTopic

var topicRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// inboxCursorKey tracks how far the direct spool has been read. It is not a
// valid topic name, so it can never collide with one.
const inboxCursorKey = ":inbox"

// --- consumption cursors -----------------------------------------------------
//
// cursors/<session>.json holds two families under one map. The bare key
// (inboxCursorKey or a topic's TopicRef.String()) is the MONITOR's ingest
// cursor: how far followSource has read as it decides what to notify. The
// "read:" / "readat:" keys below are the SESSION's consumption cursor: how far
// ReadInbox has actually handed back to a pull. They are advanced by different
// code paths and must never be conflated -- a pull silently marking a message
// as notified, or a notification silently marking it as pulled, both defeat
// the other path's bookkeeping.
//
// A topic name can never contain ":" (ValidTopic), and inboxCursorKey already
// leans on that same fact, so "read:" + any valid monitor-cursor key can never
// collide with a bare monitor-cursor key, nor with another source's "read:"
// key.

const readCursorPrefix = "read:"
const readAtCursorPrefix = "readat:"

// readCursorKey and readAtCursorKey name a source's consumption cursor and the
// unix-seconds timestamp it last advanced. source is the same key the
// monitor's own cursor is filed under: inboxCursorKey for the direct spool
// (giving "read::inbox" / "readat::inbox"), or a TopicRef's String() for a
// topic.
func readCursorKey(source string) string   { return readCursorPrefix + source }
func readAtCursorKey(source string) string { return readAtCursorPrefix + source }

// maxUnreadAge bounds how long a consumption cursor can hold a topic log's
// compaction back. Without a bound, a session that pulls once and then idles --
// or never comes back -- would pin the log open forever, which is worse than
// the message loss the cursor exists to prevent. It is refreshed only by a real
// consuming pull, never by a peek, so it measures reading rather than looking.
const maxUnreadAge = 6 * time.Hour

// effectiveOffset is the position pruneTopicDir treats one subscriber as
// having reached, for the purpose of a compaction cut.
//
// It prefers the session's *consumption* cursor (what ReadInbox has actually
// handed back) over its *monitor* cursor (what followSource has merely
// ingested), because compaction must never cut past a message the session was
// notified of but has not yet pulled. It falls back to the monitor cursor,
// and reports abandoned=true, in exactly two cases:
//
//   - the consumption cursor does not exist at all. readCursors returns a
//     plain map, so a missing key would otherwise read back as 0 -- and on
//     the day this shipped, no session anywhere has a consumption cursor.
//     Using that 0 as a real position would collapse `slowest` to 0 for every
//     topic and stop compaction fleet-wide, forever. This is the "naive fix"
//     the design explicitly forbids, so the zero-value read is guarded by the
//     presence check below rather than trusted.
//   - the consumption cursor is present but abandoned: too far behind the
//     monitor cursor, or too old, or has a missing/zero readat alongside a
//     present read cursor (which reads the same as "too old").
//
// abandoned is false when the fallback is simply "no consumption cursor
// exists yet" -- that is the expected, unremarkable state for the entire
// fleet on day one, not something worth counting as a problem.
func effectiveOffset(cursors map[string]int64, topic string, now time.Time) (off int64, state cursorState) {
	monitorOff := cursors[topic]
	roff, ok := cursors[readCursorKey(topic)]
	if !ok {
		return monitorOff, cursorAbsent
	}
	rat, hasAt := cursors[readAtCursorKey(topic)]
	if !hasAt || rat <= 0 {
		// Seeded by Subscribe but never advanced by a pull. This session takes
		// notifications and does not use the pull path, so protecting its
		// position would hold the log open for nothing.
		return monitorOff, cursorNeverPulled
	}
	if now.Sub(time.Unix(rat, 0)) > maxUnreadAge {
		return monitorOff, cursorStale
	}
	// Deliberately NOT bounded by how far behind the monitor this is.
	//
	// An earlier version cut once the gap passed a byte threshold, checked
	// before the timestamp, and that destroyed exactly the case the cursor
	// exists for: a session pulls at 11:58, a peer publishes a megabyte and a
	// half at 12:00, prune runs at 12:01 and cuts the whole burst away before
	// the session -- which is awake and reading on time -- ever asks for it.
	// "Abandoned" has to mean "nobody is coming back", not "the burst was big".
	// A session that keeps pulling keeps its cursor moving, so the gap closes
	// on its own; one that stops pulling stops refreshing readat and ages out
	// within maxUnreadAge. Peeking does not refresh it, so only real
	// consumption counts as being alive.
	return roff, cursorFresh
}

// cursorState says why pruneTopicDir used the offset it did, so the prune
// result can report the one case worth reporting: a session that pulled, then
// stopped, and is now holding a log open. A cursor that was never seeded, or
// seeded and never used, is the unremarkable state of most of the fleet and is
// not worth counting as a problem.
type cursorState int

const (
	cursorFresh cursorState = iota
	cursorAbsent
	cursorNeverPulled
	cursorStale
)

// ValidTopic keeps topic names safe as filenames and readable in a
// notification line. It validates the bare name; the global prefix is stripped
// and checked separately by ParseTopicRef.
func ValidTopic(t string) error {
	if !topicRe.MatchString(t) {
		return fmt.Errorf("invalid topic %q: use lowercase letters, digits, dot, dash or underscore (max 64)", t)
	}
	return nil
}

// TopicRef is a topic name plus which tree its log lives in. Everywhere a topic
// is accepted -- CLI, MCP, project config -- a leading "@" selects the global
// one, and this is the single place that decision is made.
type TopicRef struct {
	Name   string
	Global bool
}

// ParseTopicRef validates a topic as typed.
func ParseTopicRef(s string) (TopicRef, error) {
	s = strings.TrimSpace(s)
	ref := TopicRef{Name: s}
	if rest, ok := strings.CutPrefix(s, GlobalPrefix); ok {
		ref = TopicRef{Name: rest, Global: true}
	}
	if err := ValidTopic(ref.Name); err != nil {
		return TopicRef{}, err
	}
	return ref, nil
}

// String is the form a user types, the form stored in a subscription list, and
// the key a cursor is filed under -- so `deploys` and `@deploys` keep separate
// read positions, as two different logs must.
func (r TopicRef) String() string {
	if r.Global {
		return GlobalPrefix + r.Name
	}
	return r.Name
}

// TopicLabel is how a topic is written in output: "#deploys" for one that
// resolves inside a namespace, "@ops" for one the whole machine shares. It
// takes the string rather than a TopicRef because every caller is echoing back
// a value some earlier call already validated.
func TopicLabel(topic string) string {
	if strings.HasPrefix(topic, GlobalPrefix) {
		return topic
	}
	return "#" + topic
}

func (r TopicRef) path(n Namespace) string {
	if r.Global {
		return filepath.Join(SharedTopicsDir(), r.Name+".ndjson")
	}
	return filepath.Join(n.TopicsDir(), r.Name+".ndjson")
}

// payloadsDir is where an overflowing message on this topic spills its body.
// A global topic spills to the shared tree, because a recipient in another
// namespace has to be able to read it -- and Render only follows a pointer
// into a directory it already knows.
func (r TopicRef) payloadsDir(n Namespace) string {
	if r.Global {
		return SharedPayloadsDir()
	}
	return n.PayloadsDir()
}

// lockPath guards the log while it is being rewritten, so an append cannot
// land in the middle of a compaction. A global topic's lock lives in the shared
// tree: two namespaces holding their own locks over one log is no lock at all.
func (r TopicRef) lockPath(n Namespace) string {
	if r.Global {
		return filepath.Join(sharedLocksDir(), "topic-"+r.Name+".lock")
	}
	return filepath.Join(n.LocksDir(), "topic-"+r.Name+".lock")
}

// TopicPath is the log a topic reference names, or "" when the reference is not
// a usable topic at all. Callers validate first; returning "" keeps a bad name
// from steering a path join if one ever does not.
func (n Namespace) TopicPath(topic string) string {
	ref, err := ParseTopicRef(topic)
	if err != nil {
		return ""
	}
	return ref.path(n)
}

func (n Namespace) cursorPath(sessionID string) string {
	return filepath.Join(n.CursorsDir(), sessionID+".json")
}

// Publish appends a message to a topic log. Every subscriber's monitor picks
// it up independently; there is no fan-out at write time, which keeps
// publishing O(1) regardless of how many sessions are listening.
func Publish(topic string, d Draft, from Sender) (*Message, error) {
	return CurrentNamespace().Publish(topic, d, from)
}

func (n Namespace) Publish(topic string, d Draft, from Sender) (*Message, error) {
	ref, err := ParseTopicRef(topic)
	if err != nil {
		return nil, err
	}
	// A private namespace is sealed against machine-wide topics in both
	// directions. Blocking only what flows in would be a privacy hole rather
	// than a feature: a namespace that can still broadcast to @all publishes
	// exactly what it was made private to keep in.
	if ref.Global && n.IsPrivate() {
		return nil, fmt.Errorf("namespace %q is private, so it cannot publish to the machine-wide topic %s", n, ref)
	}
	if err := n.EnsureDirs(); err != nil {
		return nil, err
	}
	body := Sanitize(d.Text)
	if body == "" {
		return nil, fmt.Errorf("refusing to publish an empty message")
	}
	subject, err := validateSubject(d.Subject)
	if err != nil {
		return nil, err
	}
	brief, err := validateBrief(d.Brief)
	if err != nil {
		return nil, err
	}
	if err := validatePriority(d.Priority); err != nil {
		return nil, err
	}

	msg := &Message{
		ID:       newMessageID(),
		TS:       nowRFC3339(),
		From:     from,
		Topic:    ref.String(),
		Text:     body,
		Subject:  subject,
		Brief:    brief,
		Priority: d.Priority,
	}
	if len([]rune(body)) > BodyBudget {
		p := filepath.Join(ref.payloadsDir(n), msg.ID+".txt")
		if err := os.WriteFile(p, []byte(d.Text), 0o600); err == nil {
			msg.Payload = p
		}
	}

	line, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	// Hold the topic lock so an append cannot land mid-compaction.
	unlock, err := blockingExclusive(ref.lockPath(n))
	if err != nil {
		return nil, err
	}
	defer unlock.Close()

	f, err := os.OpenFile(ref.path(n), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open topic: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return nil, fmt.Errorf("write topic: %w", err)
	}
	return msg, nil
}

// Subscribe adds a topic to a session's subscription list. The running monitor
// notices within about a second and starts following it -- no restart.
func Subscribe(sessionID, topic string) error {
	return CurrentNamespace().Subscribe(sessionID, topic)
}

func (n Namespace) Subscribe(sessionID, topic string) error {
	ref, err := ParseTopicRef(topic)
	if err != nil {
		return err
	}
	// Start at the end so subscribing does not replay the topic's history
	// into the session as a burst of notifications.
	if err := n.seedCursor(sessionID, ref); err != nil {
		return err
	}
	return n.MutateEntry(sessionID, func(e *Entry) error {
		for _, t := range e.Subscriptions {
			if t == ref.String() {
				return nil
			}
		}
		e.Subscriptions = append(e.Subscriptions, ref.String())
		sort.Strings(e.Subscriptions)
		return nil
	})
}

func Unsubscribe(sessionID, topic string) error {
	return CurrentNamespace().Unsubscribe(sessionID, topic)
}

func (n Namespace) Unsubscribe(sessionID, topic string) error {
	// Unsubscribing is not validated the way subscribing is: whatever is in the
	// list has to be removable, including something a hand-edited entry put
	// there.
	want := strings.TrimSpace(topic)
	return n.MutateEntry(sessionID, func(e *Entry) error {
		out := e.Subscriptions[:0]
		for _, t := range e.Subscriptions {
			if t != want {
				out = append(out, t)
			}
		}
		e.Subscriptions = out
		return nil
	})
}

// --- log bases -------------------------------------------------------------
//
// A cursor is a LOGICAL offset: bytes since the beginning of a log's life,
// counting everything compaction has since thrown away. The base file records
// how much that is, so physical position = logical - base.
//
// Storing raw file positions instead, and rewinding every subscriber's cursor
// when the log is compacted, cannot be made correct. Compaction and the
// followers then both mutate the same numbers, and those numbers change
// meaning halfway through the operation: a follower that reloaded between the
// rename and its own rewind adopted an un-rewound cursor pointing at the end
// of the compacted file and skipped everything in it, while one whose write
// landed after the rewind held an old-coordinate offset larger than the new
// file, which reads as truncation and replays the whole log. Both were
// reproducible; both are message loss or a duplicate flood in a system whose
// entire job is delivery.
//
// With logical offsets, compaction touches no cursor at all. It adds to the
// base, every stored offset keeps its meaning, and there is nothing left for a
// follower to race with.

func basePath(logPath string) string {
	return strings.TrimSuffix(logPath, ".ndjson") + ".base"
}

// readBase reports how many bytes have been compacted away from a log. A
// missing base file means none have, which is also the right answer for the
// direct inbox spool, which is never compacted.
func readBase(logPath string) int64 {
	b, err := os.ReadFile(basePath(logPath))
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// writeBase records the new base after a compaction. Callers hold the topic
// lock, so this does not serialise against another compaction; the temp file
// is for readers, who must never observe a half-written number.
func writeBase(logPath string, n int64) error {
	tmp, err := os.CreateTemp(filepath.Dir(logPath), "base-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.WriteString(strconv.FormatInt(n, 10)); err != nil {
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
	return os.Rename(name, basePath(logPath))
}

// --- cursors ---------------------------------------------------------------
//
// Each session keeps its own read offset per topic, so a shared append-only
// log serves every subscriber without any of them consuming from the others.

func (n Namespace) readCursors(sessionID string) map[string]int64 {
	m := map[string]int64{}
	b, err := os.ReadFile(n.cursorPath(sessionID))
	if err != nil {
		return m
	}
	_ = json.Unmarshal(b, &m)
	return m
}

func (n Namespace) writeCursors(sessionID string, m map[string]int64) error {
	if err := n.EnsureDirs(); err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	// A unique temp file: concurrent writers sharing one fixed ".tmp" name
	// race, and the loser's rename fails because its file is already gone.
	tmp, err := os.CreateTemp(n.CursorsDir(), "cursor-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
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
	return os.Rename(name, n.cursorPath(sessionID))
}

// mutateCursors serialises read-modify-write on the cursor map, which is
// otherwise last-writer-wins between the monitor's followers and any CLI or
// MCP call that subscribes.
func (n Namespace) mutateCursors(sessionID string, fn func(map[string]int64)) error {
	unlock, err := n.lockSession(sessionID)
	if err != nil {
		return err
	}
	defer unlock()
	m := n.readCursors(sessionID)
	fn(m)
	return n.writeCursors(sessionID, m)
}

// seedCursor starts a new subscription at the end of the log, so joining a
// topic does not replay its history as a burst of notifications. The offset is
// logical, so it stays correct across every later compaction.
func (n Namespace) seedCursor(sessionID string, ref TopicRef) error {
	path := ref.path(n)
	var size int64
	if fi, err := os.Stat(path); err == nil {
		size = fi.Size()
	}
	end := readBase(path) + size
	return n.mutateCursors(sessionID, func(m map[string]int64) {
		// Only ever seed. Subscribe is not guarded against being called for a
		// topic this session already follows, and re-seeding to the end there
		// would skip whatever arrived since -- silently, and for a deaf session
		// that means dropping mail already queued on the log.
		if _, seen := m[ref.String()]; !seen {
			m[ref.String()] = end
		}
		// Seed the consumption cursor to the same place, and only here.
		//
		// It cannot be left absent to fall back on the monitor's cursor at read
		// time, because the monitor advances its own within about 200ms of a
		// message landing -- long before any session gets round to asking for
		// it. A consumption cursor that chased it would find everything already
		// behind it and ReadInbox would answer "nothing unread" forever, which
		// is the whole feature rendered inert.
		//
		// Seeded together and then left alone, the two say different true
		// things: the monitor's is how far notifications have got, and this one
		// is how far the session has actually read.
		// readat is deliberately NOT set here. It is written only when
		// ReadInbox actually advances this cursor, so its absence means "this
		// session has never pulled" -- and compaction treats a never-pulled
		// cursor as abandoned rather than letting every session that only ever
		// takes notifications hold its topic logs open for the staleness
		// window. Pulling once opts a session into that protection; never
		// pulling costs the fleet nothing.
		if _, seen := m[readCursorKey(ref.String())]; !seen {
			m[readCursorKey(ref.String())] = end
		}
	})
}

// ListTopics reports every topic reachable from this namespace -- its own logs
// and the shared ones, plus anything a live session subscribes to -- with its
// subscriber count.
//
// A global topic is counted across every namespace, because that is what
// "@ops" means: the number a publisher wants is how many sessions will hear
// them, not how many happen to be next to them.
func ListTopics() ([]TopicInfo, error) { return CurrentNamespace().ListTopics() }

func (n Namespace) ListTopics() ([]TopicInfo, error) {
	if err := ensureHome(); err != nil {
		return nil, err
	}
	counts := map[string]int{PublicTopic: 0, GlobalPublicTopic: 0}

	paths, _ := filepath.Glob(filepath.Join(n.TopicsDir(), "*.ndjson"))
	for _, p := range paths {
		t := strings.TrimSuffix(filepath.Base(p), ".ndjson")
		if _, ok := counts[t]; !ok {
			counts[t] = 0
		}
	}
	shared, _ := filepath.Glob(filepath.Join(SharedTopicsDir(), "*.ndjson"))
	for _, p := range shared {
		t := GlobalPrefix + strings.TrimSuffix(filepath.Base(p), ".ndjson")
		if _, ok := counts[t]; !ok {
			counts[t] = 0
		}
	}

	local, err := n.ListSessions(false, false)
	if err != nil {
		return nil, err
	}
	for _, e := range local {
		for _, t := range e.Subscriptions {
			if !strings.HasPrefix(t, GlobalPrefix) {
				counts[t]++
			}
		}
	}
	// Global topics are counted machine-wide, including this namespace.
	for _, e := range allSessions() {
		for _, t := range e.Subscriptions {
			if strings.HasPrefix(t, GlobalPrefix) {
				counts[t]++
			}
		}
	}

	out := make([]TopicInfo, 0, len(counts))
	for t, num := range counts {
		out = append(out, TopicInfo{
			Name:        t,
			Subscribers: num,
			Global:      strings.HasPrefix(t, GlobalPrefix),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// allSessions is every live session on the machine. Used only where a global
// topic makes the whole machine the right denominator.
// allSessions is the audience for a global topic: every session on the machine
// except those in a namespace sealed against them.
//
// Filtering here rather than at each call site is deliberate. This is the one
// function that crosses namespaces on purpose, so it is the one place the
// exception to that has to hold.
func allSessions() []*Entry {
	out, err := ListAllSessions(false, false)
	if err != nil {
		return nil
	}
	kept := out[:0]
	for _, e := range out {
		if e.NS().IsPrivate() {
			continue
		}
		kept = append(kept, e)
	}
	return kept
}

// matchingSubscribers returns every entry, besides exceptSessionID, that
// subscribes to ref. It is the one place that walks the subscriber list, so
// SubscriberCount and SubscriberBreakdown cannot drift on what counts as
// "subscribed" -- only on what they do with the entries once found.
func (n Namespace) matchingSubscribers(ref TopicRef, exceptSessionID string) []*Entry {
	entries := []*Entry{}
	if ref.Global {
		entries = allSessions()
	} else if got, err := n.ListSessions(false, false); err == nil {
		entries = got
	}
	var matched []*Entry
	for _, e := range entries {
		if e.SessionID == exceptSessionID {
			continue
		}
		for _, t := range e.Subscriptions {
			if t == ref.String() {
				matched = append(matched, e)
				break
			}
		}
	}
	return matched
}

// SubscriberCount reports how many live sessions besides exceptSessionID would
// receive a publish to this topic. A deaf one does not count here: its spool
// still gets the message, but nothing is reading that spool, so it is not part
// of the number a publisher checks to decide whether a topic is worth writing
// to right now.
func (n Namespace) SubscriberCount(topic, exceptSessionID string) int {
	ref, err := ParseTopicRef(topic)
	if err != nil {
		return 0
	}
	count := 0
	for _, e := range n.matchingSubscribers(ref, exceptSessionID) {
		if e.Status == StatusLive {
			count++
		}
	}
	return count
}

// SubscriberBreakdown counts a topic's subscribers by whether they can
// actually receive. A deaf session is not gone: its spool keeps every message
// published while it stayed deaf, complete and in order. But nothing reads
// that spool until the same session id comes back under `claude --resume`,
// which might be in a minute or might be never -- so folding deaf in with live
// tells a publisher "reached" about a session that has not actually seen
// anything yet. Separating the two lets a claim or a question be judged by who
// is really listening, not by who merely subscribed at some point.
func (n Namespace) SubscriberBreakdown(topic, exceptSessionID string) (live, deaf int) {
	ref, err := ParseTopicRef(topic)
	if err != nil {
		return 0, 0
	}
	for _, e := range n.matchingSubscribers(ref, exceptSessionID) {
		switch e.Status {
		case StatusLive:
			live++
		case StatusDeaf:
			deaf++
		}
	}
	return live, deaf
}

// TopicInfo is one row of `pigeon topics`.
type TopicInfo struct {
	Name        string `json:"name"`
	Subscribers int    `json:"subscribers"`
	// Global marks a topic that lives outside every namespace, so a listing can
	// say which rows are shared without re-parsing the name.
	Global bool `json:"global,omitempty"`
}

// --- retention -------------------------------------------------------------

// minCompactBytes is the smallest saving worth rewriting a file for. Below it
// the churn -- and the offset shuffle every reader has to absorb -- costs more
// than the disk it reclaims.
const minCompactBytes = 64 * 1024

// PruneResult reports what a retention pass reclaimed.
type PruneResult struct {
	TopicsRemoved   int
	TopicsCompacted int
	PayloadsRemoved int
	BytesReclaimed  int64
	// AbandonedCursors counts subscribers whose consumption cursor
	// (read:<topic>, advanced by ReadInbox) was present but stale enough --
	// by byte distance or by age -- that a compaction pass fell back to their
	// monitor cursor instead of waiting on it forever. pruneTopicDir has no
	// logger to report through, so this count is that report: a caller that
	// wants to know who was abandoned can compare a session's own cursors
	// before and after.
	AbandonedCursors int
}

// Add folds one pass into another, so a caller sweeping several namespaces
// reports one total.
func (r *PruneResult) Add(o PruneResult) {
	r.TopicsRemoved += o.TopicsRemoved
	r.TopicsCompacted += o.TopicsCompacted
	r.PayloadsRemoved += o.PayloadsRemoved
	r.BytesReclaimed += o.BytesReclaimed
	r.AbandonedCursors += o.AbandonedCursors
}

// PruneTopics reclaims space in this namespace's topic logs.
//
// A topic log is append-only and every subscriber reads it at its own offset,
// so the prefix that every live subscriber has already passed is dead weight.
// This drops that prefix and rewinds each subscriber's cursor by the same
// amount. A topic nobody subscribes to is removed outright.
//
// Call it after dead sessions have been pruned: a dead session's stale cursor
// would otherwise pin the whole history.
func PruneTopics() (PruneResult, error) { return CurrentNamespace().PruneTopics() }

func (n Namespace) PruneTopics() (PruneResult, error) {
	var res PruneResult
	if err := ensureHome(); err != nil {
		return res, err
	}
	sessions, err := n.ListSessions(false, false)
	if err != nil {
		return res, err
	}
	res, err = pruneTopicDir(n.TopicsDir(), false, func(ref TopicRef) []*Entry {
		return subscribersOf(sessions, ref)
	}, func(e *Entry) Namespace { return n }, n)
	if err != nil {
		return res, err
	}
	num, bytes := n.reclaimPayloads()
	res.PayloadsRemoved += num
	res.BytesReclaimed += bytes
	return res, nil
}

// PruneSharedTopics reclaims space in the global topic logs.
//
// It is deliberately not a per-namespace pass: a shared log's subscribers are
// spread across every namespace, and cutting a prefix one of them has not read
// yet would silently drop that session's mail. Counting machine-wide is the
// only version of this that is safe, so it is the only one there is.
func PruneSharedTopics() (PruneResult, error) {
	var res PruneResult
	if err := ensureHome(); err != nil {
		return res, err
	}
	everyone := allSessions()
	res, err := pruneTopicDir(SharedTopicsDir(), true, func(ref TopicRef) []*Entry {
		return subscribersOf(everyone, ref)
	}, func(e *Entry) Namespace { return e.NS() }, CurrentNamespace())
	if err != nil {
		return res, err
	}
	// A shared payload is referenced from a shared topic log, so the reference
	// set is machine-wide like the pass that produced it.
	refs := map[string]bool{}
	paths, _ := filepath.Glob(filepath.Join(SharedTopicsDir(), "*.ndjson"))
	for _, p := range paths {
		collectPayloadRefs(p, refs)
	}
	num, bytes := reclaimPayloadsIn(SharedPayloadsDir(), refs)
	res.PayloadsRemoved += num
	res.BytesReclaimed += bytes
	return res, nil
}

func subscribersOf(entries []*Entry, ref TopicRef) []*Entry {
	var subs []*Entry
	for _, e := range entries {
		for _, t := range e.Subscriptions {
			if t == ref.String() {
				subs = append(subs, e)
				break
			}
		}
	}
	return subs
}

// pruneTopicDir is the retention pass over one directory of logs. It is shared
// between namespaced and global topics because the rule is identical; only who
// counts as a subscriber, and whose cursor file to rewind, differ.
func pruneTopicDir(dir string, global bool, subscribers func(TopicRef) []*Entry,
	cursorNS func(*Entry) Namespace, lockNS Namespace) (PruneResult, error) {

	var res PruneResult
	paths, err := filepath.Glob(filepath.Join(dir, "*.ndjson"))
	if err != nil {
		return res, err
	}

	for _, p := range paths {
		name := strings.TrimSuffix(filepath.Base(p), ".ndjson")
		if ValidTopic(name) != nil {
			continue
		}
		ref := TopicRef{Name: name, Global: global}
		subs := subscribers(ref)

		unlock, err := blockingExclusive(ref.lockPath(lockNS))
		if err != nil {
			continue
		}

		fi, err := os.Stat(p)
		if err != nil {
			unlock.Close()
			continue
		}

		// Nobody is listening, so nothing in the file can still be wanted. The
		// base goes with it: a log that comes back later starts a fresh life,
		// and a stale base would put every new subscriber past the end of it.
		if len(subs) == 0 {
			if err := os.Remove(p); err == nil {
				_ = os.Remove(basePath(p))
				res.TopicsRemoved++
				res.BytesReclaimed += fi.Size()
			}
			unlock.Close()
			continue
		}

		// Cursors are logical, so convert once here and work in file
		// coordinates for the cut itself.
		base := readBase(p)
		logicalEnd := base + fi.Size()
		slowest := logicalEnd
		now := time.Now()
		for _, e := range subs {
			cursors := cursorNS(e).readCursors(e.SessionID)
			off, state := effectiveOffset(cursors, ref.String(), now)
			if state == cursorStale {
				res.AbandonedCursors++
			}
			if off < slowest {
				slowest = off
			}
		}
		cut := slowest - base
		if cut < minCompactBytes {
			unlock.Close()
			continue
		}

		if err := compactFrom(p, cut); err != nil {
			unlock.Close()
			continue
		}
		// No cursor is touched. Every subscriber's logical offset already
		// means the same message it did before, which is the whole point of
		// storing them that way: there is no window in which a follower can
		// see a compacted file and an uncompacted cursor, or the reverse.
		//
		// Write the base after the rename, never before. A follower that looks
		// in between computes a physical offset past the end of the new file
		// and simply waits for the next poll, which costs a fraction of a
		// second; the opposite order would leave the base claiming bytes were
		// cut that a failed rename had left in place.
		if err := writeBase(p, base+cut); err != nil {
			unlock.Close()
			continue
		}
		res.TopicsCompacted++
		res.BytesReclaimed += cut
		unlock.Close()
	}
	return res, nil
}

// compactFrom rewrites path keeping only the bytes from off onward, via a temp
// file and a rename so a reader never observes a half-written log.
func compactFrom(path string, off int64) error {
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()
	if _, err := src.Seek(off, io.SeekStart); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "compact-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := io.Copy(tmp, src); err != nil {
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
	return os.Rename(name, path)
}
