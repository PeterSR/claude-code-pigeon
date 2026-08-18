package pigeon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

	// Nothing set is OFF. Installing a plugin is not a decision to run a
	// background process in every session from then on.
	writeMonitorConfig(t, UserConfig{})
	if on, src := MonitorEnabled(); on || src != "default" {
		t.Errorf("with nothing set = %v from %q, want false from default", on, src)
	}

	writeMonitorConfig(t, UserConfig{Monitor: MonitorOn})
	if on, src := MonitorEnabled(); !on || src != UserConfigPath() {
		t.Errorf("with the config on = %v from %q, want true from the config", on, src)
	}

	// The env var outranks the config either way: it is what lets one session
	// opt out of a machine that announces mail, and one session opt in on a
	// machine that does not.
	t.Setenv(EnvMonitor, MonitorOff)
	if on, src := MonitorEnabled(); on || src != EnvMonitor {
		t.Errorf("env over config = %v from %q, want false from %s", on, src, EnvMonitor)
	}

	writeMonitorConfig(t, UserConfig{Monitor: MonitorOff})
	t.Setenv(EnvMonitor, MonitorOn)
	if on, src := MonitorEnabled(); !on || src != EnvMonitor {
		t.Errorf("env over config = %v from %q, want true from %s", on, src, EnvMonitor)
	}
}

// Ignored has to mean ignored: an unusable value decides nothing, and the layer
// below it decides instead. It must not be able to arm a monitor by accident,
// which is the direction that costs somebody a process they never asked for,
// and it must not be able to silence one somebody did ask for either.
func TestAnUnusableMonitorSettingDecidesNothing(t *testing.T) {
	withUserHome(t)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv(EnvMonitor, "")

	// An unusable config value falls through to the default, which is off.
	writeMonitorConfig(t, UserConfig{Monitor: "maybe"})
	if on, _ := MonitorEnabled(); on {
		t.Error("an unusable config value armed a monitor; it must fall through to the default")
	}

	// An unusable env value falls through to the config rather than deciding.
	writeMonitorConfig(t, UserConfig{Monitor: MonitorOn})
	t.Setenv(EnvMonitor, "maybe")
	on, src := MonitorEnabled()
	if !on {
		t.Error("an unusable env value overrode a config that says on")
	}
	if !strings.Contains(src, EnvMonitor) || !strings.Contains(src, UserConfigPath()) {
		t.Errorf("origin = %q, want it to name both the ignored override and what decided", src)
	}

	writeMonitorConfig(t, UserConfig{Monitor: MonitorOff})
	if on, _ := MonitorEnabled(); on {
		t.Error("an unusable env value overrode a config that says off")
	}
}

// Off is the default, so choosing it should leave no key behind rather than
// writing "off". A config file should record what someone changed.
func TestSetMonitorEnabledRoundTrips(t *testing.T) {
	withUserHome(t)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv(EnvMonitor, "")
	writeMonitorConfig(t, UserConfig{})

	if err := SetMonitorEnabled(true); err != nil {
		t.Fatalf("SetMonitorEnabled(true): %v", err)
	}
	resetUserConfigForTest()
	if on, _ := MonitorEnabled(); !on {
		t.Error("monitoring still off after being turned on")
	}

	if err := SetMonitorEnabled(false); err != nil {
		t.Fatalf("SetMonitorEnabled(false): %v", err)
	}
	resetUserConfigForTest()
	if on, _ := MonitorEnabled(); on {
		t.Error("monitoring still on after being turned off")
	}
	if got := LoadUserConfig().Monitor; got != "" {
		t.Errorf("turning monitoring off wrote %q; the default should leave no key", got)
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
