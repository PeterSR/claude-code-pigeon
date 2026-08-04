package pigeon

import (
	"reflect"
	"testing"

	wb "github.com/PeterSR/claude-code-weaverbird/provider"
)

// valueByID finds the record for id in vals, or ok=false if WeaverbirdValue
// did not return one this render. Most tests below care about one widget
// at a time even though WeaverbirdValue now answers for up to three, so
// this replaces indexing vals[0] directly.
func valueByID(vals []wb.Value, id string) (wb.Value, bool) {
	for _, v := range vals {
		if v.ID == id {
			return v, true
		}
	}
	return wb.Value{}, false
}

// findSpecWidget fetches a widget from a spec by id, failing the test if
// it is not declared -- used by tests that only care about one widget's
// fields among BuildWeaverbirdSpec's three.
func findSpecWidget(t *testing.T, spec wb.Spec, id string) wb.Widget {
	t.Helper()
	for _, w := range spec.Widgets {
		if w.ID == id {
			return w
		}
	}
	t.Fatalf("no widget %q in spec.Widgets = %+v", id, spec.Widgets)
	return wb.Widget{}
}

// TestBuildWeaverbirdSpec_Widget pins pigeon.wait's static contract
// exactly as before: id, title, priority 50, and a cache with both the
// ttl_sec ceiling and, when a session id can be resolved from the
// environment, a file-invalidation pointing at that exact session's
// spool. BuildWeaverbirdSpec now also declares pigeon.monitor and
// pigeon.peers (TestBuildWeaverbirdSpec_OptInDetailWidgets) and the
// pigeon.detail group (TestBuildWeaverbirdSpec_DetailGroup); pigeon.wait
// itself is unchanged and stays first in Widgets.
func TestBuildWeaverbirdSpec_Widget(t *testing.T) {
	withHome(t)
	t.Setenv(EnvSessionID, "aaaa1111")

	spec := BuildWeaverbirdSpec()
	if spec.Provider != "pigeon" || spec.Icon != "🕊" {
		t.Errorf("spec = %+v, want provider pigeon icon 🕊", spec)
	}
	if len(spec.Widgets) != 3 {
		t.Fatalf("len(Widgets) = %d, want 3 (pigeon.wait, pigeon.monitor, pigeon.peers)", len(spec.Widgets))
	}
	if spec.Widgets[0].ID != "pigeon.wait" {
		t.Errorf("Widgets[0].ID = %q, want pigeon.wait first", spec.Widgets[0].ID)
	}

	w := findSpecWidget(t, spec, "pigeon.wait")
	if w.Title != "Messages" || w.Priority != 50 {
		t.Errorf("widget = %+v, want id pigeon.wait title Messages priority 50", w)
	}
	if w.Cache == nil || w.Cache.TTLSec != 30 {
		t.Fatalf("Cache = %+v, want ttl_sec=30", w.Cache)
	}
	wantFile := CurrentNamespace().SpoolPath("aaaa1111")
	if w.Cache.Invalidate == nil || w.Cache.Invalidate.File != wantFile {
		t.Errorf("Cache.Invalidate = %+v, want file %q", w.Cache.Invalidate, wantFile)
	}
	if !w.IsDefault() {
		t.Errorf("pigeon.wait.Default = %v, want default-visible (unchanged)", w.Default)
	}
}

// TestBuildWeaverbirdSpec_OptInDetailWidgets pins the two new widgets'
// static contract: both opt-in (Default: wb.OptIn()), row 1, their own
// priorities, and a 15s ttl_sec cache with no file invalidation of their
// own.
func TestBuildWeaverbirdSpec_OptInDetailWidgets(t *testing.T) {
	withHome(t)
	spec := BuildWeaverbirdSpec()

	for _, tc := range []struct {
		id       string
		priority int
	}{
		{"pigeon.monitor", 20},
		{"pigeon.peers", 10},
	} {
		w := findSpecWidget(t, spec, tc.id)
		if w.Priority != tc.priority {
			t.Errorf("%s.Priority = %d, want %d", tc.id, w.Priority, tc.priority)
		}
		if w.Row != 1 {
			t.Errorf("%s.Row = %d, want 1", tc.id, w.Row)
		}
		if w.IsDefault() {
			t.Errorf("%s.Default = %v, want opt-in (IsDefault() false)", tc.id, w.Default)
		}
		if w.Cache == nil || w.Cache.TTLSec != 15 {
			t.Errorf("%s.Cache = %+v, want ttl_sec=15", tc.id, w.Cache)
		}
	}
}

