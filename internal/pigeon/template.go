package pigeon

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
)

// Templating for the identity a project hands its sessions.
//
// A checkout worked on twice at once wants two sessions you can tell apart,
// and only a session knows which one it is: its directory, its branch, how
// many peers are already here. So the config declares a shape rather than a
// literal and it is rendered per session.
//
// That makes this an injection surface twice over. The source arrives with a
// `git clone`, and the result is rendered into *other* sessions' notification
// lines. So the source is bounded before it is parsed, the output is bounded
// while it is written, a rendered name is validated by ValidName rather than
// repaired, and a rendered description is flattened and bounded like any other
// free text. Nothing here may fail a registration: a session whose template is
// broken still comes up, unnamed, with the reason reported.

const (
	// maxTemplateBytes bounds the source before it reaches the parser. A name
	// or description template is a line, not a program.
	maxTemplateBytes = 1024
	// maxRenderBytes bounds what execution may write. Without it,
	// `{{printf "%99999999s" "x"}}` in a cloned repository is a memory bomb
	// pointed at every session started in that checkout.
	maxRenderBytes = 4096
	// maxGitFileBytes bounds the reads that answer `.Branch`. HEAD is one
	// line; a repository that says otherwise is not one we need to read.
	maxGitFileBytes = 4096
	// gitDirName is where a repository keeps HEAD, or a file pointing at
	// wherever a worktree or submodule keeps it instead.
	gitDirName = ".git"
)

var errRenderTooLong = errors.New("rendered more than " + strconv.Itoa(maxRenderBytes) + " bytes")

// TemplateContext is everything a config template may refer to. It is
// deliberately small: each field is either free (already in hand) or one file
// read, because it is computed for every session at startup.
type TemplateContext struct {
	// Cwd is the full project directory.
	Cwd string
	// Dir is its basename, which is what most projects actually want.
	Dir string
	// Branch is the checked-out branch, empty when this is not a repository.
	Branch string
	Host   string
	User   string
	// Session is the full session id; Short is its 8-character prefix, which
	// is what `pigeon ls` shows and what fits in a name.
	Session string
	Short   string
	// Seq is 1 plus the number of live sessions already in this directory, so
	// `{{.Name}}-{{.Seq}}` names the second session in a checkout api-2.
	Seq int
	// Name is populated only for the onNameTaken template, where it holds the
	// name that was already taken. It is empty everywhere else.
	Name string
	// Label is the host's own session name (what /status shows, for Claude
	// Code), and LabelSource is how it arrived at it. They let a project reuse
	// the host label, e.g. `"name": "{{.Label}}"`, and are filled in only when
	// rendering for the current session -- the only one whose index pigeon can
	// key off CLAUDE_PID. They are empty otherwise, including in tests.
	Label       string
	LabelSource string
	// ClaudeName and ClaudeNameSource are deprecated aliases for Label and
	// LabelSource: an existing project config may already reference
	// `.ClaudeName`, so both spellings keep resolving to the same value rather
	// than breaking that config on upgrade.
	ClaudeName       string
	ClaudeNameSource string
}

// NewTemplateContext gathers the facts a template may refer to for one session.
//
// The namespace is needed for .Seq alone, and it matters: peers in another
// namespace are not peers, so counting them would name the first session in a
// checkout api-3 because two unrelated ones happen to sit in the same path.
func NewTemplateContext(ns Namespace, sessionID, cwd string) TemplateContext {
	dir := ""
	if cwd != "" {
		dir = filepath.Base(cwd)
	}
	// The Claude name is only knowable for the current session: its index is
	// keyed by CLAUDE_PID, which names this process's session and no other. For
	// any other id (a test, a peer) the fields stay empty rather than guess.
	var claude ClaudeSession
	if sessionID == CurrentSessionID() {
		claude = LookupClaudeSession(CurrentClaudePID(), sessionID)
	}
	return TemplateContext{
		Cwd:              cwd,
		Dir:              dir,
		Branch:           gitBranch(cwd),
		Host:             hostname(),
		User:             currentUsername(),
		Session:          sessionID,
		Short:            Short(sessionID),
		Seq:              seqInCwd(ns, sessionID, cwd),
		Label:            claude.Name,
		LabelSource:      claude.Source,
		ClaudeName:       claude.Name,
		ClaudeNameSource: claude.Source,
	}
}

