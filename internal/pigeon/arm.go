package pigeon

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Arming pigeon by hand must be exactly as capable as arming it via the
// plugin. It is: the monitor takes nothing from the plugin at runtime. It
// reads its identity from CLAUDE_CODE_SESSION_ID and its state from
// PIGEON_HOME, both of which are present whether Claude Code spawned it from
// monitors.json or the model spawned it with the Monitor tool. The only
// difference is who starts the process, and neither path is privileged.

// MonitorCommand is the exact command line that runs the inbox monitor,
// using this binary's absolute path so it does not depend on PATH.
func MonitorCommand() string {
	exe, err := os.Executable()
	if err != nil {
		return "pigeon monitor"
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return shellQuote(exe) + " monitor"
}

// Arm explains how to start the monitor in the current session without the
// plugin, and reports whether one is already listening.
func Arm(w io.Writer) error {
	sid := CurrentSessionID()
	if sid == "" {
		fmt.Fprintf(w, "Not inside a Claude Code session, so there is nothing to arm here.\n\n")
		fmt.Fprintf(w, "Run this from a session, or install the plugin so every session arms itself:\n")
		fmt.Fprintf(w, "  pigeon install\n")
		return nil
	}

	// Self rather than ReadEntry(sid): after a clear this process holds a newer
	// session id than the monitor armed with, and answering "no monitor here"
	// would talk somebody into arming a second one for the same session.
	if _, e, err := Self(); err == nil && e.Status == StatusLive {
		fmt.Fprintf(w, "Already armed: a monitor is listening for session %s.\n", Short(e.SessionID))
		fmt.Fprintf(w, "Address: %s\n", e.Addr())
		return nil
	}

	cmd := MonitorCommand()

	// Said before the instructions, not after, because otherwise someone
	// follows them and watches the monitor register and exit immediately. The
	// env override is shown rather than described: arming by hand is an
	// explicit act for one session, and it should be able to outrank a standing
	// preference without anyone having to change the machine's config first.
	if on, origin := MonitorEnabled(); !on {
		fmt.Fprintf(w, "Monitoring is off for this machine (from %s), so a monitor armed the\n", origin)
		fmt.Fprintf(w, "ordinary way will register and stand down. Mail still arrives over the\n")
		fmt.Fprintf(w, "socket, and `pigeon inbox` still reads it.\n\n")
		fmt.Fprintf(w, "To announce mail in every session from now on:  pigeon monitoring on\n")
		fmt.Fprintf(w, "To override for this session only, arm with:\n\n")
		fmt.Fprintf(w, "  %s=%s %s\n\n", EnvMonitor, MonitorOn, cmd)
		return nil
	}

	fmt.Fprintf(w, "Session %s has no listening monitor.\n\n", Short(sid))
	fmt.Fprintf(w, "Arm it for THIS session only, with the Monitor tool:\n\n")
	fmt.Fprintf(w, "  Monitor(\n")
	fmt.Fprintf(w, "    command=%q,\n", cmd)
	fmt.Fprintf(w, "    persistent=true,\n")
	fmt.Fprintf(w, "    description=\"pigeon inbox\"\n")
	fmt.Fprintf(w, "  )\n\n")
	fmt.Fprintf(w, "Arm it for EVERY session from now on:\n\n")
	fmt.Fprintf(w, "  pigeon install         # if the plugin is not installed yet\n")
	fmt.Fprintf(w, "  pigeon monitoring on   # then restart Claude Code\n\n")
	fmt.Fprintf(w, "Both run the same monitor with the same capabilities; the setting\n")
	fmt.Fprintf(w, "just saves you doing it per session.\n")
	return nil
}
