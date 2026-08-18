package pigeon

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Everything pigeon does rests on host behaviour that is shipped but not
// documented: that Claude Code starts plugin-declared monitors, that it injects
// CLAUDE_CODE_SESSION_ID into them, and that it delivers their stdout as a
// task notification. None of that is API. When a Claude Code upgrade changes
// any of it, the failure is silent -- messages simply stop arriving and nothing
// says why.
//
// Doctor exists to make that failure legible. It checks each link in the chain
// separately and names the one that broke, so "pigeon stopped working" becomes
// a diagnosis rather than a guess.

const (
	// minCCVersion is the oldest Claude Code that injects
	// CLAUDE_CODE_SESSION_ID into plugin monitors. Below it the monitor cannot
	// identify its own session and refuses to run at all.
	minCCVersion = "2.1.105"
	// testedCCVersion is the newest release the mechanism was verified against
	// end to end. Newer is not assumed broken -- it is untested, which is a
	// different and milder claim.
	testedCCVersion = "2.1.218"
)

// CheckLevel is how much a failed check matters.
type CheckLevel int

const (
	// CheckOK: this link in the chain works.
	CheckOK CheckLevel = iota
	// CheckWarn: degraded or unverified, but messages can still arrive.
	CheckWarn
	// CheckFail: messages cannot arrive until this is fixed.
	CheckFail
)

func (l CheckLevel) String() string {
	switch l {
	case CheckOK:
		return "ok"
	case CheckWarn:
		return "warn"
	default:
		return "fail"
	}
}

// Check is one diagnosis.
type Check struct {
	Name   string     `json:"name"`
	Level  CheckLevel `json:"-"`
	Status string     `json:"status"`
	Detail string     `json:"detail"`
	// Hint is what to do about it, and is set only when there is something
	// specific to do. A hint that just restates the problem is noise.
	Hint string `json:"hint,omitempty"`
}

func ok(name, detail string) Check {
	return Check{Name: name, Level: CheckOK, Status: "ok", Detail: detail}
}

func warn(name, detail, hint string) Check {
	return Check{Name: name, Level: CheckWarn, Status: "warn", Detail: detail, Hint: hint}
}

func fail(name, detail, hint string) Check {
	return Check{Name: name, Level: CheckFail, Status: "fail", Detail: detail, Hint: hint}
}

// Diagnose runs every check and returns them in delivery-chain order: identity,
// then state, then plugin, then this session's actual reachability. Order
// matters because the first failure usually explains every one after it.
func Diagnose() []Check {
	var out []Check
	out = append(out, checkSession())
	out = append(out, checkOptOut())
	out = append(out, checkVersion())
	out = append(out, checkNamespace())
	out = append(out, checkState())
	out = append(out, checkPlugin()...)
	out = append(out, checkProjectConfig())
	out = append(out, checkRegistration()...)
	out = append(out, checkSocket())
	out = append(out, checkClaudeSession())
	out = append(out, checkPeers())
	return out
}

// checkClaudeSession reports whether pigeon can read Claude Code's own session
// name -- the /status label it reflects in `ls`, list_sessions and templates.
//
// This is auxiliary, not a delivery link, so it never fails: a miss means the
// session shows pigeon's own derived name instead, and mail still arrives. It
// earns a check anyway because the file it reads is another undocumented Claude
// Code internal, so when a host upgrade moves it, this is where the silence gets
// a name rather than the label just quietly going blank everywhere.
func checkClaudeSession() Check {
	sid, err := CurrentRuntime().SessionID()
	if err != nil {
		return ok("claude name", "not inside a session; nothing to read")
	}
	pid := CurrentClaudePID()
	if pid <= 0 {
		return warn("claude name", EnvClaudePID+" is unset, so the session index cannot be located",
			"harmless unless you want the /status name; pigeon falls back to its own")
	}
	path := filepath.Join(claudeConfigDir(), "sessions", strconv.Itoa(pid)+".json")
	name, source := CurrentRuntime().Label(pid, sid)
	if name == "" {
		return warn("claude name", "no readable session index at "+path,
			"Claude Code may have moved or renamed it; pigeon shows its own derived name instead")
	}
	detail := name
	if source != "" {
		detail += " (" + source + ")"
	}
	return ok("claude name", detail)
}

