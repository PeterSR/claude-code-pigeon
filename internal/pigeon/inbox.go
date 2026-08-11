package pigeon

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// InboxItem is one message as the pulling session sees it -- the pull-path
// counterpart to the single clipped line a monitor prints. Nothing here is
// truncated: a pull is a Read, not a push bound by a notification budget.
type InboxItem struct {
	Message *Message
	// Source is "" for the direct spool, or the subscribed topic the message
	// arrived on -- the same string a monitor cursor is filed under.
	Source string
	Age    time.Duration
}

// InboxQuery controls one ReadInbox call.
type InboxQuery struct {
	// Limit caps how many messages come back. 0 (or negative) means the
	// default of 10; a value above the hard max of 50 is clamped rather than
	// rejected.
	Limit int
	// UnreadOnly restricts the read to what has not already been pulled, using
	// the consumption cursor (falling back to the monitor cursor -- see
	// ReadInbox). Without it, ReadInbox answers "what are my last N messages"
	// regardless of what has already been consumed.
	UnreadOnly bool
	// Topic restricts the read to one subscribed source. Empty means every
	// source: the direct spool plus every subscription.
	Topic string
	// MarkRead advances the consumption cursor over exactly what was
	// returned, so a later UnreadOnly call does not see it again.
	MarkRead bool
}

const (
	defaultInboxLimit = 10
	maxInboxLimit     = 50
)

