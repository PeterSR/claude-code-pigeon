package pigeon

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Per-project defaults, so a session that starts in a checkout comes up already
// named and already listening to the topics that checkout cares about.
//
// The file travels with the repository, which is the point -- everyone working
// on it gets the same wiring without configuring anything -- and also the
// hazard. A name and description are rendered into *other* sessions'
// notification lines, and a topic name becomes a path component, so every
// field here is validated exactly as strictly as one typed at the CLI. A file
// arriving with a `git clone` is untrusted input.
//
// The identity fields are templates, because the useful defaults are the ones
// a file cannot know: which directory, which branch, how many sessions are
// already here. See template.go for what that costs and how it is bounded.

// ProjectConfigDir and ProjectConfigName locate the file inside a project.
const (
	ProjectConfigDir  = ".claude"
	ProjectConfigName = "pigeon.json"
)

const (
	// maxConfigBytes bounds the read. The file is small by construction, and
	// nothing good comes of streaming a hostile repository's 2 GB "config".
	maxConfigBytes = 64 << 10
	// maxConfigTopics bounds how many logs one config can attach a session to.
	// Each topic is a followed file and a cursor; a config should not be able
	// to open a thousand of them.
	maxConfigTopics = 32
	// maxConfigDescription bounds the free-text field before it reaches a
	// notification line, where the budget is spent on the message itself.
	maxConfigDescription = 200
)

// ProjectConfig is what a project may declare for sessions started in it.
//
// Name, Description and OnNameTaken hold Go text/template source. A string
// with no actions renders to itself, so a plain "api" is still a plain "api"
// and nobody has to escape anything.
type ProjectConfig struct {
	// Name is the address this session takes, if no other live session
	// already holds it.
	Name string `json:"name,omitempty"`
	// Description is free text shown in `pigeon ls`.
	Description string `json:"description,omitempty"`
	// OnNameTaken is tried, once, when Name renders to something another live
	// session already answers to.
	OnNameTaken string `json:"onNameTaken,omitempty"`
	// Topics are subscribed in addition to the default public mailbox.
	Topics []string `json:"topics,omitempty"`
	// Enabled is a pointer because absent has to mean enabled: a plain bool
	// would take every project without the field off the bus.
	Enabled *bool `json:"enabled,omitempty"`
	// Private keeps this project's directory and description out of the
	// registry entry, so neither surfaces in an unrelated session's listing or
	// notification line.
	Private bool `json:"private,omitempty"`
}

// IsEnabled reports whether sessions started in this project take part at all.
func (c *ProjectConfig) IsEnabled() bool {
	return c == nil || c.Enabled == nil || *c.Enabled
}

// empty reports a config that declares nothing, which is reported as no config
// rather than as one that will visibly do nothing.
func (c *ProjectConfig) empty() bool {
	return c.Name == "" && c.Description == "" && c.OnNameTaken == "" &&
		len(c.Topics) == 0 && c.Enabled == nil && !c.Private
}

// Resolved is what a config actually gives one session, once its templates
// have been rendered against that session. Problems carries anything that was
// dropped on the way and why: a session whose name could not be produced still
// comes up, and something has to say what happened.
type Resolved struct {
	Name        string
	Description string
	Topics      []string
	Private     bool
	Problems    []string
}

// Resolve renders a config for one session.
//
// It is what both a starting monitor and `pigeon doctor` ask, so the value
// doctor reports is the value a session would actually get rather than a
// second implementation of the same rules.
func (c *ProjectConfig) Resolve(sessionID, cwd string) Resolved {
	var res Resolved
	if c == nil {
		return res
	}
	res.Topics = c.Topics
	res.Private = c.Private
	ctx := NewTemplateContext(sessionID, cwd)

	if c.Name != "" {
		name, err := renderName(c.Name, ctx)
		switch {
		case err != nil:
			res.Problems = append(res.Problems, fmt.Sprintf("name: %v; staying unnamed", err))
		case !NameTaken(name, sessionID):
			res.Name = name
		case c.OnNameTaken == "":
			res.Problems = append(res.Problems,
				fmt.Sprintf("name %q is already taken by a live session; staying unnamed", name))
		default:
			// Exactly one retry, deliberately. A fallback that is itself taken
			// means this checkout cannot tell its sessions apart, and hunting
			// for a free name would invent an address nobody declared.
			ctx.Name = name
			alt, err := renderName(c.OnNameTaken, ctx)
			switch {
			case err != nil:
				res.Problems = append(res.Problems,
					fmt.Sprintf("name %q is already taken and onNameTaken: %v; staying unnamed", name, err))
			case NameTaken(alt, sessionID):
				res.Problems = append(res.Problems,
					fmt.Sprintf("name %q is already taken and so is the fallback %q; staying unnamed", name, alt))
			default:
				res.Name = alt
			}
		}
	}

	if c.Description != "" {
		desc, err := renderDescription(c.Description, ctx)
		if err != nil {
			res.Problems = append(res.Problems, fmt.Sprintf("description: %v; leaving it unset", err))
		} else {
			res.Description = desc
		}
	}
	return res
}