// templateFuncs is deliberately tiny. Every function here exists to turn
// something a checkout already knows -- a directory, a branch -- into
// something ValidName accepts, and nothing here can reach outside the process.
var templateFuncs = template.FuncMap{
	"snake": func(s string) string { return fold(s, '_') },
	"kebab": func(s string) string { return fold(s, '-') },
	"lower": strings.ToLower,
	"upper": strings.ToUpper,
	// Argument order puts the string last so both `{{trunc 8 .Dir}}` and
	// `{{.Dir | trunc 8}}` work; a pipeline appends its value as the last
	// argument.
	"trunc": cutRunes,
	"default": func(fallback, s string) string {
		if strings.TrimSpace(s) == "" {
			return fallback
		}
		return s
	},
}

// RenderName renders a name template for one session and validates the result.
func RenderName(src, sessionID, cwd string) (string, error) {
	return CurrentNamespace().RenderName(src, sessionID, cwd)
}

func (n Namespace) RenderName(src, sessionID, cwd string) (string, error) {
	return renderName(src, NewTemplateContext(n, sessionID, cwd))
}

// RenderDescription renders a description template for one session, then
// flattens and bounds it the way any other free text is.
func RenderDescription(src, sessionID, cwd string) (string, error) {
	return CurrentNamespace().RenderDescription(src, sessionID, cwd)
}

func (n Namespace) RenderDescription(src, sessionID, cwd string) (string, error) {
	return renderDescription(src, NewTemplateContext(n, sessionID, cwd))
}

// renderName rejects rather than repairs. Quietly rewriting a rendered name
// would hand the session an address nobody declared, which is the same rule a
// literal name in the config already follows.
func renderName(src string, ctx TemplateContext) (string, error) {
	out, err := renderTemplate(src, ctx)
	if err != nil {
		return "", err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", errors.New("rendered to nothing")
	}
	if err := ValidName(out); err != nil {
		return "", err
	}
	return out, nil
}

func renderDescription(src string, ctx TemplateContext) (string, error) {
	out, err := renderTemplate(src, ctx)
	if err != nil {
		return "", err
	}
	return truncateRunes(Sanitize(out), maxConfigDescription), nil
}

// renderTemplate is the only place a config's text is executed.
func renderTemplate(src string, ctx TemplateContext) (string, error) {
	t, err := parseTemplate(src)
	if err != nil {
		return "", err
	}
	w := &boundedWriter{max: maxRenderBytes}
	if err := t.Execute(w, ctx); err != nil {
		return "", templateError(err)
	}
	return w.b.String(), nil
}

// parseTemplate bounds the source before the parser ever sees it.
func parseTemplate(src string) (*template.Template, error) {
	if len(src) > maxTemplateBytes {
		return nil, fmt.Errorf("template is %d bytes, over the %d limit", len(src), maxTemplateBytes)
	}
	t, err := template.New("pigeon").Funcs(templateFuncs).Parse(src)
	if err != nil {
		return nil, templateError(err)
	}
	return t, nil
}

// checkTemplate reports whether a source is worth keeping: short enough to
// parse, and syntactically valid. It is everything LoadProjectConfig can
// settle about a template, since what one produces is not knowable until there
// is a session to render it against.
func checkTemplate(src string) error {
	_, err := parseTemplate(src)
	return err
}

// hasTemplateActions reports whether a string is a template at all. A plain
// string is a literal and stays one: nobody should have to escape anything to
// call a session "api".
func hasTemplateActions(s string) bool { return strings.Contains(s, "{{") }

