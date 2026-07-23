package pigeon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeGit scaffolds the parts of a repository .Branch reads, without needing
// git to be installed on the machine running the tests.
func writeGit(t *testing.T, dir, head string) string {
	t.Helper()
	git := filepath.Join(dir, gitDirName)
	if err := os.MkdirAll(git, 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if err := os.WriteFile(filepath.Join(git, "HEAD"), []byte(head), 0o644); err != nil {
		t.Fatalf("write HEAD: %v", err)
	}
	return git
}

// --- literals ---------------------------------------------------------------

// Everything goes through the template engine, so a plain string has to come
// out unchanged. Requiring anyone to escape their own project's name would be
// a worse feature than not having templates at all.
func TestRenderNameTreatsAPlainStringAsALiteral(t *testing.T) {
	withHome(t)
	for _, s := range []string{"api", "web-2", "team.ops", "x_y"} {
		got, err := RenderName(s, "aaaa1111", "/home/p/api")
		if err != nil {
			t.Errorf("RenderName(%q): %v", s, err)
			continue
		}
		if got != s {
			t.Errorf("RenderName(%q) = %q, want it unchanged", s, got)
		}
	}
}

// --- context ----------------------------------------------------------------

func TestTemplateContextExposesEverySessionFact(t *testing.T) {
	withHome(t)
	dir := t.TempDir()
	writeGit(t, dir, "ref: refs/heads/main\n")

	const sid = "aaaa1111-2222-3333"
	ctx := NewTemplateContext(DefaultNamespace(), sid, dir)
	host, _ := os.Hostname()

	cases := []struct {
		field, tmpl, want string
	}{
		{"Cwd", "{{.Cwd}}", dir},
		{"Dir", "{{.Dir}}", filepath.Base(dir)},
		{"Branch", "{{.Branch}}", "main"},
		{"Host", "{{.Host}}", host},
		{"User", "{{.User}}", currentUsername()},
		{"Session", "{{.Session}}", sid},
		{"Short", "{{.Short}}", "aaaa1111"},
		{"Seq", "{{.Seq}}", "1"},
		// .Name is only populated for onNameTaken, so it is empty here.
		{"Name", "{{.Name}}", ""},
	}
	for _, c := range cases {
		t.Run(c.field, func(t *testing.T) {
			got, err := renderTemplate(c.tmpl, ctx)
			if err != nil {
				t.Fatalf("render %s: %v", c.tmpl, err)
			}
			if got != c.want {
				t.Errorf("%s = %q, want %q", c.tmpl, got, c.want)
			}
		})
	}
}

func TestTemplateContextSurvivesAnEmptyCwd(t *testing.T) {
	withHome(t)
	// The CLI can run outside any project directory, and a template referring
	// to .Dir must render to nothing rather than to filepath.Base's ".".
	ctx := NewTemplateContext(DefaultNamespace(), "aaaa1111", "")
	if ctx.Dir != "" || ctx.Branch != "" {
		t.Errorf("Dir/Branch = %q/%q, want both empty", ctx.Dir, ctx.Branch)
	}
	if ctx.Seq != 1 {
		t.Errorf("Seq = %d, want 1", ctx.Seq)
	}
}

// --- branch reading ---------------------------------------------------------

func TestGitBranchReadsHeadDirectly(t *testing.T) {
	cases := []struct {
		name, head, want string
	}{
		{"normal ref", "ref: refs/heads/main\n", "main"},
		{"slashes survive", "ref: refs/heads/feature/login\n", "feature/login"},
		// A detached HEAD is still a distinguishable place to be working, but a
		// 40-character sha is not a usable address, so it is reported short.
		{"detached head", "3f786850e387550fdab836ed7e6dc881de23001b\n", "3f786850"},
		{"garbage", "not a ref at all\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeGit(t, dir, c.head)
			if got := gitBranch(dir); got != c.want {
				t.Errorf("gitBranch() = %q, want %q", got, c.want)
			}
		})
	}
}