// ProjectDisabled reports whether a project has taken its own sessions off the
// bus with "enabled": false.
//
// The environment outranks the file in both directions: a launcher states how
// it started a session, which is better information than a file that arrived
// with a clone, so an explicit PIGEON=1 keeps a session addressable in a
// project that disables itself. A config that will not load disables nothing;
// failing closed here would cost a session its mail over a typo.
func ProjectDisabled(projectDir string) bool {
	if optOutSet() {
		return false
	}
	cfg, _, err := LoadProjectConfig(projectDir)
	if err != nil {
		return false
	}
	return !cfg.IsEnabled()
}

// ProjectConfigPath is where the config lives for a given project directory.
func ProjectConfigPath(projectDir string) string {
	return filepath.Join(projectDir, ProjectConfigDir, ProjectConfigName)
}

// LoadProjectConfig reads and validates a project's config.
//
// It returns a nil config when there is no such file, which is the common case
// and not an error. A field that fails validation is dropped and reported in
// problems rather than rejecting the whole file: a typo in one topic should not
// also cost the session its name. Only an unreadable or malformed file is an
// error, because that is a mistake the author wants to hear about.
func LoadProjectConfig(projectDir string) (cfg *ProjectConfig, problems []string, err error) {
	if strings.TrimSpace(projectDir) == "" {
		return nil, nil, nil
	}
	path := ProjectConfigPath(projectDir)

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	defer f.Close()

	b, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(b) > maxConfigBytes {
		return nil, nil, fmt.Errorf("%s is larger than %d bytes", path, maxConfigBytes)
	}

	var raw ProjectConfig
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", path, err)
	}

	out := &ProjectConfig{}

	// A name is an address and is rendered into other sessions' notifications,
	// so it is rejected rather than sanitised. Quietly rewriting it would hand
	// the session an address nobody expects it to answer to.
	if n := strings.TrimSpace(raw.Name); n != "" {
		if err := checkNameSource(n); err != nil {
			problems = append(problems, fmt.Sprintf("name: %v", err))
		} else {
			out.Name = n
		}
	}
	if n := strings.TrimSpace(raw.OnNameTaken); n != "" {
		if err := checkNameSource(n); err != nil {
			problems = append(problems, fmt.Sprintf("onNameTaken: %v", err))
		} else {
			out.OnNameTaken = n
		}
	}

	// A literal description is flattened and bounded here, where the file is.
	// A template's output is not knowable until there is a session to render
	// it against, so that one keeps its source and is flattened and bounded
	// after it renders instead -- see renderDescription.
	if d := strings.TrimSpace(raw.Description); d != "" {
		switch {
		case !hasTemplateActions(d):
			out.Description = truncateRunes(Sanitize(d), maxConfigDescription)
		default:
			if err := checkTemplate(d); err != nil {
				problems = append(problems, fmt.Sprintf("description: %v", err))
			} else {
				out.Description = d
			}
		}
	}

	out.Enabled = raw.Enabled
	out.Private = raw.Private

	seen := map[string]bool{PublicTopic: true}
	for _, t := range raw.Topics {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		if err := ValidTopic(t); err != nil {
			problems = append(problems, fmt.Sprintf("topics: %v", err))
			continue
		}
		if len(out.Topics) >= maxConfigTopics {
			problems = append(problems, fmt.Sprintf("topics: more than %d listed; the rest were ignored", maxConfigTopics))
			break
		}
		seen[t] = true
		out.Topics = append(out.Topics, t)
	}
	sort.Strings(out.Topics)

	if out.empty() {
		// Nothing usable survived. Say so rather than reporting a config that
		// will visibly do nothing.
		return nil, problems, nil
	}
	return out, problems, nil
}

// checkNameSource validates as much of a declared name as the file itself can
// settle. A literal is a name, and is held to exactly the rules a name typed
// at the CLI is, so a typo is reported where it was made. A template is only
// checked for shape here; what it produces is validated in Resolve, against
// the session it produced it for.
func checkNameSource(s string) error {
	if !hasTemplateActions(s) {
		return ValidName(s)
	}
	return checkTemplate(s)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
