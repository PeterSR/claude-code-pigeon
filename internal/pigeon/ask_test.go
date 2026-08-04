package pigeon

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// waitForAskID polls for the ask record Ask() writes before it publishes the
// question, so a test can discover the id a concurrent Ask call generated in
// order to answer it while that call is still blocking.
func waitForAskID(t *testing.T, ns Namespace) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		paths, _ := filepath.Glob(filepath.Join(ns.AsksDir(), "*.json"))
		if len(paths) == 1 {
			return strings.TrimSuffix(filepath.Base(paths[0]), ".json")
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for an ask record to appear")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// askAsync runs Ask in a goroutine and returns a channel for its result, so a
// test can answer while the blocking call is still in flight.
func askAsync(ns Namespace, topic string, d Draft, from Sender, deadline time.Duration) <-chan struct {
	res *AskResult
	err error
} {
	out := make(chan struct {
		res *AskResult
		err error
	}, 1)
	go func() {
		res, err := ns.Ask(topic, d, from, deadline)
		out <- struct {
			res *AskResult
			err error
		}{res, err}
	}()
	return out
}

func TestAskReturnsEarlyOnFullQuorum(t *testing.T) {
	withHome(t)
	ns := DefaultNamespace()
	const topic = "deploys"

	a := armed(t, "peer-aaaaaaaa", "peer-a")
	b := armed(t, "peer-bbbbbbbb", "peer-b")
	if err := ns.Subscribe(a.SessionID, topic); err != nil {
		t.Fatal(err)
	}
	if err := ns.Subscribe(b.SessionID, topic); err != nil {
		t.Fatal(err)
	}

	from := Sender{Kind: "session", SessionID: "asker-1", Name: "asker"}
	done := askAsync(ns, topic, Draft{Text: "is anyone running git?"}, from, 5*time.Second)

	id := waitForAskID(t, ns)
	if err := ns.Answer(id, Sender{Kind: "session", SessionID: a.SessionID, Name: a.Name}, VerdictOK, ""); err != nil {
		t.Fatalf("Answer(a): %v", err)
	}
	if err := ns.Answer(id, Sender{Kind: "session", SessionID: b.SessionID, Name: b.Name}, VerdictOK, ""); err != nil {
		t.Fatalf("Answer(b): %v", err)
	}

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Ask: %v", r.err)
		}
		if !r.res.Quorum {
			t.Error("Quorum = false, want true: both audience members answered")
		}
		if r.res.ClosedAt >= 5*time.Second {
			t.Errorf("ClosedAt = %s, did not return early", r.res.ClosedAt)
		}
		if len(r.res.Answers) != 2 {
			t.Errorf("len(Answers) = %d, want 2", len(r.res.Answers))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Ask did not return once every audience member had answered")
	}
}

func TestAskReportsNonAnswerersWithStatusAtDeadline(t *testing.T) {
	withHome(t)
	ns := DefaultNamespace()
	const topic = "deploys"

	// liveMember stays live for the whole test and simply never answers.
	liveMember := liveEntry(t, "peer-live0000", "peer-live", "/tmp/live")
	liveMember.HeartbeatAt = nowRFC3339()
	if err := ns.WriteEntry(liveMember); err != nil {
		t.Fatal(err)
	}
	liveLock, ok, err := tryExclusive(ns.LockPath(liveMember.SessionID))
	if err != nil || !ok {
		t.Fatalf("lock live member: ok=%v err=%v", ok, err)
	}
	t.Cleanup(func() { liveLock.Close() })

	// deafMember is live at ask time -- so it is in the audience -- but its
	// monitor goes away before the deadline fires. The report at close must
	// say deaf, not silently repeat what was true when it was asked.
	deafMember := liveEntry(t, "peer-deaf0000", "peer-deaf", "/tmp/deaf")
	deafMember.HeartbeatAt = nowRFC3339()
	if err := ns.WriteEntry(deafMember); err != nil {
		t.Fatal(err)
	}
	deafLock, ok, err := tryExclusive(ns.LockPath(deafMember.SessionID))
	if err != nil || !ok {
		t.Fatalf("lock deaf member: ok=%v err=%v", ok, err)
	}

	for _, id := range []string{liveMember.SessionID, deafMember.SessionID} {
		if err := ns.Subscribe(id, topic); err != nil {
			t.Fatal(err)
		}
	}

	from := Sender{Kind: "session", SessionID: "asker-1", Name: "asker"}
	done := askAsync(ns, topic, Draft{Text: "anyone running git?"}, from, 500*time.Millisecond)

	waitForAskID(t, ns)
	// Take the deaf member's monitor down before the deadline fires.
	deafLock.Close()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Ask: %v", r.err)
		}
		if r.res.Quorum {
			t.Error("Quorum = true, want false: nobody answered")
		}
		if len(r.res.Answers) != 0 {
			t.Errorf("len(Answers) = %d, want 0", len(r.res.Answers))
		}
		rendered := RenderAskResult(r.res)
		if !strings.Contains(rendered, "no answer  peer-live (live)") {
			t.Errorf("rendering did not report peer-live as a live non-answerer:\n%s", rendered)
		}
		if !strings.Contains(rendered, "no answer  peer-deaf (deaf)") {
			t.Errorf("rendering did not report peer-deaf as a deaf non-answerer:\n%s", rendered)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Ask did not return by its deadline")
	}
}

