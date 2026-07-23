package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PeterSR/claude-code-pigeon/internal/pigeon"
)

// The CLI is the surface every user touches, and almost all of its behaviour
// is in the wiring rather than the library: which flag maps to which call,
// what lands on stdout versus stderr, and what the exit code is. run() takes
// its edges as arguments precisely so that can be asserted here without
// spawning a process.

type result struct {
	code   int
	stdout string
	stderr string
}

func (r result) String() string {
	return "exit=" + itoa(r.code) + "\n--- stdout ---\n" + r.stdout + "--- stderr ---\n" + r.stderr
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	return string(rune('0' + n))
}

// invoke runs the CLI with no stdin. Tests that need one call invokeStdin.
func invoke(t *testing.T, args ...string) result {
	t.Helper()
	return invokeStdin(t, "", args...)
}

func invokeStdin(t *testing.T, stdin string, args ...string) result {
	t.Helper()
	var out, errb strings.Builder
	code := run(args, strings.NewReader(stdin), &out, &errb)
	return result{code: code, stdout: out.String(), stderr: errb.String()}
}

// withHome isolates state per test. Nothing here may touch a real
// ~/.claude/pigeon, and nothing may leak between tests.
func withHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(pigeon.EnvHome, dir)
	return dir
}

// asSession makes the CLI believe it is running inside a Claude Code session,
// and registers that session so it is addressable.
//
// The entry is deaf, not live: liveness means a monitor holds the flock, and
// there is no monitor in a unit test. Deaf is still a registered, addressable
// session, which is what these tests need.
func asSession(t *testing.T, id, name string) *pigeon.Entry {
	t.Helper()
	t.Setenv(pigeon.EnvSessionID, id)
	return register(t, id, name)
}

func register(t *testing.T, id, name string) *pigeon.Entry {
	t.Helper()
	return registerIn(t, pigeon.CurrentNamespace(), id, name)
}

// registerIn puts a session in a named namespace, for the tests about what one
// namespace shows of another.
func registerIn(t *testing.T, ns pigeon.Namespace, id, name string) *pigeon.Entry {
	t.Helper()
	if err := ns.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}
	pid := os.Getpid()
	e := &pigeon.Entry{
		SessionID: id,
		Name:      name,
		Cwd:       filepath.Join("/tmp", "work-"+name),
		PID:       pid,
		ProcStart: pigeon.ProcStart(pid),
	}
	if err := ns.WriteEntry(e); err != nil {
		t.Fatalf("WriteEntry: %v", err)
	}
	return e
}

// mustNS parses a namespace a test wrote itself, where a rejection is a bug in
// the test rather than a case worth handling.
func mustNS(t *testing.T, name string) pigeon.Namespace {
	t.Helper()
	ns, err := pigeon.ParseNamespace(name)
	if err != nil {
		t.Fatalf("ParseNamespace(%q): %v", name, err)
	}
	return ns
}

func wantContains(t *testing.T, r result, where, substr string) {
	t.Helper()
	got := r.stdout
	if where == "stderr" {
		got = r.stderr
	}
	if !strings.Contains(got, substr) {
		t.Errorf("%s does not contain %q\n%s", where, substr, r)
	}
}

// --- dispatch --------------------------------------------------------------

func TestNoArgsPrintsUsage(t *testing.T) {
	withHome(t)
	r := invoke(t)
	if r.code != 0 {
		t.Errorf("exit = %d, want 0\n%s", r.code, r)
	}
	wantContains(t, r, "stdout", "message passing between live Claude Code sessions")
}

func TestHelpAndVersion(t *testing.T) {
	withHome(t)
	for _, arg := range []string{"help", "--help", "-h"} {
		if r := invoke(t, arg); r.code != 0 || !strings.Contains(r.stdout, "pigeon send") {
			t.Errorf("%s: %s", arg, r)
		}
	}
	for _, arg := range []string{"version", "--version", "-v"} {
		if r := invoke(t, arg); r.code != 0 || !strings.Contains(r.stdout, "pigeon dev") {
			t.Errorf("%s: %s", arg, r)
		}
	}
}

func TestUnknownCommandExitsNonZero(t *testing.T) {
	withHome(t)
	r := invoke(t, "frobnicate")
	if r.code != 1 {
		t.Errorf("exit = %d, want 1\n%s", r.code, r)
	}
	wantContains(t, r, "stderr", `unknown command "frobnicate"`)
	if r.stdout != "" {
		t.Errorf("errors must not go to stdout: %q", r.stdout)
	}
}

