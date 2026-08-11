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
	"regexp"
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

// SubjectLimit caps a message's subject line, in runes. validateBounded
// rejects an oversize subject at send time rather than truncating it: the
// subject is the one part of a message guaranteed to reach the recipient (see
// Render), so cutting it silently would leave the sender believing a shorter
// line arrived intact when it did not. Render enforces the same bound again,
// independently, because a spool line can be hand-written and never pass
// through validateBounded at all.
const SubjectLimit = 120

// BriefLimit caps a message's brief, in runes. A brief is the default view of
// `inbox`, not the notification path, so it does not need Subject's "must
// survive a hard clip" discipline -- but it still has to be an outright
// reject rather than a silent truncation, for the same reason a sender must
// be able to trust that a subject arrived exactly as written: 600 runes is
// generous enough for two or three real sentences while stopping a "brief"
// that is actually the whole message again.
const BriefLimit = 600

// PriorityAlert is the one non-default value Priority may hold.
//
// Exactly two levels exist on purpose: "" (normal) and "alert" -- nothing in
// between. A three-level scheme just moves the SHOUTING problem down one rung:
// give senders "high" and "urgent" and every routine message drifts up to
// "high" the same way it used to arrive in capitals, and the level meant to
// interrupt loses its meaning again. Two levels, with the second one scarce
// and reserved (see newRateLimiter's alertReserve), is what keeps it scarce.
const PriorityAlert = "alert"

// validatePriority rejects anything but the two levels Priority may hold.
// Shared by Send and Publish so the rule cannot drift between the two paths.
func validatePriority(p string) error {
	if p != "" && p != PriorityAlert {
		return fmt.Errorf("priority %q is not valid; use \"\" (normal) or %q", p, PriorityAlert)
	}
	return nil
}

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
	Subject string `json:"subject,omitempty"`
	// Brief is the middle tier between Subject and Text: a sender-written
	// summary long enough to decide whether the full body is worth reading.
	// It never reaches Render (see Render's doc comment) -- only `inbox`'s
	// pull path shows it -- so it carries none of Subject's "must survive a
	// hard clip" weight.
	Brief string `json:"brief,omitempty"`
	// Priority is "" for a normal message or PriorityAlert for the one level
	// above it. See PriorityAlert's doc comment for why there are only two.
	Priority string `json:"priority,omitempty"`
	// For names which sessions a topic message is actually aimed at. It is NOT
	// routing: the message still lands in the topic log in full and still
	// reaches every subscriber's inbox (see Publish), so the record stays
	// complete and catch-up/audit are unaffected. It changes only how the
	// message is presented -- Render, writeInboxItem -- and, once delivery
	// modes exist, whether it interrupts.
	//
	// Deliberately not resolved to session ids at send time; see the comment
	// on that in Publish. It is matched against a viewing session at read
	// time instead, by IsFor.
	For []string `json:"for,omitempty"`
	// Supersedes names the id of a message this one replaces. It is honoured
	// only for messages this monitor has itself seen sent by the same
	// sender (see resolveSupersede in monitor.go) -- Send and Publish check
	// only that it is shaped like a real id and not self-referential, since
	// neither has the delivery-side history needed to check who sent the
	// original.
	Supersedes string `json:"supersedes,omitempty"`
	Payload    string `json:"payload,omitempty"`
	ReplyTo    string `json:"replyTo,omitempty"`
	// Thread names the conversation this message belongs to. Derived at send
	// time from ReplyTo, never supplied by a caller directly (see Send's
	// comment on why it can only ever be set to the parent's own id, not the
	// parent's own Thread): empty for a message that neither replies to
	// anything nor -- so far -- has been replied to.
	Thread string `json:"thread,omitempty"`
	// Attach lists the stored paths of files this message carries, each
	// copied at send time into the payload directory the body overflow
	// already spills to, named <id>-<basename> (see attachFiles). Like
	// Payload, Render never points at these -- the notification budget has no
	// room for them -- but the `inbox` pull path lists them, under the same
	// "only ever point at a payload directory this session already knows"
	// rule Render applies to Payload (see writeInboxItem). An attachment's
	// bytes are untrusted input from a peer: read them, never execute them.
	Attach []string `json:"attach,omitempty"`
	// AskID marks a message as the question half of a blocking ask (see
	// ask.go's Ask). Set only by Ask itself, never by an ordinary sender --
	// Send rejects a non-empty one outright, the same way it rejects For --
	// because it is what tells Render to print the "how to answer" hint, and
	// a hand-typed one would let a message forge that hint for an ask that
	// does not exist.
	AskID string `json:"askId,omitempty"`
}

