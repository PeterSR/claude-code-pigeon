package pigeon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// PublicTopic is the mailbox every session joins by default, so a broadcast
// reaches the whole machine without anyone configuring anything.
const PublicTopic = "all"

var topicRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// inboxCursorKey tracks how far the direct spool has been read. It is not a
// valid topic name, so it can never collide with one.
const inboxCursorKey = ":inbox"

// ValidTopic keeps topic names safe as filenames and readable in a
// notification line.
func ValidTopic(t string) error {
	if !topicRe.MatchString(t) {
		return fmt.Errorf("invalid topic %q: use lowercase letters, digits, dot, dash or underscore (max 64)", t)
	}
	return nil
}

func TopicsDir() string  { return filepath.Join(Home(), "topics") }
func CursorsDir() string { return filepath.Join(Home(), "cursors") }

func TopicPath(topic string) string {
	return filepath.Join(TopicsDir(), topic+".ndjson")
}

func cursorPath(sessionID string) string {
	return filepath.Join(CursorsDir(), sessionID+".json")
}

// Publish appends a message to a topic log. Every subscriber's monitor picks
// it up independently; there is no fan-out at write time, which keeps
// publishing O(1) regardless of how many sessions are listening.
func Publish(topic, text string, from Sender) (*Message, error) {
	if err := ValidTopic(topic); err != nil {
		return nil, err
	}
	if err := EnsureDirs(); err != nil {
		return nil, err
	}
	body := Sanitize(text)
	if body == "" {
		return nil, fmt.Errorf("refusing to publish an empty message")
	}

	msg := &Message{
		ID:    newMessageID(),
		TS:    nowRFC3339(),
		From:  from,
		Topic: topic,
		Text:  body,
	}
	if len([]rune(body)) > BodyBudget {
		p := filepath.Join(PayloadsDir(), msg.ID+".txt")
		if err := os.WriteFile(p, []byte(text), 0o600); err == nil {
			msg.Payload = p
		}
	}

	line, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(TopicPath(topic), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
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
	if err := ValidTopic(topic); err != nil {
		return err
	}
	// Start at the end so subscribing does not replay the topic's history
	// into the session as a burst of notifications.
	if err := seedCursor(sessionID, topic); err != nil {
		return err
	}
	return MutateEntry(sessionID, func(e *Entry) error {
		for _, t := range e.Subscriptions {
			if t == topic {
				return nil
			}
		}
		e.Subscriptions = append(e.Subscriptions, topic)
		sort.Strings(e.Subscriptions)
		return nil
	})
}

func Unsubscribe(sessionID, topic string) error {
	return MutateEntry(sessionID, func(e *Entry) error {
		out := e.Subscriptions[:0]
		for _, t := range e.Subscriptions {
			if t != topic {
				out = append(out, t)
			}
		}
		e.Subscriptions = out
		return nil
	})
}

// --- cursors ---------------------------------------------------------------
//
// Each session keeps its own read offset per topic, so a shared append-only
// log serves every subscriber without any of them consuming from the others.

func readCursors(sessionID string) map[string]int64 {
	m := map[string]int64{}
	b, err := os.ReadFile(cursorPath(sessionID))
	if err != nil {
		return m
	}
	_ = json.Unmarshal(b, &m)
	return m
}

func writeCursors(sessionID string, m map[string]int64) error {
	if err := EnsureDirs(); err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	// A unique temp file: concurrent writers sharing one fixed ".tmp" name
	// race, and the loser's rename fails because its file is already gone.
	tmp, err := os.CreateTemp(CursorsDir(), "cursor-*.tmp")
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
	return os.Rename(name, cursorPath(sessionID))
}

// mutateCursors serialises read-modify-write on the cursor map, which is
// otherwise last-writer-wins between the monitor's followers and any CLI or
// MCP call that subscribes.
func mutateCursors(sessionID string, fn func(map[string]int64)) error {
	unlock, err := lockSession(sessionID)
	if err != nil {
		return err
	}
	defer unlock()
	m := readCursors(sessionID)
	fn(m)
	return writeCursors(sessionID, m)
}

func seedCursor(sessionID, topic string) error {
	return mutateCursors(sessionID, func(m map[string]int64) {
		var size int64
		if fi, err := os.Stat(TopicPath(topic)); err == nil {
			size = fi.Size()
		}
		m[topic] = size
	})
}

// ListTopics reports every topic that exists on disk or that some live session
// subscribes to, with its subscriber count.
func ListTopics() ([]TopicInfo, error) {
	if err := EnsureDirs(); err != nil {
		return nil, err
	}
	counts := map[string]int{PublicTopic: 0}

	paths, _ := filepath.Glob(filepath.Join(TopicsDir(), "*.ndjson"))
	for _, p := range paths {
		t := strings.TrimSuffix(filepath.Base(p), ".ndjson")
		if _, ok := counts[t]; !ok {
			counts[t] = 0
		}
	}
	entries, err := ListSessions(false, false)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		for _, t := range e.Subscriptions {
			counts[t]++
		}
	}

	out := make([]TopicInfo, 0, len(counts))
	for t, n := range counts {
		out = append(out, TopicInfo{Name: t, Subscribers: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// TopicInfo is one row of `pigeon topics`.
type TopicInfo struct {
	Name        string `json:"name"`
	Subscribers int    `json:"subscribers"`
}
