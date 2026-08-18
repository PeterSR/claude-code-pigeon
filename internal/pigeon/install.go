package pigeon

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// projectURL is the canonical home of the project, surfaced in the generated
// plugin manifest so an installed plugin says where it came from.
const projectURL = "https://github.com/PeterSR/claude-code-pigeon"

// pigeonUsageSkill is bundled into the plugin by Install. Unlike
// skills/pigeon-session-coordination (an opt-in example carrying opinions
// about when to message another session), this one is strictly informational
// -- the tool list, status meanings, known platform limitations -- which is
// what makes it safe to install as a side effect of running a binary. Edit
// the source, not a copy under a plugin directory: it is never read back.
//
//go:embed pigeonusage/SKILL.md
var pigeonUsageSkill []byte

// Plugin install target. A plugin scaffolded under ~/.claude/skills/<name>
// auto-loads on the next session as <name>@skills-dir, so there is no
// marketplace to stand up and nothing to clone -- the binary writes its own
// plugin.
//
// It must be personal scope: a project-scope plugin's monitors are silently
// dropped, even after the workspace is trusted.
func pluginDir() (string, error) {
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, ".claude", "skills", "pigeon"), nil
}

type pluginManifest struct {
	Name        string       `json:"name"`
	Description string       `json:"description"`
	Version     string       `json:"version"`
	Author      pluginAuthor `json:"author"`
	Homepage    string       `json:"homepage,omitempty"`
	License     string       `json:"license,omitempty"`
}

type pluginAuthor struct {
	Name string `json:"name"`
	URL  string `json:"url,omitempty"`
}

type monitorSpec struct {
	Name        string `json:"name"`
	Command     string `json:"command"`
	Description string `json:"description"`
	When        string `json:"when"`
}

// The hook manifest. Claude Code's schema nests two deep -- an event holds
// matcher groups, a group holds commands -- which is more structure than pigeon
// needs, but writing it flat would simply not load.
type hooksFile struct {
	Description string                 `json:"description"`
	Hooks       map[string][]hookGroup `json:"hooks"`
}

type hookGroup struct {
	// Matcher selects which occurrences of the event fire this group. Empty
	// means all of them, which is what SessionEnd wants.
	Matcher string     `json:"matcher,omitempty"`
	Hooks   []hookSpec `json:"hooks"`
}

type hookSpec struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	// Timeout is in seconds. Registration is a directory scan and two small
	// file writes, so this is a backstop against a wedged filesystem rather
	// than a budget, and it is short because a session start waits on it.
	Timeout int `json:"timeout,omitempty"`
}

// monitorsPath and hooksPath are where the two manifests live inside the plugin.
func monitorsPath(dir string) string { return filepath.Join(dir, "monitors", "monitors.json") }
func hooksPath(dir string) string    { return filepath.Join(dir, "hooks", "hooks.json") }

// monitorManifest is the monitor entry, or nothing.
//
// Nothing is the whole point. `when` accepts only "always" or an on-skill-invoke
// trigger, and a monitor command may not reference ${user_config.*} -- Claude
// Code rejects the plugin outright if it does -- so there is no way to express
// "arm this only if the machine wants it" INSIDE the manifest. The condition
// therefore has to be applied when the manifest is written. A machine that has
// not asked for announcing gets a manifest with no monitor in it, and no
// process is started at session start at all.
func monitorManifest(exe string, on bool) []monitorSpec {
	if !on {
		// An empty array rather than a deleted file: the difference between
		// "this plugin has no monitors" and "this plugin is half-installed" is
		// worth keeping legible, to Claude Code and to anyone reading the
		// directory.
		return []monitorSpec{}
	}
	return []monitorSpec{{
		Name:        "pigeon-inbox",
		Command:     shellQuote(exe) + " monitor",
		Description: "Incoming pigeon messages from other Claude Code sessions",
		When:        "always",
	}}
}

