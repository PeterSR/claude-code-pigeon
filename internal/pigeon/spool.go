package pigeon

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// BodyBudget caps the message text carried inline in a notification.
//
// Claude Code clips each monitor notification at ~512 characters total; others
// measured the boundary byte-by-byte (462B of payload + a 49B prefix arrives
// whole, one more byte appends "...(truncated)"). Everything past this budget
// goes to a payload file and the notification carries a pointer instead.
// Send a pointer, not a payload.
const BodyBudget = 300

// RenderBudget is a hard ceiling on the whole notification line, decorations
// included. Claude Code clips at ~512 characters, and a clipped line loses its
// trailing payload pointer -- which is exactly the part the recipient needs in
// order to read an overflowing message.
const RenderBudget = 460

// Sender identifies who sent a message. Stamped automatically -- a session
// never has to state its own address, so replies always have somewhere to go.
type Sender struct {
	Kind      string `json:"kind"` // "session" | "shell"
	SessionID string `json:"sessionId,omitempty"`
	Name      string `json:"name,omitempty"`
	Cwd       string `json:"cwd,omitempty"`
}

// Addr is what the recipient types to reply, or "" when there is nobody to
// reply to. A shell sender has no inbox, so we deliberately offer no reply
// handle rather than one that would fail.
func (s Sender) Addr() string {
	if s.Kind != "session" {
		return ""
	}
	if s.Name != "" {
		return s.Name
	}
	return Short(s.SessionID)
}

func (s Sender) Display() string {
	if s.Name != "" {
		return s.Name
	}
	if s.SessionID != "" {
		return Short(s.SessionID)
	}
	if s.Kind == "shell" {
		return ShellIdentity()
	}
	return "unknown"
}

// Message is one line of a session's spool.
type Message struct {
	ID      string `json:"id"`
	TS      string `json:"ts"`
	From    Sender `json:"from"`
	To      string `json:"to,omitempty"`
	Topic   string `json:"topic,omitempty"`
	Text    string `json:"text"`
	Payload string `json:"payload,omitempty"`
	ReplyTo string `json:"replyTo,omitempty"`
}

// CurrentSender builds the `from` stamp for this process. Both the MCP server
// and the CLI inherit CLAUDE_CODE_SESSION_ID when they run inside a session,
// so this is correct without configuration; outside one it degrades to a
// shell identity.
func CurrentSender() Sender {
	sid := CurrentSessionID()
	if sid == "" {
		return Sender{Kind: "shell", Name: ShellIdentity(), Cwd: CurrentCwd()}
	}
	s := Sender{Kind: "session", SessionID: sid, Cwd: CurrentCwd()}
	if e, err := ReadEntry(sid); err == nil {
		s.Name = e.Name
		if e.Cwd != "" {
			s.Cwd = e.Cwd
		}
	}
	return s
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

func SpoolPath(sessionID string) string {
	return filepath.Join(InboxDir(), sessionID+".ndjson")
}

// Pending counts messages on a session's spool that no monitor has read.
//
// For a live session this is essentially always zero: the monitor tails the
// spool and emits within about a second, so there is no standing backlog to
// report. It goes non-zero exactly when the session is deaf, which is the one
// case worth surfacing -- mail is accumulating for a session id that only
// `claude --resume` will ever bring back.
func Pending(sessionID string) int {
	if ValidSessionID(sessionID) != nil {
		return 0
	}
	path := SpoolPath(sessionID)
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()

	off := readCursors(sessionID)[inboxCursorKey]
	// A cursor past the end means the spool was truncated or replaced under
	// us. Counting from zero over-reports, but reporting nothing pending for a
	// deaf session is the worse failure of the two.
	if off < 0 || off > endOffset(path) {
		off = 0
	}
	if off > 0 {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return 0
		}
	}

	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			n++
		}
	}
	return n
}

func newMessageID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("m_%d", time.Now().UnixNano())
	}
	return "m_" + hex.EncodeToString(b[:])
}