// checkNamespace names the group this session is in and where that came from.
//
// Getting it wrong is silent by construction: a session in the wrong namespace
// registers fine, sends fine, and simply cannot see anyone. Since nothing else
// in the UI mentions namespaces at all, "where did this come from" is the whole
// diagnosis.
func checkNamespace() Check {
	ns, origin := ResolveNamespace()
	detail := fmt.Sprintf("%s (from %s)", ns, origin)
	if raw := strings.TrimSpace(os.Getenv(EnvNamespace)); raw != "" && ValidNamespace(raw) != nil {
		return warn("namespace", detail,
			"set "+EnvNamespace+" to a valid namespace or unset it; this session is in "+ns.String())
	}
	// A private namespace changes what this session can reach, so a session
	// wondering why a peer or a broadcast never arrived should find the reason
	// here rather than deduce it.
	if ns.IsPrivate() {
		detail += " -- private: sealed from machine-wide topics, and invisible to sessions outside it"
	}
	return ok("namespace", detail)
}

// checkProjectConfig makes a project's defaults visible. Without this, a
// session named by a file in the checkout is a small mystery: nobody typed the
// name, and nothing else reports where it came from.
//
// It reports the *rendered* values rather than the file's text, because with
// templates the file no longer says what a session gets. Rendering goes
// through the same Resolve a starting monitor uses, so this cannot drift into
// a second, more optimistic implementation of the same rules.
func checkProjectConfig() Check {
	cwd := CurrentCwd()
	path := ProjectConfigPath(cwd)
	cfg, problems, err := LoadProjectConfig(cwd)
	if err != nil {
		return warn("project config", err.Error(),
			"fix or remove the file; sessions here fall back to no project defaults")
	}
	if cfg == nil && len(problems) == 0 {
		return ok("project config", "none at "+path)
	}

	if !cfg.IsEnabled() {
		if optOutSet() {
			return warn("project config",
				fmt.Sprintf("%s: \"enabled\": false, overridden by %s=%s", path, EnvOptOut, os.Getenv(EnvOptOut)),
				"the environment wins, so this session does take part")
		}
		return warn("project config", path+`: "enabled": false, so sessions started here stay off the bus`,
			"remove the field, or set "+EnvOptOut+"=1 in this session's environment to override it")
	}

	res := cfg.Resolve(CurrentNamespace(), CurrentSessionID(), cwd)
	problems = append(problems, res.Problems...)
	if len(problems) > 0 {
		return warn("project config", path+": "+strings.Join(problems, "; "),
			"the rejected fields are ignored; the rest still apply")
	}

	parts := make([]string, 0, 5)
	if cfg.Namespace != "" {
		parts = append(parts, "namespace="+cfg.Namespace)
	}
	if res.Name != "" {
		parts = append(parts, "name="+res.Name)
	}
	if res.Description != "" {
		parts = append(parts, "description set")
	}
	if len(res.Topics) > 0 {
		parts = append(parts, "topics="+strings.Join(res.Topics, ","))
	}
	if res.Private {
		parts = append(parts, "private: cwd and description are not published")
	}
	return ok("project config", path+" ("+strings.Join(parts, ", ")+")")
}

// checkSession is first because nothing downstream can be interpreted without
// it: a session id is how a monitor knows whose mail it is carrying.
func checkSession() Check {
	raw := strings.TrimSpace(os.Getenv(EnvSessionID))
	switch {
	case raw == "":
		return warn("session", "not running inside a Claude Code session",
			"run `pigeon doctor` from inside a session to check that session's delivery path")
	case ValidSessionID(raw) != nil:
		return fail("session", fmt.Sprintf("%s is set but not a usable id (%q)", EnvSessionID, raw),
			"something is overwriting the variable Claude Code sets; unset it and restart")
	default:
		return ok("session", Short(raw))
	}
}

func checkOptOut() Check {
	if OptedOut() {
		return warn("opt-out", fmt.Sprintf("%s=%s, so this session stays off the bus", EnvOptOut, os.Getenv(EnvOptOut)),
			"unset "+EnvOptOut+" and restart the session to take part")
	}
	return ok("opt-out", "not opted out")
}

func checkVersion() Check {
	v := strings.TrimSpace(CurrentRuntime().Version())
	if v == "" {
		return warn("claude code", "version unknown ("+EnvVersion+" is unset)", "")
	}
	switch {
	case compareVersions(v, minCCVersion) < 0:
		return fail("claude code", v+" is too old to inject "+EnvSessionID,
			"upgrade to "+minCCVersion+" or newer")
	case compareVersions(v, testedCCVersion) > 0:
		return warn("claude code", v+" is newer than the tested "+testedCCVersion,
			"pigeon rides undocumented host behaviour; if delivery broke, this is the first thing to suspect")
	default:
		return ok("claude code", v)
	}
}

