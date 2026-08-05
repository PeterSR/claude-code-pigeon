package pigeon

// pigeon can already SEND from anywhere: a spool append is a plain file
// write, and Send/Publish know nothing about which agent host, if any, ever
// reads it. RECEIVING is the half that is not host-agnostic. Turning a spool
// append into something a live session actually notices needs a process the
// host arms at session start, injects an identity into, and is willing to
// read stdout from and turn into a notification. Today that process is
// Claude Code's plugin monitor, and every method below is something only
// Claude Code currently knows how to answer.
//
// This interface exists to gather those assumptions in one place, not to
// invite a second implementation. There is no second host today, and adding
// one is not the point: the point is that the next Claude-Code-specific fact
// this package needs has one obvious place to go, instead of a sixth file
// that reaches for CLAUDE_CODE_SESSION_ID on its own.

// Runtime is everything pigeon needs from its host in order to deliver into a
// live agent session.
type Runtime interface {
	// Name identifies the runtime, e.g. "claude-code". Stored verbatim on
	// Entry.Runtime and shown by doctor; nothing in this package branches on
	// it, since there is only ever one value today.
	Name() string

	// SessionID identifies which live session this process belongs to.
	//
	// It must fail loudly, never guess. A session id is how a monitor knows
	// whose mail it is carrying, and a wrong guess does not error -- it
	// delivers a stranger's mail into a session that never asked for it. See
	// CurrentSessionID's doc comment for the concrete failure mode
	// (Bloodhound's start-time heuristic) that this refusal exists to avoid.
	SessionID() (string, error)

	// Label returns the host's own name for a session -- e.g. the name
	// /status shows -- and where that name came from ("derived" from the
	// working directory, or a value that marks it as user-chosen). Both are
	// empty when the host's own record cannot be read, which is the normal
	// degraded state rather than an error: pigeon falls back to a name it
	// derived itself.
	Label(pid int, sessionID string) (name, source string)

	// Budget reports the three numbers that shape a notification: the whole
	// rendered line's ceiling in runes, the inline message body's ceiling
	// within it, and how many notifications per minute the host tolerates
	// before it kills the process producing them. See BodyBudget and
	// RenderBudget for where these numbers actually come from -- Budget
	// reports them, it does not own them.
	Budget() (renderRunes, bodyRunes, perMinute int)

	// Supported reports whether this host can receive at all. False on a
	// platform where the host never arms a monitor in the first place --
	// pigeon can still send from there, it just cannot be delivered to.
	Supported() bool

	// IsAgentSpawned reports whether this process was spawned by the agent
	// host, as opposed to a terminal the user opened by hand.
	//
	// READ THIS BEFORE TOUCHING IT. This is what gates whether a private
	// namespace is visible at all: a private namespace exists precisely so a
	// checkout's own back-and-forth does not land in an agent's context by
	// accident (see checkPrivateAccess in userconfig.go), and IsAgentSpawned
	// is the single test that separates "the agent is asking" from "the user,
	// at their own keyboard, is asking." A caller that answers this WRONGLY
	// IN THE "YES, THIS IS THE AGENT" DIRECTION -- reporting a process as
	// agent-spawned when it is not, or a bug that returns the safe-looking
	// answer when it cannot actually tell -- does not error out and does not
	// even fail loudly the way SessionID above is required to. A private
	// namespace just quietly stops being private. There is no downstream
	// check left to catch that; this is the check.
	//
	// Today's answer clears that bar because it is not a heuristic: Claude
	// Code injects CLAUDE_CODE_SESSION_ID into every process it spawns, MCP
	// server and shell tool call alike, several layers removed from the model
	// or not, and a terminal a person opened themselves never has it set.
	// Nothing here is guessed from a parent process name, a working
	// directory, or anything a user's own shell could set to spoof it -- see
	// InsideClaudeSession's doc comment for the exact boundary this buys and,
	// as it says plainly, the one it does not: a determined caller that can
	// unset its own environment can still look. What this buys is that a
	// private namespace never lands in a model's context by accident, and
	// that only holds because the signal comes from the host, not from
	// anything the calling process controls.
	//
	// Any runtime implemented after this one has to clear the same bar before
	// it may answer true here at all: a signal the host itself injects into
	// everything it spawns, unforgeable from a plain terminal. Short of that
	// -- a well-known env var a user could export themselves, a socket or
	// file that might simply still be lying around from a previous run, a
	// process-tree walk that can be defeated by re-parenting -- the honest
	// answer is to leave the namespace visible and say why in the error, the
	// way PrivacyError already does, not to guess toward "yes" because that
	// happens to be the answer that hides more.
	IsAgentSpawned() bool

	// Version is the host's own version string, e.g. from
	// CLAUDE_CODE_VERSION. Empty when the host did not set it, which doctor
	// treats as "unknown" rather than a failure.
	Version() string
}

// CurrentRuntime is the runtime this process is asking on behalf of.
//
// It is always the claudeCode implementation today. See this file's package
// comment for why: nothing here is meant to invite a second one, only to
// stop a second Claude-Code assumption from landing in a seventh file.
func CurrentRuntime() Runtime { return claudeCode{} }
