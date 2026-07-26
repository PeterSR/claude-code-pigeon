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
	// Namespace is where the sender's own address resolves. Carried because a
	// message can arrive from outside the recipient's namespace -- over a
	// global topic, or from a cross-namespace send -- and a reply typed without
	// it would go nowhere, or to a different session answering to the same name.
	Namespace string `json:"namespace,omitempty"`
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
	ns := CurrentNamespace()
	sid := CurrentSessionID()
	if sid == "" {
		// A shell has no entry to consult, so ask the project directly. Running
		// `pigeon send` from a private checkout is the same disclosure as a
		// session in it doing so, and the sender is the only one who can tell.
		cwd := CurrentCwd()
		if cfg, _, err := LoadProjectConfig(cwd); err == nil && cfg != nil && cfg.Private {
			cwd = ""
		}
		return Sender{Kind: "shell", Name: ShellIdentity(), Cwd: cwd, Namespace: ns.String()}
	}
	// The cwd starts empty and is filled in only once the entry says it may be.
	// Setting it up front and blanking it for a private session inside the
	// success branch looks equivalent and is not: this process resolves its
	// namespace from its own environment and working directory, which need not
	// be the ones the monitor armed with, so the lookup can miss and leave the
	// directory in place -- exactly what `private` exists to prevent, published
	// to every recipient. Missing the entry must fail closed.
	s := Sender{Kind: "session", SessionID: sid, Namespace: ns.String()}
	// locateSession falls back to every namespace, for the same reason the
	// status widget needs it: this process may not resolve the one the monitor
	// registered in.
	_, e, err := locateSession(sid)
	if err == nil {
		s.Name = e.Name
		if !e.Private {
			if e.Cwd != "" {
				s.Cwd = e.Cwd
			} else {
				s.Cwd = CurrentCwd()
			}
		}
	}
	return s
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

func (n Namespace) SpoolPath(sessionID string) string {
	return filepath.Join(n.InboxDir(), sessionID+".ndjson")
}

func SpoolPath(sessionID string) string { return CurrentNamespace().SpoolPath(sessionID) }

// Pending counts messages on a session's spool that no monitor has read.
//
// For a live session this is essentially always zero: the monitor tails the
// spool and emits within about a second, so there is no standing backlog to
// report. It goes non-zero exactly when the session is deaf, which is the one
// case worth surfacing -- mail is accumulating for a session id that only
// `claude --resume` will ever bring back.
func Pending(sessionID string) int { return CurrentNamespace().Pending(sessionID) }

func (n Namespace) Pending(sessionID string) int {
	if ValidSessionID(sessionID) != nil {
		return 0
	}
	path := n.SpoolPath(sessionID)
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()

	// Cursors are logical offsets, so convert through the base the same way a
	// follower does. The spool is never compacted, so in practice the base is
	// zero and this is the identity -- but deriving it rather than assuming it
	// is what stops this from silently disagreeing with the monitor if that
	// ever changes.
	fi, err := f.Stat()
	if err != nil {
		return 0
	}
	base := readBase(path)
	physical := n.readCursors(sessionID)[inboxCursorKey] - base
	switch {
	case physical < 0:
		// Behind what was cut away: everything still on disk is unread.
		physical = 0
	case physical > fi.Size():
		// Past the end: the spool was truncated or replaced under us, so the
		// stored position means nothing. Count everything that is there. This
		// over-reports, but the number exists to warn that mail is piling up
		// on a deaf session, and reporting none would hide exactly that.
		physical = 0
	}
	if physical > 0 {
		if _, err := f.Seek(physical, io.SeekStart); err != nil {
			return 0
		}
	}

	num := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			num++
		}
	}
	return num
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
//
// Square brackets are structural HERE, which angle brackets never were: every
// hint this format carries is `[reply: ...]`, `[full text: ...]`, `[ns: ...]`.
// A body that may write a bare `[` can therefore forge a payload pointer at
// any path it likes, a reply address it does not own, or a whole second
// notification from a peer that never sent one -- all through an ordinary
// `pigeon send`, with no access to the state directory at all. Only the
// renderer may emit a bare bracket.
//
// The control-character rule covers the formatting categories too, because
// unicode.IsControl only reports Latin-1 C0/C1: a bidi override or a zero
// width joiner is not "control" by that test and would reach the line intact.
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
		case r == '[':
			b.WriteRune('⟦') // ⟦
		case r == ']':
			b.WriteRune('⟧') // ⟧
		case unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Co, unicode.Cs):
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
	// A budget with no room for the ellipsis, let alone a character before it.
	// r[:n-1] on a zero budget indexes backwards and panics, which turns a
	// tight notification line into a dead monitor.
	if n <= 1 {
		if n <= 0 {
			return ""
		}
		return "…"
	}
	return string(r[:n-1]) + "…"
}

// Send appends one message to the target's spool and returns it. The namespace
// is the recipient's, not the sender's: a cross-namespace send has to land in
// the inbox the recipient's monitor is actually following.
func Send(to *Entry, text string, from Sender, replyTo string) (*Message, error) {
	return CurrentNamespace().Send(to, text, from, replyTo)
}

