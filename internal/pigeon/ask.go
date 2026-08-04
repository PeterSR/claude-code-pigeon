package pigeon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Ask is a BLOCKING request/reply primitive, for exactly the case a fire-and-
// forget publish cannot cover: a question that has to be answered before the
// asker does something irreversible.
//
// The incident this fixes: a coordinator published a question, waited with a
// plain `sleep 25`, saw silence, and treated it as consent -- and the answers
// that were already on their way arrived after the irreversible action, too
// late to matter. Two things were wrong with that, and Ask exists to fix both:
//
//   - the asker has to be MADE to wait, not merely told answers are coming.
//     Ask does that itself: it publishes the question and then tails the
//     answer log in the same call, on the same poll cadence followSource uses,
//     and returns only once every audience member has answered or the
//     deadline passes. Putting the deadline in the monitor and notifying the
//     asker later would not make the asker wait at all -- it would just move
//     the same race somewhere harder to see -- and would depend on the
//     monitor, which is documented (see monitor.go) as dying on session
//     resume without reliably being rearmed.
//   - a non-answer must be reported as a non-answer, never folded into
//     agreement. RenderAskResult never summarises silence as "no objections";
//     it names every audience member who did not answer, and their status as
//     of the moment the ask closed, because "did not answer" means something
//     different for a session mid-turn than for one that is dead.
//
// Storage: asks/<id>.json holds the question, the asker, the deadline and the
// audience snapshot (see askRecord), written once and never modified again.
// asks/<id>.ndjson holds the answers, one JSON object per line, appended with
// O_APPEND the same way Send's spool is -- a single write below PIPE_BUF is
// atomic, so concurrent answerers cannot interleave a partial line and no
// lock is needed. A namespaced topic's files live in that namespace's AsksDir;
// a machine-wide "@" topic's live in SharedAsksDir, mirroring exactly how
// TopicsDir/SharedTopicsDir already split.

// AskDefaultDeadline is how long Ask waits when the caller does not say.
const AskDefaultDeadline = 30 * time.Second

// AskMaxDeadline is the longest Ask will ever block for. A blocking MCP call
// holds the whole session idle for its duration, so anything longer belongs
// in an ordinary published message the asker checks back on later, not in a
// call that parks the session.
const AskMaxDeadline = 300 * time.Second

// askNoteLimit bounds an answer's free-text note, in runes. An answer is a
// verdict plus a short reason, not a message in its own right -- 300 runes is
// generous for "why", and a sender who needs more should send one.
const askNoteLimit = 300

// Verdicts an Answer may carry. Exactly three -- agree, object, or report a
// concrete block -- the same discipline PriorityAlert applies to priority:
// no partial-credit fourth value to drift toward.
const (
	VerdictOK      = "ok"
	VerdictObject  = "object"
	VerdictBlocked = "blocked"
)

// validateVerdict rejects anything but the three values an Answer may hold.
func validateVerdict(v string) error {
	switch v {
	case VerdictOK, VerdictObject, VerdictBlocked:
		return nil
	}
	return fmt.Errorf("verdict %q is not valid; use %q, %q or %q", v, VerdictOK, VerdictObject, VerdictBlocked)
}

// Answer is one reply to an ask, appended to asks/<id>.ndjson.
type Answer struct {
	From    Sender `json:"from"`
	Verdict string `json:"verdict"` // "ok" | "object" | "blocked"
	Note    string `json:"note,omitempty"`
	TS      string `json:"ts"`
}

// AskMember is one entry of an ask's audience snapshot: who was asked, and
// where their entry lives, taken at ask time. Namespace is carried because a
// machine-wide ask's audience can span namespaces, and reporting a member's
// status at close (see nonAnswerStatus) means reading their entry back from
// wherever it actually is.
type AskMember struct {
	SessionID string `json:"sessionId"`
	Name      string `json:"name,omitempty"`
	Namespace string `json:"namespace,omitempty"`
}

// Display is the best short handle for a member in a rendered tally.
func (m AskMember) Display() string {
	if m.Name != "" {
		return m.Name
	}
	return Short(m.SessionID)
}

