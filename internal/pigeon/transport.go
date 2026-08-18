package pigeon

import (
	"fmt"
	"os"
	"strings"
)

// pigeon has two ways to wake a recipient, and only one of them existed first.
//
// The spool is doing two jobs at once. It is the durable RECORD every read path
// keys off -- inbox, thread, catch-up, supersede, both cursors -- and it is also
// the delivery SIGNAL, because a monitor tails it and prints a notification
// line. Those two jobs come apart cleanly, and only the second has ever been
// the fragile half: a monitor is armed once, by the host, at session start, and
// nothing respawns one that dies. See runtime.go's package comment and the
// README's "How it works" for the two ways that fails, both silently.
//
// A socket push replaces the delivery signal and NOTHING ELSE. The record is
// written exactly as before, so a message delivered over the socket reads back
// identically in `pigeon inbox`, threads the same, and is superseded the same.
// What changes is only who does the waking: pigeon itself, synchronously, at
// send time, instead of a background process inside the recipient that may or
// may not still be alive.
//
// That is the whole reason to keep the split rather than let the socket
// transport carry the message outright. A push is fire-and-forget into a
// conversation; it leaves no record either side can read back, cannot be
// threaded, and cannot be caught up on. The spool is what makes a pigeon
// message a message rather than an interruption.

// Transport names how a recipient is woken.
type Transport string

const (
	// TransportAuto pushes over the socket when the recipient can be reached
	// that way, and otherwise leaves the message on the spool for the
	// recipient's monitor. This is the default, and it is the mode in which a
	// session whose monitor has died still gets its mail.
	TransportAuto Transport = "auto"
	// TransportSocket pushes over the socket and reports it when that is not
	// possible, instead of quietly falling back. For a caller that wants to
	// know it reached a live conversation rather than a file.
	TransportSocket Transport = "socket"
	// TransportMonitor is the original path: append and let the recipient's
	// monitor find it. Kept because it is the only transport that can honour
	// the digest and quiet delivery modes -- see wake's comment on that -- and
	// because a caller may want the notification budget and the rate limiter
	// that live in the monitor.
	TransportMonitor Transport = "monitor"
)

// ValidTransport parses a transport name.
func ValidTransport(s string) (Transport, error) {
	switch t := Transport(strings.ToLower(strings.TrimSpace(s))); t {
	case TransportAuto, TransportSocket, TransportMonitor:
		return t, nil
	default:
		return "", fmt.Errorf("unknown transport %q: want auto, socket or monitor", s)
	}
}

// CurrentTransport resolves which transport to use, and reports where the
// answer came from so a surprise can be explained -- the same shape as
// CurrentNamespace's reporting, and for the same reason.
//
// Highest wins: an explicit --via, then PIGEON_TRANSPORT, then the machine
// config, then auto. A per-invocation statement outranks a standing preference,
// because the caller making it knows something about this particular message
// that a file written weeks ago does not.
//
// Deliberately NOT a `.claude/pigeon.json` field. That file arrives with a
// `git clone`, and how your sessions get interrupted is not a cloned
// repository's call -- the same line userconfig.go already draws for privacy.
func CurrentTransport(flag string) (Transport, string) {
	if strings.TrimSpace(flag) != "" {
		t, err := ValidTransport(flag)
		if err != nil {
			return TransportAuto, err.Error() + ", so it was ignored"
		}
		return t, "--via"
	}
	if raw := strings.TrimSpace(os.Getenv(EnvTransport)); raw != "" {
		t, err := ValidTransport(raw)
		if err != nil {
			return TransportAuto, EnvTransport + " is not a usable transport, so it was ignored"
		}
		return t, EnvTransport
	}
	if raw := strings.TrimSpace(LoadUserConfig().Transport); raw != "" {
		t, err := ValidTransport(raw)
		if err == nil {
			return t, UserConfigPath()
		}
	}
	return TransportAuto, "default"
}

// transport is the transport this draft is to be delivered with: what the
// caller asked for, or the resolved default when it asked for nothing.
func (d Draft) transport() Transport {
	if d.Via != "" {
		return d.Via
	}
	t, _ := CurrentTransport("")
	return t
}