// ReadInbox is the pull path: a session asks for its mail and gets full text
// back, rather than a monitor notification clipped to BodyBudget with a
// payload pointer for whatever did not fit.
//
// It reads and writes only the consumption cursor family (read:<source> /
// readat:<source>); it never touches the monitor's own ingest cursors
// (:inbox, or the bare topic name), and the monitor never touches these.
// Conflating the two families would make a pull silently suppress a
// notification, or a notification silently mark mail as pulled -- see the
// cursor-family comment in topics.go.
func (n Namespace) ReadInbox(sessionID string, q InboxQuery) ([]InboxItem, int, error) {
	if err := ValidSessionID(sessionID); err != nil {
		return nil, 0, err
	}
	limit := q.Limit
	if limit <= 0 {
		limit = defaultInboxLimit
	}
	if limit > maxInboxLimit {
		limit = maxInboxLimit
	}

	e, err := n.ReadEntry(sessionID)
	if err != nil {
		return nil, 0, fmt.Errorf("session %s is not registered in namespace %q", Short(sessionID), n)
	}

	type source struct {
		// key is the cursor-family key this source's monitor and consumption
		// cursors are filed under: inboxCursorKey for the direct spool, or a
		// TopicRef's String() for a topic.
		key  string
		path string
		// extSource is what InboxItem.Source reports.
		extSource string
	}
	sources := []source{{key: inboxCursorKey, path: n.SpoolPath(sessionID), extSource: ""}}
	for _, t := range e.Subscriptions {
		ref, err := ParseTopicRef(t)
		if err != nil {
			// The entry is a file on disk and can hold anything; an
			// unparseable subscription is skipped rather than followed
			// blind, the same rule manageSubscriptions applies.
			continue
		}
		if ref.Global && n.IsPrivate() {
			// A private namespace is sealed against machine-wide topics
			// however they got into the entry -- a security rule, not a
			// convenience, copied from manageSubscriptions (monitor.go).
			continue
		}
		sources = append(sources, source{key: ref.String(), path: ref.path(n), extSource: ref.String()})
	}

	if q.Topic != "" {
		ref, err := ParseTopicRef(q.Topic)
		if err != nil {
			return nil, 0, err
		}
		filtered := sources[:0]
		for _, s := range sources {
			if s.key == ref.String() {
				filtered = append(filtered, s)
			}
		}
		if len(filtered) == 0 {
			// Reading back "No unread messages" for a topic this session does
			// not follow is indistinguishable from the topic being quiet, and
			// the caller would believe it had checked.
			return nil, 0, fmt.Errorf("not subscribed to %s in namespace %q", TopicLabel(ref.String()), n)
		}
		sources = filtered
	}

	cursors := n.readCursors(sessionID)

	// candidate carries the offset a returned message ends at, alongside
	// which source it came from, so a later Limit truncation can still work
	// out -- per source -- exactly how far that source's cursor may advance.
	type candidate struct {
		item   InboxItem
		srcKey string
		end    int64
	}
	var all []candidate

	for _, s := range sources {
		var start int64
		if q.UnreadOnly {
			if off, ok := cursors[readCursorKey(s.key)]; ok {
				start = off
			} else if off, ok := cursors[s.key]; ok {
				// No consumption cursor yet: fall back to the monitor cursor,
				// not to zero. A session that has been running for hours must
				// not have its whole history dumped into context the first
				// time it pulls.
				start = off
			}
			// Neither present: start stays 0, which is correct for a source
			// that has never been followed at all -- there is no
			// "already notified" position to protect.
		}
		msgs, err := readSourceFrom(s.path, start)
		if err != nil {
			return nil, 0, err
		}
		for _, pm := range msgs {
			if pm.msg.From.SessionID != "" && pm.msg.From.SessionID == sessionID {
				// A session never pulls its own broadcast, matching
				// RunMonitor's rule for pushed notifications (monitor.go).
				continue
			}
			all = append(all, candidate{
				item:   InboxItem{Message: pm.msg, Source: s.extSource},
				srcKey: s.key,
				end:    pm.end,
			})
		}
	}

	// TS is produced by nowRFC3339(), always UTC RFC3339, so lexical order on
	// the string is chronological order -- no parse needed just to sort.
	sort.SliceStable(all, func(i, j int) bool {
		return all[i].item.Message.TS < all[j].item.Message.TS
	})
	// Which end of the batch to keep depends on what the caller is doing, and
	// getting it backwards wedges the whole pull path.
	//
	// An unread pull is DRAINING: it must take the OLDEST unread messages, so
	// successive pulls walk forward and the backlog empties in order. Taking the
	// newest instead looks reasonable and deadlocks -- the dropped messages and
	// the kept ones come from the same source, so every kept message sits behind
	// a gap, the cursor may not advance past any of them, and the next pull
	// returns exactly the same batch. A backlog larger than Limit would then
	// redeliver its tail forever while its head became permanently unreachable.
	//
	// A browse (UnreadOnly false) is the opposite: "show me what has been going
	// on" wants the most RECENT messages.
	more := 0
	kept, dropped := all, []candidate(nil)
	if len(kept) > limit {
		more = len(kept) - limit
		if q.UnreadOnly {
			kept, dropped = all[:limit], all[limit:]
		} else {
			kept, dropped = all[len(all)-limit:], all[:len(all)-limit]
		}
	}

	firstDropped := map[string]int64{}
	for _, c := range dropped {
		if cur, ok := firstDropped[c.srcKey]; !ok || c.end < cur {
			firstDropped[c.srcKey] = c.end
		}
	}

	now := time.Now()
	items := make([]InboxItem, 0, len(kept))
	// advance records, per source, the offset just past the newest message that
	// source contributed AND that no older unreturned message of its own sits
	// behind. A source contributing nothing is left out entirely, so its cursor
	// is untouched.
	advance := map[string]int64{}
	for _, c := range kept {
		it := c.item
		if t, terr := time.Parse(time.RFC3339, it.Message.TS); terr == nil {
			it.Age = now.Sub(t)
		}
		items = append(items, it)
		if drop, ok := firstDropped[c.srcKey]; ok && c.end > drop {
			// Behind a gap in this source. Leave the cursor where the gap
			// starts so the next pull picks the run up whole.
			continue
		}
		if cur, ok := advance[c.srcKey]; !ok || c.end > cur {
			advance[c.srcKey] = c.end
		}
	}

	// A browse never consumes. Marking read on a "show me recent history" call
	// would make the same flag mean different things depending on how much
	// history happened to exist, and would let a glance at the log silently
	// discard unread mail.
	if q.MarkRead && q.UnreadOnly && len(advance) > 0 {
		nowUnix := now.Unix()
		if err := n.mutateCursors(sessionID, func(m map[string]int64) {
			for key, off := range advance {
				// Never backwards. Two pulls overlapping in time can each
				// compute a position from the same starting cursor, and the
				// one that finishes last would otherwise rewind the other's
				// progress and redeliver what it had already handed back.
				if off > m[readCursorKey(key)] {
					m[readCursorKey(key)] = off
				}
				m[readAtCursorKey(key)] = nowUnix
			}
		}); err != nil {
			return nil, 0, err
		}
	}

	return items, more, nil
}

