package pigeon

import (
	"strings"
	"testing"
)

// RenderInbox and writeInboxItem apply the same "-> you" marker Render does,
// keyed off the same Message.IsFor -- so a pull and a push notification agree
// on who a topic message was actually for.

// A topic message naming the viewing session shows "-> you" in its header;
// one that does not, or a viewer pigeon does not know, shows today's exact
// output.
func TestRenderInboxMarksAnItemAddressedToTheViewer(t *testing.T) {
	m := &Message{
		ID: "m_i1", TS: nowRFC3339(),
		From: Sender{Kind: "shell", Name: "sh"}, Topic: "deploys",
		Text: "roll it back", For: []string{"beta"},
	}
	items := []InboxItem{{Message: m, Source: "deploys"}}

	addressed := &Entry{SessionID: "bbbb2222", Name: "beta"}
	got := RenderInbox(items, 0, true, "brief", "--all", addressed)
	if !strings.Contains(got, "-> you") {
		t.Errorf("RenderInbox for the addressed session did not show the marker:\n%s", got)
	}

	notNamed := &Entry{SessionID: "cccc3333", Name: "gamma"}
	gotUnaddressed := RenderInbox(items, 0, true, "brief", "--all", notNamed)
	if strings.Contains(gotUnaddressed, "-> you") {
		t.Errorf("RenderInbox showed the marker to a session the message does not name:\n%s", gotUnaddressed)
	}

	gotUnknown := RenderInbox(items, 0, true, "brief", "--all", nil)
	if strings.Contains(gotUnknown, "-> you") {
		t.Errorf("RenderInbox showed the marker for an unknown viewer:\n%s", gotUnknown)
	}
	// An unknown viewer's rendering is otherwise identical to a named,
	// unaddressed one: the only difference either could ever produce is the
	// marker, and neither gets it.
	if gotUnaddressed != gotUnknown {
		t.Errorf("RenderInbox(not addressed) = %q, RenderInbox(unknown viewer) = %q; want identical", gotUnaddressed, gotUnknown)
	}
}

// A message with no For at all is for everyone, so nobody -- however they
// are named -- sees the marker.
func TestRenderInboxShowsNoMarkerWhenForIsEmpty(t *testing.T) {
	m := &Message{
		ID: "m_i2", TS: nowRFC3339(),
		From: Sender{Kind: "shell", Name: "sh"}, Topic: "deploys",
		Text: "everyone please note",
	}
	items := []InboxItem{{Message: m, Source: "deploys"}}
	self := &Entry{SessionID: "bbbb2222", Name: "beta"}
	if got := RenderInbox(items, 0, true, "brief", "--all", self); strings.Contains(got, "-> you") {
		t.Errorf("RenderInbox showed the marker for a message with no For:\n%s", got)
	}
}

// A direct message has no topic and can never carry a non-empty For (Send
// rejects it), but a hand-written spool line is not bound by that -- so
// writeInboxItem must gate the marker on Topic != "" the same way Render
// does, not on For alone.
func TestRenderInboxNeverMarksADirectMessage(t *testing.T) {
	m := &Message{
		ID: "m_i3", TS: nowRFC3339(),
		From: Sender{Kind: "shell", Name: "sh"},
		Text: "a direct message with a hand-planted For", For: []string{"beta"},
	}
	items := []InboxItem{{Message: m}}
	self := &Entry{SessionID: "bbbb2222", Name: "beta"}
	if got := RenderInbox(items, 0, true, "brief", "--all", self); strings.Contains(got, "-> you") {
		t.Errorf("RenderInbox marked a direct message as addressed:\n%s", got)
	}
}

// --- supersedes --------------------------------------------------------------

// The one case supersedeLinks exists for: both messages of a legitimate pair
// -- same sender -- sit in the same returned batch. The original shows it
// was superseded, naming the correction; the correction shows what it
// corrects, naming the original.
func TestRenderInboxMarksBothSidesOfASupersedeWithinTheBatch(t *testing.T) {
	from := Sender{Kind: "session", SessionID: "aaaa1111", Name: "alpha"}
	orig := &Message{ID: "m_orig00001a", TS: nowRFC3339(), From: from, Topic: "alerts", Text: "STOP AND READ"}
	corr := &Message{ID: "m_corr00001a", TS: nowRFC3339(), From: from, Topic: "alerts", Text: "false alarm", Supersedes: orig.ID}
	items := []InboxItem{{Message: orig, Source: "alerts"}, {Message: corr, Source: "alerts"}}

	got := RenderInbox(items, 0, true, "brief", "--all", nil)
	if !strings.Contains(got, "[SUPERSEDED by "+corr.ID+"]") {
		t.Errorf("the superseded message was not marked:\n%s", got)
	}
	if !strings.Contains(got, "[correction of "+orig.ID+"]") {
		t.Errorf("the correcting message was not marked:\n%s", got)
	}
}

// A supersede claim naming a message actually sent by someone else must not
// be honoured just because both happen to sit in the same batch -- the same
// sender-must-match rule the monitor enforces at delivery (resolveSupersede
// in monitor.go), reapplied here since ReadInbox is a wholly separate path.
func TestRenderInboxDoesNotLinkASupersedeFromADifferentSenderWithinTheBatch(t *testing.T) {
	origSender := Sender{Kind: "session", SessionID: "aaaa1111", Name: "alpha"}
	impostor := Sender{Kind: "session", SessionID: "bbbb2222", Name: "beta"}
	orig := &Message{ID: "m_orig00002a", TS: nowRFC3339(), From: origSender, Topic: "alerts", Text: "STOP AND READ"}
	fake := &Message{ID: "m_fake00002a", TS: nowRFC3339(), From: impostor, Topic: "alerts", Text: "false alarm", Supersedes: orig.ID}
	items := []InboxItem{{Message: orig, Source: "alerts"}, {Message: fake, Source: "alerts"}}

	got := RenderInbox(items, 0, true, "brief", "--all", nil)
	if strings.Contains(got, "SUPERSEDED") || strings.Contains(got, "correction of") {
		t.Errorf("a cross-sender supersede claim was honoured within the batch:\n%s", got)
	}
}

// supersedeLinks works only within the batch it is handed: a claim naming an
// id that is not present in this batch -- because it was already read, or
// simply fell outside the page returned -- links to nothing. This documents
// that limit rather than hiding it.
func TestRenderInboxIgnoresASupersedeNamingAnIDOutsideTheBatch(t *testing.T) {
	from := Sender{Kind: "session", SessionID: "aaaa1111", Name: "alpha"}
	m := &Message{ID: "m_corr00003a", TS: nowRFC3339(), From: from, Topic: "alerts", Text: "false alarm", Supersedes: "m_notinbatch1"}
	items := []InboxItem{{Message: m, Source: "alerts"}}

	got := RenderInbox(items, 0, true, "brief", "--all", nil)
	if strings.Contains(got, "correction of") {
		t.Errorf("supersedeLinks matched an id outside the batch:\n%s", got)
	}
}
