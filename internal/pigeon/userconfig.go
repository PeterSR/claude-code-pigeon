package pigeon

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// The machine-level config: what *you* have decided, as opposed to what a
// checkout has declared about itself.
//
// The distinction is the whole reason this file exists separately from
// `.claude/pigeon.json`. A project config arrives with a `git clone`, so if
// privacy policy lived there, a cloned repository could mark its own namespace
// private and hide its sessions from you, or mark yours public and expose them.
// Policy has to come from the user; only membership can come from a project. A
// repo may say "my sessions belong in namespace acme"; only this file may say
// "acme is private".
//
// It lives where user configuration lives, not under ~/.claude: this is about
// you, not about any one tool's state directory.

// UserConfigPath is $XDG_CONFIG_HOME/pigeon/config.json, falling back to
// ~/.config/pigeon/config.json as the specification requires.
func UserConfigPath() string {
	if v := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); v != "" {
		return filepath.Join(v, "pigeon", "config.json")
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "pigeon", "config.json")
	}
	return filepath.Join(h, ".config", "pigeon", "config.json")
}

// maxUserConfigBytes bounds the read. The file holds a namespace preference and
// a short table; anything larger is not it.
const maxUserConfigBytes = 64 << 10

// NamespacePolicy is what this machine has decided about one namespace.
type NamespacePolicy struct {
	// Private hides the namespace from anything running inside a Claude Code
	// session, and seals it off from machine-wide topics. See PrivateNamespaces
	// in the README for exactly what that does and does not buy.
	Private bool `json:"private,omitempty"`
}

// UserConfig is the machine-level config file.
type UserConfig struct {
	// Namespace is the default for shell invocations, the same preference
	// `pigeon namespace <name>` writes.
	Namespace string `json:"namespace,omitempty"`
	// Namespaces is policy per namespace. Absent means the default policy,
	// which is not private.
	Namespaces map[string]NamespacePolicy `json:"namespaces,omitempty"`
}

var (
	userConfigOnce   sync.Once
	userConfigCached UserConfig
)

// LoadUserConfig reads the machine config, or returns the zero value when there
// is none. A missing file is the common case and never an error; an unreadable
// or malformed one degrades to "no policy" rather than taking the bus down,
// because a JSON typo must not cost a session its mail.
//
// It is read once per process. Every path that renders a listing consults it,
// and a config that changed halfway through one command would produce a listing
// that was privately inconsistent with itself.
func LoadUserConfig() UserConfig {
	userConfigOnce.Do(func() { userConfigCached = readUserConfig() })
	return userConfigCached
}

func readUserConfig() UserConfig {
	var c UserConfig
	f, err := os.Open(UserConfigPath())
	if err != nil {
		return c
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxUserConfigBytes))
	if err != nil {
		return c
	}
	if json.Unmarshal(b, &c) != nil {
		return UserConfig{}
	}
	if ValidNamespace(c.Namespace) != nil {
		c.Namespace = ""
	}
	// A namespace name that could never be one cannot have a policy either.
	for name := range c.Namespaces {
		if ValidNamespace(name) != nil {
			delete(c.Namespaces, name)
		}
	}
	return c
}

// IsPrivate reports whether this machine has declared a namespace private.
func (n Namespace) IsPrivate() bool {
	return LoadUserConfig().Namespaces[n.String()].Private
}

// SetNamespacePolicy writes one namespace's policy to the machine config,
// preserving everything else in the file.
func SetNamespacePolicy(ns Namespace, p NamespacePolicy) error {
	path := UserConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	c := readUserConfig()
	if c.Namespaces == nil {
		c.Namespaces = map[string]NamespacePolicy{}
	}
	if p == (NamespacePolicy{}) {
		delete(c.Namespaces, ns.String())
	} else {
		c.Namespaces[ns.String()] = p
	}
	if err := writeUserConfig(c); err != nil {
		return err
	}
	// The cache is per-process and this process just invalidated it.
	userConfigCached = c
	return nil
}

// SetUserNamespace persists the namespace shell invocations default to.
func SetUserNamespace(ns Namespace) error {
	path := UserConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	c := readUserConfig()
	c.Namespace = ns.String()
	if err := writeUserConfig(c); err != nil {
		return err
	}
	userConfigCached = c
	return nil
}

func writeUserConfig(c UserConfig) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(UserConfigPath())
	// Written by rename like every other file here, so a reader never sees a
	// half-written policy and briefly treats a private namespace as public.
	tmp, err := os.CreateTemp(dir, "config-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Rename(name, UserConfigPath())
}

// --- who is asking -----------------------------------------------------------

// InsideClaudeSession reports whether this process was spawned by Claude Code.
//
// This is what a private namespace is actually hiding from. The MCP server is
// the obvious surface, but an agent with a shell can run the CLI just as
// easily, and both inherit CLAUDE_CODE_SESSION_ID from the session that started
// them. A terminal you opened yourself does not.
//
// It is a boundary against ordinary reach, not against a determined caller:
// anything that can run `env -u CLAUDE_CODE_SESSION_ID pigeon ls` can still
// look. Making it more than that would need a privilege this process does not
// have. What it buys is that a private namespace never lands in a model's
// context by accident, which is the thing worth having.
func InsideClaudeSession() bool {
	return strings.TrimSpace(os.Getenv(EnvSessionID)) != ""
}

// PrivacyError explains a refusal that only applies inside a session, so the
// message can say how to get the answer rather than only that it was withheld.
type PrivacyError struct{ NS Namespace }

func (e *PrivacyError) Error() string {
	return fmt.Sprintf("namespace %q is private: it cannot be listed or addressed from inside "+
		"a Claude Code session. Run this from your own terminal.", e.NS)
}

// checkPrivateAccess refuses to reach into a private namespace from inside a
// Claude Code session.
//
// A session already in that namespace is unaffected. Privacy here means the
// namespace is not visible or addressable from outside; within it everything
// behaves exactly as it does anywhere else, including seeing the other
// namespaces around it.
// CheckNamespaceAccess is the exported form, for the CLI and the MCP server.
// Every path that accepts a namespace from outside runs it.
func CheckNamespaceAccess(target Namespace) error { return checkPrivateAccess(target) }

func checkPrivateAccess(target Namespace) error {
	if !target.IsPrivate() {
		return nil
	}
	if !InsideClaudeSession() {
		return nil
	}
	if CurrentNamespace().Is(target) {
		return nil
	}
	return &PrivacyError{NS: target}
}

// resetUserConfigForTest clears the per-process cache. Tests write a config and
// then read it back in the same process, which the sync.Once would otherwise
// make impossible.
func resetUserConfigForTest() {
	userConfigOnce = sync.Once{}
	userConfigCached = UserConfig{}
}
