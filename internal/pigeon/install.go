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
	mons := []monitorSpec{{
		Name:        "pigeon-inbox",
		Command:     shellQuote(exe) + " monitor",
		Description: "Incoming pigeon messages from other Claude Code sessions",
		When:        "always",
	}}
	if err := writeJSON(filepath.Join(dir, "monitors", "monitors.json"), mons); err != nil {
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