// askRecord is the question itself, filed at asks/<id>.json. Written once by
// Ask and never modified again -- an answer lives in the sibling .ndjson file,
// not here -- so reading it needs no lock.
type askRecord struct {
	ID          string      `json:"id"`
	Topic       string      `json:"topic"`
	Text        string      `json:"text"`
	Subject     string      `json:"subject,omitempty"`
	From        Sender      `json:"from"`
	TS          string      `json:"ts"`
	DeadlineSec int         `json:"deadlineSec"`
	Audience    []AskMember `json:"audience"`
}

// AskResult is the tally Ask returns once it stops waiting: either every
// audience member answered, or the deadline passed.
type AskResult struct {
	ID       string
	Answers  []Answer
	Audience []AskMember   // snapshot at ask time
	ClosedAt time.Duration // how long it actually took
	Quorum   bool          // every audience member answered
}

func askRecordPath(dir, id string) string  { return filepath.Join(dir, id+".json") }
func askAnswersPath(dir, id string) string { return filepath.Join(dir, id+".ndjson") }

// clampAskDeadline applies the default and the ceiling. A non-positive value
// means the caller did not say, not that it asked for an instant timeout --
// zero would make Ask return immediately having waited for nobody, which is
// never what "ask" without a deadline means.
func clampAskDeadline(d time.Duration) (time.Duration, error) {
	if d <= 0 {
		return AskDefaultDeadline, nil
	}
	if d > AskMaxDeadline {
		return 0, fmt.Errorf(
			"ask deadline %s exceeds the maximum of %s; a wait longer than that belongs in a published message that is checked on later, not a blocking call that holds this session idle",
			d, AskMaxDeadline)
	}
	return d, nil
}

// askDirFor is where ref's ask records and answer logs live -- namespaced or
// shared, exactly the split TopicRef.path already makes for the log itself.
func askDirFor(ref TopicRef, n Namespace) string {
	if ref.Global {
		return SharedAsksDir()
	}
	return n.AsksDir()
}

// Ask publishes topic as an alert carrying a fresh ask id, then blocks --
// tailing the answer log itself, on the same poll cadence followSource uses --
// until every live subscriber of topic at the moment of asking has answered,
// or deadline passes. See this file's doc comment for why blocking here is
// the point, not a shortcut.
func Ask(topic string, d Draft, from Sender, deadline time.Duration) (*AskResult, error) {
	return CurrentNamespace().Ask(topic, d, from, deadline)
}

func (n Namespace) Ask(topic string, d Draft, from Sender, deadline time.Duration) (*AskResult, error) {
	deadline, err := clampAskDeadline(deadline)
	if err != nil {
		return nil, err
	}

	ref, err := ParseTopicRef(topic)
	if err != nil {
		return nil, err
	}
	if ref.Global && n.IsPrivate() {
		return nil, fmt.Errorf("namespace %q is private, so it cannot ask on the machine-wide topic %s", n, ref)
	}

	// Validated up front, with the exact rules Publish itself applies, so the
	// record below is written from values Publish is then guaranteed to
	// accept -- there is no window where a record exists but the question
	// never actually went out, or the reverse.
	body := Sanitize(d.Text)
	if body == "" {
		return nil, fmt.Errorf("refusing to ask an empty question")
	}
	subject, err := validateSubject(d.Subject)
	if err != nil {
		return nil, err
	}

	dir := askDirFor(ref, n)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}

	id := newMessageID()
	rec := &askRecord{
		ID:          id,
		Topic:       ref.String(),
		Text:        body,
		Subject:     subject,
		From:        from,
		TS:          nowRFC3339(),
		DeadlineSec: int(deadline / time.Second),
		Audience:    n.askAudience(ref, from.SessionID),
	}
	// Written before the question goes out: a recipient can act on the
	// notification the instant it arrives, and the record has to already
	// exist for that answer to find anywhere to land.
	if err := writeAskRecord(dir, rec); err != nil {
		return nil, err
	}

	d.AskID = id
	d.Priority = PriorityAlert // an ask is a stop-work request, never routine
	if _, err := n.Publish(topic, d, from); err != nil {
		_ = os.Remove(askRecordPath(dir, id))
		return nil, err
	}

	return awaitAnswers(dir, rec, deadline)
}