// TestBuildWeaverbirdSpec_DetailGroup pins the pigeon.detail group: all
// three widgets, pigeon.wait first, in the order weaverbird will render
// them when a layout asks for the group by id.
func TestBuildWeaverbirdSpec_DetailGroup(t *testing.T) {
	withHome(t)
	spec := BuildWeaverbirdSpec()

	if len(spec.Groups) != 1 {
		t.Fatalf("len(Groups) = %d, want 1", len(spec.Groups))
	}
	g := spec.Groups[0]
	if g.ID != "pigeon.detail" {
		t.Errorf("Groups[0].ID = %q, want pigeon.detail", g.ID)
	}
	want := []string{"pigeon.wait", "pigeon.monitor", "pigeon.peers"}
	if !reflect.DeepEqual(g.Widgets, want) {
		t.Errorf("Groups[0].Widgets = %v, want %v (pigeon.wait first)", g.Widgets, want)
	}
}

// TestBuildWeaverbirdSpec_NoSessionOmitsInvalidate covers the case a spec
// invocation cannot resolve any session id at all: still a valid cache
// with the ttl_sec ceiling, just no file to watch.
func TestBuildWeaverbirdSpec_NoSessionOmitsInvalidate(t *testing.T) {
	withHome(t)
	t.Setenv(EnvSessionID, "")

	spec := BuildWeaverbirdSpec()
	w := spec.Widgets[0]
	if w.Cache == nil || w.Cache.TTLSec != 30 {
		t.Fatalf("Cache = %+v, want ttl_sec=30", w.Cache)
	}
	if w.Cache.Invalidate != nil {
		t.Errorf("Invalidate = %+v, want nil (no session id to resolve a spool from)", w.Cache.Invalidate)
	}
}

// TestWeaverbirdValue_SilentWhenLive: the whole design rests on a session
// that can receive getting no pigeon.wait record at all. Unlike
// pigeon.wait, pigeon.monitor is not an alarm -- it reports the healthy
// state too -- so it is expected to answer here; pigeon.peers stays
// silent since this session is the only one registered.
func TestWeaverbirdValue_SilentWhenLive(t *testing.T) {
	withHome(t)
	armed(t, "aaaa1111", "alpha")
	t.Setenv(EnvSessionID, "aaaa1111")

	vals, err := WeaverbirdValue(wb.Session{}, nil)
	if err != nil {
		t.Fatalf("WeaverbirdValue: %v", err)
	}
	if _, ok := valueByID(vals, "pigeon.wait"); ok {
		t.Errorf("vals = %+v, want no pigeon.wait record (live session)", vals)
	}
	if mv, ok := valueByID(vals, "pigeon.monitor"); !ok || mv.FullText != "monitor live" || mv.ShortText != "live" || mv.Class != wb.ClassOK {
		t.Errorf("pigeon.monitor = %+v, ok=%v, want monitor live/ok", mv, ok)
	}
	if _, ok := valueByID(vals, "pigeon.peers"); ok {
		t.Errorf("vals = %+v, want no pigeon.peers record (no other session registered)", vals)
	}
}

// TestWeaverbirdValue_DeafWithWaitingCount: a deaf monitor with mail on
// the spool is the case a count is real for. pigeon.monitor rides along
// with the same deaf status; pigeon.peers stays silent since this
// session is the only one registered.
func TestWeaverbirdValue_DeafWithWaitingCount(t *testing.T) {
	withHome(t)
	beta := liveEntry(t, "bbbb2222", "beta", "/tmp/work")
	t.Setenv(EnvSessionID, "bbbb2222")

	for i := 0; i < 3; i++ {
		if _, err := Send(beta, Draft{Text: "queued message"}, Sender{Kind: "shell", Name: "test"}); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}

	vals, err := WeaverbirdValue(wb.Session{}, nil)
	if err != nil {
		t.Fatalf("WeaverbirdValue: %v", err)
	}
	if len(vals) != 2 {
		t.Fatalf("vals = %+v, want pigeon.wait and pigeon.monitor only", vals)
	}
	v, ok := valueByID(vals, "pigeon.wait")
	if !ok || v.Class != wb.ClassWarn || v.FullText != "3 waiting" || v.ShortText != "3" {
		t.Errorf("pigeon.wait = %+v, ok=%v, want warn/\"3 waiting\"/\"3\"", v, ok)
	}
	mv, ok := valueByID(vals, "pigeon.monitor")
	if !ok || mv.FullText != "monitor deaf" || mv.ShortText != "deaf" || mv.Class != wb.ClassWarn {
		t.Errorf("pigeon.monitor = %+v, ok=%v, want monitor deaf/warn", mv, ok)
	}
}