func TestAnswerFromOutsideAudienceIsReportedSeparately(t *testing.T) {
	withHome(t)
	ns := DefaultNamespace()
	const topic = "deploys"

	member := armed(t, "peer-in0000000", "peer-in")
	if err := ns.Subscribe(member.SessionID, topic); err != nil {
		t.Fatal(err)
	}

	from := Sender{Kind: "session", SessionID: "asker-1", Name: "asker"}
	done := askAsync(ns, topic, Draft{Text: "anyone running git?"}, from, 400*time.Millisecond)

	id := waitForAskID(t, ns)
	// A drive-by session that was never part of the audience still gets its
	// answer recorded, but it must never count toward quorum or be presented
	// as though it were one of the people actually asked.
	outsider := Sender{Kind: "session", SessionID: "outsider-1", Name: "outsider"}
	if err := ns.Answer(id, outsider, VerdictOK, "fyi"); err != nil {
		t.Fatalf("Answer(outsider): %v", err)
	}

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Ask: %v", r.err)
		}
		if r.res.Quorum {
			t.Error("Quorum = true, want false: the real audience member never answered")
		}
		if len(r.res.Answers) != 1 {
			t.Fatalf("len(Answers) = %d, want 1", len(r.res.Answers))
		}
		rendered := RenderAskResult(r.res)
		if !strings.Contains(rendered, "outside the audience") {
			t.Errorf("rendering did not call out the outsider's answer separately:\n%s", rendered)
		}
		if !strings.Contains(rendered, "outsider") {
			t.Errorf("rendering dropped the outsider's answer entirely:\n%s", rendered)
		}
		if !strings.Contains(rendered, "no answer  peer-in") {
			t.Errorf("rendering did not report the real audience member as unanswered:\n%s", rendered)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Ask did not return by its deadline")
	}
}

func TestSecondAnswerFromTheSameSessionReplacesTheFirst(t *testing.T) {
	withHome(t)
	ns := DefaultNamespace()
	const topic = "deploys"

	member := armed(t, "peer-in0000000", "peer-in")
	if err := ns.Subscribe(member.SessionID, topic); err != nil {
		t.Fatal(err)
	}

	from := Sender{Kind: "session", SessionID: "asker-1", Name: "asker"}
	done := askAsync(ns, topic, Draft{Text: "anyone running git?"}, from, 5*time.Second)

	id := waitForAskID(t, ns)
	answerer := Sender{Kind: "session", SessionID: member.SessionID, Name: member.Name}
	if err := ns.Answer(id, answerer, VerdictObject, "wait, no"); err != nil {
		t.Fatal(err)
	}
	if err := ns.Answer(id, answerer, VerdictOK, "actually fine"); err != nil {
		t.Fatal(err)
	}

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Ask: %v", r.err)
		}
		if !r.res.Quorum {
			t.Error("Quorum = false, want true: the one audience member did answer")
		}
		if len(r.res.Answers) != 1 {
			t.Fatalf("len(Answers) = %d, want 1 (a second answer replaces the first, not adds to it)", len(r.res.Answers))
		}
		if r.res.Answers[0].Verdict != VerdictOK {
			t.Errorf("Verdict = %q, want %q (last answer wins)", r.res.Answers[0].Verdict, VerdictOK)
		}
		if r.res.Answers[0].Note != "actually fine" {
			t.Errorf("Note = %q, want the second note", r.res.Answers[0].Note)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Ask did not return once the replaced answer completed quorum")
	}
}

