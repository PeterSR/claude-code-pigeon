package pigeon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	wb "github.com/PeterSR/claude-code-weaverbird/provider"
)

// writeClaudeIndex plants a Claude Code session index at a throwaway config dir
// and points EnvConfigDir at it, so LookupClaudeSession reads the test's file
// and never a real ~/.claude.
func writeClaudeIndex(t *testing.T, pid int, sessionID, name, source string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(EnvConfigDir, dir)
	sdir := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sdir, 0o700); err != nil {
		t.Fatalf("mkdir sessions: %v", err)
	}
	rec := map[string]any{
		"pid":        pid,
		"sessionId":  sessionID,
		"name":       name,
		"nameSource": source,
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sdir, strconv.Itoa(pid)+".json"), b, 0o600); err != nil {
		t.Fatalf("write index: %v", err)
	}
}

// startingSession plants a Claude Code session index for sid that started `ago`
// in the past and points EnvConfigDir at it, without registering a pigeon
// entry: the shape of a session whose monitor has not armed yet.
func startingSession(t *testing.T, sid string, ago time.Duration) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(EnvConfigDir, dir)
	sdir := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sdir, 0o700); err != nil {
		t.Fatalf("mkdir sessions: %v", err)
	}
	rec := map[string]any{
		"pid":       os.Getpid(),
		"sessionId": sid,
		"startedAt": time.Now().Add(-ago).UnixMilli(),
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sdir, strconv.Itoa(os.Getpid())+".json"), b, 0o600); err != nil {
		t.Fatalf("write index: %v", err)
	}
}

// clearedSession plants a Claude Code session index saying this test process
// is now running sid, as Claude Code does after a session is cleared: same
// pid, same process start time, a brand new session id, and the *original*
// startedAt, which is what puts the session well past the arming grace.
func clearedSession(t *testing.T, sid string, startedAgo time.Duration) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(EnvConfigDir, dir)
	sdir := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sdir, 0o700); err != nil {
		t.Fatalf("mkdir sessions: %v", err)
	}
	pid := os.Getpid()
	rec := map[string]any{
		"pid":       pid,
		"procStart": ProcStart(pid),
		"sessionId": sid,
		"startedAt": time.Now().Add(-startedAgo).UnixMilli(),
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sdir, strconv.Itoa(pid)+".json"), b, 0o600); err != nil {
		t.Fatalf("write index: %v", err)
	}
}

// Clearing a session mints a new session id inside the same claude process,
// but the monitor was spawned once when that process started and keeps the id
// it armed with. The widget is handed the new id, finds no entry filed under
// it, and -- since Claude Code keeps the original startedAt across the change
// -- cannot fall back on the arming grace either. Matching the process is what
// keeps it from crying "not armed" at a session that is draining mail fine.
func TestWidgetFindsASessionWhoseIDWasReplaced(t *testing.T) {
	withHome(t)
	const armedWith = "aaaa1111-2222-3333-4444-555555555555"
	const nowKnownAs = "ffff9999-8888-7777-6666-555555555555"

	armed(t, armedWith, "alpha") // registered, listening, under the old id
	t.Setenv(EnvSessionID, nowKnownAs)
	clearedSession(t, nowKnownAs, armGrace+time.Hour)

	vals, err := WeaverbirdValue(wb.Session{}, nil)
	if err != nil {
		t.Fatalf("WeaverbirdValue: %v", err)
	}
	if v, ok := valueByID(vals, "pigeon.wait"); ok {
		t.Errorf("pigeon.wait = %+v, want silence: the monitor is live under %s", v, Short(armedWith))
	}
	if mv, ok := valueByID(vals, "pigeon.monitor"); !ok || mv.FullText != "monitor live" {
		t.Errorf("pigeon.monitor = %+v, ok=%v, want monitor live", mv, ok)
	}
}

// The process match is exact, not a consolation prize: an entry belonging to
// some other process must never stand in for this session's own. Otherwise a
// session with no monitor at all would read as armed because an unrelated one
// happens to be registered.
func TestWidgetStillNotArmedWhenAnotherProcessOwnsTheEntry(t *testing.T) {
	withHome(t)
	const nowKnownAs = "ffff9999-8888-7777-6666-555555555555"

	// A live, listening session belonging to a different claude process.
	other := armed(t, "aaaa1111-2222-3333-4444-555555555555", "alpha")
	other.PID = 999999
	other.ProcStart = "1"
	if err := WriteEntry(other); err != nil {
		t.Fatalf("WriteEntry: %v", err)
	}

	t.Setenv(EnvSessionID, nowKnownAs)
	clearedSession(t, nowKnownAs, armGrace+time.Hour)

	vals, err := WeaverbirdValue(wb.Session{}, nil)
	if err != nil {
		t.Fatalf("WeaverbirdValue: %v", err)
	}
	if v, ok := valueByID(vals, "pigeon.wait"); !ok || v.FullText != "not armed" {
		t.Errorf("pigeon.wait = %+v, ok=%v, want not armed (no entry for this process)", v, ok)
	}
}

