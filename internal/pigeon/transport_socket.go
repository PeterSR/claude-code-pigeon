package pigeon

import (
	"context"
	"errors"
	"fmt"
	"time"

	ccsock "github.com/PeterSR/claude-code-socket-transport"
)

// The socket half of transport.go: turning a pigeon Entry into a live Claude
// Code inbox and putting one rendered line in front of it.
//
// Everything Claude-Code-specific about the protocol -- the socket path, the
// frame format, the auth key files, the attribution envelope -- lives in
// ccsock, which reverse-engineered it. This file is only the join: pigeon's
// registry is the addressing authority, ccsock is the wire.
//
// That direction matters and is not arbitrary. ccsock can resolve a target by
// name or session id itself, and pigeon deliberately does not use those: a name
// in pigeon is an address it hands out and guarantees uniqueness for within a
// namespace, while a name in ccsock is Claude Code's own label, which pigeon's
// README is explicit is NOT an address. Letting both resolve names would mean
// two resolutions that can disagree about who "api" is, and the one that
// answered would depend on which code path a message happened to take.

// socketTimeout bounds one push. It is short on purpose: a push happens inline
// in a send, and a fan-out across a topic's subscribers pays it per recipient.
// A session that cannot take a frame off its socket in this long is one whose
// monitor path is the better bet anyway.
const socketTimeout = 2 * time.Second

// socketProbeTimeout bounds the liveness check behind `pigeon ls`. Not used on
// the send path, which simply attempts the push and treats a refused connection
// as the answer -- probing first would double the connections per message to
// buy information the send itself is about to produce.
const socketProbeTimeout = 250 * time.Millisecond

var socketClient = &ccsock.Client{Timeout: socketTimeout}

// errNoSocket reports a recipient that has no reachable inbox socket, as
// opposed to one whose push was attempted and failed.
var errNoSocket = errors.New("no reachable Claude Code inbox socket")

// claudeSessionFor resolves a pigeon entry to the Claude Code session it names.
//
// THE JOIN IS ON THE PROCESS, NOT ON THE SESSION ID, and that is not a
// shortcut. The obvious key looks like the session id -- both registries carry
// one, and pigeon already verifies Claude Code's index against it in
// LookupClaudeSession -- but the two ids are not the same id once a session has
// run /clear. /clear mints a fresh session id, which Claude Code writes into
// its own registry immediately; pigeon's monitor was armed before it and keeps
// the id it was born with for its whole lifetime, because monitors cannot be
// rebound mid-session. Measured on a machine with fifteen live sessions, two
// disagreed this way. Keying on the session id would have refused to deliver to
// both, and refused for a reason nothing in either registry would explain.
//
// The pid and its process start token are the same in both registries, byte for
// byte, because both read field 22 of /proc/<pid>/stat. So (pid, procStart)
// identifies one running process, which is what "the same session" actually
// means to a person looking at a window -- and it is a strictly stronger guard
// against a recycled pid than the session id ever was, since a recycled pid
// necessarily has a different start time.
//
// Nothing is weakened by dropping the session-id check on this side, because
// the frame carries one anyway: Client.Send stamps the message with the session
// id from the registry entry it was handed, and the receiver drops a frame
// whose id is not its own. The id that has to match is Claude Code's current
// one, which is exactly the one this lookup produces.
func claudeSessionFor(e *Entry) (ccsock.Session, error) {
	if e == nil {
		return ccsock.Session{}, errNoSocket
	}
	if e.PID > 0 {
		if s, err := ccsock.FindByPID(e.PID); err == nil && s.SocketPath != "" && sameClaudeProcess(e, s) {
			return s, nil
		}
	}
	// The pid drifted, or its entry describes a different process. A session id
	// is exact when both sides still agree on one, so it is worth the second
	// look before giving up -- it just cannot be the primary key.
	if e.SessionID == "" {
		return ccsock.Session{}, errNoSocket
	}
	s, err := ccsock.FindBySessionID(e.SessionID)
	if err != nil {
		return ccsock.Session{}, fmt.Errorf("%w: %v", errNoSocket, err)
	}
	if s.SocketPath == "" {
		return ccsock.Session{}, errNoSocket
	}
	return s, nil
}

// sameClaudeProcess reports whether a Claude Code registry entry describes the same
// running process as a pigeon entry.
//
// The start token is the real test and the session id is the fallback, for the
// platform where pigeon has no token to compare: ProcStart reads /proc, so it
// is empty anywhere that is not Linux (see procstart_other.go). There, a
// /clear'd session is simply not reachable over the socket until pigeon can
// record a token of its own -- a degradation, and a visible one, rather than a
// guess about which process a pid belongs to.
func sameClaudeProcess(e *Entry, s ccsock.Session) bool {
	if e.ProcStart != "" && s.ProcStart != "" {
		return e.ProcStart == s.ProcStart
	}
	return e.SessionID != "" && e.SessionID == s.SessionID
}

// socketPush puts one rendered line in front of a session over its inbox
// socket. A nil error means the receiver took the frame, which is not the same
// as the session having seen it -- see the note on refused inbound in the
// README.
//
// fromName is the sender's pigeon display name, and setting it is not cosmetic.
// It makes ccsock wrap the text in Claude Code's own attribution envelope, so
// the line arrives marked as coming from another session. Left empty, the text
// would arrive as a bare user turn, and Render's doc comment already records
// what that does to a model: it starts fabricating user turns of its own.
func socketPush(e *Entry, line, fromName string) error {
	s, err := claudeSessionFor(e)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), socketTimeout)
	defer cancel()
	// Client.Send stamps the frame with this session's own id, and the receiver
	// drops a frame whose id is not its own. So even a socket path that has
	// been reused underneath us cannot be misdelivered to.
	_, err = socketClient.Send(ctx, s, ccsock.Message{
		Text:     line,
		FromName: fromName,
	})
	return err
}

// SocketReachable reports whether a session can be woken over its inbox socket
// right now. Used by listings and doctor, never by the send path.
//
// This is Claude Code's own liveness test -- it connects -- which makes it a
// second, independent answer to the question `pigeon ls` has only ever been
// able to ask of the monitor lock. A session can have a dead monitor and a
// perfectly good socket, which is the exact state this whole transport exists
// to rescue, and until now that state was reported simply as "deaf".
func SocketReachable(e *Entry) bool {
	s, err := claudeSessionFor(e)
	if err != nil {
		return false
	}
	return s.Reachable(socketProbeTimeout)
}