// A bad flag must surface as a normal error. With flag.ExitOnError it would
// call os.Exit and take the test binary down with it.
func TestBadFlagDoesNotExitTheProcess(t *testing.T) {
	withHome(t)
	r := invoke(t, "ls", "--nonsense")
	if r.code != 1 {
		t.Errorf("exit = %d, want 1\n%s", r.code, r)
	}
}

// --- ls --------------------------------------------------------------------

func TestListEmptyExplainsHowToRegister(t *testing.T) {
	withHome(t)
	r := invoke(t, "ls")
	wantContains(t, r, "stdout", "no registered pigeon sessions")
	wantContains(t, r, "stdout", "pigeon install")
}

func TestListMarksSelfAndWarnsAboutDeaf(t *testing.T) {
	withHome(t)
	asSession(t, "aaaa1111-0000-0000-0000-000000000000", "alpha")
	register(t, "bbbb2222-0000-0000-0000-000000000000", "beta")

	r := invoke(t, "ls")
	wantContains(t, r, "stdout", "alpha")
	wantContains(t, r, "stdout", "beta")
	wantContains(t, r, "stdout", "* this session, reachable as: pigeon send alpha")
	// A deaf session in the list is the whole reason the status column exists.
	wantContains(t, r, "stdout", "monitor is not listening")
}

func TestListSaysWhenCallerIsNotRegistered(t *testing.T) {
	withHome(t)
	register(t, "bbbb2222-0000-0000-0000-000000000000", "beta")
	t.Setenv(pigeon.EnvSessionID, "cccc3333-0000-0000-0000-000000000000")

	r := invoke(t, "ls")
	wantContains(t, r, "stdout", "is not registered, so nothing can reach it")
}

func TestListFromPlainShellSaysRepliesWillNotWork(t *testing.T) {
	withHome(t)
	register(t, "bbbb2222-0000-0000-0000-000000000000", "beta")
	t.Setenv(pigeon.EnvSessionID, "")

	r := invoke(t, "ls")
	wantContains(t, r, "stdout", "not inside a Claude Code session")
	wantContains(t, r, "stdout", "cannot be replied to")
}

// Status and addr are derived at read time, so --json has to add them
// explicitly or every consumer has to reimplement the liveness rules.
func TestListJSONCarriesDerivedStatusAndAddr(t *testing.T) {
	withHome(t)
	asSession(t, "aaaa1111-0000-0000-0000-000000000000", "alpha")

	r := invoke(t, "ls", "--json")
	if r.code != 0 {
		t.Fatalf("%s", r)
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, r.stdout)
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	if got[0]["status"] != "deaf" || got[0]["addr"] != "alpha" {
		t.Errorf("status/addr = %v/%v, want deaf/alpha", got[0]["status"], got[0]["addr"])
	}
}

// --- send and publish ------------------------------------------------------

func TestSendRequiresTargetAndText(t *testing.T) {
	withHome(t)
	for _, args := range [][]string{{"send"}, {"send", "alpha"}} {
		if r := invoke(t, args...); r.code != 1 || !strings.Contains(r.stderr, "usage:") {
			t.Errorf("%v: %s", args, r)
		}
	}
}

func TestSendUnknownTargetFails(t *testing.T) {
	withHome(t)
	r := invoke(t, "send", "nobody", "hello")
	if r.code != 1 {
		t.Errorf("exit = %d, want 1\n%s", r.code, r)
	}
}

func TestSendQueuesOnTheSpoolAndWarnsWhenDeaf(t *testing.T) {
	withHome(t)
	register(t, "bbbb2222-0000-0000-0000-000000000000", "beta")

	r := invoke(t, "send", "beta", "the build is green")
	if r.code != 0 {
		t.Fatalf("%s", r)
	}
	wantContains(t, r, "stdout", "sent -> bbbb2222 (beta)")
	// The warning is the load-bearing part: a deaf target accepts mail that
	// only `claude --resume` will ever deliver.
	wantContains(t, r, "stderr", "no listening monitor")

	spool, err := os.ReadFile(pigeon.SpoolPath("bbbb2222-0000-0000-0000-000000000000"))
	if err != nil {
		t.Fatalf("read spool: %v", err)
	}
	if !strings.Contains(string(spool), "the build is green") {
		t.Errorf("message did not reach the spool: %s", spool)
	}
}

