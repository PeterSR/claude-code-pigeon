package pigeon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withPluginHome isolates both halves of an install: the plugin under HOME and
// the state tree under PIGEON_HOME. Neither may ever be a real ~/.claude.
func withPluginHome(t *testing.T) (home, plugin string) {
	t.Helper()
	withHome(t)
	home = withUserHome(t)
	// Claimed, not merely ignored: pluginDir reads it ahead of the home
	// directory, so a developer who has it set would otherwise watch these
	// tests scaffold into, and Uninstall delete from, their real Claude Code
	// config directory. Redirecting the home directory alone is no longer
	// enough to contain them.
	t.Setenv(EnvConfigDir, "")

	got, err := pluginDir()
	if err != nil {
		t.Fatalf("pluginDir: %v", err)
	}
	want := filepath.Join(home, ".claude", "skills", "pigeon")
	if got != want {
		t.Fatalf("pluginDir() = %q, want %q -- a plugin outside personal scope has its monitors silently dropped", got, want)
	}
	return home, got
}

// thisBinary is what Install must write into the plugin: the absolute,
// symlink-resolved path of the running executable.
func thisBinary(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", exe, err)
	}
	return resolved
}

func readJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("parse %s: %v\n%s", path, err, b)
	}
}

// The plugin belongs wherever Claude Code keeps its config, which is not always
// under the home directory. Resolving it from the home directory alone put the
// plugin somewhere Claude Code never looks on a machine that sets
// CLAUDE_CONFIG_DIR, so install reported success and nothing loaded -- and
// `pigeon monitoring on|off`, which rewrites the manifest in place, could not be
// exercised without writing to the real one.
func TestPluginDirFollowsTheClaudeConfigDir(t *testing.T) {
	withHome(t)
	home := withUserHome(t)

	t.Setenv(EnvConfigDir, "")
	got, err := pluginDir()
	if err != nil {
		t.Fatalf("pluginDir: %v", err)
	}
	if want := filepath.Join(home, ".claude", "skills", "pigeon"); got != want {
		t.Errorf("with %s unset, pluginDir() = %q, want %q", EnvConfigDir, got, want)
	}

	elsewhere := t.TempDir()
	t.Setenv(EnvConfigDir, elsewhere)
	got, err = pluginDir()
	if err != nil {
		t.Fatalf("pluginDir: %v", err)
	}
	if want := filepath.Join(elsewhere, "skills", "pigeon"); got != want {
		t.Errorf("pluginDir() = %q, want %q -- a plugin written under the home directory instead would never be loaded", got, want)
	}

	// The whole install has to follow, not just the path calculation: a
	// manifest is only worth writing where something will read it.
	if err := Install("1.2.3", &strings.Builder{}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := os.Stat(filepath.Join(elsewhere, "skills", "pigeon", "hooks", "hooks.json")); err != nil {
		t.Errorf("install did not write into %s: %v", EnvConfigDir, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills", "pigeon")); err == nil {
		t.Error("install also scaffolded under the home directory, which is the path nothing loads")
	}
}

func TestInstallWritesTheWholePlugin(t *testing.T) {
	_, plugin := withPluginHome(t)
	var out strings.Builder
	if err := Install("1.2.3", &out); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// Every file matters: without the manifest the plugin does not load,
	// without hooks.json nothing registers and the session is invisible to its
	// peers, and without .mcp.json the tools the docs advertise simply do not
	// exist for a documented install.
	var man pluginManifest
	readJSONFile(t, filepath.Join(plugin, ".claude-plugin", "plugin.json"), &man)
	if man.Name != "pigeon" {
		t.Errorf("plugin name = %q, want pigeon (it is the load id, pigeon@skills-dir)", man.Name)
	}
	if man.Version != "1.2.3" {
		t.Errorf("plugin version = %q, want the version passed to Install", man.Version)
	}
	if man.Description == "" {
		t.Error("plugin manifest has no description")
	}

	// No monitor by default. This is the assertion the whole design rests on:
	// installing the plugin must not start a background process in every
	// session from then on.
	var mons []monitorSpec
	readJSONFile(t, monitorsPath(plugin), &mons)
	if len(mons) != 0 {
		t.Errorf("a default install listed %d monitor(s), want none: %+v", len(mons), mons)
	}

	// Registration is what replaces it, and it is not optional: a session that
	// never registers has no entry, and with no entry it has no address at all,
	// socket included.
	var hooks hooksFile
	readJSONFile(t, hooksPath(plugin), &hooks)
	start := hooks.Hooks["SessionStart"]
	if len(start) != 1 || len(start[0].Hooks) != 1 {
		t.Fatalf("SessionStart hook is not a single command: %+v", start)
	}
	if !strings.HasSuffix(start[0].Hooks[0].Command, " register") {
		t.Errorf("SessionStart runs %q, want it to end with \" register\"", start[0].Hooks[0].Command)
	}
	// Resume matters as much as startup: it is the case where the monitor's
	// own rearm has never been reliable, so a session comes back alive and
	// unregistered.
	if m := start[0].Matcher; !strings.Contains(m, "startup") || !strings.Contains(m, "resume") {
		t.Errorf("SessionStart matcher = %q, want it to cover startup and resume", m)
	}
	end := hooks.Hooks["SessionEnd"]
	if len(end) != 1 || len(end[0].Hooks) != 1 {
		t.Fatalf("SessionEnd hook is not a single command: %+v", end)
	}
	if !strings.HasSuffix(end[0].Hooks[0].Command, " deregister") {
		t.Errorf("SessionEnd runs %q, want it to end with \" deregister\"", end[0].Hooks[0].Command)
	}

	var mcp struct {
		Servers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	readJSONFile(t, filepath.Join(plugin, ".mcp.json"), &mcp)
	srv, ok := mcp.Servers["pigeon"]
	if !ok {
		t.Fatalf("no server named pigeon in .mcp.json: %+v", mcp.Servers)
	}
	if srv.Command != thisBinary(t) {
		t.Errorf("mcp command = %q, want the absolute binary path %q", srv.Command, thisBinary(t))
	}
	if len(srv.Args) != 1 || srv.Args[0] != "mcp" {
		t.Errorf("mcp args = %v, want [mcp]", srv.Args)
	}

	// Bundled automatically, unlike skills/pigeon-session-coordination: it is
	// strictly informational, so it does not need the same opt-in as a skill
	// carrying opinions about when to message another session.
	skillPath := filepath.Join(plugin, "skills", "pigeon-usage", "SKILL.md")
	skillBody, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("read %s: %v", skillPath, err)
	}
	if !strings.Contains(string(skillBody), "name: pigeon-usage") {
		t.Errorf("bundled skill frontmatter missing name: pigeon-usage:\n%s", skillBody)
	}

	// The state tree has to exist before the first session starts writing to it.
	if fi, err := os.Stat(SessionsDir()); err != nil || !fi.IsDir() {
		t.Errorf("state tree not created by Install: %v", err)
	}
	// The operator needs to be told to restart; monitors cannot be rebound.
	if !strings.Contains(out.String(), "restart") {
		t.Errorf("install output does not mention restarting Claude Code:\n%s", out.String())
	}
}

// TestInstalledMonitorCommandNeverInterpolatesTheSessionID is a regression test.
//
// Writing ${CLAUDE_CODE_SESSION_ID} into the monitor command looks right and is
// the bug that breaks other projects: manifest substitution reads Claude Code's
// own environ, which carries no CLAUDE_* variables, so it expands to nothing and
// the monitor can never identify its session. The monitor reads the variable
// from its own environment instead, so the command must be the plain binary
// path plus " monitor" and nothing else.
func TestInstalledMonitorCommandNeverInterpolatesTheSessionID(t *testing.T) {
	_, plugin := withPluginHome(t)
	// The monitor is only in the manifest when the machine has asked for it,
	// so ask before checking what was written.
	if err := SetMonitorEnabled(true); err != nil {
		t.Fatalf("SetMonitorEnabled: %v", err)
	}
	if err := Install("dev", &strings.Builder{}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	raw, err := os.ReadFile(monitorsPath(plugin))
	if err != nil {
		t.Fatalf("read monitors.json: %v", err)
	}
	for _, bad := range []string{"${" + EnvSessionID + "}", "${", "$" + EnvSessionID} {
		if strings.Contains(string(raw), bad) {
			t.Errorf("monitors.json contains %q; it does not substitute and the monitor would never identify its session:\n%s", bad, raw)
		}
	}

	var mons []monitorSpec
	readJSONFile(t, monitorsPath(plugin), &mons)
	if len(mons) != 1 {
		t.Fatalf("got %d monitors with monitoring on, want exactly 1", len(mons))
	}
	if mons[0].When != "always" {
		t.Errorf("monitor when = %q, want always -- anything else leaves sessions unarmed", mons[0].When)
	}
	cmd := mons[0].Command
	if !strings.HasSuffix(cmd, " monitor") {
		t.Fatalf("monitor command = %q, want it to end with \" monitor\"", cmd)
	}
	// The path must be absolute so the monitor does not depend on PATH being
	// set the same way inside the session.
	bin := strings.TrimSuffix(cmd, " monitor")
	bin = strings.Trim(bin, `"`)
	if !filepath.IsAbs(bin) {
		t.Errorf("monitor binary %q is not an absolute path", bin)
	}
	if want := shellQuote(thisBinary(t)) + " monitor"; cmd != want {
		t.Errorf("monitor command = %q, want %q", cmd, want)
	}
}

func TestInstallIsIdempotent(t *testing.T) {
	_, plugin := withPluginHome(t)
	// Re-running install is the obvious thing to do after an upgrade, so it
	// must overwrite cleanly rather than fail on existing directories.
	if err := Install("1.0.0", &strings.Builder{}); err != nil {
		t.Fatalf("Install (first): %v", err)
	}
	if err := Install("2.0.0", &strings.Builder{}); err != nil {
		t.Fatalf("Install (second): %v", err)
	}

	var man pluginManifest
	readJSONFile(t, filepath.Join(plugin, ".claude-plugin", "plugin.json"), &man)
	if man.Version != "2.0.0" {
		t.Errorf("plugin version = %q, want the newer install to win", man.Version)
	}
	// Reinstalling must not quietly undo the setting in either direction. An
	// upgrade is the obvious thing to run after changing anything, and a
	// reinstall that re-armed every session would hand back the process
	// somebody turned off.
	var mons []monitorSpec
	readJSONFile(t, monitorsPath(plugin), &mons)
	if len(mons) != 0 {
		t.Errorf("got %d monitor(s) after two default installs, want none", len(mons))
	}

	if err := SetMonitorEnabled(true); err != nil {
		t.Fatalf("SetMonitorEnabled: %v", err)
	}
	if err := Install("3.0.0", &strings.Builder{}); err != nil {
		t.Fatalf("Install (third): %v", err)
	}
	readJSONFile(t, monitorsPath(plugin), &mons)
	if len(mons) != 1 {
		t.Errorf("reinstalling with monitoring on gave %d monitor(s), want 1", len(mons))
	}
}

func TestUninstallLeavesStateAloneUnlessPurged(t *testing.T) {
	_, plugin := withPluginHome(t)
	if err := Install("dev", &strings.Builder{}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	// Something worth losing: a registered session with queued mail.
	to := liveEntry(t, "aaaa1111-2222", "alpha", "/tmp/a")
	if _, err := Send(to, Draft{Text: "still queued"}, Sender{Kind: "shell", Name: "sh"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	var out strings.Builder
	if err := Uninstall(false, &out); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(plugin); !os.IsNotExist(err) {
		t.Errorf("plugin directory survived uninstall: %v", err)
	}
	if _, err := ReadEntry("aaaa1111-2222"); err != nil {
		t.Errorf("uninstall took the state tree with it: %v", err)
	}
	if _, err := os.Stat(SpoolPath("aaaa1111-2222")); err != nil {
		t.Errorf("queued mail was destroyed by a plain uninstall: %v", err)
	}
	// Sessions already running keep their monitor, and the operator should know.
	if !strings.Contains(out.String(), "until they restart") {
		t.Errorf("uninstall output does not explain that running sessions keep their monitor:\n%s", out.String())
	}
}

func TestUninstallPurgeRemovesState(t *testing.T) {
	_, plugin := withPluginHome(t)
	if err := Install("dev", &strings.Builder{}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	liveEntry(t, "aaaa1111-2222", "alpha", "/tmp/a")

	if err := Uninstall(true, &strings.Builder{}); err != nil {
		t.Fatalf("Uninstall(purge): %v", err)
	}
	if _, err := os.Stat(plugin); !os.IsNotExist(err) {
		t.Errorf("plugin directory survived a purge: %v", err)
	}
	if _, err := os.Stat(Home()); !os.IsNotExist(err) {
		t.Errorf("state tree survived a purge: %v", err)
	}
}

// --- quoting ---------------------------------------------------------------

func TestShellQuote(t *testing.T) {
	// The monitor command is handed to a shell, so a path that needs quoting
	// and does not get it silently arms nothing.
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain path needs no quoting", "/usr/local/bin/pigeon", "/usr/local/bin/pigeon"},
		{"space", "/opt/my tools/pigeon", `"/opt/my tools/pigeon"`},
		{"double quote", `/opt/a"b/pigeon`, `"/opt/a\"b/pigeon"`},
		{"single quote", "/opt/it's/pigeon", `"/opt/it's/pigeon"`},
		// A dollar or a backtick in a path would otherwise be expanded or run
		// by the shell, which is a command-injection surface, not a typo.
		{"dollar", "/opt/$HOME/pigeon", `"/opt/\$HOME/pigeon"`},
		{"backtick", "/opt/`id`/pigeon", "\"/opt/\\`id\\`/pigeon\""},
		{"backslash", `/opt/a\b/pigeon`, `"/opt/a\\b/pigeon"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shellQuote(c.in); got != c.want {
				t.Errorf("shellQuote(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestMonitorCommandIsAbsolute(t *testing.T) {
	cmd := MonitorCommand()
	if !strings.HasSuffix(cmd, " monitor") {
		t.Fatalf("MonitorCommand() = %q, want it to end with \" monitor\"", cmd)
	}
	bin := strings.Trim(strings.TrimSuffix(cmd, " monitor"), `"`)
	if !filepath.IsAbs(bin) {
		t.Errorf("MonitorCommand() = %q; arming must not depend on PATH", cmd)
	}
}

// --- arm -------------------------------------------------------------------

// holdSessionLock takes the monitor's liveness lock the way a real monitor
// does, so status checks report "someone is listening" without one running.
// The lock conflicts across open descriptions, so this works in-process.
func holdSessionLock(t *testing.T, sid string) {
	t.Helper()
	c, acquired, err := tryExclusive(LockPath(sid))
	if err != nil {
		t.Fatalf("tryExclusive(%s): %v", LockPath(sid), err)
	}
	if !acquired {
		t.Fatalf("could not take %s; nothing else should hold it in a test", LockPath(sid))
	}
	t.Cleanup(func() { _ = c.Close() })
}

func TestArmOutsideASession(t *testing.T) {
	withHome(t)
	t.Setenv(EnvSessionID, "")
	var out strings.Builder
	if err := Arm(&out); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	// There is no session to arm from a plain shell, so the only useful answer
	// is the plugin.
	if !strings.Contains(out.String(), "pigeon install") {
		t.Errorf("Arm outside a session should point at the plugin:\n%s", out.String())
	}
	if strings.Contains(out.String(), "Monitor(") {
		t.Errorf("Arm printed a Monitor call with no session to arm:\n%s", out.String())
	}
}

func TestArmPrintsTheMonitorCallWhenNothingIsListening(t *testing.T) {
	// The printed command is shell-quoted for the POSIX shell Claude Code
	// runs a monitor with. On a platform that arms no monitors there is no
	// such shell, and quoting a Windows path for one is meaningless.
	if !MonitorSupported {
		t.Skip("monitors are not armed on this platform")
	}
	withHome(t)
	const sid = "arm-1111-2222"
	t.Setenv(EnvSessionID, sid)
	liveEntry(t, sid, "alpha", "/tmp/a")

	var out strings.Builder
	if err := Arm(&out); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	got := out.String()
	// Manual arming is a first-class path, so the exact call has to be there
	// ready to run, persistent, and pointing at this binary.
	// "pigeon monitoring on" rather than "pigeon install": installing no longer
	// arms anything, so pointing someone at it would leave them with the same
	// unarmed session they started with.
	for _, want := range []string{"Monitor(", "persistent=true", MonitorCommand(), "pigeon monitoring on"} {
		if !strings.Contains(got, want) {
			t.Errorf("Arm output is missing %q:\n%s", want, got)
		}
	}
}

func TestArmReportsAnAlreadyListeningMonitor(t *testing.T) {
	withHome(t)
	const sid = "arm-3333-4444"
	t.Setenv(EnvSessionID, sid)
	e := liveEntry(t, sid, "alpha", "/tmp/a")
	e.HeartbeatAt = nowRFC3339()
	if err := WriteEntry(e); err != nil {
		t.Fatalf("WriteEntry: %v", err)
	}
	holdSessionLock(t, sid)

	var out strings.Builder
	if err := Arm(&out); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "Already armed") {
		t.Errorf("Arm did not notice the live monitor:\n%s", got)
	}
	// Arming twice would double-deliver, so it must not offer the call.
	if strings.Contains(got, "Monitor(") {
		t.Errorf("Arm offered to arm a second monitor:\n%s", got)
	}
	if !strings.Contains(got, "alpha") {
		t.Errorf("Arm did not report the session's address:\n%s", got)
	}
}

// TestMonitoringSettingRewritesTheManifest: the setting has to reach the file
// Claude Code actually loads, or "off" would mean a monitor that starts and
// stands down -- which is a process per session, which is the thing being
// avoided.
func TestMonitoringSettingRewritesTheManifest(t *testing.T) {
	_, plugin := withPluginHome(t)
	if err := Install("dev", &strings.Builder{}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	var mons []monitorSpec
	readJSONFile(t, monitorsPath(plugin), &mons)
	if len(mons) != 0 {
		t.Fatalf("installed with %d monitor(s), want none by default", len(mons))
	}

	if _, _, err := SyncPluginManifest(true); err != nil {
		t.Fatalf("SyncPluginManifest(true): %v", err)
	}
	readJSONFile(t, monitorsPath(plugin), &mons)
	if len(mons) != 1 {
		t.Fatalf("after turning monitoring on: %d monitor(s), want 1", len(mons))
	}

	if _, _, err := SyncPluginManifest(false); err != nil {
		t.Fatalf("SyncPluginManifest(false): %v", err)
	}
	readJSONFile(t, monitorsPath(plugin), &mons)
	if len(mons) != 0 {
		t.Fatalf("after turning monitoring off: %d monitor(s), want none", len(mons))
	}

	// The hooks survive both, because registration is not what anyone opted
	// out of.
	var hooks hooksFile
	readJSONFile(t, hooksPath(plugin), &hooks)
	if len(hooks.Hooks["SessionStart"]) == 0 {
		t.Error("turning monitoring off removed the registration hook; the session would have no address at all")
	}
}

// A machine that never ran `pigeon install` has nothing to rewrite, and that is
// not a failure: arming by hand is a supported way to run pigeon.
func TestSyncPluginManifestWithNoInstalledPlugin(t *testing.T) {
	withHome(t)
	withUserHome(t)
	changed, _, err := SyncPluginManifest(true)
	if err != nil {
		t.Fatalf("SyncPluginManifest: %v", err)
	}
	if changed {
		t.Error("reported writing a manifest when no plugin is installed")
	}
}

// A one-session override must not be able to rewrite what every other session
// loads.
func TestMonitorConfiguredIgnoresTheEnvironmentOverride(t *testing.T) {
	withHome(t)
	withUserHome(t)
	t.Setenv("XDG_CONFIG_HOME", "")
	writeMonitorConfig(t, UserConfig{})

	t.Setenv(EnvMonitor, MonitorOn)
	if MonitorConfigured() {
		t.Errorf("%s decided the machine's manifest; only the config may", EnvMonitor)
	}
	if on, _ := MonitorEnabled(); !on {
		t.Errorf("%s stopped deciding this session, which it must still do", EnvMonitor)
	}
}