func (n Namespace) Send(to *Entry, text string, from Sender, replyTo string) (*Message, error) {
	if err := n.EnsureDirs(); err != nil {
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

	// Overflow goes to a file the recipient can Read on demand, in the
	// recipient's own payload directory: Render will not follow a pointer into
	// anywhere else.
	if len([]rune(body)) > BodyBudget {
		p := filepath.Join(n.PayloadsDir(), msg.ID+".txt")
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
	f, err := os.OpenFile(n.SpoolPath(to.SessionID), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
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
//
// The receiver is a namespace because two of the decisions here depend on
// where the message landed: which payload directories may be pointed at, and
// whether the sender is close enough that a bare reply address would reach it.

func (n Namespace) Render(m *Message) string {
	// Everything here arrives from a peer, including the fields that look like
	// metadata: a sender controls its own cwd, name, namespace and topic, and a
	// spool line could have been written by hand. Sanitise and bound each one
	// rather than trusting that it was checked on the way in.
	const (
		maxName  = 40
		maxWhere = 32
		maxTopic = 33 // one more than a topic name, for the "@" of a global one
		maxNS    = 32
	)

	// The "@" is presentation here rather than a path decision, so a hostile
	// spool line can misdescribe its own topic and nothing else.
	global := strings.HasPrefix(m.Topic, GlobalPrefix)
	// A message from another namespace reached this session over a global topic
	// or a deliberate cross-namespace send. Either way its address does not
	// resolve here, which is the one thing the recipient has to be told.
	foreign := m.From.Namespace != "" && m.From.Namespace != n.String()
	sender := truncate(Sanitize(m.From.Namespace), maxNS)

	var prefix string
	switch {
	case m.Topic == "":
		prefix = "[pigeon] message from "
	case global:
		prefix = "[pigeon " + truncate(Sanitize(m.Topic), maxTopic) + "] from "
	default:
		prefix = "[pigeon #" + truncate(Sanitize(m.Topic), maxTopic) + "] from "
	}
	prefix += truncate(Sanitize(m.From.Display()), maxName)

	var where string
	if w := filepath.Base(m.From.Cwd); w != "" && w != "." && w != "/" {
		where = " (" + truncate(Sanitize(w), maxWhere) + ")"
	}
	// Named only where a message can have come from outside this namespace: a
	// global topic, or a direct message that crossed. On a namespaced topic it
	// is a constant, and a constant in every notification is noise.
	var nsTag string
	if sender != "" && (global || foreign) {
		nsTag = " [ns: " + sender + "]"
	}

	// The payload pointer must never be the thing that gets cut: it is the only
	// route to a message whose body did not fit, so losing it strands the
	// message. The old arithmetic clamped the body's allowance *up* when the
	// rest was large, pushing the line past the budget so that a final truncate
	// trimmed the end -- which is exactly where the pointer sits.
	//
	// So nothing is trimmed. Parts are dropped whole, in order of what the
	// recipient can reconstruct without them, and the pointer is never in that
	// order at all. It is also emitted first among the hints, so that if a
	// pathological path leaves the line over budget even with everything else
	// gone, the backstop truncate eats a hint rather than the pointer.
	var reply, topicHint, payload string
	if addr := m.From.Addr(); addr != "" {
		qualifier := ""
		if foreign {
			// Without this the reply either finds nobody or finds a different
			// session that happens to answer to the same name here.
			qualifier = "-n " + sender + " "
		}
		reply = " [reply: pigeon send " + qualifier + truncate(Sanitize(addr), maxName) + "]"
	} else {
		// Saying nothing is not enough: a recipient reads "from
		// shell:user@host", assumes it is an address, and wastes a call
		// discovering it is not. Say so outright.
		reply = " [no reply address: sent from a shell, not a session]"
	}
	if m.Topic != "" {
		topicHint = " [topic: pigeon publish " + truncate(Sanitize(m.Topic), maxTopic) + "]"
	}
	// Only ever point at a payload directory this session already knows: its
	// own, or the shared one a global topic spills to. A hand-written spool line
	// could otherwise name any path and have it read back as trustworthy.
	if p := m.Payload; p != "" && filepath.Base(p) != "" {
		if d := filepath.Dir(p); d == n.PayloadsDir() || d == SharedPayloadsDir() {
			payload = " [full text: " + p + "]"
		}
	}

	// A body spilled to a file is recoverable from the pointer, so it may be
	// squeezed to nothing. One that was not spilled is all the recipient will
	// ever get, so keep a readable minimum and give up something else instead.
	minBody := 24
	if payload != "" {
		minBody = 0
	}

	// Each step gives up the cheapest thing left. The topic is already in the
	// header; the working directory and the reply address are both recoverable
	// with `pigeon ls`; the namespace tag only qualifies a reply that is by then
	// already gone.
	for _, give := range []func(){
		func() {},
		func() { topicHint = "" },
		func() { where = "" },
		func() { reply = "" },
		func() { nsTag = "" },
	} {
		give()
		head := prefix + where + nsTag
		room := RenderBudget - len([]rune(head)) - len([]rune(payload+reply+topicHint)) - 4
		if room < minBody {
			continue
		}
		if room > BodyBudget {
			room = BodyBudget
		}
		if room < 0 {
			room = 0
		}
		body := truncate(Sanitize(m.Text), room)
		if body == "" {
			return head + payload + reply + topicHint
		}
		return head + " :: " + body + payload + reply + topicHint
	}

	// Everything droppable is gone and it still does not fit, which needs a
	// pathological state path. The pointer is the only part with no substitute,
	// so give up the header for it before giving up any of it.
	if len([]rune(prefix+payload)) <= RenderBudget {
		return prefix + payload
	}
	if len([]rune(payload)) <= RenderBudget {
		return strings.TrimSpace(payload)
	}
	return truncate(prefix+payload, RenderBudget)
}

// ParseMessage reads one spool line.
func ParseMessage(line string) (*Message, error) {
	var m Message
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		return nil, err
	}
	return &m, nil
}