func TestClaudeSessionAgeReadsStartedAt(t *testing.T) {
	sid := "aaaa1111-2222-3333-4444-555555555555"
	startingSession(t, sid, 3*time.Second)

	age, ok := claudeSessionAge(sid, time.Now())
	if !ok {
		t.Fatal("age not found for a planted index")
	}
	if age < 2*time.Second || age > 30*time.Second {
		t.Errorf("age = %v, want roughly 3s", age)
	}
}

func TestClaudeSessionAgeUnknownWhenAbsent(t *testing.T) {
	t.Setenv(EnvConfigDir, t.TempDir()) // no sessions dir at all
	if _, ok := claudeSessionAge("aaaa1111-2222-3333-4444-555555555555", time.Now()); ok {
		t.Error("age reported for a session with no index")
	}
}

// A monitor takes a beat to arm, and the host renders (and caches) the status
// line before then. A just-started session with no entry is arming, not
// un-armed, so the alarm stays silent rather than sticking a false "not armed"
// onto an idle bar.
func TestWidgetSilentWhileMonitorArming(t *testing.T) {
	withHome(t)
	sid := "eeee1111-2222-3333-4444-555555555555"
	t.Setenv(EnvSessionID, sid)
	startingSession(t, sid, 1*time.Second) // young, and no pigeon entry

	vals, err := WeaverbirdValue(wb.Session{}, nil)
	if err != nil {
		t.Fatalf("WeaverbirdValue: %v", err)
	}
	if v, ok := valueByID(vals, "pigeon.wait"); ok {
		t.Errorf("a just-started session rendered %+v, want nothing while arming", v)
	}
}

// Past the grace window a missing entry is real: the monitor never armed.
func TestWidgetNotArmedOnceGracePasses(t *testing.T) {
	withHome(t)
	sid := "eeee1111-2222-3333-4444-555555555555"
	t.Setenv(EnvSessionID, sid)
	startingSession(t, sid, armGrace+time.Minute) // old, still no entry

	vals, err := WeaverbirdValue(wb.Session{}, nil)
	if err != nil {
		t.Fatalf("WeaverbirdValue: %v", err)
	}
	if v, ok := valueByID(vals, "pigeon.wait"); !ok || v.FullText != "not armed" {
		t.Errorf("pigeon.wait = %+v, ok=%v, want not armed once the grace window has passed", v, ok)
	}
}

func TestLookupClaudeSessionReadsNameAndSource(t *testing.T) {
	pid := os.Getpid()
	sid := "abcd1234-5678-90ab-cdef-000000000001"
	writeClaudeIndex(t, pid, sid, "personal-71", "derived")

	cs := LookupClaudeSession(pid, sid)
	if cs.Name != "personal-71" || cs.Source != "derived" {
		t.Fatalf("got %+v, want name=personal-71 source=derived", cs)
	}
}

// The index is keyed by pid, which the OS recycles. A file at this pid that
// belongs to a different session must never be used to label this one.
func TestLookupClaudeSessionRejectsPidReuse(t *testing.T) {
	pid := os.Getpid()
	writeClaudeIndex(t, pid, "someone-else-0000-0000-0000-000000000002", "their-name", "user")

	cs := LookupClaudeSession(pid, "abcd1234-5678-90ab-cdef-000000000001")
	if cs != (ClaudeSession{}) {
		t.Fatalf("got %+v, want empty for a mismatched session id", cs)
	}
}

func TestLookupClaudeSessionMissingIsEmpty(t *testing.T) {
	t.Setenv(EnvConfigDir, t.TempDir()) // no sessions/ dir at all
	if cs := LookupClaudeSession(os.Getpid(), "abcd1234-5678-90ab-cdef-000000000001"); cs != (ClaudeSession{}) {
		t.Fatalf("got %+v, want empty when the index is absent", cs)
	}
}

func TestLookupClaudeSessionRejectsBadInput(t *testing.T) {
	writeClaudeIndex(t, os.Getpid(), "abcd1234-5678-90ab-cdef-000000000001", "n", "derived")
	if cs := LookupClaudeSession(0, "abcd1234-5678-90ab-cdef-000000000001"); cs != (ClaudeSession{}) {
		t.Errorf("pid 0 should yield empty, got %+v", cs)
	}
	if cs := LookupClaudeSession(os.Getpid(), "../escape"); cs != (ClaudeSession{}) {
		t.Errorf("an invalid session id should yield empty, got %+v", cs)
	}
}

// The name is a label a user can set to anything in Claude Code, and it lands in
// a table column and a JSON field, so it is flattened and bounded like any other
// free text pigeon surfaces.
func TestLookupClaudeSessionSanitizesAndBounds(t *testing.T) {
	pid := os.Getpid()
	sid := "abcd1234-5678-90ab-cdef-000000000001"
	long := strings.Repeat("x", 200)
	writeClaudeIndex(t, pid, sid, "line1\nline2\t"+long, "derived")

	cs := LookupClaudeSession(pid, sid)
	if strings.ContainsAny(cs.Name, "\n\t") {
		t.Errorf("name still holds control characters: %q", cs.Name)
	}
	if n := len([]rune(cs.Name)); n > maxClaudeName {
		t.Errorf("name is %d runes, want <= %d", n, maxClaudeName)
	}
}