// Draft is a message being composed. Text is required; everything else is
// optional and validated before it reaches a spool.
type Draft struct {
	Text       string
	Subject    string
	Brief      string
	Priority   string
	For        []string
	Supersedes string
	ReplyTo    string
	// AskID is set only by Ask (see ask.go); see Message.AskID for why.
	AskID string
	// Attach lists local file paths to copy into the recipient's payload
	// directory at send time (see attachFiles); Message.Attach then names
	// where each one landed. Never reaches Render -- see the note on
	// Message.Attach.
	Attach []string
}

// IsFor reports whether a topic message is addressed to e: true when m.For is
// empty (a message for everyone), or when e answers to one of the named
// entries -- by its declared name, its host label, or its short session id,
// all case-insensitively.
//
// The label is in there because most sessions never declare a name. Six of the
// nine live on the machine this was written on had none, so a For list that
// only matched declared names could not address two thirds of the fleet, and
// once For decides who gets interrupted (see the addressing gate in
// RunMonitor) "cannot be named" would quietly mean "never notified".
func (m *Message) IsFor(e *Entry) bool {
	if len(m.For) == 0 {
		return true
	}
	if e == nil {
		return false
	}
	short := Short(e.SessionID)
	for _, f := range m.For {
		if e.Name != "" && strings.EqualFold(f, e.Name) {
			return true
		}
		if e.Label != "" && strings.EqualFold(f, e.Label) {
			return true
		}
		if strings.EqualFold(f, short) {
			return true
		}
		// The full id as well as the short one. list_sessions prints both, and
		// a sender that copies the long form must not silently address nobody
		// now that For decides who is interrupted.
		if strings.EqualFold(f, e.SessionID) {
			return true
		}
	}
	return false
}