func checkState() Check {
	// Sample the mode before EnsureDirs runs: it chmods the tree back to 0700,
	// so checking afterwards would only ever see the repaired value and this
	// finding could never fire. Loose permissions on the spool mean anyone on
	// the box can put text into a live agent's context, so it is worth saying
	// that it happened even though pigeon has already fixed it.
	loosened := os.FileMode(0)
	if fi, err := os.Stat(Home()); err == nil {
		if perm := fi.Mode().Perm(); perm&0o077 != 0 {
			loosened = perm
		}
	}

	if err := EnsureDirs(); err != nil {
		return fail("state dir", Home()+": "+err.Error(), "check permissions on the parent directory")
	}
	// Writability is the thing that actually matters, and it is not implied by
	// the directory existing -- a read-only mount or a root-owned ~/.claude
	// both pass a stat and fail a send.
	probe, err := os.CreateTemp(Home(), ".doctor-*")
	if err != nil {
		return fail("state dir", Home()+" is not writable: "+err.Error(),
			"pigeon cannot queue or read mail without it")
	}
	name := probe.Name()
	probe.Close()
	_ = os.Remove(name)

	if loosened != 0 {
		return warn("state dir", fmt.Sprintf("%s was mode %04o, not owner-only (tightened to 0700)", Home(), loosened),
			"anyone who could write there can inject text into your sessions; check who had access")
	}
	return ok("state dir", Home())
}

// checkPlugin returns several checks: the plugin is three separate artefacts
// and any one of them can be stale on its own.
func checkPlugin() []Check {
	dir, err := pluginDir()
	if err != nil {
		return []Check{fail("plugin", "cannot locate home directory: "+err.Error(), "")}
	}
	if _, err := os.Stat(dir); err != nil {
		return []Check{fail("plugin", "not installed at "+dir, "run `pigeon install`, then restart Claude Code")}
	}

	out := []Check{ok("plugin", dir)}

	// The monitor spec is the link that arms sessions. A missing or renamed
	// entry means every new session comes up unreachable.
	var mons []monitorSpec
	monPath := filepath.Join(dir, "monitors", "monitors.json")
	if b, err := os.ReadFile(monPath); err != nil {
		out = append(out, fail("monitor spec", "missing "+monPath, "run `pigeon install` again"))
	} else if err := json.Unmarshal(b, &mons); err != nil || len(mons) == 0 {
		out = append(out, fail("monitor spec", monPath+" is unreadable", "run `pigeon install` again"))
	} else {
		out = append(out, checkMonitorBinary(mons[0].Command))
	}

	// The MCP registration is independent: a session can receive mail
	// perfectly while the model has no tools to send any.
	mcpPath := filepath.Join(dir, ".mcp.json")
	if b, err := os.ReadFile(mcpPath); err != nil {
		out = append(out, warn("mcp server", "missing "+mcpPath,
			"the CLI still works; run `pigeon install` to restore the model-facing tools"))
	} else {
		var cfg struct {
			MCPServers map[string]struct {
				Command string `json:"command"`
			} `json:"mcpServers"`
		}
		if err := json.Unmarshal(b, &cfg); err != nil || cfg.MCPServers["pigeon"].Command == "" {
			out = append(out, warn("mcp server", mcpPath+" is unreadable", "run `pigeon install` again"))
		} else {
			out = append(out, ok("mcp server", cfg.MCPServers["pigeon"].Command+" mcp"))
		}
	}
	return out
}

// checkMonitorBinary verifies the path baked into monitors.json still resolves,
// and flags the upgrade trap: `go install` writes a new binary, the plugin
// keeps pointing at wherever the old one lived, and sessions silently arm the
// stale one -- or nothing at all, if it was removed.
func checkMonitorBinary(cmd string) Check {
	bin := monitorBinary(cmd)
	if bin == "" {
		return warn("monitor binary", "cannot parse the command in monitors.json ("+cmd+")", "run `pigeon install` again")
	}
	fi, err := os.Stat(bin)
	if err != nil {
		return fail("monitor binary", bin+" does not exist",
			"the plugin points at a binary that has moved or been deleted; run `pigeon install` again")
	}
	if !isExecutable(fi) {
		return fail("monitor binary", bin+" is not executable", "chmod +x it, or run `pigeon install` again")
	}
	if self, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(self); err == nil {
			self = resolved
		}
		if self != bin {
			return warn("monitor binary", "plugin runs "+bin+", but this is "+self,
				"sessions arm the plugin's copy, not this one; run `pigeon install` to point it here")
		}
	}
	return ok("monitor binary", bin)
}