// A pid is exact and unique among live sessions, so it resolves a target the
// same way a session id or declared name does.
func TestResolveTargetByPID(t *testing.T) {
	withHome(t)
	e := liveEntry(t, "aaaa1111-2222-3333-4444-555555555555", "alpha", t.TempDir())

	got, err := ResolveTarget(strconv.Itoa(e.PID))
	if err != nil {
		t.Fatalf("ResolveTarget by pid: %v", err)
	}
	if got.SessionID != e.SessionID {
		t.Fatalf("resolved %s, want %s", got.SessionID, e.SessionID)
	}
}

// A pid is also a valid hex id-prefix, so a single entry can match both the pid
// tier and the prefix tier. The two matches are the same entry, so it must
// resolve cleanly rather than report itself ambiguous.
func TestResolveTargetPidAndPrefixOverlapIsNotAmbiguous(t *testing.T) {
	withHome(t)
	pid := os.Getpid()
	// A session id that begins with the pid's digits, so token=str(pid) matches
	// this entry by pid and by prefix at once.
	sid := strconv.Itoa(pid) + "1111-2222-3333-444444444444"
	e := liveEntry(t, sid, "beta", t.TempDir())

	got, err := ResolveTarget(strconv.Itoa(pid))
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	if got.SessionID != e.SessionID {
		t.Fatalf("resolved %s, want %s", got.SessionID, e.SessionID)
	}
}

// A derived Claude name is the cwd basename plus a suffix, so publishing it
// would leak the very directory a private session withholds.
func TestWriteEntryWithholdsClaudeNameWhenPrivate(t *testing.T) {
	withHome(t)
	sid := "cccc1111-2222-3333-4444-555555555555"
	if err := WriteEntry(&Entry{
		SessionID:        sid,
		Cwd:              "/home/someone/dev/secret-client",
		ClaudeName:       "secret-client-a1",
		ClaudeNameSource: "derived",
		Private:          true,
	}); err != nil {
		t.Fatalf("WriteEntry: %v", err)
	}
	e, err := ReadEntry(sid)
	if err != nil {
		t.Fatalf("ReadEntry: %v", err)
	}
	if e.ClaudeName != "" || e.ClaudeNameSource != "" {
		t.Fatalf("private entry published claude name %q (%q); want withheld", e.ClaudeName, e.ClaudeNameSource)
	}
}

// A project can reuse the host label, e.g. "name": "{{.ClaudeName}}". The field
// is populated only for the current session, whose index pigeon can locate.
func TestTemplateClaudeNameRendersForCurrentSession(t *testing.T) {
	withHome(t)
	pid := os.Getpid()
	sid := "dddd1111-2222-3333-4444-555555555555"
	t.Setenv(EnvSessionID, sid)
	t.Setenv(EnvClaudePID, strconv.Itoa(pid))
	writeClaudeIndex(t, pid, sid, "chosen-title", "user")

	got, err := RenderName("{{.ClaudeName}}", sid, t.TempDir())
	if err != nil {
		t.Fatalf("RenderName: %v", err)
	}
	if got != "chosen-title" {
		t.Fatalf("rendered %q, want chosen-title", got)
	}
}

// .Label is an alias of .ClaudeName, so a template may use either word.
func TestTemplateLabelAliasesClaudeName(t *testing.T) {
	withHome(t)
	pid := os.Getpid()
	sid := "dddd1111-2222-3333-4444-555555555555"
	t.Setenv(EnvSessionID, sid)
	t.Setenv(EnvClaudePID, strconv.Itoa(pid))
	writeClaudeIndex(t, pid, sid, "chosen-title", "user")

	ctx := NewTemplateContext(DefaultNamespace(), sid, t.TempDir())
	if ctx.Label != ctx.ClaudeName || ctx.Label != "chosen-title" {
		t.Fatalf("Label=%q, ClaudeName=%q; want both chosen-title", ctx.Label, ctx.ClaudeName)
	}
	got, err := RenderName("{{.Label}}", sid, t.TempDir())
	if err != nil {
		t.Fatalf("RenderName: %v", err)
	}
	if got != "chosen-title" {
		t.Fatalf("rendered %q, want chosen-title", got)
	}
}

// For any id that is not the current session, the field stays empty rather than
// guess: another session's index is keyed by a pid this process does not know.
func TestTemplateClaudeNameEmptyForOtherSession(t *testing.T) {
	withHome(t)
	t.Setenv(EnvSessionID, "dddd1111-2222-3333-4444-555555555555")
	t.Setenv(EnvClaudePID, strconv.Itoa(os.Getpid()))
	writeClaudeIndex(t, os.Getpid(), "dddd1111-2222-3333-4444-555555555555", "chosen-title", "user")

	// Rendering for a *different* session id must not pick up this one's label.
	ctx := NewTemplateContext(DefaultNamespace(), "eeee9999-0000-0000-0000-000000000000", t.TempDir())
	if ctx.ClaudeName != "" {
		t.Fatalf("ClaudeName=%q for a non-current session, want empty", ctx.ClaudeName)
	}
}
