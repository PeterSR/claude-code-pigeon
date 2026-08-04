package pigeon

import (
	"strings"
	"testing"
)

// --- ReplyTo / Thread --------------------------------------------------------

// A reply's ReplyTo is stamped through, and Thread is derived from it -- the
// one-hop approximation described on Send's Thread comment, since Send has no
// history to look the parent's own Thread up in.
func TestSendReplyToSetsThread(t *testing.T) {
	withHome(t)
	beta := liveEntry(t, "bbbb2222", "beta", "/tmp/work")
	from := Sender{Kind: "shell", Name: "test"}

	root, err := Send(beta, Draft{Text: "kicking off the release"}, from)
	if err != nil {
		t.Fatalf("Send (root): %v", err)
	}
	reply, err := Send(beta, Draft{Text: "sounds good", ReplyTo: root.ID}, from)
	if err != nil {
		t.Fatalf("Send (reply): %v", err)
	}
	if reply.ReplyTo != root.ID {
		t.Errorf("ReplyTo = %q, want %q", reply.ReplyTo, root.ID)
	}
	if reply.Thread != root.ID {
		t.Errorf("Thread = %q, want %q (the parent's id)", reply.Thread, root.ID)
	}
	if root.Thread != "" {
		t.Errorf("the root's own Thread = %q, want empty: it never named a parent", root.Thread)
	}
}

// Publish carries the same behaviour as Send.
func TestPublishReplyToSetsThread(t *testing.T) {
	withHome(t)
	from := Sender{Kind: "shell", Name: "test"}

	root, err := Publish("deploys", Draft{Text: "shipping v2"}, from)
	if err != nil {
		t.Fatalf("Publish (root): %v", err)
	}
	reply, err := Publish("deploys", Draft{Text: "+1", ReplyTo: root.ID}, from)
	if err != nil {
		t.Fatalf("Publish (reply): %v", err)
	}
	if reply.Thread != root.ID {
		t.Errorf("Thread = %q, want %q", reply.Thread, root.ID)
	}
}

// ReplyTo gets the same shape and self-reference defence Supersedes already
// has, on both Send and Publish.
func TestReplyToRejectsAnythingThatDoesNotLookLikeAMessageID(t *testing.T) {
	withHome(t)
	beta := liveEntry(t, "bbbb2222", "beta", "/tmp/work")
	from := Sender{Kind: "shell", Name: "test"}

	for _, bad := range []string{"not-an-id", "m_", "m_deadbeef", "m_DEADBEEF1234"} {
		if _, err := Send(beta, Draft{Text: "hi", ReplyTo: bad}, from); err == nil {
			t.Errorf("Send with replyTo %q should have been rejected", bad)
		}
		if _, err := Publish("deploys", Draft{Text: "hi", ReplyTo: bad}, from); err == nil {
			t.Errorf("Publish with replyTo %q should have been rejected", bad)
		}
	}
}

func TestReplyToRejectsSelfReference(t *testing.T) {
	if _, err := validateReplyTo("m_deadbeef1234", "m_deadbeef1234"); err == nil {
		t.Error("validateReplyTo accepted a message naming itself")
	}
	if got, err := validateReplyTo("", "m_someother123"); err != nil || got != "" {
		t.Errorf("validateReplyTo(\"\") = (%q, %v), want (\"\", nil)", got, err)
	}
}

// --- inbox grouping ----------------------------------------------------------

func inboxItem(id, replyTo, thread, text string) InboxItem {
	m := &Message{
		ID: id, TS: nowRFC3339(), Thread: thread, ReplyTo: replyTo,
		From: Sender{Kind: "session", SessionID: "aaaa1111", Name: "alpha"},
		Topic: "deploys", Text: text,
	}
	return InboxItem{Message: m, Source: "deploys"}
}