// templateError trims the parser's "template: pigeon:1:" prefix and flattens
// the message. The text can quote the offending source, which came from a
// cloned repository, and this ends up in a log line and in `pigeon doctor`.
func templateError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, errRenderTooLong) {
		return errRenderTooLong
	}
	msg := err.Error()
	if _, rest, found := strings.Cut(msg, "template: pigeon:"); found {
		if _, after, ok := strings.Cut(rest, ": "); ok {
			msg = after
		}
	}
	return errors.New(Sanitize(msg))
}

// boundedWriter fails the execution once the output passes max, rather than
// truncating it. A template that produced more than this was not describing a
// name or a one-line description, and half of whatever it was describing is
// not a better answer than none.
type boundedWriter struct {
	b   strings.Builder
	max int
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	if w.b.Len()+len(p) > w.max {
		return 0, errRenderTooLong
	}
	return w.b.Write(p)
}

// seqInCwd counts the live sessions already working in this directory.
//
// The caller is excluded so the count means the same thing whether it is asked
// before this session registers (from a monitor) or after (from the CLI).
// Sessions the registry does not carry a cwd for cannot be counted, which
// includes any that declared themselves private.
func seqInCwd(ns Namespace, sessionID, cwd string) int {
	if cwd == "" {
		return 1
	}
	all, err := ns.ListSessions(false, false)
	if err != nil {
		return 1
	}
	want := filepath.Clean(cwd)
	n := 1
	for _, e := range all {
		if e.SessionID == sessionID || e.Cwd == "" {
			continue
		}
		if filepath.Clean(e.Cwd) == want {
			n++
		}
	}
	return n
}

// gitBranch reads the checked-out branch straight from .git/HEAD.
//
// Deliberately not `git rev-parse`: a monitor starts with every session, and
// spawning a subprocess inside a directory whose contents arrived with a clone
// buys nothing that reading one file does not. Anything unexpected reads as
// "not a repository" and leaves .Branch empty, which `default` can cover.
func gitBranch(dir string) string {
	if dir == "" {
		return ""
	}
	git := filepath.Join(dir, gitDirName)
	fi, err := os.Stat(git)
	if err != nil {
		return ""
	}
	if !fi.IsDir() {
		// A worktree or a submodule keeps .git as a file holding
		// "gitdir: <path>", and that is where HEAD actually lives.
		b, err := readBounded(git, maxGitFileBytes)
		if err != nil {
			return ""
		}
		rest, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
		if !ok {
			return ""
		}
		git = strings.TrimSpace(rest)
		if git == "" {
			return ""
		}
		if !filepath.IsAbs(git) {
			git = filepath.Join(dir, git)
		}
	}

	b, err := readBounded(filepath.Join(git, "HEAD"), maxGitFileBytes)
	if err != nil {
		return ""
	}
	head := strings.TrimSpace(string(b))
	if ref, ok := strings.CutPrefix(head, "ref:"); ok {
		return strings.TrimPrefix(strings.TrimSpace(ref), "refs/heads/")
	}
	// Detached HEAD: the file holds a raw commit. Report it short rather than
	// empty, because a detached checkout is still a distinguishable place to
	// be working, and rather than in full, because a 40-character sha is not a
	// usable address.
	if isHex(head) {
		return Short(head)
	}
	return ""
}

func readBounded(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, max))
}

func isHex(s string) bool {
	if len(s) < 7 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

// currentUsername is the login name, with any Windows domain prefix dropped:
// a backslash cannot appear in a name anyway, and the domain is not part of
// who is working here.
func currentUsername() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	name := u.Username
	if i := strings.LastIndexAny(name, `\/`); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// fold lowercases and replaces every run of characters a name may not contain
// with one separator. This is what makes a real branch usable as an address:
// "feature/API v2" is a plausible branch and not a plausible name.
func fold(s string, sep rune) string {
	var b strings.Builder
	pending := false
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			if pending && b.Len() > 0 {
				b.WriteRune(sep)
			}
			pending = false
			b.WriteRune(r)
		default:
			pending = true
		}
	}
	return b.String()
}

// cutRunes truncates without an ellipsis: the "…" truncate adds elsewhere is
// for display, and a name carrying one would not be a valid address.
func cutRunes(n int, s string) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