// ReadThread reconstructs one conversation end to end from the logs this
// session can see -- its own spool and every topic it subscribes to -- by
// following ReplyTo links out from id in both directions. It is a read, not a
// subscription: no cursor is touched and nothing is marked as read.
//
// Membership is decided by walking ReplyTo directly, not by matching the
// Thread field: Thread is stamped once at send time and, absent a bounded
// parent lookup Send has no way to perform (see its comment), it only ever
// holds the immediate parent's id -- exactly what ReplyTo already holds.
// Walking ReplyTo recovers the whole chain regardless of how many hops it
// runs; matching Thread values alone would not, past the first hop.
func (n Namespace) ReadThread(sessionID, id string) ([]InboxItem, error) {
	if !messageIDRe.MatchString(id) {
		return nil, fmt.Errorf("%q does not look like a message id (want m_ followed by 12 lowercase hex digits)", id)
	}
	e, err := n.ReadEntry(sessionID)
	if err != nil {
		return nil, fmt.Errorf("session %s is not registered in namespace %q", Short(sessionID), n)
	}

	type source struct {
		path      string
		extSource string
	}
	sources := []source{{path: n.SpoolPath(sessionID), extSource: ""}}
	for _, t := range e.Subscriptions {
		ref, err := ParseTopicRef(t)
		if err != nil {
			continue
		}
		if ref.Global && n.IsPrivate() {
			continue
		}
		sources = append(sources, source{path: ref.path(n), extSource: ref.String()})
	}

	byID := map[string]InboxItem{}
	for _, s := range sources {
		msgs, err := readSourceFrom(s.path, 0)
		if err != nil {
			return nil, err
		}
		for _, pm := range msgs {
			// A message lives on exactly one log; the first source to see an id
			// wins, which only ever matters for a hand-written duplicate.
			if _, ok := byID[pm.msg.ID]; !ok {
				byID[pm.msg.ID] = InboxItem{Message: pm.msg, Source: s.extSource}
			}
		}
	}

	if _, ok := byID[id]; !ok {
		return nil, fmt.Errorf("message %s is not in any log this session can see", id)
	}

	// The connected component of the ReplyTo graph containing id: every
	// visible message chained to it, whether by replying to it (directly or
	// transitively) or by being what it replies to.
	children := map[string][]string{}
	for mid, it := range byID {
		if it.Message.ReplyTo != "" {
			children[it.Message.ReplyTo] = append(children[it.Message.ReplyTo], mid)
		}
	}
	seen := map[string]bool{id: true}
	queue := []string{id}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if parent := byID[cur].Message.ReplyTo; parent != "" && !seen[parent] {
			if _, ok := byID[parent]; ok {
				seen[parent] = true
				queue = append(queue, parent)
			}
		}
		for _, child := range children[cur] {
			if !seen[child] {
				seen[child] = true
				queue = append(queue, child)
			}
		}
	}

	// Ordered by the reply chain, parent before child, not by timestamp.
	//
	// TS is RFC3339 at one-second resolution, and a thread is exactly the case
	// where several messages land inside one second -- so sorting on it leaves
	// ties, and the ties used to be broken by Go's randomised map iteration.
	// The same three messages came back in a different order run to run. Reply
	// order is also simply the right order for reading a conversation: it is
	// what the participants meant, and it survives clock skew between the
	// sessions that wrote the lines.
	roots := make([]string, 0, len(seen))
	for mid := range seen {
		parent := byID[mid].Message.ReplyTo
		if parent == "" || !seen[parent] {
			roots = append(roots, mid)
		}
	}
	// Siblings, and multiple roots, fall back to time and then to id, so the
	// result is fully determined whatever order the logs were read in.
	bySiblingOrder := func(ids []string) {
		sort.Slice(ids, func(i, j int) bool {
			a, b := byID[ids[i]].Message, byID[ids[j]].Message
			if a.TS != b.TS {
				return a.TS < b.TS
			}
			return a.ID < b.ID
		})
	}
	bySiblingOrder(roots)

	now := time.Now()
	items := make([]InboxItem, 0, len(seen))
	var walk func(id string)
	walk = func(id string) {
		it := byID[id]
		if t, terr := time.Parse(time.RFC3339, it.Message.TS); terr == nil {
			it.Age = now.Sub(t)
		}
		items = append(items, it)
		kids := append([]string(nil), children[id]...)
		bySiblingOrder(kids)
		for _, child := range kids {
			if seen[child] {
				walk(child)
			}
		}
	}
	for _, r := range roots {
		walk(r)
	}
	return items, nil
}