func TestAskRejectsADeadlineOverTheMax(t *testing.T) {
	withHome(t)
	ns := DefaultNamespace()
	from := Sender{Kind: "session", SessionID: "asker-1", Name: "asker"}
	_, err := ns.Ask("deploys", Draft{Text: "hi"}, from, AskMaxDeadline+time.Second)
	if err == nil {
		t.Fatal("Ask did not reject a deadline over the maximum")
	}
	if !strings.Contains(err.Error(), "belongs in a published message") {
		t.Errorf("error does not explain the alternative: %v", err)
	}
	// Nothing should have been written for a rejected ask.
	paths, _ := filepath.Glob(filepath.Join(ns.AsksDir(), "*.json"))
	if len(paths) != 0 {
		t.Errorf("a rejected ask left %d record(s) behind", len(paths))
	}
}

func TestRenderedAskTallyNeverImpliesConsentOnANonAnswer(t *testing.T) {
	withHome(t)
	res := &AskResult{
		ID: "m_deadbeef0000",
		Audience: []AskMember{
			{SessionID: "s1", Name: "inv-invoices", Namespace: DefaultNamespaceName},
		},
		ClosedAt: 5 * time.Second,
		Quorum:   false,
	}
	out := RenderAskResult(res)
	lower := strings.ToLower(out)
	for _, phrase := range []string{"no objections", "all clear", "nobody objected", "consensus"} {
		if strings.Contains(lower, phrase) {
			t.Errorf("rendering contains %q, which implies consent from a non-answer:\n%s", phrase, out)
		}
	}
	if !strings.Contains(out, "no answer") {
		t.Errorf("rendering did not explicitly call out the non-answer:\n%s", out)
	}
}

func TestAskRejectsAnEmptyQuestion(t *testing.T) {
	withHome(t)
	ns := DefaultNamespace()
	from := Sender{Kind: "session", SessionID: "asker-1", Name: "asker"}
	if _, err := ns.Ask("deploys", Draft{Text: "   "}, from, time.Second); err == nil {
		t.Fatal("Ask accepted an empty question")
	}
}

func TestAnswerRejectsAnInvalidVerdict(t *testing.T) {
	withHome(t)
	ns := DefaultNamespace()
	from := Sender{Kind: "session", SessionID: "s1", Name: "s1"}
	if err := ns.Answer("m_000000000000", from, "maybe", ""); err == nil {
		t.Fatal("Answer accepted an invalid verdict")
	}
}

func TestAnswerRejectsAShellSender(t *testing.T) {
	withHome(t)
	ns := DefaultNamespace()
	shell := Sender{Kind: "shell", Name: "shell:me@host"}
	if err := ns.Answer("m_000000000000", shell, VerdictOK, ""); err == nil {
		t.Fatal("Answer accepted a shell sender, which has no session id to key quorum on")
	}
}

func TestSendRejectsAnAskID(t *testing.T) {
	withHome(t)
	ns := DefaultNamespace()
	to := liveEntry(t, "target-1", "target", "/tmp/x")
	from := Sender{Kind: "session", SessionID: "s1"}
	if _, err := ns.Send(to, Draft{Text: "hi", AskID: "m_000000000000"}, from); err == nil {
		t.Fatal("Send accepted an AskID on a direct message")
	}
}

func TestRenderIncludesAnAskHint(t *testing.T) {
	m := &Message{
		ID:    "m_aaaaaaaaaaaa",
		From:  Sender{Kind: "session", SessionID: "abcdefgh-1", Name: "asker"},
		Topic: "deploys",
		Text:  "is anyone running git?",
		AskID: "m_9f2c1a2b3c4d",
	}
	got := DefaultNamespace().Render(m, nil)
	want := "[ask: pigeon answer m_9f2c1a2b3c4d ok|object|blocked]"
	if !strings.Contains(got, want) {
		t.Errorf("Render did not include the ask hint:\n%s", got)
	}
	if len([]rune(got)) > RenderBudget {
		t.Errorf("Render with an ask hint exceeded RenderBudget: %d chars", len([]rune(got)))
	}
}

func TestRenderIgnoresAMalformedAskID(t *testing.T) {
	m := &Message{
		ID:    "m_aaaaaaaaaaaa",
		From:  Sender{Kind: "session", SessionID: "abcdefgh-1", Name: "asker"},
		Topic: "deploys",
		Text:  "hello",
		AskID: "not-a-real-id",
	}
	got := DefaultNamespace().Render(m, nil)
	if strings.Contains(got, "[ask:") {
		t.Errorf("Render trusted a hand-written, malformed AskID:\n%s", got)
	}
}