// Over the inline budget the notification must carry a pointer, and the CLI
// has to say where the body went or the sender assumes it was lost.
func TestSendSpillsLongBodyToAPayloadFile(t *testing.T) {
	withHome(t)
	register(t, "bbbb2222-0000-0000-0000-000000000000", "beta")

	r := invoke(t, "send", "beta", strings.Repeat("x", pigeon.BodyBudget+50))
	if r.code != 0 {
		t.Fatalf("%s", r)
	}
	wantContains(t, r, "stdout", "full text at")
}

func TestPublishReportsSubscriberCountExcludingSelf(t *testing.T) {
	withHome(t)
	asSession(t, "aaaa1111-0000-0000-0000-000000000000", "alpha")
	beta := register(t, "bbbb2222-0000-0000-0000-000000000000", "beta")
	if err := pigeon.Subscribe(beta.SessionID, "deploys"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := pigeon.Subscribe("aaaa1111-0000-0000-0000-000000000000", "deploys"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	r := invoke(t, "publish", "deploys", "shipping in 5")
	wantContains(t, r, "stdout", "published to #deploys (1 subscriber(s) besides you)")
}

func TestPublishRequiresTopicAndText(t *testing.T) {
	withHome(t)
	if r := invoke(t, "publish", "deploys"); r.code != 1 {
		t.Errorf("%s", r)
	}
}

// --- subscriptions ---------------------------------------------------------

func TestSubscribeAndUnsubscribeRoundTrip(t *testing.T) {
	withHome(t)
	asSession(t, "aaaa1111-0000-0000-0000-000000000000", "alpha")

	if r := invoke(t, "subscribe", "deploys"); r.code != 0 {
		t.Fatalf("%s", r)
	}
	r := invoke(t, "topics")
	wantContains(t, r, "stdout", "deploys")
	wantContains(t, r, "stdout", "*") // marked as mine

	if r := invoke(t, "unsubscribe", "deploys"); r.code != 0 {
		t.Fatalf("%s", r)
	}
	e, err := pigeon.ReadEntry("aaaa1111-0000-0000-0000-000000000000")
	if err != nil {
		t.Fatalf("ReadEntry: %v", err)
	}
	for _, s := range e.Subscriptions {
		if s == "deploys" {
			t.Errorf("still subscribed: %v", e.Subscriptions)
		}
	}
}

func TestSubscribeOutsideASessionFails(t *testing.T) {
	withHome(t)
	t.Setenv(pigeon.EnvSessionID, "")
	r := invoke(t, "subscribe", "deploys")
	if r.code != 1 {
		t.Errorf("exit = %d, want 1\n%s", r.code, r)
	}
	wantContains(t, r, "stderr", "not inside a Claude Code session")
}

func TestSubscribeTakesExactlyOneTopic(t *testing.T) {
	withHome(t)
	asSession(t, "aaaa1111-0000-0000-0000-000000000000", "alpha")
	for _, args := range [][]string{{"subscribe"}, {"subscribe", "a", "b"}} {
		if r := invoke(t, args...); r.code != 1 {
			t.Errorf("%v: %s", args, r)
		}
	}
}

// --- identity --------------------------------------------------------------

func TestNameSetsAnAddressAndRejectsBadOnes(t *testing.T) {
	withHome(t)
	asSession(t, "aaaa1111-0000-0000-0000-000000000000", "")

	if r := invoke(t, "name", "alpha"); r.code != 0 {
		t.Fatalf("%s", r)
	}
	if r := invoke(t, "name"); !strings.Contains(r.stdout, "alpha") {
		t.Errorf("name readback: %s", r)
	}
	// A name is rendered into other sessions' notifications, so structural
	// characters must never reach it.
	if r := invoke(t, "name", "al pha!"); r.code != 1 {
		t.Errorf("accepted an unsafe name: %s", r)
	}
}

func TestNameRejectsOneAlreadyTaken(t *testing.T) {
	withHome(t)
	register(t, "bbbb2222-0000-0000-0000-000000000000", "beta")
	asSession(t, "aaaa1111-0000-0000-0000-000000000000", "")

	r := invoke(t, "name", "beta")
	if r.code != 1 {
		t.Errorf("exit = %d, want 1\n%s", r.code, r)
	}
	wantContains(t, r, "stderr", "already uses the name")
}

func TestNameFromATemplate(t *testing.T) {
	withHome(t)
	t.Setenv(pigeon.EnvProjectDir, "/home/p/api")
	asSession(t, "aaaa1111-0000-0000-0000-000000000000", "")

	if r := invoke(t, "name", "--template", "{{.Dir}}-{{.Seq}}"); r.code != 0 {
		t.Fatalf("%s", r)
	}
	if r := invoke(t, "name"); !strings.Contains(r.stdout, "api-1") {
		t.Errorf("name readback: %s", r)
	}
	// A template renders to a name or to nothing; it never renders to an
	// address this session may not answer to.
	r := invoke(t, "name", "--template", "{{.Cwd}}")
	if r.code != 1 {
		t.Errorf("accepted a rendered path as a name: %s", r)
	}
	wantContains(t, r, "stderr", "invalid name")

	if r := invoke(t, "name", "--template", "{{.Dir}}", "literal"); r.code != 1 {
		t.Errorf("accepted a template and a literal together: %s", r)
	}
}

// A collision has to name the value that collided, since the template itself
// is not the thing another session is holding.
func TestNameTemplateReportsACollision(t *testing.T) {
	withHome(t)
	t.Setenv(pigeon.EnvProjectDir, "/home/p/api")
	register(t, "bbbb2222-0000-0000-0000-000000000000", "api")
	asSession(t, "aaaa1111-0000-0000-0000-000000000000", "")

	r := invoke(t, "name", "--template", "{{.Dir}}")
	if r.code != 1 {
		t.Fatalf("a taken name was stolen: %s", r)
	}
	wantContains(t, r, "stderr", `the template rendered "api"`)
}

func TestDescribeFromATemplate(t *testing.T) {
	withHome(t)
	t.Setenv(pigeon.EnvProjectDir, "/home/p/api")
	asSession(t, "aaaa1111-0000-0000-0000-000000000000", "alpha")

	if r := invoke(t, "describe", "--template", `{{.Dir}} on {{.Branch | default "no branch"}}`); r.code != 0 {
		t.Fatalf("%s", r)
	}
	if r := invoke(t, "describe"); !strings.Contains(r.stdout, "api on no branch") {
		t.Errorf("describe readback: %s", r)
	}
	if r := invoke(t, "describe", "--template", "{{.Nope}}"); r.code != 1 {
		t.Errorf("a broken template was accepted: %s", r)
	}
	if r := invoke(t, "describe", "--template", "{{.Dir}}", "literal"); r.code != 1 {
		t.Errorf("accepted a template and a literal together: %s", r)
	}
	if r := invoke(t, "describe", "--nonsense"); r.code != 1 {
		t.Errorf("a bad flag did not surface as an error: %s", r)
	}
}

// A private session's blank cwd and description are a deliberate policy, not a
// half-finished registration. Both commands that would otherwise look broken
// have to say which.
func TestPrivateSessionSaysWhyItsFieldsAreBlank(t *testing.T) {
	withHome(t)
	e := asSession(t, "aaaa1111-0000-0000-0000-000000000000", "client")
	e.Private = true
	if err := pigeon.WriteEntry(e); err != nil {
		t.Fatalf("WriteEntry: %v", err)
	}

	wantContains(t, invoke(t, "whoami"), "stdout", "private:")
	r := invoke(t, "describe", "the acquisition")
	wantContains(t, r, "stdout", "not published to other sessions")
	stored, err := pigeon.ReadEntry(e.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Description != "" {
		t.Errorf("description = %q, want it withheld", stored.Description)
	}
}

func TestDescribeStoresSanitisedText(t *testing.T) {
	withHome(t)
	asSession(t, "aaaa1111-0000-0000-0000-000000000000", "alpha")

	if r := invoke(t, "describe", "refactoring\nthe parser"); r.code != 0 {
		t.Fatalf("%s", r)
	}
	e, err := pigeon.ReadEntry("aaaa1111-0000-0000-0000-000000000000")
	if err != nil {
		t.Fatalf("ReadEntry: %v", err)
	}
	if strings.ContainsAny(e.Description, "\n\r") {
		t.Errorf("newline survived into the description: %q", e.Description)
	}
	if r := invoke(t, "describe"); !strings.Contains(r.stdout, "refactoring the parser") {
		t.Errorf("describe readback: %s", r)
	}
}

func TestWhoamiInsideAndOutsideASession(t *testing.T) {
	withHome(t)
	asSession(t, "aaaa1111-0000-0000-0000-000000000000", "alpha")
	r := invoke(t, "whoami")
	wantContains(t, r, "stdout", "others reach you with:  pigeon send alpha")

	t.Setenv(pigeon.EnvSessionID, "")
	r = invoke(t, "whoami")
	wantContains(t, r, "stdout", "not inside a Claude Code session")
}

func TestWhoamiSaysWhenUnregistered(t *testing.T) {
	withHome(t)
	t.Setenv(pigeon.EnvSessionID, "cccc3333-0000-0000-0000-000000000000")
	r := invoke(t, "whoami")
	wantContains(t, r, "stdout", "not registered")
}

// --- doctor and statusline -------------------------------------------------

func TestDoctorJSONListsChecks(t *testing.T) {
	withHome(t)
	asSession(t, "aaaa1111-0000-0000-0000-000000000000", "alpha")

	r := invoke(t, "doctor", "--json")
	var checks []map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &checks); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, r.stdout)
	}
	if len(checks) == 0 {
		t.Fatal("no checks reported")
	}
	// A deaf session cannot receive, so doctor has to fail rather than report
	// a clean bill of health.
	if r.code != 1 {
		t.Errorf("exit = %d, want 1 for a deaf session\n%s", r.code, r)
	}
}