// hooksManifest is what registers a session, and it is written the same way
// whatever the monitor setting says. Being findable is not the part anybody
// opted out of.
//
// SessionStart matches startup and resume but deliberately not "clear": a
// cleared session keeps its process and its socket, and Claude Code mints a new
// id for it, so registering on clear would add a second entry for one session
// and leave the first answering for a spool nothing writes to.
func hooksManifest(exe string) hooksFile {
	q := shellQuote(exe)
	return hooksFile{
		Description: "Registers this session with pigeon so other sessions can find it",
		Hooks: map[string][]hookGroup{
			"SessionStart": {{
				Matcher: "startup|resume",
				Hooks:   []hookSpec{{Type: "command", Command: q + " register", Timeout: 10}},
			}},
			"SessionEnd": {{
				Hooks: []hookSpec{{Type: "command", Command: q + " deregister", Timeout: 10}},
			}},
		},
	}
}

// SyncPluginManifest rewrites the installed plugin's monitor manifest to match
// this machine's setting. Called by `pigeon monitoring on|off`, because that
// setting is now a decision about whether a process exists rather than a value
// something reads at runtime, and a decision like that has to reach the file
// Claude Code actually loads.
//
// A missing plugin directory is not an error: plenty of people arm the monitor
// by hand and never run `pigeon install`, and telling them their setting failed
// would be wrong. It reports whether it wrote anything, so the caller can say
// so honestly.
func SyncPluginManifest(on bool) (changed bool, path string, err error) {
	dir, err := pluginDir()
	if err != nil {
		return false, "", err
	}
	if _, err := os.Stat(dir); err != nil {
		return false, "", nil
	}
	exe, err := os.Executable()
	if err != nil {
		return false, "", fmt.Errorf("locate binary: %w", err)
	}
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	if err := os.MkdirAll(filepath.Dir(monitorsPath(dir)), 0o755); err != nil {
		return false, "", err
	}
	if err := writeJSON(monitorsPath(dir), monitorManifest(exe, on)); err != nil {
		return false, "", err
	}
	// Written here too, so that a plugin installed before hooks existed picks
	// them up the first time somebody touches the setting, rather than staying
	// silently unregisterable.
	if err := os.MkdirAll(filepath.Dir(hooksPath(dir)), 0o755); err != nil {
		return false, "", err
	}
	if err := writeJSON(hooksPath(dir), hooksManifest(exe)); err != nil {
		return false, "", err
	}
	return true, monitorsPath(dir), nil
}