// monitorBinary reverses the quoting Install applies, so the path can be
// stat'ed. Returns "" when the command is not one we wrote.
func monitorBinary(cmd string) string {
	s := strings.TrimSpace(cmd)
	if !strings.HasSuffix(s, " monitor") {
		return ""
	}
	s = strings.TrimSuffix(s, " monitor")
	if len(s) >= 2 && strings.HasPrefix(s, `"`) && strings.HasSuffix(s, `"`) {
		s = unescapeDouble(s[1 : len(s)-1])
	}
	return s
}

func unescapeDouble(s string) string {
	out := make([]rune, 0, len(s))
	esc := false
	for _, r := range s {
		switch {
		case esc:
			out = append(out, r)
			esc = false
		case r == '\\':
			esc = true
		default:
			out = append(out, r)
		}
	}
	return string(out)
}

// checkRegistration is the payoff: whatever the configuration says, is this
// session actually reachable right now?
func checkRegistration() []Check {
	if CurrentSessionID() == "" {
		return nil
	}
	// Self, not ReadEntry: doctor answers "can this session receive", and a
	// session that has been cleared is registered under the id its monitor
	// armed with rather than the one this process was handed. Reporting an
	// unreachable session that is in fact reachable sends you restarting a
	// perfectly healthy monitor.
	own, e, err := Self()
	if err != nil {
		return []Check{fail("this session", "not registered, so nothing can reach it",
			"install the plugin and restart, or run `pigeon arm` to arm this session alone")}
	}

	out := make([]Check, 0, 3)
	switch e.Status {
	case StatusLive:
		out = append(out, ok("this session", "live, reachable as `pigeon send "+e.Addr()+"`"))
	case StatusDeaf:
		// A dead monitor used to be the end of the story, and this check said so
		// with a FAIL. It is now only half of it: a session whose socket answers
		// still receives every message a sender chooses to push there, so
		// reporting a failure would send someone restarting a session that works
		// -- the same false alarm the `socket` status exists to remove from `ls`.
		if SocketReachable(e) {
			// Told apart because the advice differs completely. A monitor that
			// died is a fault and the fix is to restart; a monitor that was
			// turned off is the configuration working, and telling someone to
			// restart would undo what they chose and then not even fix it,
			// since the next monitor would read the same setting and stand
			// down again.
			if on, origin := MonitorEnabled(); !on {
				out = append(out, ok("this session",
					"monitoring is off (from "+origin+"); registered and reachable over its socket"))
			} else {
				out = append(out, warn("this session",
					"no monitor is listening, but its socket answers, so mail sent over the socket still arrives",
					"topics on digest or quiet still need a monitor; restart the session to get one back"))
			}
		} else {
			if on, origin := MonitorEnabled(); !on {
				out = append(out, fail("this session",
					"monitoring is off (from "+origin+") and its socket did not answer, so nothing can reach it",
					"socket delivery is the only path left when monitoring is off, and it is unavailable here; "+
						"run `pigeon monitoring on` and restart the session"))
			} else {
				out = append(out, fail("this session",
					"registered, no monitor is listening, and its socket did not answer either",
					"the monitor died or never started; restart the session, or run `pigeon arm`"))
			}
		}
	default:
		out = append(out, fail("this session", "registered but its process looks gone",
			"run `pigeon prune`"))
	}

	// Counted on the spool the entry actually names, which is not necessarily
	// the one this process's own namespace and session id would point at.
	if n := own.Pending(e.SessionID); n > 0 {
		out = append(out, warn("queued mail",
			fmt.Sprintf("%d message(s) waiting on this session's spool", n),
			"a monitor for this same session id will read them; a new session gets a new id and will not"))
	}

	if len(e.Subscriptions) > 0 {
		out = append(out, ok("topics", strings.Join(e.Subscriptions, ", ")))
	}
	return out
}