// positionedMessage pairs a parsed message with the logical offset just past
// it, so a caller that truncates a batch can still work out, per source,
// exactly how far that source's cursor may advance.
type positionedMessage struct {
	msg *Message
	end int64
}

// readSourceFrom reads every complete message in path from a logical offset
// through EOF, in one pass.
//
// It does not seek backwards and does not poll. A pull is a snapshot, not a
// subscription: these logs stay well under the 64 KB compaction threshold, so
// reading the remainder whole costs nothing worth trading for the bug surface
// of a backwards seek over a file another process may be appending to right
// now.
//
// Offsets convert through readBase exactly as followSource does at
// monitor.go:495, because the log may have been compacted since offset was
// recorded. Both edge cases get the same correction followSource eventually
// settles on, just applied immediately rather than after its polling grace
// period -- a one-shot read has no "next pass" to retry on:
//
//   - physical < 0: compaction cut past where offset pointed. Those messages
//     are gone, so resume at the base -- the start of what survives.
//   - physical > size: the file is shorter than offset. For a live follower
//     this is almost always the narrow rename-then-write-base window of a
//     compaction in progress, and resolves itself within a poll or two; a
//     one-shot read has no later poll to resolve it on, so it takes the same
//     resolution followSource reaches once its grace period lapses: treat the
//     position as unrecoverable and start over from the base.
//
// readSourceFrom reads one source from a logical offset to EOF.
//
// Stat, readBase and Open are three separate syscalls with no lock between
// them, and compaction renames the log and then writes the new base. A pull
// that interleaves with that sees one era's base against the other era's file,
// seeks to the wrong place, and returns offsets understated by the size of the
// cut -- which MarkRead would then persist, so the following pull reads as
// "compacted past us", resets to the base, and redelivers the whole surviving
// log. So the base is re-read after the pass and the read is retried once if it
// moved. One retry is enough: compaction holds the topic lock, so two cuts
// cannot both land inside a single retry.
func readSourceFrom(path string, offset int64) ([]positionedMessage, error) {
	for attempt := 0; ; attempt++ {
		out, base, err := readSourceOnce(path, offset)
		if err != nil {
			return nil, err
		}
		if attempt > 0 || readBase(path) == base {
			return out, nil
		}
	}
}

func readSourceOnce(path string, offset int64) ([]positionedMessage, int64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		// No log yet -- a topic never published to, or the direct spool
		// before a first message. Nothing to read, not an error.
		return nil, 0, nil
	}
	base := readBase(path)
	physical := offset - base
	if physical < 0 || physical > fi.Size() {
		physical = 0
		offset = base
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, base, nil
	}
	defer f.Close()
	if physical > 0 {
		if _, err := f.Seek(physical, io.SeekStart); err != nil {
			return nil, base, err
		}
	}

	var out []positionedMessage
	r := bufio.NewReader(f)
	for {
		line, rerr := r.ReadString('\n')
		if rerr != nil {
			// Partial trailing line: leave it for a later read once it is
			// whole, the same as followSource.
			break
		}
		offset += int64(len(line))
		s := strings.TrimSpace(line)
		if s == "" {
			continue
		}
		m, perr := ParseMessage(s)
		if perr != nil {
			// A hand-written or corrupt line: skip it, the same as
			// followSource.
			continue
		}
		out = append(out, positionedMessage{msg: m, end: offset})
	}
	return out, base, nil
}