// Sanitize flattens text to a single safe line.
//
// The notification line is a prompt-injection surface: a sender who can emit
// newlines or angle brackets can forge a trailing directive or spoof another
// peer. inter-session has two open issues of exactly this shape (#6, #7), so
// we neutralise the structural characters rather than trusting senders.
func Sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteRune(' ')
		case r == '<':
			b.WriteRune('‹') // ‹
		case r == '>':
			b.WriteRune('›') // ›
		case unicode.IsControl(r):
			// drop
		default:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// Send appends one message to the target's spool and returns it.
func Send(to *Entry, text string, from Sender, replyTo string) (*Message, error) {
	if err := EnsureDirs(); err != nil {
		return nil, err
	}
	body := Sanitize(text)
	if body == "" {
		return nil, fmt.Errorf("refusing to send an empty message")
	}

	msg := &Message{
		ID:      newMessageID(),
		TS:      time.Now().UTC().Format(time.RFC3339),
		From:    from,
		To:      to.SessionID,
		Text:    body,
		ReplyTo: replyTo,
	}

	// Overflow goes to a file the recipient can Read on demand.
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
	line = append(line, '\n')

	// A single O_APPEND write below PIPE_BUF is atomic, so concurrent senders
	// never interleave partial lines. No lock needed on the spool itself.
	f, err := os.OpenFile(SpoolPath(to.SessionID), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open spool: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(line); err != nil {
		return nil, fmt.Errorf("write spool: %w", err)
	}
	return msg, nil
}

// Render turns a message into the single line the monitor prints, which
// becomes one <task_notification> in the receiving session.
//
// Phrased as a report about an event, never as an instruction. Waking a
// session with imperative text makes the model echo "Human:" blocks or
// fabricate user turns outright (anthropics/claude-code#60360).
func Render(m *Message) string {
	// Everything here arrives from a peer, including the fields that look like
	// metadata: a sender controls its own cwd, name and topic, and a spool line
	// could have been written by hand. Sanitise and bound each one rather than
	// trusting that it was checked on the way in.
	const (
		maxName  = 40
		maxWhere = 32
		maxTopic = 32
	)

	var b strings.Builder
	if m.Topic != "" {
		b.WriteString("[pigeon #" + truncate(Sanitize(m.Topic), maxTopic) + "] from ")
	} else {
		b.WriteString("[pigeon] message from ")
	}
	b.WriteString(truncate(Sanitize(m.From.Display()), maxName))
	if where := filepath.Base(m.From.Cwd); where != "" && where != "." && where != "/" {
		b.WriteString(" (" + truncate(Sanitize(where), maxWhere) + ")")
	}
	head := b.String()

	// Build the trailing hints first: they are small, fixed, and the payload
	// pointer must never be the thing that gets cut.
	var tail strings.Builder
	if addr := m.From.Addr(); addr != "" {
		tail.WriteString(" [reply: pigeon send " + truncate(Sanitize(addr), maxName) + "]")
	} else {
		// Saying nothing is not enough: a recipient reads "from
		// shell:user@host", assumes it is an address, and wastes a call
		// discovering it is not. Say so outright.
		tail.WriteString(" [no reply address: sent from a shell, not a session]")
	}
	if m.Topic != "" {
		tail.WriteString(" [topic: pigeon publish " + truncate(Sanitize(m.Topic), maxTopic) + "]")
	}
	// Only ever point at our own payload directory. A hand-written spool line
	// could otherwise name any path and have it read back as trustworthy.
	if p := m.Payload; p != "" && filepath.Dir(p) == PayloadsDir() && filepath.Base(p) != "" {
		tail.WriteString(" [full text: " + p + "]")
	}

	room := RenderBudget - len([]rune(head)) - len([]rune(tail.String())) - 4
	if room < 16 {
		room = 16
	}
	if room > BodyBudget {
		room = BodyBudget
	}
	line := head + " :: " + truncate(Sanitize(m.Text), room) + tail.String()
	// Belt and braces: whatever the pieces did, the line itself is bounded.
	return truncate(line, RenderBudget)
}

// ParseMessage reads one spool line.
func ParseMessage(line string) (*Message, error) {
	var m Message
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		return nil, err
	}
	return &m, nil
}