// Install writes the plugin that arms the monitor at session start.
func Install(version string, w io.Writer) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate binary: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return fmt.Errorf("resolve binary: %w", err)
	}

	dir, err := pluginDir()
	if err != nil {
		return err
	}
	for _, d := range []string{
		filepath.Join(dir, ".claude-plugin"),
		filepath.Join(dir, "monitors"),
		filepath.Join(dir, "hooks"),
		filepath.Join(dir, "skills", "pigeon-usage"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}

	man := pluginManifest{
		Name:        "pigeon",
		Description: "Message passing between live Claude Code sessions",
		Version:     version,
		Author:      pluginAuthor{Name: "PeterSR", URL: projectURL},
		Homepage:    projectURL,
		License:     "MIT",
	}
	if err := writeJSON(filepath.Join(dir, ".claude-plugin", "plugin.json"), man); err != nil {
		return err
	}

	// The command is the absolute path to this binary, so the monitor does not
	// depend on PATH being set the same way inside the session.
	//
	// Do NOT try to interpolate ${CLAUDE_CODE_SESSION_ID} here: manifest
	// substitution reads Claude Code's own environ, which carries no CLAUDE_*
	// variables, so it expands to nothing. The monitor reads it from its own
	// environment instead.
	//
	// Whether there IS a monitor entry is this machine's decision, read from
	// the config rather than from MonitorEnabled: PIGEON_MONITOR overrides one
	// session, and one session's override must not rewrite the manifest every
	// other session loads.
	armed := MonitorConfigured()
	if err := writeJSON(monitorsPath(dir), monitorManifest(exe, armed)); err != nil {
		return err
	}

	// The hooks go in unconditionally. They are what makes the monitor
	// optional: a session registers from SessionStart and deregisters from
	// SessionEnd, so it is addressable, and reachable over its socket, without
	// any process of pigeon's running alongside it.
	if err := writeJSON(hooksPath(dir), hooksManifest(exe)); err != nil {
		return err
	}

	// Register the MCP server too, otherwise the tools the docs advertise
	// simply do not exist for a session that installed the documented way.
	// Claude Code injects CLAUDE_CODE_SESSION_ID into MCP servers it spawns,
	// so the server identifies its own session with no extra configuration.
	mcpCfg := map[string]any{
		"mcpServers": map[string]any{
			"pigeon": map[string]any{
				"command": exe,
				"args":    []string{"mcp"},
			},
		},
	}
	if err := writeJSON(filepath.Join(dir, ".mcp.json"), mcpCfg); err != nil {
		return err
	}

	// Strictly informational -- see the doc comment on pigeonUsageSkill for why
	// this one bundles automatically while pigeon-session-coordination does not.
	if err := os.WriteFile(filepath.Join(dir, "skills", "pigeon-usage", "SKILL.md"), pigeonUsageSkill, 0o644); err != nil {
		return err
	}

	if err := EnsureDirs(); err != nil {
		return err
	}

	ns, origin := ResolveNamespace()
	fmt.Fprintf(w, "installed pigeon plugin\n")
	fmt.Fprintf(w, "  plugin:  %s\n", dir)
	fmt.Fprintf(w, "  binary:  %s\n", exe)
	fmt.Fprintf(w, "  state:   %s\n", Home())
	fmt.Fprintf(w, "  ns:      %s (from %s)\n", ns, origin)
	fmt.Fprintf(w, "  mcp:     pigeon (%d tools)\n", len(tools()))
	fmt.Fprintf(w, "  skill:   pigeon-usage (bundled)\n")
	fmt.Fprintf(w, "  hooks:   register on session start, deregister on session end\n")
	if armed {
		fmt.Fprintf(w, "  monitor: armed every session (pigeon monitoring off to stop)\n")
	} else {
		fmt.Fprintf(w, "  monitor: none -- sessions are reachable over their socket\n")
		fmt.Fprintf(w, "           run `pigeon monitoring on` to be told when mail arrives\n")
	}
	fmt.Fprintf(w, "\nloads as pigeon@skills-dir on the NEXT session start.\n")
	fmt.Fprintf(w, "monitors cannot be rebound mid-session, so restart Claude Code.\n")
	return nil
}

// Uninstall removes the plugin, leaving state alone unless purge is set.
func Uninstall(purge bool, w io.Writer) error {
	dir, err := pluginDir()
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	fmt.Fprintf(w, "removed %s\n", dir)
	if purge {
		if err := os.RemoveAll(Home()); err != nil {
			return err
		}
		fmt.Fprintf(w, "removed %s\n", Home())
	}
	fmt.Fprintf(w, "existing sessions keep their monitor until they restart.\n")
	return nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// shellQuote quotes a path for the shell Claude Code runs a monitor command
// with. The command is built from an absolute binary path, so this is defence
// against odd install locations rather than hostile input -- but "odd" covers
// a great many characters, so allow only plainly safe ones through unquoted.
func shellQuote(s string) string {
	if s == "" {
		return `""`
	}
	for _, r := range s {
		safe := r == '/' || r == '.' || r == '-' || r == '_' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !safe {
			return `"` + escapeDouble(s) + `"`
		}
	}
	return s
}

func escapeDouble(s string) string {
	out := make([]rune, 0, len(s)+8)
	for _, r := range s {
		switch r {
		case '"', '\\', '$', '`':
			out = append(out, '\\', r)
		default:
			out = append(out, r)
		}
	}
	return string(out)
}