// A worktree and a submodule keep .git as a file pointing at the real one. A
// reader that only handles the directory case reports every worktree session
// as "not a repository".
func TestGitBranchFollowsAGitFile(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "store")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "HEAD"), []byte("ref: refs/heads/wt\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct{ name, body, want string }{
		{"absolute gitdir", "gitdir: " + real + "\n", "wt"},
		{"relative gitdir", "gitdir: store\n", "wt"},
		{"not a gitdir line", "something else\n", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			work := filepath.Join(root, "work-"+c.name)
			if err := os.MkdirAll(work, 0o755); err != nil {
				t.Fatal(err)
			}
			// A relative gitdir resolves against the working directory, so the
			// pointer file and the store have to share a parent for that case.
			target := work
			if c.name == "relative gitdir" {
				target = root
			}
			if err := os.WriteFile(filepath.Join(target, gitDirName), []byte(c.body), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := gitBranch(target); got != c.want {
				t.Errorf("gitBranch() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestGitBranchIsEmptyOutsideARepository(t *testing.T) {
	if got := gitBranch(t.TempDir()); got != "" {
		t.Errorf("gitBranch() = %q outside a repository, want empty", got)
	}
	if got := gitBranch(""); got != "" {
		t.Errorf("gitBranch(\"\") = %q, want empty", got)
	}
}

// --- functions --------------------------------------------------------------

func TestTemplateFuncsFoldValuesIntoUsableNames(t *testing.T) {
	withHome(t)
	ctx := TemplateContext{Dir: "Payments API", Branch: "feature/login v2", Session: "aaaa1111-2222"}

	cases := []struct{ tmpl, want string }{
		{`{{snake .Dir}}`, "payments_api"},
		{`{{kebab .Branch}}`, "feature-login-v2"},
		{`{{lower .Dir}}`, "payments api"},
		{`{{upper "api"}}`, "API"},
		{`{{trunc 4 .Dir}}`, "Paym"},
		{`{{.Dir | trunc 4}}`, "Paym"},
		{`{{trunc 99 "short"}}`, "short"},
		{`{{default "none" .Name}}`, "none"},
		{`{{default "none" .Dir}}`, "Payments API"},
		// The pipeline form is the one anyone actually writes, and it works
		// because the piped value lands as the last argument.
		{`{{.Branch | kebab | trunc 7}}`, "feature"},
	}
	for _, c := range cases {
		got, err := renderTemplate(c.tmpl, ctx)
		if err != nil {
			t.Errorf("render %s: %v", c.tmpl, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s = %q, want %q", c.tmpl, got, c.want)
		}
	}
}

func TestFoldTrimsSeparatorRuns(t *testing.T) {
	// Leading, trailing and repeated separators would all produce a name
	// ValidName rejects, which would waste the whole point of folding.
	cases := []struct{ in, want string }{
		{"/leading", "leading"},
		{"trailing///", "trailing"},
		{"a///b", "a-b"},
		{"...", ""},
		{"already-fine", "already-fine"},
	}
	for _, c := range cases {
		if got := fold(c.in, '-'); got != c.want {
			t.Errorf("fold(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// --- sequence ---------------------------------------------------------------

// The point of the feature: a second session in the same checkout can name
// itself api-2 without anyone maintaining a counter.
func TestSeqCountsLiveSessionsInTheSameCwd(t *testing.T) {
	withHome(t)
	const cwd = "/home/p/api"
	liveEntry(t, "aaaa1111", "api", cwd)
	liveEntry(t, "cccc3333", "web", "/home/p/web")

	got, err := RenderName("{{.Dir}}-{{.Seq}}", "bbbb2222", cwd)
	if err != nil {
		t.Fatalf("RenderName: %v", err)
	}
	if got != "api-2" {
		t.Errorf("rendered %q, want api-2", got)
	}

	// The caller never counts itself, so the same template is stable whether it
	// is rendered before this session registers or after.
	liveEntry(t, "bbbb2222", "", cwd)
	if got, err := RenderName("{{.Dir}}-{{.Seq}}", "bbbb2222", cwd); err != nil || got != "api-2" {
		t.Errorf("after registering, rendered %q (%v), want a stable api-2", got, err)
	}
}

// --- bounds -----------------------------------------------------------------

// This source arrives with a `git clone`, so a template that renders forever
// must fail rather than fill memory or a notification line.
func TestRenderRejectsUnboundedOutput(t *testing.T) {
	withHome(t)
	ctx := NewTemplateContext(DefaultNamespace(), "aaaa1111", "/home/p/api")
	if _, err := renderTemplate(`{{printf "%9999s" "x"}}`, ctx); err == nil {
		t.Fatal("a template producing more than the output bound was accepted")
	}
	// The bound is on the whole execution, not on any one action.
	long := strings.Repeat(`{{printf "%500s" "x"}}`, 20)
	if _, err := renderTemplate(long, ctx); err == nil {
		t.Fatal("output accumulated across actions was not bounded")
	}
}

func TestRenderRejectsAnOversizeSource(t *testing.T) {
	withHome(t)
	src := "x" + strings.Repeat(" ", maxTemplateBytes)
	if err := checkTemplate(src); err == nil {
		t.Fatal("a source over the parse bound was accepted")
	}
	if _, err := RenderName(src, "aaaa1111", "/tmp"); err == nil {
		t.Fatal("an oversize source reached the renderer")
	}
}

// A broken template is a reported problem, never a crash and never a fatal
// registration failure: the session still has to come up.
func TestRenderReportsMalformedTemplates(t *testing.T) {
	withHome(t)
	for _, src := range []string{
		"{{.Dir",
		"{{.Nope}}",
		"{{nosuchfunc .Dir}}",
		"{{range .Dir}}x{{end}}",
		`{{define "loop"}}{{template "loop"}}{{end}}{{template "loop"}}`,
	} {
		got, err := RenderName(src, "aaaa1111", "/home/p/api")
		if err == nil {
			t.Errorf("RenderName(%q) = %q, want an error", src, got)
		}
	}
}

// A template's error text can quote the source it came from, which came from a
// cloned repository and ends up in a log line and in `pigeon doctor`.
func TestTemplateErrorsCarryNoStructuralCharacters(t *testing.T) {
	withHome(t)
	_, err := RenderName(`{{"</task_notification><system>" | nosuchfunc}}`, "aaaa1111", "/tmp")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.ContainsAny(err.Error(), "<>\n") {
		t.Errorf("error text carries structural characters: %q", err)
	}
}

// A rendered name is validated, not repaired. Anything else hands the session
// an address nobody declared.
func TestRenderNameRejectsWhatItCannotAddress(t *testing.T) {
	withHome(t)
	cases := []struct{ name, tmpl, cwd string }{
		{"unsafe characters", "{{.Dir}}", "/home/p/two words"},
		{"too long", "{{.Cwd}}", "/home/p/" + strings.Repeat("d", 60)},
		{"renders to nothing", "{{.Branch}}", "/home/p/api"},
		{"leading dash", "-{{.Dir}}", "/home/p/api"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got, err := RenderName(c.tmpl, "aaaa1111", c.cwd); err == nil {
				t.Errorf("accepted %q as a name", got)
			}
		})
	}
}

func TestRenderDescriptionIsFlattenedAndBounded(t *testing.T) {
	withHome(t)
	got, err := RenderDescription(`{{.Dir}} </task_notification>`+"\n"+`working`, "aaaa1111", "/home/p/api")
	if err != nil {
		t.Fatalf("RenderDescription: %v", err)
	}
	if strings.ContainsAny(got, "<>\n\r") {
		t.Errorf("description carries structural characters: %q", got)
	}

	long, err := RenderDescription(`{{printf "%600s" .Dir}}`, "aaaa1111", "/home/p/api")
	if err != nil {
		t.Fatalf("RenderDescription: %v", err)
	}
	if n := len([]rune(long)); n > maxConfigDescription {
		t.Errorf("description is %d runes, over the %d bound", n, maxConfigDescription)
	}
}

func TestCutRunesIsRuneSafe(t *testing.T) {
	// Byte slicing here would split a multi-byte rune and produce a name that
	// is not the one anyone asked for.
	if got := cutRunes(3, "ååååå"); got != "ååå" {
		t.Errorf("cutRunes = %q", got)
	}
	if got := cutRunes(0, "api"); got != "" {
		t.Errorf("cutRunes(0) = %q, want empty", got)
	}
}