func TestDoctorTextMentionsTheBrokenLink(t *testing.T) {
	withHome(t)
	asSession(t, "aaaa1111-0000-0000-0000-000000000000", "alpha")

	r := invoke(t, "doctor")
	wantContains(t, r, "stdout", "this session")
	wantContains(t, r, "stdout", "FAIL")
}

// The statusline is spawned per render and does not reliably inherit
// CLAUDE_CODE_SESSION_ID, so the id on stdin has to win.
func TestStatuslineTakesSessionIDFromStdin(t *testing.T) {
	withHome(t)
	register(t, "bbbb2222-0000-0000-0000-000000000000", "beta")
	t.Setenv(pigeon.EnvSessionID, "")

	r := invokeStdin(t, `{"session_id":"bbbb2222-0000-0000-0000-000000000000"}`, "statusline", "--plain")
	if r.code != 0 {
		t.Fatalf("%s", r)
	}
	wantContains(t, r, "stdout", "pigeon deaf")
}

func TestStatuslineSaysNothingForAnUnknownSession(t *testing.T) {
	withHome(t)
	t.Setenv(pigeon.EnvSessionID, "")
	r := invokeStdin(t, `{}`, "statusline")
	if r.stdout != "" {
		t.Errorf("stdout = %q, want empty", r.stdout)
	}
}