// askAudience snapshots who this ask is actually for: the live subscribers of
// ref besides the asker. A deaf or dead subscriber is not included -- there is
// nobody there to hold this session's own wait open for -- but if a member
// included here goes deaf or dies before the deadline, that is reported at
// close (see nonAnswerStatus), not silently dropped from the count.
func (n Namespace) askAudience(ref TopicRef, exceptSessionID string) []AskMember {
	var out []AskMember
	for _, e := range n.matchingSubscribers(ref, exceptSessionID) {
		if e.Status != StatusLive {
			continue
		}
		out = append(out, AskMember{SessionID: e.SessionID, Name: e.Name, Namespace: e.Namespace})
	}
	return out
}

// awaitAnswers is Ask's blocking half: poll the answer log until every
// audience member has answered or the deadline passes, then return the tally.
// The poll cadence matches followSource's -- see monitor.go -- because that is
// already the latency budget the rest of pigeon accepts for "arrived".
func awaitAnswers(dir string, rec *askRecord, deadline time.Duration) (*AskResult, error) {
	start := time.Now()
	hardDeadline := start.Add(deadline)

	want := map[string]bool{}
	for _, m := range rec.Audience {
		want[m.SessionID] = true
	}

	for {
		answers, err := readAnswers(dir, rec.ID)
		if err != nil {
			return nil, err
		}
		answered := map[string]bool{}
		for _, a := range answers {
			answered[a.From.SessionID] = true
		}
		quorum := true
		for sid := range want {
			if !answered[sid] {
				quorum = false
				break
			}
		}

		if quorum || !time.Now().Before(hardDeadline) {
			return &AskResult{
				ID:       rec.ID,
				Answers:  answers,
				Audience: rec.Audience,
				ClosedAt: time.Since(start),
				Quorum:   quorum,
			}, nil
		}

		sleep := pollInterval
		if remaining := time.Until(hardDeadline); remaining < sleep {
			sleep = remaining
		}
		if sleep > 0 {
			time.Sleep(sleep)
		}
	}
}

// Answer records one reply to a pending ask. One answer per session: a second
// call from the same session replaces the first (readAnswers keeps the last),
// on the theory that a peer who changes their mind before the deadline should
// be able to say so, and the newer verdict is the one that reflects reality.
//
// Only a real session may answer -- quorum is tracked by session id, and a
// shell sender has none, so its "answer" could never be matched to anyone in
// the audience and would only ever show up as an unexplained outsider.
//
// There is deliberately no package-level Answer(...) wrapper alongside this
// method, unlike Ask/Publish/Subscribe: it would collide with the Answer
// type name above it.
func (n Namespace) Answer(askID string, from Sender, verdict, note string) error {
	if err := validateVerdict(verdict); err != nil {
		return err
	}
	if !messageIDRe.MatchString(askID) {
		return fmt.Errorf("ask id %q does not look like a valid id (want m_ followed by 12 lowercase hex digits)", askID)
	}
	if from.Kind != "session" || from.SessionID == "" {
		return fmt.Errorf("only a Claude Code session can answer an ask -- a shell has no session id to record the reply against")
	}
	note = Sanitize(note)
	if r := len([]rune(note)); r > askNoteLimit {
		return fmt.Errorf("note is %d runes; the limit is %d", r, askNoteLimit)
	}

	dir, _, err := locateAskDir(n, askID)
	if err != nil {
		return err
	}
	return appendAnswer(dir, askID, Answer{From: from, Verdict: verdict, Note: note, TS: nowRFC3339()})
}

// locateAskDir finds where askID's record actually lives: this namespace's
// own AsksDir, or the shared one a machine-wide topic's ask uses. Two checks,
// not a search across every namespace, because that is the same namespaced-
// vs-shared split the ask was filed under in the first place (see askDirFor)
// -- an answerer does not need to know which one to try because there are
// only ever two candidates.
func locateAskDir(n Namespace, id string) (string, *askRecord, error) {
	if rec, err := readAskRecord(n.AsksDir(), id); err == nil {
		return n.AsksDir(), rec, nil
	}
	if rec, err := readAskRecord(SharedAsksDir(), id); err == nil {
		return SharedAsksDir(), rec, nil
	}
	return "", nil, fmt.Errorf("no such ask %q -- it may have expired and been pruned, or the id is wrong", id)
}

