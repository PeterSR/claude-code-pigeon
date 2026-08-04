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