// --- install and uninstall -------------------------------------------------

// withTempHome redirects the plugin lookup at a throwaway directory. Without
// it these tests would scaffold and then delete the developer's real
// ~/.claude/skills/pigeon.
func withTempHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	return home
}

func TestInstallThenUninstallRemovesThePlugin(t *testing.T) {
	withHome(t)
	home := withTempHome(t)
	plugin := filepath.Join(home, ".claude", "skills", "pigeon")

	if r := invoke(t, "install"); r.code != 0 {
		t.Fatalf("%s", r)
	}
	if _, err := os.Stat(filepath.Join(plugin, "monitors", "monitors.json")); err != nil {
		t.Fatalf("install wrote no monitor spec: %v", err)
	}

	r := invoke(t, "uninstall")
	if r.code != 0 {
		t.Fatalf("%s", r)
	}
	if _, err := os.Stat(plugin); !os.IsNotExist(err) {
		t.Errorf("plugin still present after uninstall: %v", err)
	}
	// Uninstall must not take queued mail with it -- that is what --purge is
	// for, and losing it silently would be the worse default.
	wantContains(t, r, "stdout", "existing sessions keep their monitor")
}

func TestUninstallPurgeAlsoRemovesState(t *testing.T) {
	stateDir := withHome(t)
	withTempHome(t)

	if r := invoke(t, "install"); r.code != 0 {
		t.Fatalf("%s", r)
	}
	if r := invoke(t, "uninstall", "--purge"); r.code != 0 {
		t.Fatalf("%s", r)
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Errorf("state dir survived --purge: %v", err)
	}
}

// --- stdin handling --------------------------------------------------------

// Claude Code pipes the statusline a JSON payload. A file or pipe must be
// read; a terminal must not be, or running `pigeon statusline` by hand hangs
// with no indication why.
func TestStatuslineStdinReadsAPipedFile(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "input-*")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer f.Close()
	if _, err := f.WriteString(`{"session_id":"aaaa1111"}`); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatalf("seek: %v", err)
	}

	got := statuslineStdin(f)
	if got == nil {
		t.Fatal("a regular file was treated as a terminal")
	}
	b, err := io.ReadAll(got)
	if err != nil || !strings.Contains(string(b), "aaaa1111") {
		t.Errorf("read back %q, err %v", b, err)
	}
}