// UnmatchedFor returns the entries of m.For that no live session in n answers
// to, so a sender can be told it addressed nobody.
//
// Worth reporting because For is no longer advisory: a typo, a stale name off
// an older listing, or a session that exited between the listing and the
// publish all now mean the message interrupted no one at all. The sender would
// otherwise be told only how many sessions subscribe to the topic, carry on,
// and read the resulting silence as consent.
func (n Namespace) UnmatchedFor(m *Message) []string {
	if m == nil || len(m.For) == 0 {
		return nil
	}
	entries, err := n.ListSessions(false, false)
	if err != nil {
		return nil
	}
	var out []string
	for _, f := range m.For {
		probe := &Message{For: []string{f}}
		matched := false
		for _, e := range entries {
			if probe.IsFor(e) {
				matched = true
				break
			}
		}
		if !matched {
			out = append(out, f)
		}
	}
	return out
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
	// Self falls back to every namespace, and then to this claude process, for
	// the same reason: this process may not resolve the namespace the monitor
	// registered in, and after a clear it does not even hold the id the monitor
	// registered under.
	found, e, err := Self()
	if err == nil {
		// Stamped from the entry rather than from this process's environment,
		// because a reply is addressed to what this says. The environment's id
		// is the one Claude Code minted most recently; the entry's is the one a
		// monitor is actually listening on, and after a clear those differ. The
		// namespace goes with it: an address is only an address in the
		// namespace holding the spool it names.
		s.SessionID, s.Namespace = e.SessionID, found.String()
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

// messageIDRe matches exactly what newMessageID produces: "m_" plus 12
// lowercase hex digits. The fallback branch above (clock-based, used only
// when crypto/rand fails) does not match this and is deliberately not
// accepted as a supersede target either -- accepting it would mean guessing
// whether a caller's "m_..." string is a real id or typed-out text that
// merely looks like one.
var messageIDRe = regexp.MustCompile(`^m_[0-9a-f]{12}$`)

// payloadBaseRe bounds the basename of a payload path to characters that
// cannot carry structure into a line that quotes it.
//
// The containing directory was already checked at every site that shows one of
// these paths, and that check is not enough on its own. The DIRECTORY is ours;
// the BASENAME came from a peer -- either off a spool line, which this package
// assumes throughout may have been hand-written and never validated, or from
// the name of a file a sender chose to attach. A basename holding "]" forges
// the end of the bracket hint it sits in, and one holding a newline -- legal in
// a POSIX filename, so this needs no hand-written line at all -- ends the
// notification line and starts a second, entirely attacker-authored one. Every
// other peer-controlled field is bounded at render time for exactly this
// reason; the payload path was the one reaching output raw.
//
// Checked rather than rewritten, the choice askHint makes for AskID just above:
// a path that has been sanitised is no longer a path, and a pointer that does
// not open is worse than no pointer at all.
//
// Deliberately a character class rather than the exact "<message id>.txt" shape
// this package writes. Matching the id shape would read as tighter and would
// strand real mail: newMessageID has a clock-based fallback for when crypto/rand
// fails, which messageIDRe does not accept, and a payload file written by an
// older build answers to no shape this build knows. The characters are what the
// hazard is made of, so the characters are what this bounds -- losing a body
// with no way to reach it is the failure the pointer exists to prevent.
// Unbounded in length on purpose. What makes a basename dangerous here is the
// characters in it, and length is already somebody else's job twice over: the
// filesystem caps a path component, and the give-up ladder below is the thing
// that decides what a long pointer costs the rest of the line. Capping it here
// as well would drop the pointer for a path that is merely long -- stranding a
// body that has no other route -- in the name of a hazard length is not.
var payloadBaseRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// trustedPayloadPath reports whether p is one this session may show a reader or
// read on their behalf: a safely-named file sitting directly in a payloads
// directory n already knows -- its own, or the shared one a global topic spills
// to. Everything else is a path a peer merely named, and naming a path is not
// the same as it being ours.
func (n Namespace) trustedPayloadPath(p string) bool {
	base := filepath.Base(p)
	if p == "" || !payloadBaseRe.MatchString(base) {
		return false
	}
	// The character class admits a leading dot, since a payload name is only
	// ever "<id>.txt" and an id has no business being rejected for its first
	// character. That lets through the two names that are not files at all:
	// "<payloads>/.." names the directory above the one whose contents we are
	// vouching for, and neither it nor "." has a body to point at.
	if base == "." || base == ".." {
		return false
	}
	d := filepath.Dir(p)
	return d == n.PayloadsDir() || d == SharedPayloadsDir()
}

// safeAttachName bounds the sender-chosen half of a stored attachment's name to
// what attachNameRe will later accept, so the untrusted part cannot carry
// structure into any output that lists it. Applied when the file is written
// rather than only when it is shown, because the name becomes a real filename
// on disk and a newline is legal in one.
func safeAttachName(base string) string {
	var b strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	name := b.String()
	if len([]rune(name)) > 64 {
		name = string([]rune(name)[:64])
	}
	// A basename that was entirely unsafe characters, or empty to begin with,
	// still has to be nameable: two attachments reduced to nothing would also
	// collide with each other, which the caller reports as a duplicate.
	if name == "" {
		name = "file"
	}
	return name
}

// validateSupersedes checks that a draft's Supersedes value, if given, is
// shaped like a real message id and does not name the message it is on. This
// is everything Send and Publish can check: neither holds the history needed
// to know who actually sent the named id, so the one check that matters most
// -- that only the original sender may supersede a message -- happens later,
// at delivery time (see resolveSupersede in monitor.go), against messages
// the monitor has itself seen.
func validateSupersedes(id, ownID string) (string, error) {
	if id == "" {
		return "", nil
	}
	if !messageIDRe.MatchString(id) {
		return "", fmt.Errorf("supersedes %q does not look like a message id (want m_ followed by 12 lowercase hex digits)", id)
	}
	if id == ownID {
		return "", fmt.Errorf("a message may not supersede itself")
	}
	return id, nil
}

// validateReplyTo checks that a draft's ReplyTo, if given, is shaped like a
// real message id -- the same defence Send and Publish already apply to
// Supersedes (see validateSupersedes above): neither holds the delivery
// history to confirm the named id was ever actually sent, so shape is all
// that can be checked at this point.
func validateReplyTo(id, ownID string) (string, error) {
	if id == "" {
		return "", nil
	}
	if !messageIDRe.MatchString(id) {
		return "", fmt.Errorf("replyTo %q does not look like a message id (want m_ followed by 12 lowercase hex digits)", id)
	}
	if id == ownID {
		return "", fmt.Errorf("a message may not reply to itself")
	}
	return id, nil
}

// maxAttachments caps how many files one message may attach. Five is enough
// for "here are the changed files" without turning a message into a batch
// upload, which the notification budget was never built to carry -- not that
// it has to: attachments never reach Render at all (see Message.Attach).
const maxAttachments = 5

// maxAttachmentBytes caps each attached file's size. 256 KiB keeps a
// message's whole attachment set a rounding error next to what the state
// directory already holds, while still fitting a real diff or log excerpt.
const maxAttachmentBytes = 256 * 1024

// attachFiles copies each of paths into dir, named "<msgID>-<basename>" so two
// senders attaching a same-named file (stage-hunk.sh) cannot collide, and
// returns the stored paths in the order given. It copies rather than keeping
// the sender's own path, because a pointer this package hands out has to
// survive after the source file changes or disappears -- the same promise the
// body-overflow payload file already makes.
func attachFiles(dir, msgID string, paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	if len(paths) > maxAttachments {
		return nil, fmt.Errorf("%d attachments given; the limit is %d", len(paths), maxAttachments)
	}
	stored := make([]string, 0, len(paths))
	used := map[string]bool{}
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("attach %q: %w", p, err)
		}
		if fi.IsDir() {
			return nil, fmt.Errorf("attach %q: is a directory, not a file", p)
		}
		if fi.Size() > maxAttachmentBytes {
			return nil, fmt.Errorf("attach %q is %d bytes; the limit is %d (%d KiB)",
				p, fi.Size(), maxAttachmentBytes, maxAttachmentBytes/1024)
		}
		name := msgID + "-" + safeAttachName(filepath.Base(p))
		if used[name] {
			return nil, fmt.Errorf("attach %q: another attachment already uses the basename %q", p, filepath.Base(p))
		}
		used[name] = true
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("attach %q: %w", p, err)
		}
		dest := filepath.Join(dir, name)
		if err := os.WriteFile(dest, data, 0o600); err != nil {
			return nil, fmt.Errorf("attach %q: %w", p, err)
		}
		stored = append(stored, dest)
	}
	return stored, nil
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

