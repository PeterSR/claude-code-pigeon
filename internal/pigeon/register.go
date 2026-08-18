package pigeon

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	ccsock "github.com/PeterSR/claude-code-socket-transport"
)

// Session registration, as a thing that happens on its own rather than as
// something the monitor does on its way to delivering.
//
// It used to be the monitor's, and that one fact made the monitor process
// mandatory. No monitor meant no registry entry, and the entry is what carries
// a session's pid, its process start token and its namespace -- so a session
// without one is unreachable by EVERY transport, the socket included. That is
// why "monitoring off" could only ever turn off the delivering half while the
// process itself stayed, parked and doing nothing. Nobody wants a process per
// session in order to be findable.
//
// A session lifecycle hook breaks the tie. It runs at session start, writes the
// entry, and exits. An exiting hook costs nothing: unlike a monitor it is not
// reported back into the session as having ended without output, so there is no
// line for a model to read as a failure and no tokens spent explaining it. The
// monitor is then free to be what it always should have been -- the part that
// ANNOUNCES mail -- listed in the plugin manifest only on a machine that wants
// announcing, and absent entirely on one that does not.
//
// It fixes something older too. SessionStart fires on `resume` as well as
// `startup`, so a resumed session registers every time. The monitor did not:
// Claude Code rearms it inconsistently across a resume, sometimes under a new
// id and sometimes not at all, which is exactly how a session ends up alive,
// working, and invisible to its peers.

// hookInput is the JSON Claude Code writes to a lifecycle hook's stdin. Only
// the fields pigeon needs are named; the rest are ignored rather than rejected,
// since a host adding a field must not break a hook that never asked for it.
type hookInput struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	// HookEventName is "SessionStart" or "SessionEnd". Read only for the log,
	// because the command already knows which one it is; a mismatch is worth
	// seeing rather than acting on.
	HookEventName string `json:"hook_event_name"`
}

// maxHookInputBytes bounds the read. The payload is a handful of short strings
// plus a transcript path; anything larger is not it.
const maxHookInputBytes = 64 << 10

// readHookInput parses the payload, and treats every failure as an absent
// payload rather than an error. A hook that refuses to run because stdin was
// not what it expected is a hook that unregisters a session over a formatting
// change, and the environment is a perfectly good second source for everything
// here.
func readHookInput(in io.Reader) hookInput {
	var h hookInput
	if in == nil {
		return h
	}
	b, err := io.ReadAll(io.LimitReader(in, maxHookInputBytes))
	if err != nil || len(b) == 0 {
		return h
	}
	_ = json.Unmarshal(b, &h)
	return h
}

// hookSessionID prefers the payload over the environment. Both are Claude
// Code's own answer, but the payload names the session this event is ABOUT,
// while the environment names the session this process was spawned from; on a
// resume those can differ, and the event is the one that is right.
func hookSessionID(h hookInput) string {
	if id := strings.TrimSpace(h.SessionID); id != "" {
		if ValidSessionID(id) == nil {
			return id
		}
	}
	return CurrentSessionID()
}

// adoptHookCwd publishes the payload's working directory as this process's
// project directory.
//
// Several things below resolve that from the environment rather than from a
// parameter -- the namespace, the project config, the checkout topic, the
// default subscriptions -- and they must all agree on one answer. Setting it
// once here is what makes them agree, instead of each separately falling back
// to whatever directory the hook happened to be spawned in.
//
// The payload wins over an inherited value rather than deferring to it, and
// that is deliberate: the payload describes the session this event is about,
// while an inherited variable describes whatever spawned the process. When they
// disagree the event is the one that is right, and a project opt-out read from
// the wrong directory is not a small error -- it puts a checkout on the bus that
// asked to stay off it.
func adoptHookCwd(h hookInput) {
	if cwd := strings.TrimSpace(h.Cwd); cwd != "" {
		_ = os.Setenv(EnvProjectDir, cwd)
	}
}

// claudePIDFor answers the one question a hook cannot take for granted: which
// operating system process is this session.
//
// The environment is asked FIRST, and the order is the whole point. CLAUDE_PID
// is injected by Claude Code into the process it spawned, so for a hook it names
// the session this event is about, directly, with nothing in between. Claude
// Code's own session registry is a lookup by session id over files on disk, and
// those files outlive the processes they describe: ccsock returns a single match
// without checking whether it is running, because liveness there is only used to
// break ties between several.
//
// Preferring the registry had a specific and nasty failure, which is why the
// order reads backwards from "ask the authority". A session killed hard leaves
// its record behind. On `claude --resume` the hook fires before the new
// process's record lands, the lookup finds exactly one match -- the dead one --
// and the entry is written with a pid that is already gone. ProcessAlive says
// false, so the session is hidden from listings, unresolvable by name, and swept
// by the next registration in the namespace. With no monitor there is no second
// registrar to correct it, so it stays invisible for its whole life: precisely
// the resume case this hook exists to fix.
//
// Both answers are therefore checked for liveness rather than trusted, and the
// registry is consulted only when the environment has nothing usable.
func claudePIDFor(sid string) int {
	return pickClaudePID(CurrentClaudePID(), sid, lookupClaudePID)
}

// lookupClaudePID asks Claude Code's session registry, reporting the start token
// alongside the pid so the caller can tell a live record from a leftover one.
func lookupClaudePID(sid string) (pid int, procStart string, ok bool) {
	s, err := ccsock.FindBySessionID(sid)
	if err != nil || s.PID <= 0 {
		return 0, "", false
	}
	return s.PID, s.ProcStart, true
}