// TestWeaverbirdValue_DeafWithNoCountWhenSpoolIsEmpty: still worth a
// warn (nothing is reading the spool), but no fabricated "0 waiting".
func TestWeaverbirdValue_DeafWithNoCountWhenSpoolIsEmpty(t *testing.T) {
	withHome(t)
	liveEntry(t, "bbbb2222", "beta", "/tmp/work")
	t.Setenv(EnvSessionID, "bbbb2222")

	vals, err := WeaverbirdValue(wb.Session{}, nil)
	if err != nil {
		t.Fatalf("WeaverbirdValue: %v", err)
	}
	if len(vals) != 2 {
		t.Fatalf("vals = %+v, want pigeon.wait and pigeon.monitor only", vals)
	}
	v, ok := valueByID(vals, "pigeon.wait")
	if !ok || v.Class != wb.ClassWarn || v.FullText != "waiting" || v.ShortText != "waiting" {
		t.Errorf("pigeon.wait = %+v, ok=%v, want warn/\"waiting\"/\"waiting\"", v, ok)
	}
}

// TestWeaverbirdValue_NotArmed: an unregistered session, old enough that
// the arming grace window does not apply, is a real alarm. It must say
// "not armed", and not the "waiting" text the deaf/dead branch uses for mail genuinely piling up on a spool -- no monitor ever
// armed here, so there is nothing counting mail at all.
func TestWeaverbirdValue_NotArmed(t *testing.T) {
	withHome(t)
	t.Setenv(EnvSessionID, "cccc3333")

	vals, err := WeaverbirdValue(wb.Session{}, nil)
	if err != nil {
		t.Fatalf("WeaverbirdValue: %v", err)
	}
	if len(vals) != 1 {
		t.Fatalf("vals = %+v, want exactly one record", vals)
	}
	v := vals[0]
	if v.Class != wb.ClassWarn || v.FullText != "not armed" || v.ShortText != "not armed" {
		t.Errorf("v = %+v, want pigeon.wait/warn/\"not armed\", not the waiting-count wording", v)
	}
	if v.FullText == "waiting" {
		t.Errorf("v.FullText = %q, not-armed must not reuse the deaf/dead waiting text", v.FullText)
	}
}

// TestWeaverbirdValue_SilentWhenOptedOut: opting out of the bus opts out
// of reporting on it too.
func TestWeaverbirdValue_SilentWhenOptedOut(t *testing.T) {
	withHome(t)
	liveEntry(t, "bbbb2222", "beta", "/tmp/work")
	t.Setenv(EnvSessionID, "bbbb2222")
	t.Setenv(EnvOptOut, "0")

	vals, err := WeaverbirdValue(wb.Session{}, nil)
	if err != nil {
		t.Fatalf("WeaverbirdValue: %v", err)
	}
	if len(vals) != 0 {
		t.Errorf("vals = %+v, want none (opted out)", vals)
	}
}

// TestWeaverbirdValue_SilentWithNoSession: a session pigeon cannot
// identify is one it has nothing to say about.
func TestWeaverbirdValue_SilentWithNoSession(t *testing.T) {
	withHome(t)
	t.Setenv(EnvSessionID, "")

	vals, err := WeaverbirdValue(wb.Session{}, nil)
	if err != nil {
		t.Fatalf("WeaverbirdValue: %v", err)
	}
	if len(vals) != 0 {
		t.Errorf("vals = %+v, want none (no session id anywhere)", vals)
	}
}