func TestStatuslineStdinPassesThroughNonFiles(t *testing.T) {
	in := strings.NewReader("{}")
	if statuslineStdin(in) != in {
		t.Error("a non-file reader was not passed through")
	}
}

// --- prune -----------------------------------------------------------------

func TestPruneReportsWhatItReclaimed(t *testing.T) {
	withHome(t)
	asSession(t, "aaaa1111-0000-0000-0000-000000000000", "alpha")

	r := invoke(t, "prune")
	if r.code != 0 {
		t.Fatalf("%s", r)
	}
	wantContains(t, r, "stdout", "pruned")
	wantContains(t, r, "stdout", "orphaned state file(s)")
	wantContains(t, r, "stdout", "reclaimed")
}

// --- namespaces ------------------------------------------------------------

// Isolation you have forgotten about looks exactly like an empty machine. The
// footer is what makes the mechanism discoverable again, and it appears only
// when there is in fact something hidden.
func TestListFooterCountsWhatIsHidden(t *testing.T) {
	withHome(t)
	asSession(t, "aaaa1111-0000-0000-0000-000000000000", "alpha")

	if r := invoke(t, "ls"); strings.Contains(r.stdout, "other namespace") {
		t.Errorf("a footer appeared with nothing hidden:\n%s", r)
	}

	acme, other := mustNS(t, "acme"), mustNS(t, "other")
	registerIn(t, acme, "bbbb2222-0000-0000-0000-000000000000", "beta")
	registerIn(t, other, "cccc3333-0000-0000-0000-000000000000", "gamma")
	registerIn(t, other, "dddd4444-0000-0000-0000-000000000000", "delta")

	r := invoke(t, "ls")
	wantContains(t, r, "stdout", "3 session(s) in 2 other namespace(s) (--all-namespaces)")
	if strings.Contains(r.stdout, "beta") {
		t.Errorf("ls listed a session from another namespace:\n%s", r)
	}

	// An empty namespace is the case where the footer matters most: without it
	// the answer reads as "nobody is running".
	r = invoke(t, "ls", "-n", "empty-one")
	wantContains(t, r, "stdout", "no registered pigeon sessions in namespace empty-one")
	wantContains(t, r, "stdout", "other namespace(s)")
}

func TestListAllNamespacesAddsAColumn(t *testing.T) {
	withHome(t)
	asSession(t, "aaaa1111-0000-0000-0000-000000000000", "alpha")
	registerIn(t, mustNS(t, "acme"), "bbbb2222-0000-0000-0000-000000000000", "beta")

	r := invoke(t, "ls", "--all-namespaces")
	for _, want := range []string{"NAMESPACE", "acme", "default", "alpha", "beta"} {
		wantContains(t, r, "stdout", want)
	}
	// The footer is about what a namespaced listing hides, and this one hides
	// nothing.
	if strings.Contains(r.stdout, "--all-namespaces)") {
		t.Errorf("the footer appeared in an all-namespaces listing:\n%s", r)
	}
	if r := invoke(t, "ls", "--all-namespaces", "-n", "acme"); r.code != 1 {
		t.Errorf("two conflicting scopes were accepted: %s", r)
	}
}

func TestListJSONCarriesTheNamespace(t *testing.T) {
	withHome(t)
	registerIn(t, mustNS(t, "acme"), "bbbb2222-0000-0000-0000-000000000000", "beta")

	r := invoke(t, "ls", "--json", "-n", "acme")
	var got []map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, r.stdout)
	}
	if len(got) != 1 || got[0]["namespace"] != "acme" {
		t.Errorf("namespace missing from --json, so consumers must infer it: %v", got)
	}
}

// Sending across is allowed: anyone who can write the state directory could
// append to that spool by hand, so refusing would buy inconvenience and no
// isolation.
func TestCrossNamespaceSendNeedsTheFlag(t *testing.T) {
	withHome(t)
	acme := mustNS(t, "acme")
	registerIn(t, acme, "bbbb2222-0000-0000-0000-000000000000", "beta")

	if r := invoke(t, "send", "beta", "hello"); r.code != 1 {
		t.Errorf("a session in another namespace resolved without -n: %s", r)
	}
	r := invoke(t, "send", "-n", "acme", "beta", "hello there")
	if r.code != 0 {
		t.Fatalf("%s", r)
	}
	wantContains(t, r, "stdout", "sent -> bbbb2222 (beta) in acme")

	spool, err := os.ReadFile(acme.SpoolPath("bbbb2222-0000-0000-0000-000000000000"))
	if err != nil {
		t.Fatalf("read spool: %v", err)
	}
	if !strings.Contains(string(spool), "hello there") {
		t.Errorf("message did not reach the recipient's namespace: %s", spool)
	}
	// --namespace is the same flag, not a second setting that could disagree.
	if r := invoke(t, "send", "--namespace", "acme", "beta", "again"); r.code != 0 {
		t.Errorf("--namespace was not accepted: %s", r)
	}
	if r := invoke(t, "send", "-n", "../escape", "beta", "x"); r.code != 1 {
		t.Errorf("a traversing namespace was accepted: %s", r)
	}
}

