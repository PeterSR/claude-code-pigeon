package pigeon

import "testing"

// The budget numbers have exactly one home -- BodyBudget, RenderBudget and
// maxPerMinute -- and Budget() only ever reports them. If this test ever has
// to change because Budget() drifted from the constants, that drift is
// exactly the bug this seam exists to prevent.
func TestCurrentRuntimeBudgetMatchesExportedConstants(t *testing.T) {
	render, body, perMinute := CurrentRuntime().Budget()
	if render != RenderBudget {
		t.Errorf("Budget() renderRunes = %d, want RenderBudget (%d)", render, RenderBudget)
	}
	if body != BodyBudget {
		t.Errorf("Budget() bodyRunes = %d, want BodyBudget (%d)", body, BodyBudget)
	}
	if perMinute != maxPerMinute {
		t.Errorf("Budget() perMinute = %d, want maxPerMinute (%d)", perMinute, maxPerMinute)
	}
}

// SessionID must fail loudly rather than guess (see Runtime's doc comment):
// with no CLAUDE_CODE_SESSION_ID in the environment, there is no session to
// guess at, so this must return an error, never a fabricated id.
func TestCurrentRuntimeSessionIDFailsWithoutGuessing(t *testing.T) {
	t.Setenv(EnvSessionID, "")
	id, err := CurrentRuntime().SessionID()
	if err == nil {
		t.Fatalf("SessionID() = %q, nil error; want an error with no %s set", id, EnvSessionID)
	}
	if id != "" {
		t.Errorf("SessionID() returned %q alongside an error; want an empty id on failure", id)
	}
}