// Two consecutive replies sharing a Thread render under one header line
// naming the thread and how many messages it holds, and -- since two is not
// "more than two" -- both are still shown in full underneath it.
func TestRenderInboxGroupsAThreadUnderOneHeader(t *testing.T) {
	items := []InboxItem{
		inboxItem("m_root000001a", "", "", "kicking off the release"),
		inboxItem("m_reply00001a", "m_root000001a", "m_root000001a", "sounds good"),
		inboxItem("m_reply00002a", "m_root000001a", "m_root000001a", "ship it"),
	}
	got := RenderInbox(items, 0, true, "brief", "--all", nil, Namespace{})
	if !strings.Contains(got, "thread m_root000001a (2 messages)") {
		t.Errorf("no thread header for the two grouped replies:\n%s", got)
	}
	// The root itself is not part of the group (its own Thread is empty), so
	// it must still appear as an ordinary item, not folded into the header.
	if !strings.Contains(got, "kicking off the release") {
		t.Errorf("the root message went missing from the render:\n%s", got)
	}
	// Both replies are still fully shown alongside the header at run length 2.
	if !strings.Contains(got, "sounds good") || !strings.Contains(got, "ship it") {
		t.Errorf("a two-message thread was collapsed instead of shown in full:\n%s", got)
	}
}

// A thread of more than two collapses to the header line alone at any detail
// short of "full" -- this is the whole point: four readers stop costing four
// blocks.
func TestRenderInboxCollapsesALongThreadUnlessFull(t *testing.T) {
	items := []InboxItem{
		inboxItem("m_root000002a", "", "", "kicking off the release"),
		inboxItem("m_reply00003a", "m_root000002a", "m_root000002a", "sounds good"),
		inboxItem("m_reply00004a", "m_root000002a", "m_root000002a", "ship it"),
		inboxItem("m_reply00005a", "m_root000002a", "m_root000002a", "already tagged"),
	}
	brief := RenderInbox(items, 0, true, "brief", "--all", nil, Namespace{})
	if !strings.Contains(brief, "thread m_root000002a (3 messages)") {
		t.Errorf("no collapsed thread header:\n%s", brief)
	}
	if strings.Contains(brief, "already tagged") {
		t.Errorf("a 3-message thread was not collapsed at brief detail:\n%s", brief)
	}

	full := RenderInbox(items, 0, true, "full", "--all", nil, Namespace{})
	if !strings.Contains(full, "thread m_root000002a (3 messages)") {
		t.Errorf("no thread header at full detail:\n%s", full)
	}
	if !strings.Contains(full, "already tagged") {
		t.Errorf("full detail must expand a collapsed thread:\n%s", full)
	}
}

// --- ReadThread ---------------------------------------------------------------

// ReadThread walks ReplyTo out from the named id in both directions, so a
// grandchild reply (whose Thread only ever names its immediate parent, not
// the root -- see Send's comment) is still recovered as part of the same
// conversation.
func TestReadThreadWalksTheWholeChainAcrossHops(t *testing.T) {
	withHome(t)
	me := liveEntry(t, "aaaa1111", "me", "/tmp/a")
	from := Sender{Kind: "shell", Name: "peer"}

	root, err := Send(me, Draft{Text: "kicking off the release"}, from)
	if err != nil {
		t.Fatalf("Send (root): %v", err)
	}
	reply, err := Send(me, Draft{Text: "sounds good", ReplyTo: root.ID}, from)
	if err != nil {
		t.Fatalf("Send (reply): %v", err)
	}
	grandchild, err := Send(me, Draft{Text: "ship it then", ReplyTo: reply.ID}, from)
	if err != nil {
		t.Fatalf("Send (grandchild): %v", err)
	}
	// grandchild.Thread names reply.ID, not root.ID -- confirming the very
	// limitation ReadThread exists to see past.
	if grandchild.Thread != reply.ID {
		t.Fatalf("test setup: grandchild.Thread = %q, want %q", grandchild.Thread, reply.ID)
	}

	got, err := CurrentNamespace().ReadThread(me.SessionID, root.ID)
	if err != nil {
		t.Fatalf("ReadThread: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("ReadThread returned %d items, want 3 (root, reply, grandchild): %+v", len(got), got)
	}
	ids := map[string]bool{}
	for _, it := range got {
		ids[it.Message.ID] = true
	}
	for _, want := range []string{root.ID, reply.ID, grandchild.ID} {
		if !ids[want] {
			t.Errorf("ReadThread result missing %s", want)
		}
	}
	// Oldest first.
	if got[0].Message.ID != root.ID {
		t.Errorf("first item = %s, want the root %s", got[0].Message.ID, root.ID)
	}
}

func TestReadThreadRejectsAnUnknownID(t *testing.T) {
	withHome(t)
	me := liveEntry(t, "aaaa1111", "me", "/tmp/a")
	if _, err := CurrentNamespace().ReadThread(me.SessionID, "m_notarealid1"); err == nil {
		t.Error("ReadThread accepted an id in no log this session can see")
	}
}
