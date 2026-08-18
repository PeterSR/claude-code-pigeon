package pigeon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeMonitorConfig writes a machine config into an isolated home and drops
// the process-wide cache, in the shape TestCurrentTransportPrecedence uses.
func writeMonitorConfig(t *testing.T, c UserConfig) {
	t.Helper()
	cfg := UserConfigPath()
	if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, b, 0o600); err != nil {
		t.Fatal(err)
	}
	resetUserConfigForTest()
	// The cache is process-wide, and SetMonitorEnabled repopulates it. Without
	// this, a config written here outlives the test and every later one in the
	// package inherits it -- which is exactly how a stray `transport: socket`
	// broke tests that never mention transports.
	t.Cleanup(resetUserConfigForTest)
}

func TestMonitorEnabledPrecedence(t *testing.T) {
	withUserHome(t)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv(EnvMonitor, "")

	writeMonitorConfig(t, UserConfig{})
	if on, src := MonitorEnabled(); !on || src != "default" {
		t.Errorf("with nothing set = %v from %q, want true from default", on, src)
	}

	writeMonitorConfig(t, UserConfig{Monitor: MonitorOff})
	if on, src := MonitorEnabled(); on || src != UserConfigPath() {
		t.Errorf("with the config off = %v from %q, want false from the config", on, src)
	}

	// The env var outranks the config, which is what makes arming one session
	// by hand possible on a machine that has turned monitoring off.
	t.Setenv(EnvMonitor, MonitorOn)
	if on, src := MonitorEnabled(); !on || src != EnvMonitor {
		t.Errorf("env over config = %v from %q, want true from %s", on, src, EnvMonitor)
	}
}

// A setting that decides whether a machine receives mail must fail towards
// receiving it. Both spellings of "unusable" are held to that.
func TestUnusableMonitorSettingStaysOn(t *testing.T) {
	withUserHome(t)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv(EnvMonitor, "")

	writeMonitorConfig(t, UserConfig{Monitor: "maybe"})
	if on, _ := MonitorEnabled(); !on {
		t.Error("an unusable config value turned monitoring off; it must be ignored")
	}

	writeMonitorConfig(t, UserConfig{})
	t.Setenv(EnvMonitor, "maybe")
	if on, _ := MonitorEnabled(); !on {
		t.Error("an unusable env value turned monitoring off; it must be ignored")
	}
}

// On is the default, so choosing it should leave no key behind rather than
// writing "on". A config file should record what someone changed.
func TestSetMonitorEnabledRoundTrips(t *testing.T) {
	withUserHome(t)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv(EnvMonitor, "")
	writeMonitorConfig(t, UserConfig{})

	if err := SetMonitorEnabled(false); err != nil {
		t.Fatalf("SetMonitorEnabled(false): %v", err)
	}
	resetUserConfigForTest()
	if on, _ := MonitorEnabled(); on {
		t.Error("monitoring still on after being turned off")
	}

	if err := SetMonitorEnabled(true); err != nil {
		t.Fatalf("SetMonitorEnabled(true): %v", err)
	}
	resetUserConfigForTest()
	if on, _ := MonitorEnabled(); !on {
		t.Error("monitoring still off after being turned on")
	}
	if got := LoadUserConfig().Monitor; got != "" {
		t.Errorf("turning monitoring on wrote %q; the default should leave no key", got)
	}
}

// Everything else in the machine config has to survive the write, since the
// file holds privacy policy that losing would silently widen.
func TestSetMonitorEnabledPreservesTheRestOfTheConfig(t *testing.T) {
	withUserHome(t)
	t.Setenv("XDG_CONFIG_HOME", "")
	writeMonitorConfig(t, UserConfig{
		Transport:  string(TransportSocket),
		Namespaces: map[string]NamespacePolicy{"acme": {Private: true}},
	})

	if err := SetMonitorEnabled(false); err != nil {
		t.Fatalf("SetMonitorEnabled: %v", err)
	}
	resetUserConfigForTest()

	got := LoadUserConfig()
	if got.Transport != string(TransportSocket) {
		t.Errorf("transport = %q, want it preserved", got.Transport)
	}
	if !got.Namespaces["acme"].Private {
		t.Error("namespace privacy was lost by writing the monitor setting")
	}
}