// --- disk I/O ----------------------------------------------------------------

// writeAskRecord persists rec atomically, the same temp-file-then-rename
// pattern every other state file in this package uses, so a reader never sees
// a half-written record.
func writeAskRecord(dir string, rec *askRecord) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "ask-*.tmp")
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
	return os.Rename(name, askRecordPath(dir, rec.ID))
}

func readAskRecord(dir, id string) (*askRecord, error) {
	b, err := os.ReadFile(askRecordPath(dir, id))
	if err != nil {
		return nil, err
	}
	var rec askRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// appendAnswer writes one line to the answer log. A single O_APPEND write
// below PIPE_BUF is atomic, so concurrent answerers cannot interleave a
// partial line -- the same property Send's spool relies on -- and no lock is
// needed for the same reason.
func appendAnswer(dir, id string, ans Answer) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	line, err := json.Marshal(ans)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(askAnswersPath(dir, id), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open ask answers: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write ask answer: %w", err)
	}
	return nil
}

// readAnswers reads every answer on file and folds repeats from the same
// session down to the last one, so a changed mind replaces rather than
// duplicates. A missing file means nobody has answered yet, which is not an
// error -- it is the expected state for the first poll of every ask.
func readAnswers(dir, id string) ([]Answer, error) {
	f, err := os.Open(askAnswersPath(dir, id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var all []Answer
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		s := strings.TrimSpace(sc.Text())
		if s == "" {
			continue
		}
		var a Answer
		if err := json.Unmarshal([]byte(s), &a); err != nil {
			continue
		}
		all = append(all, a)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return dedupeAnswers(all), nil
}

// dedupeAnswers keeps one answer per session, the last one written, but in
// the order each session was first seen -- so a peer who answers early and
// then revises later keeps their original place in the tally rather than
// jumping to the bottom.
func dedupeAnswers(all []Answer) []Answer {
	order := make([]string, 0, len(all))
	latest := map[string]Answer{}
	for _, a := range all {
		sid := a.From.SessionID
		if _, seen := latest[sid]; !seen {
			order = append(order, sid)
		}
		latest[sid] = a
	}
	out := make([]Answer, 0, len(order))
	for _, sid := range order {
		out = append(out, latest[sid])
	}
	return out
}

// --- rendering -----------------------------------------------------------

// RenderAskResult renders the tally Ask returns: who answered what, and --
// explicitly, by name -- who did not. This is the text a coordinator actually
// reads before acting on a wait, so it is deliberately never allowed to
// collapse a non-answer into agreement; see this file's doc comment for the
// incident that makes that non-negotiable.
func RenderAskResult(res *AskResult) string {
	inAudience := map[string]bool{}
	for _, m := range res.Audience {
		inAudience[m.SessionID] = true
	}

	answeredBySID := map[string]Answer{}
	var outside []Answer
	for _, a := range res.Answers {
		if inAudience[a.From.SessionID] {
			answeredBySID[a.From.SessionID] = a
		} else {
			// Never folded into the tally above or silently dropped: a
			// question sent to four people and answered by a fifth is not
			// the same event as one of the four actually answering.
			outside = append(outside, a)
		}
	}

	counts := map[string]int{}
	var blockedRows, objectRows, okRows, noAnswerRows []string
	for _, m := range res.Audience {
		a, ok := answeredBySID[m.SessionID]
		if !ok {
			counts["no answer"]++
			noAnswerRows = append(noAnswerRows,
				askRow("no answer", m.Display()+" ("+nonAnswerStatus(m)+")", ""))
			continue
		}
		counts[a.Verdict]++
		row := askRow(a.Verdict, m.Display(), a.Note)
		switch a.Verdict {
		case VerdictBlocked:
			blockedRows = append(blockedRows, row)
		case VerdictObject:
			objectRows = append(objectRows, row)
		default:
			okRows = append(okRows, row)
		}
	}

	var summary []string
	for _, v := range []string{VerdictOK, VerdictObject, VerdictBlocked} {
		if n := counts[v]; n > 0 {
			summary = append(summary, fmt.Sprintf("%d %s", n, v))
		}
	}
	if n := counts["no answer"]; n > 0 {
		summary = append(summary, fmt.Sprintf("%d no answer", n))
	}
	tally := strings.Join(summary, ", ")
	if tally == "" {
		// Nobody was live to ask in the first place -- an empty audience, not
		// a silent one. Said outright rather than printing an empty tally,
		// the same reasoning mcpPublish already applies when nobody is
		// listening on a topic.
		tally = "nobody was there to ask"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "ask %s closed after %s: %s (of %d asked)\n",
		res.ID, res.ClosedAt.Round(time.Second), tally, len(res.Audience))
	// Whatever needs attention leads: blocked and object first, then ok, then
	// -- last, because it is what the reader most needs to notice is
	// distinct from the rest -- everyone who simply never answered.
	for _, r := range blockedRows {
		b.WriteString(r + "\n")
	}
	for _, r := range objectRows {
		b.WriteString(r + "\n")
	}
	for _, r := range okRows {
		b.WriteString(r + "\n")
	}
	for _, r := range noAnswerRows {
		b.WriteString(r + "\n")
	}
	if len(outside) > 0 {
		b.WriteString("  answered but not asked (outside the audience, not counted toward quorum):\n")
		for _, a := range outside {
			b.WriteString("    " + askRow(a.Verdict, a.From.Display(), a.Note) + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// askRow formats one line of a tally: a two-space indent, the verdict (or
// "no answer") left-padded so the names that follow line up, then who and --
// when given -- why.
func askRow(label, who, note string) string {
	line := fmt.Sprintf("  %-6s  %s", label, who)
	if note != "" {
		line += " -- " + note
	}
	return line
}

// nonAnswerStatus reports a non-answering audience member's status as of now
// -- live, deaf, or dead -- because "did not answer" means something
// different for a session mid-turn than for one that vanished entirely, and
// folding the two together is exactly the silence-as-consent mistake Ask
// exists to prevent. A member whose entry cannot be found at all -- pruned,
// or the namespace itself is gone -- reads as "gone" rather than erroring:
// that is itself informative, not a failure to report.
func nonAnswerStatus(m AskMember) string {
	ns, err := ParseNamespace(m.Namespace)
	if err != nil {
		ns = DefaultNamespace()
	}
	e, err := ns.ReadEntry(m.SessionID)
	if err != nil {
		return "gone"
	}
	switch e.Status {
	case StatusLive:
		return "live"
	case StatusDeaf:
		return "deaf"
	default:
		return "dead"
	}
}

// --- retention -------------------------------------------------------------

// askRetention is how long a closed ask's record and answer log survive
// prune. Ask blocks until its own deadline, so by the time that deadline has
// passed the ask is guaranteed to be closed -- there is no "still open" state
// prune could delete out from under a caller still waiting on it. Long enough
// that "what happened with that ask" stays answerable well after the fact;
// bounded so a machine that prunes periodically does not keep one file pair
// per ask forever.
const askRetention = 24 * time.Hour

// PruneAsks removes this namespace's closed ask records (and their answer
// logs). See askRetention for what "closed" is measured from.
func (n Namespace) PruneAsks() (int, error) { return pruneAskDir(n.AsksDir()) }

// PruneSharedAsks is PruneAsks for machine-wide "@" topic asks.
func PruneSharedAsks() (int, error) { return pruneAskDir(SharedAsksDir()) }

func pruneAskDir(dir string) (int, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return 0, err
	}
	removed := 0
	now := time.Now()
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var rec askRecord
		if err := json.Unmarshal(b, &rec); err != nil {
			continue
		}
		ts, err := time.Parse(time.RFC3339, rec.TS)
		if err != nil {
			continue
		}
		closedBy := ts.Add(time.Duration(rec.DeadlineSec) * time.Second)
		if now.Sub(closedBy) <= askRetention {
			continue
		}
		id := strings.TrimSuffix(filepath.Base(p), ".json")
		if os.Remove(p) == nil {
			removed++
		}
		_ = os.Remove(askAnswersPath(dir, id))
	}
	return removed, nil
}