// TestWeaverbirdValue_SessionBeatsEnvironment: the parsed weaverbird
// Session's SessionID is authoritative over CLAUDE_CODE_SESSION_ID, which
// the per-render process does not reliably inherit.
func TestWeaverbirdValue_SessionBeatsEnvironment(t *testing.T) {
	withHome(t)
	armed(t, "aaaa1111", "alpha")
	liveEntry(t, "bbbb2222", "beta", "/tmp/work")
	t.Setenv(EnvSessionID, "aaaa1111") // live, would be silent

	vals, err := WeaverbirdValue(wb.Session{SessionID: "bbbb2222"}, nil)
	if err != nil {
		t.Fatalf("WeaverbirdValue: %v", err)
	}
	v, ok := valueByID(vals, "pigeon.wait")
	if !ok || v.FullText != "waiting" {
		t.Errorf("vals = %+v, want the stdin session's deaf/no-count record", vals)
	}
}

// A malformed payload must not blank the widget: weaverbird's own
// ParseSession is lenient and hands back a zero Session rather than an
// error, and falling back to the environment is strictly better than
// reporting nothing at all.
func TestWeaverbirdValue_FallsBackToEnvOnEmptySession(t *testing.T) {
	withHome(t)
	liveEntry(t, "bbbb2222", "beta", "/tmp/work")
	t.Setenv(EnvSessionID, "bbbb2222")

	vals, err := WeaverbirdValue(wb.Session{}, nil)
	if err != nil {
		t.Fatalf("WeaverbirdValue: %v", err)
	}
	if v, ok := valueByID(vals, "pigeon.wait"); !ok || v.FullText != "waiting" {
		t.Errorf("vals = %+v, want the env session's deaf record", vals)
	}
}

// A session id from the host reaches a file path, so it is validated rather
// than trusted, exactly as everywhere else.
func TestWeaverbirdValue_RejectsUnsafeSessionID(t *testing.T) {
	withHome(t)
	t.Setenv(EnvSessionID, "")

	vals, err := WeaverbirdValue(wb.Session{SessionID: "../../etc/passwd"}, nil)
	if err != nil {
		t.Fatalf("WeaverbirdValue: %v", err)
	}
	if len(vals) != 0 {
		t.Errorf("vals = %+v, want none for an unsafe session id", vals)
	}
}

