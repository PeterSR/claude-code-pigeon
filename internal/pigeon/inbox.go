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
func (n Namespace) ReadInbox(sessionID string, q InboxQuery) ([]InboxItem, error) {
	if err := ValidSessionID(sessionID); err != nil {
		return nil, err
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
		return nil, fmt.Errorf("session %s is not registered in namespace %q", Short(sessionID), n)
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
			return nil, err
		}
		filtered := sources[:0]
		for _, s := range sources {
			if s.key == ref.String() {
				filtered = append(filtered, s)
			}
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
			return nil, err
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
	kept := all
	if len(kept) > limit {
		kept = kept[len(kept)-limit:]
	}

	// Per source, the earliest message the Limit cut threw away. Everything at
	// or past it is unread no matter what else was returned.
	//
	// This is the whole reason the cursor cannot simply advance to the newest
	// message a source contributed. The cut is taken over messages merged from
	// every source and ordered by time, so it can land in the MIDDLE of one
	// source's run: source A writes at 10:00 and 10:05, source B writes four
	// times in between, Limit is 3, and the batch returned is B, B, A@10:05.
	// Advancing A past 10:05 would step over A@10:00, which nobody has seen and
	// nothing will show again -- the silent loss this pull path exists to end,
	// reintroduced by the pull path itself.
	firstDropped := map[string]int64{}
	if len(kept) < len(all) {
		cutoff := len(all) - len(kept)
		for _, c := range all[:cutoff] {
			if cur, ok := firstDropped[c.srcKey]; !ok || c.end < cur {
				firstDropped[c.srcKey] = c.end
			}
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

	if q.MarkRead && len(advance) > 0 {
		nowUnix := now.Unix()
		if err := n.mutateCursors(sessionID, func(m map[string]int64) {
			for key, off := range advance {
				m[readCursorKey(key)] = off
				m[readAtCursorKey(key)] = nowUnix
			}
		}); err != nil {
			return nil, err
		}
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
func readSourceFrom(path string, offset int64) ([]positionedMessage, error) {
	fi, err := os.Stat(path)
	if err != nil {
		// No log yet -- a topic never published to, or the direct spool
		// before a first message. Nothing to read, not an error.
		return nil, nil
	}
	base := readBase(path)
	physical := offset - base
	if physical < 0 || physical > fi.Size() {
		physical = 0
		offset = base
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, nil
	}
	defer f.Close()
	if physical > 0 {
		if _, err := f.Seek(physical, io.SeekStart); err != nil {
			return nil, err
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
	return out, nil
}
