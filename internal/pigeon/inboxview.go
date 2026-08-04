package pigeon

import (
	"fmt"
	"strings"
	"time"
)

// This file is the one rendering path for a pulled inbox -- ReadInbox
// (inbox.go) returns data, this turns it into the text a model or a terminal
// reads. Both the MCP inbox tool (mcp.go) and `pigeon inbox` (cmd/pigeon)
// call RenderInbox so the two surfaces never drift apart.

// ResolveInboxDetail normalises the "detail" argument shared by the inbox MCP
// tool and the `pigeon inbox` CLI: "" defaults to "full", anything else must
// be one of the two values the renderer understands. Rejecting a typo here
// is better than silently treating it as "full" and hiding the mistake.
func ResolveInboxDetail(s string) (string, error) {
	switch s {
	case "", "full":
		return "full", nil
	case "subject":
		return "subject", nil
	default:
		return "", fmt.Errorf("detail must be \"full\" or \"subject\", got %q", s)
	}
}

// RenderInbox renders the result of a ReadInbox call. unreadOnly and detail
// are the query's own settings, echoed back rather than re-derived, because
// the render has to describe what was actually asked for even when that
// leaves nothing to show. hint is the escape hatch to mention when
// unreadOnly turned up nothing -- "unread_only: false" from MCP, "--all" from
// the CLI -- since the two surfaces spell the same flag differently.
//
// Newest message is printed LAST: ReadInbox already returns items oldest
// first, so the text a caller most likely needs next sits closest to the end
// of the output, nearest the model's next token.
func RenderInbox(items []InboxItem, unreadOnly bool, detail, hint string) string {
	if len(items) == 0 {
		if unreadOnly {
			return fmt.Sprintf("No unread messages. Pass %s to see recent history.", hint)
		}
		return "No messages."
	}

	var b strings.Builder
	b.WriteString(inboxSummary(items, unreadOnly))
	b.WriteByte('\n')
	for _, it := range items {
		writeInboxItem(&b, it, detail)
	}
	return strings.TrimRight(b.String(), "\n")
}

// inboxSummary is the one-line lead: how many messages, and where from, so a
// caller can tell at a glance whether this is one topic's chatter or several
// sources worth reading differently -- before spending tokens on the bodies.
func inboxSummary(items []InboxItem, unreadOnly bool) string {
	counts := map[string]int{}
	var topics []string
	direct := 0
	for _, it := range items {
		if it.Source == "" {
			direct++
			continue
		}
		if counts[it.Source] == 0 {
			topics = append(topics, it.Source)
		}
		counts[it.Source]++
	}

	// "unread" belongs to the whole batch, not to the topic parts: when
	// UnreadOnly is set every item is unread, direct ones included, and
	// tagging only the topics read as though the direct messages were
	// something else.
	var parts []string
	for _, topic := range topics {
		parts = append(parts, fmt.Sprintf("%d on %s", counts[topic], TopicLabel(topic)))
	}
	if direct > 0 {
		parts = append(parts, fmt.Sprintf("%d direct", direct))
	}

	noun := "messages"
	if len(items) == 1 {
		noun = "message"
	}
	if unreadOnly {
		noun = "unread"
	}
	return fmt.Sprintf("%d %s (%s):", len(items), noun, strings.Join(parts, ", "))
}

// writeInboxItem appends one message's block: a header line dense enough to
// triage without opening the body, the subject if there is one, and -- unless
// detail is "subject" -- the full body. Nothing here is truncated: a pull is
// a Read, not a push bound by BodyBudget, and the whole point of the tool is
// that draining a burst this way costs less than the notifications it
// replaces.
func writeInboxItem(b *strings.Builder, it InboxItem, detail string) {
	ts := "?"
	if t, err := time.Parse(time.RFC3339, it.Message.TS); err == nil {
		ts = t.Local().Format("15:04")
	}
	fmt.Fprintf(b, "%s  %s  %s", it.Message.ID, ts, it.Message.From.Display())
	if it.Source != "" {
		// Direct messages show no topic column: there is nothing to name.
		fmt.Fprintf(b, "  %s", TopicLabel(it.Source))
	}
	fmt.Fprintf(b, "  (%s)\n", formatAge(it.Age))
	if it.Message.Subject != "" {
		fmt.Fprintf(b, "  SUBJECT: %s\n", it.Message.Subject)
	}
	if detail != "subject" {
		fmt.Fprintf(b, "  %s\n", it.Message.Text)
	}
}

// formatAge renders elapsed time the way a skimmed notification line wants
// it, not the way Duration.String does: "just now", then whole minutes, then
// whole hours, then whole days -- no fractional seconds, no dependency, just
// the tier a human actually reads.
func formatAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(d/time.Hour))
	default:
		return fmt.Sprintf("%d d ago", int(d/(24*time.Hour)))
	}
}
