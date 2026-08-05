package pigeon

import (
	"fmt"
	"os"
)

// claudeCode is the only Runtime this package implements. Every method here
// delegates to code that already existed before Runtime did -- this file adds
// no new behaviour, it names behaviour that was already scattered across
// monitor.go, claudesession.go, userconfig.go, state.go and sys_unix.go /
// sys_windows.go.
type claudeCode struct{}

func (claudeCode) Name() string { return RuntimeClaudeCode }

// SessionID delegates to CurrentSessionID, which already refuses to guess
// (see its doc comment); this only adds the error Runtime's contract
// requires instead of an empty string a caller could forget to check.
func (claudeCode) SessionID() (string, error) {
	id := CurrentSessionID()
	if id == "" {
		return "", fmt.Errorf("%s is unset or unusable -- not inside an interactive Claude Code session", EnvSessionID)
	}
	return id, nil
}

func (claudeCode) Label(pid int, sessionID string) (name, source string) {
	cs := LookupClaudeSession(pid, sessionID)
	return cs.Name, cs.Source
}

// Budget reports today's fixed numbers, unchanged. BodyBudget and
// RenderBudget stay exported constants -- other code and tests measure
// against them directly -- so this reports them rather than the other way
// around: the constants are the one home for these numbers, Budget just
// answers on their behalf.
func (claudeCode) Budget() (renderRunes, bodyRunes, perMinute int) {
	return RenderBudget, BodyBudget, maxPerMinute
}

func (claudeCode) Supported() bool { return MonitorSupported }

// IsAgentSpawned delegates to InsideClaudeSession. See Runtime's doc comment
// on IsAgentSpawned for what this decision actually gates and why the
// environment variable it reads is trustworthy enough to gate it.
func (claudeCode) IsAgentSpawned() bool { return InsideClaudeSession() }

func (claudeCode) Version() string { return os.Getenv(EnvVersion) }
