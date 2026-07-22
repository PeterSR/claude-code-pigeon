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
	home = t.TempDir()
	t.Setenv("HOME", home)

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

func TestInstallWritesTheWholePlugin(t *testing.T) {
	_, plugin := withPluginHome(t)
	var out strings.Builder
	if err := Install("1.2.3", &out); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// All three files matter: without the manifest the plugin does not load,
	// without monitors.json nothing is armed, and without .mcp.json the tools
	// the docs advertise simply do not exist for a documented install.
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

	var mons []monitorSpec
	readJSONFile(t, filepath.Join(plugin, "monitors", "monitors.json"), &mons)
	if len(mons) != 1 {
		t.Fatalf("got %d monitors, want exactly 1", len(mons))
	}
	mon := mons[0]
	if mon.When != "always" {
		t.Errorf("monitor when = %q, want always -- anything else leaves sessions unarmed", mon.When)
	}
	if mon.Name == "" || mon.Description == "" {
		t.Errorf("monitor is missing a name or description: %+v", mon)
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
	if err := Install("dev", &strings.Builder{}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(plugin, "monitors", "monitors.json"))
	if err != nil {
		t.Fatalf("read monitors.json: %v", err)
	}
	for _, bad := range []string{"${" + EnvSessionID + "}", "${", "$" + EnvSessionID} {
		if strings.Contains(string(raw), bad) {
			t.Errorf("monitors.json contains %q; it does not substitute and the monitor would never identify its session:\n%s", bad, raw)
		}
	}

	var mons []monitorSpec
	readJSONFile(t, filepath.Join(plugin, "monitors", "monitors.json"), &mons)
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
	var mons []monitorSpec
	readJSONFile(t, filepath.Join(plugin, "monitors", "monitors.json"), &mons)
	if len(mons) != 1 {
		t.Errorf("got %d monitors after two installs, want 1", len(mons))
	}
}

func TestUninstallLeavesStateAloneUnlessPurged(t *testing.T) {
	_, plugin := withPluginHome(t)
	if err := Install("dev", &strings.Builder{}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	// Something worth losing: a registered session with queued mail.
	to := liveEntry(t, "aaaa1111-2222", "alpha", "/tmp/a")
	if _, err := Send(to, "still queued", Sender{Kind: "shell", Name: "sh"}, ""); err != nil {
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
	for _, want := range []string{"Monitor(", "persistent=true", MonitorCommand(), "pigeon install"} {
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
