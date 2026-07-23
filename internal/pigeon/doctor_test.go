package pigeon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func findCheck(t *testing.T, checks []Check, name string) Check {
	t.Helper()
	for _, c := range checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check named %q in %v", name, checkNames(checks))
	return Check{}
}

func checkNames(checks []Check) []string {
	out := make([]string, 0, len(checks))
	for _, c := range checks {
		out = append(out, c.Name)
	}
	return out
}

// noCheck asserts a check was not emitted at all, which is different from it
// being emitted as ok.
func noCheck(t *testing.T, checks []Check, name string) {
	t.Helper()
	for _, c := range checks {
		if c.Name == name {
			t.Fatalf("unexpected check %q: %+v", name, c)
		}
	}
}

// --- session identity ------------------------------------------------------

// Running doctor from a plain shell is legitimate -- you are checking the
// machine, not a session -- so it warns rather than fails.
func TestDoctorWarnsOutsideASession(t *testing.T) {
	withHome(t)
	t.Setenv(EnvSessionID, "")

	checks := Diagnose()
	if got := findCheck(t, checks, "session"); got.Level != CheckWarn {
		t.Errorf("session check = %v, want warn: %+v", got.Level, got)
	}
	// With no session there is nothing to say about this session.
	noCheck(t, checks, "this session")
}

// A session id reaches a file path. If something has overwritten the variable
// with a value we would refuse to use, say so instead of silently behaving
// like a shell.
func TestDoctorFailsOnUnusableSessionID(t *testing.T) {
	withHome(t)
	t.Setenv(EnvSessionID, "../../etc/passwd")

	got := findCheck(t, Diagnose(), "session")
	if got.Level != CheckFail {
		t.Errorf("session check = %v, want fail: %+v", got.Level, got)
	}
}

func TestDoctorReportsOptOut(t *testing.T) {
	withHome(t)
	t.Setenv(EnvOptOut, "0")

	got := findCheck(t, Diagnose(), "opt-out")
	if got.Level != CheckWarn || !strings.Contains(got.Detail, "off the bus") {
		t.Errorf("opt-out check = %+v", got)
	}
}

// --- host version ----------------------------------------------------------

func TestDoctorFailsOnTooOldClaudeCode(t *testing.T) {
	withHome(t)
	t.Setenv(EnvVersion, "2.1.100")

	got := findCheck(t, Diagnose(), "claude code")
	if got.Level != CheckFail {
		t.Errorf("version check = %+v, want fail", got)
	}
}

// Newer than tested is not broken, it is unverified. Saying so is the point:
// pigeon rides undocumented host behaviour, and an upgrade is the first thing
// to suspect when delivery stops.
func TestDoctorWarnsOnUntestedClaudeCode(t *testing.T) {
	withHome(t)
	t.Setenv(EnvVersion, "9.9.9")

	got := findCheck(t, Diagnose(), "claude code")
	if got.Level != CheckWarn || !strings.Contains(got.Detail, "newer than the tested") {
		t.Errorf("version check = %+v", got)
	}
}

func TestDoctorAcceptsATestedClaudeCode(t *testing.T) {
	withHome(t)
	t.Setenv(EnvVersion, testedCCVersion)

	if got := findCheck(t, Diagnose(), "claude code"); got.Level != CheckOK {
		t.Errorf("version check = %+v, want ok", got)
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"2.1.217", "2.1.217", 0},
		{"2.1.216", "2.1.217", -1},
		{"2.1.218", "2.1.217", 1},
		{"2.2.0", "2.1.999", 1},
		{"3.0", "2.9.9", 1},
		{"2.1", "2.1.0", 0},
		// A prerelease tag must not make a version compare as older than one
		// it is plainly newer than.
		{"2.2.0-beta.1", "2.1.9", 1},
		{"garbage", "2.1.0", -1},
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// --- state directory -------------------------------------------------------

// The spool is an injection surface into a live agent. A group- or
// world-writable state directory means anyone on the box can put text in your
// context, so it is a finding, not a detail -- and it has to be sampled before
// EnsureDirs tightens it back, or the finding is unreachable.
func TestDoctorWarnsOnAWorldWritableStateDir(t *testing.T) {
	requirePOSIXModes(t)
	home := withHome(t)
	if err := os.Chmod(home, 0o777); err != nil {
		t.Skipf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(home, 0o700) })

	got := findCheck(t, Diagnose(), "state dir")
	if got.Level != CheckWarn || !strings.Contains(got.Hint, "inject") {
		t.Errorf("state dir check = %+v", got)
	}
	// Reporting it is not enough; the permission must actually be repaired.
	fi, err := os.Stat(home)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("state dir left at mode %04o, want owner-only", perm)
	}
}