// checkSocket reports whether this session can be reached over Claude Code's
// own inbox socket, which is the delivery path that does not depend on a
// monitor being alive.
//
// It is reported separately from "this session" on purpose. That check answers
// "can I receive", and the honest answer now has two independent halves that
// fail for unrelated reasons: a monitor dies when the host does not respawn it,
// a socket is missing when the host is too old or has messaging turned off.
// Folding them into one line would mean the recovery advice is right half the
// time.
//
// Never a FAIL. A machine where nothing binds a socket is exactly pigeon before
// this transport existed, and it still delivers; what is lost is the fallback,
// which is worth a warning and not an alarm.
func checkSocket() Check {
	_, e, err := Self()
	if err != nil {
		return ok("socket", "not registered; nothing to probe")
	}
	s, err := claudeSessionFor(e)
	if err != nil {
		return warn("socket", "this session has no reachable inbox socket",
			"socket delivery is unavailable here, so mail depends entirely on the monitor; "+
				"cross-session messaging needs Claude Code 2.1.224 or newer and is not available on native Windows")
	}
	if !s.Reachable(socketProbeTimeout) {
		return warn("socket", "registered at "+s.SocketPath+" but it did not answer",
			"the path is stale or the session stopped listening; mail depends entirely on the monitor until it comes back")
	}
	detail := s.SocketPath
	// Named only when the two registries disagree, which is the case that
	// confuses a reader of `pigeon ls`: the id pigeon addresses this session by
	// is not the id Claude Code currently holds, because /clear minted a new one
	// and the monitor kept the old. Delivery is unaffected -- the join is on the
	// process, see claudeSessionFor -- but silence here would make the mismatch
	// look like a bug the one time someone compares the two by hand.
	if s.SessionID != "" && s.SessionID != e.SessionID {
		detail += fmt.Sprintf(" (claude session %s, pigeon addresses this session as %s; they diverge after /clear and that is expected)",
			Short(s.SessionID), Short(e.SessionID))
	}
	return ok("socket", detail)
}

func checkPeers() Check {
	ns := CurrentNamespace()
	entries, err := ns.ListSessions(false, false)
	if err != nil {
		return warn("peers", "cannot read the registry: "+err.Error(), "")
	}
	me := SelfID()
	live, deaf := 0, 0
	for _, e := range entries {
		if e.SessionID == me {
			continue
		}
		switch e.Status {
		case StatusLive:
			live++
		case StatusDeaf:
			deaf++
		}
	}
	detail := fmt.Sprintf("%d live", live)
	if deaf > 0 {
		detail += fmt.Sprintf(", %d deaf", deaf)
	}
	// A count of what is deliberately out of sight. "0 live" in a namespace
	// nobody meant to be in looks identical to an empty machine otherwise.
	if elsewhere, spaces := peersElsewhere(ns); elsewhere > 0 {
		detail += fmt.Sprintf("; %d in %d other namespace(s)", elsewhere, spaces)
	}
	return ok("peers", detail)
}

func peersElsewhere(ns Namespace) (sessions, spaces int) {
	all, err := ListNamespaces()
	if err != nil {
		return 0, 0
	}
	for _, info := range all {
		if info.Name == ns.String() {
			continue
		}
		if n := info.Live + info.Deaf; n > 0 {
			sessions += n
			spaces++
		}
	}
	return sessions, spaces
}

// Doctor renders the diagnosis and reports whether delivery is currently
// possible. The error is deliberately vague -- the checks above it carry the
// detail, and repeating it in an exit message just makes the output longer.
func Doctor(w io.Writer, asJSON bool) error {
	checks := Diagnose()
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(checks); err != nil {
			return err
		}
	} else {
		width := 0
		for _, c := range checks {
			if len(c.Name) > width {
				width = len(c.Name)
			}
		}
		for _, c := range checks {
			mark := map[CheckLevel]string{CheckOK: "ok  ", CheckWarn: "warn", CheckFail: "FAIL"}[c.Level]
			fmt.Fprintf(w, "%s  %-*s  %s\n", mark, width, c.Name, c.Detail)
			if c.Hint != "" {
				fmt.Fprintf(w, "      %-*s  -> %s\n", width, "", c.Hint)
			}
		}
	}
	for _, c := range checks {
		if c.Level == CheckFail {
			return fmt.Errorf("delivery is broken; see the FAIL lines above")
		}
	}
	return nil
}

// compareVersions orders dotted numeric versions. Non-numeric suffixes are
// ignored rather than rejected: a prerelease tag should not make a version
// compare as older than one it is plainly newer than.
func compareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		x, y := versionPart(as, i), versionPart(bs, i)
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

func versionPart(parts []string, i int) int {
	if i >= len(parts) {
		return 0
	}
	s := parts[i]
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	n, err := strconv.Atoi(s[:end])
	if err != nil {
		return 0
	}
	return n
}
