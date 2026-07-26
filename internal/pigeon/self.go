package pigeon

import "fmt"

// Which session is this process in?
//
// The obvious answer -- CLAUDE_CODE_SESSION_ID -- is right for a process Claude
// Code spawned once and kept, and wrong for one it spawns fresh every turn.
// Clearing a session mints a new session id inside the running claude process,
// so the monitor and the MCP server, both started when that process started,
// keep serving the id they came up with, while a Bash tool call started after
// the clear is handed the new one. Both are "this session"; only one of them
// has an entry, a spool and a lock.
//
// So the question is answered against the registry rather than the environment:
// the session is whichever entry belongs to this claude process, whatever id it
// happens to be filed under. Everything that asks "who am I" goes through Self,
// so the CLI, the MCP server and the monitor cannot disagree about the answer.

// Self resolves this process's own session: the namespace holding its entry,
// and the entry itself.
//
// It fails for a plain shell, which has no session to resolve, and for a
// session with no entry anywhere -- one whose monitor never armed.
func Self() (Namespace, *Entry, error) {
	sid := CurrentSessionID()
	if sid == "" {
		return CurrentNamespace(), nil, fmt.Errorf("not inside a Claude Code session")
	}
	return locateSession(sid)
}

// SelfID is Self reduced to the id this session is actually registered under,
// or "" when it is not registered at all. Use it to recognise this session's
// own row in a listing, where the id is all that is being compared.
func SelfID() string {
	_, e, err := Self()
	if err != nil {
		return ""
	}
	return e.SessionID
}

// locateSession finds a session's entry in this namespace, failing that in any
// namespace, and failing that under whatever id its monitor armed with.
//
// A per-render or per-turn process has an environment and a working directory
// that need not match the ones the monitor armed with, so resolving a namespace
// here can land somewhere the session simply is not. Searching by exact session
// id cannot pick the wrong session, so the fallbacks cost nothing but a few
// globs in the case that would otherwise be reported as a failure.
func locateSession(sid string) (Namespace, *Entry, error) {
	ns := CurrentNamespace()
	e, err := ns.ReadEntry(sid)
	if err == nil {
		return ns, e, nil
	}
	spaces, lerr := ListNamespaces()
	if lerr != nil {
		return ns, nil, err
	}
	for _, info := range spaces {
		other, perr := ParseNamespace(info.Name)
		if perr != nil || other.Is(ns) {
			continue
		}
		if e, perr := other.ReadEntry(sid); perr == nil {
			return other, e, nil
		}
	}
	if other, e, ok := sessionOfSameProcess(sid, spaces); ok {
		return other, e, nil
	}
	return ns, nil, err
}

// sessionOfSameProcess finds the entry belonging to this session's claude
// process, for a session whose id has changed underneath its monitor.
//
// Clearing a session mints a fresh session id inside the running claude
// process. The monitor is spawned once, when that process starts, so it holds
// the id it armed with for the life of the process -- and so do the registry
// entry, the spool and the lock it owns. A caller handed the host's *current*
// id finds nothing filed under it. The arming grace cannot cover this either:
// Claude Code keeps the original startedAt across the change, so the session
// reads as hours old and the miss looks real.
//
// The match is on the process rather than the id, since the process is the one
// thing the two ids agree on. A pid plus its start time names exactly one
// process and cannot be recycled into another, which is the same test
// ProcessAlive makes before trusting any entry at all.
func sessionOfSameProcess(sid string, spaces []NamespaceInfo) (Namespace, *Entry, bool) {
	rec, ok := findClaudeSession(sid)
	if !ok || rec.PID <= 0 {
		return Namespace{}, nil, false
	}
	for _, info := range spaces {
		ns, err := ParseNamespace(info.Name)
		if err != nil {
			continue
		}
		// Dead entries are excluded: they name a process that is gone, which
		// is never the live one asking.
		entries, err := ns.ListSessions(false, false)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.PID == rec.PID && sameProcess(e.ProcStart, rec.ProcStart) {
				return ns, e, true
			}
		}
	}
	return Namespace{}, nil, false
}

// sameProcess compares two recorded process start times the way ProcessAlive
// does. An empty value on either side means that platform could not read one,
// which is a reason not to tell two processes apart rather than grounds to
// call them different.
func sameProcess(a, b string) bool { return a == "" || b == "" || a == b }