func TestDoctorAcceptsAnOwnerOnlyStateDir(t *testing.T) {
	requirePOSIXModes(t)
	withHome(t)
	if got := findCheck(t, Diagnose(), "state dir"); got.Level != CheckOK {
		t.Errorf("state dir check = %+v, want ok", got)
	}
}

// --- plugin ----------------------------------------------------------------

// withPlugin points the plugin lookup at a temp HOME and scaffolds an install
// there, so these tests never read or write the developer's real ~/.claude.
func withPlugin(t *testing.T) string {
	t.Helper()
	home := withUserHome(t)
	dir := filepath.Join(home, ".claude", "skills", "pigeon")
	for _, d := range []string{filepath.Join(dir, ".claude-plugin"), filepath.Join(dir, "monitors")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	return dir
}

func writePluginMonitor(t *testing.T, dir, command string) {
	t.Helper()
	mons := []monitorSpec{{Name: "pigeon-inbox", Command: command, When: "always"}}
	if err := writeJSON(filepath.Join(dir, "monitors", "monitors.json"), mons); err != nil {
		t.Fatalf("write monitors.json: %v", err)
	}
	cfg := map[string]any{"mcpServers": map[string]any{"pigeon": map[string]any{"command": "/usr/bin/pigeon"}}}
	if err := writeJSON(filepath.Join(dir, ".mcp.json"), cfg); err != nil {
		t.Fatalf("write .mcp.json: %v", err)
	}
}

func TestDoctorFailsWhenThePluginIsNotInstalled(t *testing.T) {
	withHome(t)
	withUserHome(t)

	got := findCheck(t, Diagnose(), "plugin")
	if got.Level != CheckFail || !strings.Contains(got.Hint, "pigeon install") {
		t.Errorf("plugin check = %+v", got)
	}
}

// The upgrade trap: `go install` writes a new binary, the plugin keeps
// pointing at wherever the old one lived, and every session silently arms the
// stale copy. Nothing else reports this.
func TestDoctorFlagsAMissingMonitorBinary(t *testing.T) {
	withHome(t)
	dir := withPlugin(t)
	writePluginMonitor(t, dir, "/nonexistent/pigeon monitor")

	got := findCheck(t, Diagnose(), "monitor binary")
	if got.Level != CheckFail || !strings.Contains(got.Detail, "does not exist") {
		t.Errorf("monitor binary check = %+v", got)
	}
}

func TestDoctorFlagsAPluginPointingAtADifferentBinary(t *testing.T) {
	withHome(t)
	dir := withPlugin(t)
	other := filepath.Join(t.TempDir(), "pigeon")
	if err := os.WriteFile(other, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	writePluginMonitor(t, dir, shellQuote(other)+" monitor")

	got := findCheck(t, Diagnose(), "monitor binary")
	if got.Level != CheckWarn || !strings.Contains(got.Hint, "pigeon install") {
		t.Errorf("monitor binary check = %+v", got)
	}
}

func TestDoctorFlagsANonExecutableMonitorBinary(t *testing.T) {
	// Windows has no execute bit, so there is no such state to detect.
	requirePOSIXModes(t)
	withHome(t)
	dir := withPlugin(t)
	other := filepath.Join(t.TempDir(), "pigeon")
	if err := os.WriteFile(other, []byte("not executable"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	writePluginMonitor(t, dir, shellQuote(other)+" monitor")

	got := findCheck(t, Diagnose(), "monitor binary")
	if got.Level != CheckFail || !strings.Contains(got.Detail, "not executable") {
		t.Errorf("monitor binary check = %+v", got)
	}
}

func TestDoctorFailsOnAnUnreadableMonitorSpec(t *testing.T) {
	withHome(t)
	dir := withPlugin(t)
	if err := os.WriteFile(filepath.Join(dir, "monitors", "monitors.json"), []byte("{{{"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := findCheck(t, Diagnose(), "monitor spec")
	if got.Level != CheckFail {
		t.Errorf("monitor spec check = %+v", got)
	}
}

// Receiving mail and having tools to send it are independent. A missing MCP
// registration warns, because the CLI still works.
func TestDoctorWarnsOnAMissingMCPRegistration(t *testing.T) {
	withHome(t)
	dir := withPlugin(t)
	writePluginMonitor(t, dir, "/nonexistent/pigeon monitor")
	if err := os.Remove(filepath.Join(dir, ".mcp.json")); err != nil {
		t.Fatalf("remove: %v", err)
	}

	got := findCheck(t, Diagnose(), "mcp server")
	if got.Level != CheckWarn {
		t.Errorf("mcp server check = %+v", got)
	}
}

// monitorBinary reverses Install's quoting, so paths that need quoting must
// survive the round trip -- otherwise doctor reports a healthy install as
// broken for anyone whose home directory has a space in it.
func TestMonitorBinaryReversesShellQuote(t *testing.T) {
	for _, path := range []string{
		"/usr/local/bin/pigeon",
		"/home/some one/bin/pigeon",
		`/home/qu"ote/bin/pigeon`,
		`/home/back\slash/bin/pigeon`,
		"/home/dollar$sign/bin/pigeon",
	} {
		cmd := shellQuote(path) + " monitor"
		if got := monitorBinary(cmd); got != path {
			t.Errorf("monitorBinary(%q) = %q, want %q", cmd, got, path)
		}
	}
}

func TestMonitorBinaryRejectsForeignCommands(t *testing.T) {
	for _, cmd := range []string{"", "/usr/bin/something-else", "pigeon mcp"} {
		if got := monitorBinary(cmd); got != "" {
			t.Errorf("monitorBinary(%q) = %q, want empty", cmd, got)
		}
	}
}

// --- this session ----------------------------------------------------------

func TestDoctorFailsForAnUnregisteredSession(t *testing.T) {
	withHome(t)
	t.Setenv(EnvSessionID, "cccc3333")

	got := findCheck(t, Diagnose(), "this session")
	if got.Level != CheckFail || !strings.Contains(got.Detail, "not registered") {
		t.Errorf("this session check = %+v", got)
	}
}

func TestDoctorFailsForADeafSession(t *testing.T) {
	withHome(t)
	liveEntry(t, "bbbb2222", "beta", "/tmp/work")
	t.Setenv(EnvSessionID, "bbbb2222")

	got := findCheck(t, Diagnose(), "this session")
	if got.Level != CheckFail || !strings.Contains(got.Detail, "no monitor is listening") {
		t.Errorf("this session check = %+v", got)
	}
}

func TestDoctorPassesForALiveSession(t *testing.T) {
	withHome(t)
	armed(t, "aaaa1111", "alpha")
	t.Setenv(EnvSessionID, "aaaa1111")

	got := findCheck(t, Diagnose(), "this session")
	if got.Level != CheckOK || !strings.Contains(got.Detail, "pigeon send alpha") {
		t.Errorf("this session check = %+v", got)
	}
}

func TestDoctorReportsQueuedMail(t *testing.T) {
	withHome(t)
	beta := liveEntry(t, "bbbb2222", "beta", "/tmp/work")
	t.Setenv(EnvSessionID, "bbbb2222")
	if _, err := Send(beta, "waiting", Sender{Kind: "shell", Name: "test"}, ""); err != nil {
		t.Fatalf("Send: %v", err)
	}

	got := findCheck(t, Diagnose(), "queued mail")
	if got.Level != CheckWarn || !strings.Contains(got.Detail, "1 message(s)") {
		t.Errorf("queued mail check = %+v", got)
	}
}

func TestDoctorOmitsQueuedMailWhenSpoolIsEmpty(t *testing.T) {
	withHome(t)
	liveEntry(t, "bbbb2222", "beta", "/tmp/work")
	t.Setenv(EnvSessionID, "bbbb2222")

	noCheck(t, Diagnose(), "queued mail")
}

func TestDoctorCountsPeersExcludingSelf(t *testing.T) {
	withHome(t)
	armed(t, "aaaa1111", "alpha")
	liveEntry(t, "bbbb2222", "beta", "/tmp/work")
	t.Setenv(EnvSessionID, "bbbb2222")

	got := findCheck(t, Diagnose(), "peers")
	if !strings.Contains(got.Detail, "1 live") {
		t.Errorf("peers check = %+v, want 1 live", got)
	}
}

// --- rendering -------------------------------------------------------------

func TestDoctorReturnsAnErrorWhenAnythingFails(t *testing.T) {
	withHome(t)
	liveEntry(t, "bbbb2222", "beta", "/tmp/work")
	t.Setenv(EnvSessionID, "bbbb2222")

	var b strings.Builder
	if err := Doctor(&b, false); err == nil {
		t.Error("Doctor returned nil for a deaf session")
	}
	out := b.String()
	if !strings.Contains(out, "FAIL") {
		t.Errorf("output does not mark the failure:\n%s", out)
	}
	// A hint that just restates the problem is noise; one that names the fix
	// is the reason doctor exists.
	if !strings.Contains(out, "->") {
		t.Errorf("output carries no hint:\n%s", out)
	}
}

func TestDoctorJSONIsMachineReadable(t *testing.T) {
	withHome(t)
	armed(t, "aaaa1111", "alpha")
	t.Setenv(EnvSessionID, "aaaa1111")
	t.Setenv(EnvVersion, testedCCVersion)

	var b strings.Builder
	_ = Doctor(&b, true)

	var checks []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal([]byte(b.String()), &checks); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, b.String())
	}
	if len(checks) == 0 {
		t.Fatal("no checks")
	}
	for _, c := range checks {
		switch c.Status {
		case "ok", "warn", "fail":
		default:
			t.Errorf("check %q has status %q", c.Name, c.Status)
		}
	}
}

// --- project config --------------------------------------------------------

// A session named by a file in the checkout is otherwise a small mystery:
// nobody typed the name, and with templates the file does not even contain it.
func TestDoctorReportsTheRenderedProjectConfig(t *testing.T) {
	withHome(t)
	dir := writeProjectConfig(t, `{"name": "{{.Dir | kebab}}-{{.Seq}}", "topics": ["ci"], "private": true}`)
	t.Setenv(EnvProjectDir, dir)
	t.Setenv(EnvSessionID, "aaaa1111")

	got := findCheck(t, Diagnose(), "project config")
	if got.Level != CheckOK {
		t.Fatalf("level = %v: %+v", got.Level, got)
	}
	want := "name=" + fold(filepath.Base(dir), '-') + "-1"
	if !strings.Contains(got.Detail, want) {
		t.Errorf("detail %q does not report the rendered %q", got.Detail, want)
	}
	for _, s := range []string{"topics=ci", "private"} {
		if !strings.Contains(got.Detail, s) {
			t.Errorf("detail %q is missing %q", got.Detail, s)
		}
	}
}

func TestDoctorReportsTemplateProblems(t *testing.T) {
	withHome(t)
	// The name renders to a path, which is not an address. doctor is where you
	// find that out, rather than by starting a session and seeing what happens.
	dir := writeProjectConfig(t, `{"name": "{{.Cwd}}"}`)
	t.Setenv(EnvProjectDir, dir)
	t.Setenv(EnvSessionID, "aaaa1111")

	got := findCheck(t, Diagnose(), "project config")
	if got.Level != CheckWarn {
		t.Fatalf("level = %v, want warn: %+v", got.Level, got)
	}
	if !strings.Contains(got.Detail, "invalid name") {
		t.Errorf("detail %q does not name the problem", got.Detail)
	}
}

func TestDoctorReportsADisabledProject(t *testing.T) {
	withHome(t)
	dir := writeProjectConfig(t, `{"enabled": false}`)
	t.Setenv(EnvProjectDir, dir)
	t.Setenv(EnvOptOut, "")

	got := findCheck(t, Diagnose(), "project config")
	if got.Level != CheckWarn || !strings.Contains(got.Detail, "stay off the bus") {
		t.Errorf("a disabled project was not reported: %+v", got)
	}

	// When the environment overrides it, say which one is actually in force
	// rather than repeating what the file asked for.
	t.Setenv(EnvOptOut, "1")
	got = findCheck(t, Diagnose(), "project config")
	if !strings.Contains(got.Detail, "overridden by "+EnvOptOut) {
		t.Errorf("the override was not reported: %+v", got)
	}
}

func TestCheckLevelString(t *testing.T) {
	cases := map[CheckLevel]string{CheckOK: "ok", CheckWarn: "warn", CheckFail: "fail"}
	for level, want := range cases {
		if got := level.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", level, got, want)
		}
	}
}
