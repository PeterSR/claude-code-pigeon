package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	wb "github.com/PeterSR/claude-code-weaverbird/provider"

	"github.com/PeterSR/claude-code-pigeon/internal/pigeon"
)

func TestWeaverbirdCommand_Spec(t *testing.T) {
	withHome(t)
	r := invoke(t, "weaverbird", "spec")
	if r.code != 0 {
		t.Fatalf("%s", r)
	}
	var spec wb.Spec
	if err := json.Unmarshal([]byte(r.stdout), &spec); err != nil {
		t.Fatalf("spec output is not JSON: %v\n%s", err, r.stdout)
	}
	if spec.Provider != "pigeon" || spec.Icon != "🕊" {
		t.Errorf("spec = %+v, want provider pigeon icon 🕊", spec)
	}
	if len(spec.Widgets) != 3 || spec.Widgets[0].ID != "pigeon.wait" {
		t.Errorf("Widgets = %+v, want pigeon.wait, pigeon.monitor, pigeon.peers, pigeon.wait first", spec.Widgets)
	}
	if spec.Widgets[0].Cache == nil || spec.Widgets[0].Cache.TTLSec != 30 {
		t.Errorf("Cache = %+v, want ttl_sec=30", spec.Widgets[0].Cache)
	}
	if len(spec.Groups) != 1 || spec.Groups[0].ID != "pigeon.detail" {
		t.Errorf("Groups = %+v, want one pigeon.detail group", spec.Groups)
	}
}

// parseValueNDJSON decodes a "weaverbird value" command's stdout, one JSON
// object per line, into a map keyed by widget id -- WeaverbirdValue can
// now answer for up to three widgets in one call, where before it was
// always exactly one line.
func parseValueNDJSON(t *testing.T, out string) map[string]wb.Value {
	t.Helper()
	byID := map[string]wb.Value{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var v wb.Value
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatalf("value line is not JSON: %v\n%s", err, line)
		}
		byID[v.ID] = v
	}
	return byID
}

// TestWeaverbirdCommand_ValueReportsWaitingCount checks the deaf path end
// to end through the CLI -- including that the id on stdin is what gets
// looked up, since the provider is spawned per render and does not reliably
// inherit CLAUDE_CODE_SESSION_ID -- not just the library call
// internal/pigeon's own tests already cover.
func TestWeaverbirdCommand_ValueReportsWaitingCount(t *testing.T) {
	withHome(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	// A missing monitor is only a fault on a machine that asked for one. No
	// monitor is armed by default, so without this the deaf path being tested
	// here reports "monitor off" instead.
	r0 := invoke(t, "monitoring", "on")
	if r0.code != 0 {
		t.Fatalf("monitoring on: %s", r0)
	}
	beta := register(t, "bbbb2222-0000-0000-0000-000000000000", "beta")
	t.Setenv(pigeon.EnvSessionID, "")

	for i := 0; i < 2; i++ {
		if _, err := pigeon.Send(beta, pigeon.Draft{Text: "queued"}, pigeon.Sender{Kind: "shell", Name: "test"}); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}

	r := invokeStdin(t, `{"session_id":"bbbb2222-0000-0000-0000-000000000000"}`, "weaverbird", "value")
	if r.code != 0 {
		t.Fatalf("%s", r)
	}
	byID := parseValueNDJSON(t, r.stdout)
	v, ok := byID["pigeon.wait"]
	if !ok || v.Class != wb.ClassWarn || v.FullText != "2 waiting" || v.ShortText != "2" {
		t.Errorf("pigeon.wait = %+v, ok=%v, want warn/\"2 waiting\"/\"2\"", v, ok)
	}
	// register (see cmd/pigeon/main_test.go) sets up a deaf entry the
	// same way liveEntry does elsewhere: alive pid, no monitor lock held.
	mv, ok := byID["pigeon.monitor"]
	if !ok || mv.FullText != "monitor deaf" || mv.ShortText != "deaf" || mv.Class != wb.ClassWarn {
		t.Errorf("pigeon.monitor = %+v, ok=%v, want monitor deaf/warn", mv, ok)
	}
}

// TestWeaverbirdCommand_ValueSilentForUnknownSession is the silent case:
// no session id anywhere (neither stdin nor environment) means nothing to
// report, not an error and not a placeholder record.
func TestWeaverbirdCommand_ValueSilentForUnknownSession(t *testing.T) {
	withHome(t)
	t.Setenv(pigeon.EnvSessionID, "")

	r := invokeStdin(t, `{}`, "weaverbird", "value")
	if r.code != 0 {
		t.Fatalf("%s", r)
	}
	if strings.TrimSpace(r.stdout) != "" {
		t.Errorf("stdout = %q, want empty (no session id at all)", r.stdout)
	}
}

// `pigeon statusline` is gone: weaverbird renders pigeon's status now, and
// two commands answering the same question is how they drift apart. An
// unknown subcommand is a usage error, not a silent no-op.
func TestStatuslineCommandIsGone(t *testing.T) {
	withHome(t)
	r := invokeStdin(t, `{"session_id":"bbbb2222-0000-0000-0000-000000000000"}`, "statusline", "--plain")
	if r.code == 0 {
		t.Errorf("`pigeon statusline` still succeeds: %s", r)
	}
	if strings.TrimSpace(r.stdout) != "" {
		t.Errorf("stdout = %q, want nothing on stdout", r.stdout)
	}
}