// wake delivers the doorbell for msg to each recipient and returns the session
// ids that were actually woken over the socket.
//
// CALLED BEFORE THE MESSAGE IS APPENDED, and that ordering is load-bearing
// rather than incidental. The ids this returns are stamped onto Message.PushedTo
// in the very line that is about to be written, which is how a recipient's
// monitor knows to stay quiet about a message its session has already been
// shown (see RunMonitor's PushedTo branch). A push that fails simply leaves that
// id out, so the monitor announces it exactly as it does today: the fallback
// needs no retraction and no second write.
//
// Nothing is lost by pushing before the record exists. The message id and its
// payload file are both created earlier still, so the rendered line already
// points at everything it needs to; the only window is that a recipient
// reacting within a millisecond or two could run `pigeon inbox` before the line
// lands. The reverse order has no such small cost -- it has an unfixable one,
// because an appended line cannot be un-appended once a push turns out to have
// failed.
//
// Recipients are filtered by the caller, which is where the delivery-mode rules
// belong: Publish knows a subscriber's mode, Send knows a direct message has
// none. See publishTargets.
func (n Namespace) wake(t Transport, msg *Message, recipients []*Entry) (pushed []string, problems []string) {
	if t == TransportMonitor || len(recipients) == 0 {
		return nil, nil
	}
	for _, e := range recipients {
		if e == nil || e.SessionID == "" {
			continue
		}
		if err := socketPush(e, n.Render(msg, e), msg.From.Display()); err != nil {
			if t == TransportSocket {
				problems = append(problems, fmt.Sprintf("%s: %v", e.Addr(), err))
			}
			continue
		}
		pushed = append(pushed, e.SessionID)
	}
	return pushed, problems
}

// AnnotateReach promotes StatusDeaf to StatusSocket for every entry whose
// Claude Code inbox socket answers, so a listing reports what pigeon can
// actually do rather than only what its own monitor can.
//
// Opt-in, and called only from the paths a person is reading -- `pigeon ls`,
// `whoami`, `doctor`, `list_sessions` -- rather than folded into Entry.status.
// A probe is a connection per entry, and status is consulted from inside
// matchingSubscribers, which every publish walks. Putting it there would add a
// round trip per subscriber to every publish to buy an answer the publish is
// about to produce anyway by simply attempting the push.
//
// Only StatusDeaf is promoted. A live monitor is already a delivery path, and
// a dead process has nothing bound to probe.
func AnnotateReach(entries []*Entry) {
	for _, e := range entries {
		if e != nil && e.Status == StatusDeaf && SocketReachable(e) {
			e.Status = StatusSocket
		}
	}
}

// pushedToSession reports whether sessionID was already woken about m over the
// socket. The counterpart of wake, read by RunMonitor to decide whether a
// message still owes this session a notification.
func pushedToSession(m *Message, sessionID string) bool {
	if m == nil || sessionID == "" {
		return false
	}
	for _, id := range m.PushedTo {
		if id == sessionID {
			return true
		}
	}
	return false
}

// publishTargets is the audience of one topic message on the socket path: the
// subscribers a push may go to, having applied from the outside the same gates
// RunMonitor's deliver applies from the inside.
//
// The gates are replicated rather than reimplemented -- every one of them is a
// pure function of the recipient's own Entry, which the sender can already read
// -- but they are replicated, which means they have to be kept in step with
// deliver's switch. Both sides are annotated with a pointer at the other.
//
// Two of the four modes deliberately never appear here:
//
//   - digest and quiet stay on the monitor. A push is irrevocable and a sender
//     cannot batch a minute of several senders' traffic into one line, which is
//     the entire thing those modes buy. `--supersedes` works by pulling a
//     message OUT of a digest buffer before it fires, and a pushed message is
//     already in front of the reader. So the two modes that trade latency for
//     control keep the only transport that can give them control.
//
// Worth stating plainly because it is the cost of ever retiring the monitor:
// digest and quiet go with it unless something else grows a per-recipient
// buffer.
func (n Namespace) publishTargets(msg *Message, subscribers []*Entry, senderSessionID string) []*Entry {
	out := make([]*Entry, 0, len(subscribers))
	for _, e := range subscribers {
		if e == nil || e.SessionID == "" || e.SessionID == senderSessionID {
			continue
		}
		// Mirrors deliver's addressing gate: a broadcast naming other sessions
		// does not interrupt this one, it just sits in their inbox.
		if len(msg.For) > 0 && !msg.IsFor(e) {
			continue
		}
		if mode := e.Delivery[msg.Topic]; mode == DeliveryDigest || mode == DeliveryQuiet {
			continue
		}
		out = append(out, e)
	}
	return out
}