// validateBounded sanitises a field and rejects it outright if it is over
// limit runes, rather than truncating it: a sender has to be able to trust
// that what arrived is exactly what was written, and a silent truncation
// breaks that promise as thoroughly as no limit at all would. An empty
// result after sanitising is not an error -- it simply means none was given.
// Shared by every bounded field (subject, brief) across Send and Publish so
// the rule cannot drift between fields or between the two paths.
func validateBounded(field, s string, limit int) (string, error) {
	v := Sanitize(s)
	if n := len([]rune(v)); n > limit {
		return "", fmt.Errorf("%s is %d runes; the limit is %d", field, n, limit)
	}
	return v, nil
}

func validateSubject(s string) (string, error) { return validateBounded("subject", s, SubjectLimit) }

func validateBrief(s string) (string, error) { return validateBounded("brief", s, BriefLimit) }

// validateAskID checks that a draft's AskID, if given, is shaped like a real
// id -- the same defence-in-depth as validateSupersedes, not a trust boundary
// Ask itself relies on: Ask generates the id and the draft it publishes in the
// same call, so this only ever rejects a caller other than Ask setting the
// field, which Send and mcpPublish already refuse to accept in the first place.
func validateAskID(id string) (string, error) {
	if id == "" {
		return "", nil
	}
	if !messageIDRe.MatchString(id) {
		return "", fmt.Errorf("askId %q does not look like a valid id (want m_ followed by 12 lowercase hex digits)", id)
	}
	return id, nil
}

