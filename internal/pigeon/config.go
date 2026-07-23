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
type ProjectConfig struct {
	// Name is the address this session takes, if no other live session
	// already holds it.
	Name string `json:"name,omitempty"`
	// Description is free text shown in `pigeon ls`.
	Description string `json:"description,omitempty"`
	// Topics are subscribed in addition to the default public mailbox.
	Topics []string `json:"topics,omitempty"`
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
		if err := ValidName(n); err != nil {
			problems = append(problems, fmt.Sprintf("name: %v", err))
		} else {
			out.Name = n
		}
	}

	if d := strings.TrimSpace(raw.Description); d != "" {
		out.Description = truncateRunes(Sanitize(d), maxConfigDescription)
	}

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

	if out.Name == "" && out.Description == "" && len(out.Topics) == 0 {
		// Nothing usable survived. Say so rather than reporting a config that
		// will visibly do nothing.
		return nil, problems, nil
	}
	return out, problems, nil
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