func TestPublishToAGlobalTopic(t *testing.T) {
	withHome(t)
	asSession(t, "aaaa1111-0000-0000-0000-000000000000", "alpha")
	other := mustNS(t, "other")
	beta := registerIn(t, other, "bbbb2222-0000-0000-0000-000000000000", "beta")
	if err := other.Subscribe(beta.SessionID, "@ops"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// The subscriber is in another namespace, which is the whole point of "@".
	r := invoke(t, "publish", "@ops", "all hands")
	wantContains(t, r, "stdout", "published to @ops (1 subscriber(s) besides you)")

	r = invoke(t, "topics")
	wantContains(t, r, "stdout", "@ops")
	wantContains(t, r, "stdout", "@all")
}

func TestTopicsAcrossNamespaces(t *testing.T) {
	withHome(t)
	asSession(t, "aaaa1111-0000-0000-0000-000000000000", "alpha")
	if r := invoke(t, "subscribe", "deploys"); r.code != 0 {
		t.Fatalf("%s", r)
	}
	other := mustNS(t, "other")
	beta := registerIn(t, other, "bbbb2222-0000-0000-0000-000000000000", "beta")
	if err := other.Subscribe(beta.SessionID, "secrets"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if _, err := other.Publish("@ops", "all hands", pigeon.Sender{Kind: "shell", Name: "sh"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// A namespaced listing shows this namespace's topics plus the shared ones,
	// which is exactly the set you can publish to from here.
	r := invoke(t, "topics")
	wantContains(t, r, "stdout", "deploys")
	wantContains(t, r, "stdout", "@ops")
	if strings.Contains(r.stdout, "secrets") {
		t.Errorf("a topic from another namespace was listed:\n%s", r)
	}

	r = invoke(t, "topics", "--all-namespaces")
	for _, want := range []string{"NAMESPACE", "deploys", "secrets", "other"} {
		wantContains(t, r, "stdout", want)
	}
	// A global topic belongs to no namespace, and is listed once rather than
	// once per namespace.
	if n := strings.Count(r.stdout, "@ops"); n != 1 {
		t.Errorf("@ops appears %d times, want once:\n%s", n, r)
	}
	if r := invoke(t, "topics", "--all-namespaces", "-n", "other"); r.code != 1 {
		t.Errorf("two conflicting scopes were accepted: %s", r)
	}
}

// A namespace that cannot be a directory name is refused everywhere it is
// accepted, rather than replaced: acting on "default" instead of what somebody
// typed is how a message reaches the wrong people.
func TestBadNamespaceIsRefusedEverywhere(t *testing.T) {
	withHome(t)
	asSession(t, "aaaa1111-0000-0000-0000-000000000000", "alpha")
	for _, args := range [][]string{
		{"ls", "-n", "../escape"},
		{"topics", "-n", "Caps"},
		{"prune", "-n", "with space"},
		{"publish", "-n", "../escape", "deploys", "hi"},
		{"prune", "--all-namespaces", "-n", "acme"},
		{"namespace", "a", "b"},
		{"namespaces", "--nonsense"},
	} {
		if r := invoke(t, args...); r.code != 1 {
			t.Errorf("%v was accepted: %s", args, r)
		}
	}
}

func TestNamespacesCommandListsAndMarksTheCurrentOne(t *testing.T) {
	withHome(t)
	asSession(t, "aaaa1111-0000-0000-0000-000000000000", "alpha")
	registerIn(t, mustNS(t, "acme"), "bbbb2222-0000-0000-0000-000000000000", "beta")

	r := invoke(t, "namespaces")
	if r.code != 0 {
		t.Fatalf("%s", r)
	}
	for _, want := range []string{"NAMESPACE", "acme", "*  default"} {
		wantContains(t, r, "stdout", want)
	}

	r = invoke(t, "namespaces", "--json")
	var got []map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, r.stdout)
	}
	if len(got) != 2 {
		t.Errorf("got %d namespaces, want acme and default: %v", len(got), got)
	}
}

// Get-or-set, like `pigeon name`. Setting it records a preference for shell
// invocations and must say plainly that it does not move a running session.
func TestNamespaceCommandGetsAndSets(t *testing.T) {
	withHome(t)
	t.Setenv(pigeon.EnvNamespace, "")
	t.Setenv(pigeon.EnvProjectDir, t.TempDir())

	r := invoke(t, "namespace")
	if strings.TrimSpace(r.stdout) != "default" {
		t.Errorf("stdout = %q, want just the name so $(pigeon namespace) is usable", r.stdout)
	}
	wantContains(t, r, "stderr", "from ")

	if r := invoke(t, "namespace", "acme"); r.code != 0 {
		t.Fatalf("%s", r)
	} else {
		wantContains(t, r, "stdout", `namespace set to "acme"`)
		wantContains(t, r, "stdout", "running sessions keep the namespace they armed with")
	}
	if r := invoke(t, "namespace"); strings.TrimSpace(r.stdout) != "acme" {
		t.Errorf("the preference did not persist: %s", r)
	}
	if r := invoke(t, "namespace", "../escape"); r.code != 1 {
		t.Errorf("a traversing namespace was accepted: %s", r)
	}
}

// Setting a preference something already outranks is a silent no-op otherwise,
// and you would go on wondering why ls looks the same.
func TestNamespaceCommandSaysWhenItIsOverridden(t *testing.T) {
	withHome(t)
	t.Setenv(pigeon.EnvNamespace, "fromenv")

	r := invoke(t, "namespace", "acme")
	if r.code != 0 {
		t.Fatalf("%s", r)
	}
	wantContains(t, r, "stderr", "fromenv still applies here")
}

func TestPruneSweepsEveryNamespace(t *testing.T) {
	withHome(t)
	asSession(t, "aaaa1111-0000-0000-0000-000000000000", "alpha")
	// A session whose project config changed namespace leaves a dead entry in
	// the old one, where nothing else will ever look at it.
	stale := mustNS(t, "old-namespace")
	if err := stale.WriteEntry(&pigeon.Entry{SessionID: "bbbb2222-0000-0000-0000-000000000000"}); err != nil {
		t.Fatalf("WriteEntry: %v", err)
	}

	if r := invoke(t, "prune"); r.code != 0 {
		t.Fatalf("%s", r)
	}
	if _, err := stale.ReadEntry("bbbb2222-0000-0000-0000-000000000000"); err != nil {
		t.Fatalf("a prune of one namespace reached into another: %v", err)
	}

	r := invoke(t, "prune", "--all-namespaces")
	if r.code != 0 {
		t.Fatalf("%s", r)
	}
	wantContains(t, r, "stdout", "swept")
	if _, err := stale.ReadEntry("bbbb2222-0000-0000-0000-000000000000"); err == nil {
		t.Error("--all-namespaces left the stale entry behind")
	}
}

// --- formatting helpers ----------------------------------------------------

func TestTruncateAndAbbrevAreRuneSafe(t *testing.T) {
	// Byte slicing here would split a multi-byte rune and emit U+FFFD into a
	// session listing.
	if got := truncate("ååååååå", 4); len([]rune(got)) != 4 {
		t.Errorf("truncate = %q (%d runes), want 4", got, len([]rune(got)))
	}
	if got := abbrev("/home/über/projects/deep", 10); len([]rune(got)) != 10 {
		t.Errorf("abbrev = %q (%d runes), want 10", got, len([]rune(got)))
	}
	if got := truncate("short", 40); got != "short" {
		t.Errorf("truncate shortened a short string: %q", got)
	}
	if got := abbrev("/tmp", 40); got != "/tmp" {
		t.Errorf("abbrev shortened a short path: %q", got)
	}
}

func TestDashMarksEmptyFields(t *testing.T) {
	for _, in := range []string{"", "   ", "\t"} {
		if got := dash(in); got != "-" {
			t.Errorf("dash(%q) = %q, want -", in, got)
		}
	}
	if got := dash("alpha"); got != "alpha" {
		t.Errorf("dash(alpha) = %q", got)
	}
}

func TestHumanBytesScales(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{512, "512 B"},
		{2048, "2.0 KiB"},
		{3 << 20, "3.0 MiB"},
	}
	for _, c := range cases {
		if got := humanBytes(c.in); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}