// TestWeaverbirdValue_FindsASessionInAnotherNamespace: this process may
// resolve a different namespace than the one the monitor armed in, and must not
// misreport a healthy-elsewhere session as not armed.
func TestWeaverbirdValue_FindsASessionInAnotherNamespace(t *testing.T) {
	withHome(t)
	acme := mustNS(t, "acme")
	liveEntryIn(t, acme, "bbbb2222", "beta", "/tmp/work")
	t.Setenv(EnvSessionID, "bbbb2222")

	if _, err := ReadEntry("bbbb2222"); err == nil {
		t.Fatal("the session is registered in the default namespace; the test proves nothing")
	}

	vals, err := WeaverbirdValue(wb.Session{}, nil)
	if err != nil {
		t.Fatalf("WeaverbirdValue: %v", err)
	}
	v, ok := valueByID(vals, "pigeon.wait")
	if !ok || v.FullText != "waiting" {
		t.Errorf("vals = %+v, want its real deaf state, not \"not armed\"", vals)
	}

	if _, err := acme.Send(&Entry{SessionID: "bbbb2222"}, Draft{Text: "queued"}, Sender{Kind: "shell", Name: "sh"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	vals, err = WeaverbirdValue(wb.Session{}, nil)
	if err != nil {
		t.Fatalf("WeaverbirdValue: %v", err)
	}
	v, ok = valueByID(vals, "pigeon.wait")
	if !ok || v.FullText != "1 waiting" {
		t.Errorf("vals = %+v, want the waiting message counted", vals)
	}
}

// TestWeaverbirdValue_MonitorDead covers pigeon.monitor's third state: a
// registered entry whose process is gone. PID 0 is what
// TestStatusDeadWhenProcessGone uses to force StatusDead without an
// actual dead pid to hunt down.
func TestWeaverbirdValue_MonitorDead(t *testing.T) {
	withHome(t)
	e := &Entry{SessionID: "gone1111", PID: 0, StartedAt: nowRFC3339()}
	if err := WriteEntry(e); err != nil {
		t.Fatalf("WriteEntry: %v", err)
	}
	t.Setenv(EnvSessionID, "gone1111")

	vals, err := WeaverbirdValue(wb.Session{}, nil)
	if err != nil {
		t.Fatalf("WeaverbirdValue: %v", err)
	}
	mv, ok := valueByID(vals, "pigeon.monitor")
	if !ok || mv.FullText != "monitor dead" || mv.ShortText != "dead" || mv.Class != wb.ClassDanger {
		t.Errorf("pigeon.monitor = %+v, ok=%v, want monitor dead/danger", mv, ok)
	}
	// A dead session is drained the same way a deaf one is: pigeon.wait
	// must still answer for it, same as before this widget existed.
	if _, ok := valueByID(vals, "pigeon.wait"); !ok {
		t.Errorf("vals = %+v, want a pigeon.wait record for the dead session too", vals)
	}
}

// TestWeaverbirdValue_MonitorOmittedWhenNotArmed covers item 1's "no
// false alarm" rule: a session with no registry entry at all, old enough
// that arming grace no longer applies, gets pigeon.wait's "not armed"
// alarm but no pigeon.monitor record -- there is no status to report,
// and guessing one would be exactly the false alarm the rule forbids.
func TestWeaverbirdValue_MonitorOmittedWhenNotArmed(t *testing.T) {
	withHome(t)
	t.Setenv(EnvSessionID, "cccc3333")

	vals, err := WeaverbirdValue(wb.Session{}, nil)
	if err != nil {
		t.Fatalf("WeaverbirdValue: %v", err)
	}
	if _, ok := valueByID(vals, "pigeon.monitor"); ok {
		t.Errorf("vals = %+v, want no pigeon.monitor record (never armed)", vals)
	}
}

// TestWeaverbirdValue_PeersCountsOtherLiveSessions covers pigeon.peers:
// three live sessions in the namespace, self included, reads as 2 peers.
func TestWeaverbirdValue_PeersCountsOtherLiveSessions(t *testing.T) {
	withHome(t)
	armed(t, "aaaa1111", "alpha")
	armed(t, "bbbb2222", "beta")
	armed(t, "cccc3333", "gamma")
	t.Setenv(EnvSessionID, "aaaa1111")

	vals, err := WeaverbirdValue(wb.Session{}, nil)
	if err != nil {
		t.Fatalf("WeaverbirdValue: %v", err)
	}
	pv, ok := valueByID(vals, "pigeon.peers")
	if !ok || pv.FullText != "2 peers" || pv.ShortText != "2" || pv.Class != wb.ClassNeutral {
		t.Errorf("pigeon.peers = %+v, ok=%v, want \"2 peers\"/\"2\"/neutral", pv, ok)
	}
}

// TestWeaverbirdValue_PeersOmittedWhenAlone covers the n<=0 case: this
// session is the only one registered, so there is no headcount worth
// showing.
func TestWeaverbirdValue_PeersOmittedWhenAlone(t *testing.T) {
	withHome(t)
	armed(t, "aaaa1111", "alpha")
	t.Setenv(EnvSessionID, "aaaa1111")

	vals, err := WeaverbirdValue(wb.Session{}, nil)
	if err != nil {
		t.Fatalf("WeaverbirdValue: %v", err)
	}
	if _, ok := valueByID(vals, "pigeon.peers"); ok {
		t.Errorf("vals = %+v, want no pigeon.peers record (alone in the namespace)", vals)
	}
}

// TestWeaverbirdValue_SilentWhenOptedOutCoversAllThreeWidgets extends
// TestWeaverbirdValue_SilentWhenOptedOut's guarantee to the two new
// widgets: OptedOut() is a per-session opt-out of the whole bus, not just
// the pigeon.wait alarm, so a peer count or a monitor status must not
// leak out for a session that asked to be left alone either.
func TestWeaverbirdValue_SilentWhenOptedOutCoversAllThreeWidgets(t *testing.T) {
	withHome(t)
	armed(t, "aaaa1111", "alpha")
	armed(t, "bbbb2222", "beta")
	t.Setenv(EnvSessionID, "aaaa1111")
	t.Setenv(EnvOptOut, "0")

	vals, err := WeaverbirdValue(wb.Session{}, nil)
	if err != nil {
		t.Fatalf("WeaverbirdValue: %v", err)
	}
	if len(vals) != 0 {
		t.Errorf("vals = %+v, want none (opted out, all three widgets)", vals)
	}
}