// pickClaudePID is claudePIDFor with the registry lookup injected, so the
// ordering it encodes can be tested without a Claude Code installation to look
// at -- and it is the ordering, not the lookup, that was wrong.
func pickClaudePID(envPID int, sid string, lookup func(string) (int, string, bool)) int {
	// ProcStart(envPID) rather than a recorded token: this pid came from the
	// environment, so there is nothing to compare it against, and the only
	// question worth asking is whether it is running.
	if envPID > 0 && ProcessAlive(envPID, ProcStart(envPID)) {
		return envPID
	}
	if pid, procStart, ok := lookup(sid); ok && ProcessAlive(pid, procStart) {
		return pid
	}
	// Deliberately NOT falling back to a pid that is not running. An entry
	// carrying a dead pid is worse than one carrying none: it looks answerable,
	// is not, and gets swept anyway.
	return 0
}

// RegisterHook is `pigeon register`: the SessionStart hook.
//
// It writes NOTHING to stdout, and that is a contract rather than a style. A
// SessionStart hook's stdout is fed back into the session as context, so a
// status line here would be tokens spent in every session on the fact that a
// plugin is installed. Everything it has to say goes to stderr, which the model
// never sees.
//
// It also returns nil on almost everything. A hook that fails is a hook that
// can hold up or clutter a session start, and there is nothing here worth that:
// the worst case of a failed registration is a session its peers cannot see,
// which is the state pigeon already handles and doctor already reports.
func RegisterHook(in io.Reader, stderr io.Writer) error {
	logf := func(format string, a ...any) {
		fmt.Fprintf(stderr, "[pigeon] "+format+"\n", a...)
	}

	h := readHookInput(in)

	adoptHookCwd(h)

	sid := hookSessionID(h)
	if sid == "" {
		logf("no session id in the hook payload or the environment; nothing to register")
		return nil
	}

	if OptedOut() {
		logf("disabled via %s -- not registering", EnvOptOut)
		return nil
	}
	cwd := CurrentCwd()
	if ProjectDisabled(cwd) {
		logf("disabled by %s -- not registering", ProjectConfigPath(cwd))
		return nil
	}

	ns, origin := ResolveNamespace()
	if err := ns.EnsureDirs(); err != nil {
		logf("could not prepare %s: %v", ns, err)
		return nil
	}

	facts := sessionFacts{PID: claudePIDFor(sid), Cwd: cwd}
	if facts.PID <= 0 {
		// Loud, because everything downstream decides against this pid --
		// liveness, pruning, socket resolution, every listing -- so an entry
		// without one is registered and invisible at the same time, and nothing
		// else will ever say why.
		logf("WARNING: cannot tell which process session %s is. %s is unset or names a process", Short(sid), EnvClaudePID)
		logf("that is gone, and Claude Code's session registry has no live record for it. Registering")
		logf("anyway, but this session will look dead to its peers; `pigeon doctor` will say so.")
	}
	if err := register(ns, sid, CurrentRuntime(), facts, logf); err != nil {
		logf("registration failed: %v", err)
		return nil
	}

	// The monitor used to create this on its way past. Nothing else does, and a
	// spool that does not exist is not an error anywhere -- but creating it here
	// keeps `pigeon inbox` from having to explain an absent file to somebody who
	// has simply not been sent anything yet.
	spool := ns.SpoolPath(sid)
	if f, err := os.OpenFile(spool, os.O_WRONLY|os.O_CREATE, 0o600); err == nil {
		f.Close()
	}

	logf("registered session=%s namespace=%s (from %s) pid=%d", Short(sid), ns, origin, facts.PID)
	if on, morigin := MonitorEnabled(); !on {
		logf("no monitor for this session (monitoring off, from %s); mail arrives over the socket and `pigeon inbox` reads it", morigin)
	}
	return nil
}

// DeregisterHook is `pigeon deregister`: the SessionEnd hook.
//
// This is the tidy exit that the monitor's deferred RemoveEntry used to be, and
// having it back matters more than it looks. Without it every session that ever
// ran would leave an entry behind to be swept later by whoever registers next,
// so `pigeon ls` would show the dead alongside the living for as long as nobody
// started a session. The spool is deliberately left: mail queued for a session
// outlives the session, because the same id can come back. Not forever --
// reconcileOrphans sweeps a departed session's spool and cursors once they are
// past its grace period -- but for long enough that resuming finds what arrived
// while it was gone.
func DeregisterHook(in io.Reader, stderr io.Writer) error {
	logf := func(format string, a ...any) {
		fmt.Fprintf(stderr, "[pigeon] "+format+"\n", a...)
	}

	h := readHookInput(in)
	adoptHookCwd(h)

	sid := hookSessionID(h)
	if sid == "" {
		return nil
	}

	// Searched for rather than resolved, because a session's namespace is
	// decided at registration from a working directory and a config that may
	// both have moved since. locateSession asks every namespace, which is the
	// only way to be sure the entry being removed is this session's own.
	ns, e, err := locateSession(sid)
	if err != nil || e == nil {
		return nil
	}
	ns.RemoveEntry(e.SessionID)
	logf("deregistered session=%s from namespace %s", Short(e.SessionID), ns)
	return nil
}