// SubjectNudge returns a hint to append to a send/publish confirmation when
// the message left with no subject and a body long enough that the recipient
// only ever sees a truncated prefix of it. Without this the sender has no way
// to learn that; the confirmation is the only signal it gets back at all.
func SubjectNudge(msg *Message) string {
	if msg.Subject != "" {
		return ""
	}
	if n := len([]rune(msg.Text)); n > BodyBudget {
		return fmt.Sprintf("\nNo subject given, and the body is %d chars -- recipients see a prefix cut at %d. A subject would let them triage it.",
			n, BodyBudget)
	}
	return ""
}

// Send appends one message to the target's spool and returns it. The namespace
// is the recipient's, not the sender's: a cross-namespace send has to land in
// the inbox the recipient's monitor is actually following.
func Send(to *Entry, d Draft, from Sender) (*Message, error) {
	return CurrentNamespace().Send(to, d, from)
}

func (n Namespace) Send(to *Entry, d Draft, from Sender) (*Message, error) {
	if err := n.EnsureDirs(); err != nil {
		return nil, err
	}
	body := Sanitize(d.Text)
	if body == "" {
		return nil, fmt.Errorf("refusing to send an empty message")
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
	// A direct message already has exactly one recipient -- the target Send
	// was called with. A second, disagreeing list of names would be a trap:
	// whichever one a reader believed would sometimes be wrong.
	if len(d.For) > 0 {
		return nil, fmt.Errorf("for is only valid on a topic publish, not a direct send: a direct message already has exactly one recipient")
	}
	// An ask needs a topic's live-subscriber list to know its audience, which
	// a direct send has no equivalent of -- there is nobody to snapshot.
	if d.AskID != "" {
		return nil, fmt.Errorf("askId is only valid on a topic publish, not a direct send")
	}

	id := newMessageID()
	supersedes, err := validateSupersedes(d.Supersedes, id)
	if err != nil {
		return nil, err
	}
	replyTo, err := validateReplyTo(d.ReplyTo, id)
	if err != nil {
		return nil, err
	}

	msg := &Message{
		ID:         id,
		TS:         time.Now().UTC().Format(time.RFC3339),
		From:       from,
		To:         to.SessionID,
		Text:       body,
		Subject:    subject,
		Brief:      brief,
		Priority:   d.Priority,
		Supersedes: supersedes,
		ReplyTo:    replyTo,
	}
	// Thread groups a reply with its parent's conversation. Send never sees
	// more of the log than the one draft it was handed -- unlike
	// resolveSupersede, which runs inside the monitor's own follow loop and so
	// can check a claim against messages it has itself streamed past -- so
	// there is no bounded lookup available here to find the parent's own
	// Thread. The parent's id is there is: Thread is set to ReplyTo itself,
	// which is exactly right for a reply to a root message and one hop short
	// for a reply to a reply. `pigeon thread` and the inbox grouper both
	// account for that by walking ReplyTo directly rather than trusting this
	// field to already be resolved to a single root id.
	if replyTo != "" {
		msg.Thread = replyTo
	}

	// Overflow goes to a file the recipient can Read on demand, in the
	// recipient's own payload directory: Render will not follow a pointer into
	// anywhere else.
	if len([]rune(body)) > BodyBudget {
		p := filepath.Join(n.PayloadsDir(), msg.ID+".txt")
		if err := os.WriteFile(p, []byte(d.Text), 0o600); err == nil {
			msg.Payload = p
		}
	}
	if len(d.Attach) > 0 {
		stored, err := attachFiles(n.PayloadsDir(), msg.ID, d.Attach)
		if err != nil {
			return nil, err
		}
		msg.Attach = stored
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
//
// self is the entry of the session this is being rendered for, so the "->
// you" marker (see the addressed comment below) can be decided. It may be
// nil -- a caller that does not know who it is rendering for (or is
// rendering generically, e.g. in a test) simply gets the un-addressed form.
func (n Namespace) Render(m *Message, self *Entry) string {
	// Everything here arrives from a peer, including the fields that look like
	// metadata: a sender controls its own cwd, name, namespace and topic, and a
	// spool line could have been written by hand. Sanitise and bound each one
	// rather than trusting that it was checked on the way in.
	const (
		maxName    = 40
		maxWhere   = 32
		maxTopic   = 33 // one more than a topic name, for the "@" of a global one
		maxNS      = 32
		maxSubject = SubjectLimit
	)

	// The "@" is presentation here rather than a path decision, so a hostile
	// spool line can misdescribe its own topic and nothing else.
	global := strings.HasPrefix(m.Topic, GlobalPrefix)
	// A message from another namespace reached this session over a global topic
	// or a deliberate cross-namespace send. Either way its address does not
	// resolve here, which is the one thing the recipient has to be told.
	foreign := m.From.Namespace != "" && m.From.Namespace != n.String()
	sender := truncate(Sanitize(m.From.Namespace), maxNS)

	// A spool line can be hand-written, so anything but the exact sentinel
	// reads as normal -- the same bound-defensively rule as every other field
	// here, and the reason this compares to the constant rather than echoing
	// m.Priority into the line.
	alert := m.Priority == PriorityAlert
	bang := ""
	if alert {
		bang = "!"
	}

	// addressed marks a topic message that names this session among its For
	// list -- as opposed to one with no For at all, which is for everyone and
	// gets no marker. The marker itself is a fixed string chosen by this
	// boolean, never text built from m.For: For comes off disk and a spool
	// line can be hand-written, so echoing any of it into the line would let
	// a hostile entry forge its own bracketed hint the same way a raw
	// priority value could (see the sentinel comparison above).
	addressed := m.Topic != "" && len(m.For) > 0 && m.IsFor(self)
	marker := ""
	if addressed {
		marker = " -> you"
	}

	// correction marks a message that legitimately supersedes another. By
	// the time Render runs, that check has already happened: Render has no
	// history to check a "supersedes" claim against, so RunMonitor's
	// resolveSupersede clears m.Supersedes before this ever sees the message
	// unless the claim verified. What is left here is trusted the same way
	// the sentinel comparisons above are -- and the marker itself is a fixed
	// string chosen by this boolean, never the target id: RenderInbox is the
	// one place that names an id, because it verifies the claim itself
	// rather than trusting an upstream mutation (see supersedeLinks).
	correction := m.Supersedes != ""
	corrTag := ""
	if correction {
		corrTag = " ↺ correction"
	}

	var prefix string
	switch {
	case m.Topic == "":
		prefix = "[pigeon"
		if alert {
			prefix += " !"
		}
		prefix += corrTag + "] message from "
	case global:
		prefix = "[pigeon " + bang + truncate(Sanitize(m.Topic), maxTopic) + marker + corrTag + "] from "
	default:
		prefix = "[pigeon " + bang + "#" + truncate(Sanitize(m.Topic), maxTopic) + marker + corrTag + "] from "
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
	// askHint is how a recipient discovers that this notification is a
	// question the asker is actually blocked waiting on, and exactly what to
	// type back. This is the fix for the failure that motivated Ask at all:
	// a question with no visible way to answer gets read, sits unanswered,
	// and the asker cannot tell a real "nobody objects" from "nobody saw it
	// in time" -- so the hint is never dropped, the same discipline the
	// payload pointer gets, rather than being one more thing the give-up
	// ladder below may shed under budget pressure.
	//
	// m.AskID is checked against the same shape validateAskID enforces at
	// publish time, because a spool line can be hand-written: an unshaped
	// value must not be able to forge an "answer this" hint for an ask that
	// was never actually asked.
	var askHint string
	if messageIDRe.MatchString(m.AskID) {
		askHint = " [ask: pigeon answer " + m.AskID + " ok|object|blocked]"
	}
	// Only ever point at a payload directory this session already knows: its
	// own, or the shared one a global topic spills to. A hand-written spool line
	// could otherwise name any path and have it read back as trustworthy.
	if p := m.Payload; n.trustedPayloadPath(p) {
		payload = " [full text: " + p + "]"
	}

	// The subject sits in the never-dropped class alongside the payload
	// pointer: send-time validation already bounds it to SubjectLimit, but a
	// spool line can be hand-written and never pass through that check, so it
	// is sanitised and bounded again here regardless of where the message came
	// from -- the same reasoning that already applies to name/cwd/topic/
	// namespace above.
	subject := truncate(Sanitize(m.Subject), maxSubject)

	// A body spilled to a file is recoverable from the pointer, so it may be
	// squeezed to nothing. A subject makes the same case for a different
	// reason: it is itself a readable minimum, so a long one must not force
	// every rung of the ladder below to give up the reply address just to
	// protect a body that already has a substitute. A message with neither is
	// all the recipient will ever get, so only then keep a readable minimum
	// for the body and give up something else instead.
	minBody := 24
	if payload != "" || subject != "" {
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
		room := RenderBudget - len([]rune(head)) - len([]rune(payload+askHint+reply+topicHint)) - 4
		if subject != "" {
			// Reserved unconditionally, unlike the body's speculative 4
			// characters above: the subject is never dropped, so this
			// allowance is always spent, never merely offered.
			room -= 4 + len([]rune(subject))
		}
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
		switch {
		case body == "" && subject == "":
			return head + payload + askHint + reply + topicHint
		case body == "":
			return head + " :: " + subject + payload + askHint + reply + topicHint
		case subject == "":
			return head + " :: " + body + payload + askHint + reply + topicHint
		default:
			return head + " :: " + subject + " :: " + body + payload + askHint + reply + topicHint
		}
	}

	// Everything droppable is gone and it still does not fit, which needs a
	// pathological state path. The pointer is the only part with no substitute,
	// so give up the header for it before giving up any of it.
	//
	// The subject is never-dropped everywhere above, and this is the one place
	// that claim weakens: reaching here needs a payload path long enough to
	// exhaust the budget by itself, which a deep enough home directory
	// supplies, and by then there may be no room for anything else. Keep the
	// subject where it still fits -- a line saying who sent it and what it
	// says beats one saying only who sent it -- but never at the pointer's
	// expense, because the pointer is the only part with no substitute. The ask
	// hint rides along wherever it still fits, but -- unlike above -- it is the
	// first thing given up here: this path only exists because the payload
	// pointer alone is already close to the budget, and losing the message
	// entirely is worse than losing the one bracket telling the recipient how
	// to reply.
	if subject != "" && len([]rune(prefix+" :: "+subject+payload+askHint)) <= RenderBudget {
		return prefix + " :: " + subject + payload + askHint
	}
	if subject != "" && len([]rune(prefix+" :: "+subject+payload)) <= RenderBudget {
		return prefix + " :: " + subject + payload
	}
	if len([]rune(prefix+payload+askHint)) <= RenderBudget {
		return prefix + payload + askHint
	}
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
