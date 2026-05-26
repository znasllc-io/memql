package recorder

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/safety"
)

// stubRunner captures the queries Record produces and optionally
// returns a canned error. Used to assert query shape + the error-
// swallow path.
type stubRunner struct {
	mu      sync.Mutex
	queries []string
	err     error
}

func (s *stubRunner) RunMutation(_ context.Context, query string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queries = append(s.queries, query)
	return s.err
}
func (s *stubRunner) Queries() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.queries))
	copy(out, s.queries)
	return out
}

func TestRecordEmitsExpectedQueryShape(t *testing.T) {
	runner := &stubRunner{}
	p := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.Record(context.Background(),
		safety.NewExecAction(safety.SurfaceWorkbench, "ls -la", safety.CallerContext{
			AgentID: "a1", OwnerUserID: "u1", PlanID: "p1", CorrelationID: "c1",
		}),
		safety.Classification{
			Tier: safety.TierLow, Categories: []safety.Category{safety.CategoryDestructive},
			Reason: "ok", RuleID: "shell.test", Confidence: 0.9,
			Source: safety.SourceRule, LatencyMs: 12,
		},
		safety.DecisionAllow, safety.ModeShadow)

	qs := runner.Queries()
	if len(qs) != 1 {
		t.Fatalf("expected 1 query, got %d", len(qs))
	}
	q := qs[0]
	// Header + tail.
	if !strings.HasPrefix(q, "mutationInsertSafetyClassification({") || !strings.HasSuffix(q, "})") {
		t.Errorf("query wrapper malformed: %q", q)
	}
	// Required fields land in the query.
	for _, fragment := range []string{
		`surface: "workbench"`,
		`action: "exec"`,
		`tier: "low"`,
		`decision: "allow"`,
		`source: "rule"`,
		`mode: "shadow"`,
		`ruleId: "shell.test"`,
		`reason: "ok"`,
		`agentId: "a1"`,
		`ownerUserId: "u1"`,
		`planId: "p1"`,
		`correlationId: "c1"`,
		`categories: "destructive"`,
	} {
		if !strings.Contains(q, fragment) {
			t.Errorf("query missing %q\nFull: %s", fragment, q)
		}
	}
}

func TestRecordDropsSurfaceFieldsOnCredentialAccess(t *testing.T) {
	// safety.RedactedPayload is pass-through for Command / URL /
	// Paths. When the classifier flags credential_access the rule
	// fired on something in those fields (URL userinfo, env-var
	// assignment, etc.), so persisting them verbatim would leak the
	// credential into shadow-mode telemetry. The recorder must drop
	// them before marshalling.
	runner := &stubRunner{}
	p := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	desc := safety.NewExecAction(safety.SurfaceWorkbench,
		"curl https://user:secret-token-xyz@example.com/api",
		safety.CallerContext{})
	p.Record(context.Background(), desc,
		safety.Classification{
			Tier:       safety.TierHigh,
			Categories: []safety.Category{safety.CategoryCredentialAccess},
			Reason:     "URL contains userinfo",
			Source:     safety.SourceRule,
		},
		safety.DecisionDeny, safety.ModeShadow)

	q := runner.Queries()[0]
	if strings.Contains(q, "secret-token-xyz") {
		t.Errorf("credential leaked into persisted query: %q", q)
	}
	if !strings.Contains(q, "[REDACTED:credential_access]") {
		t.Errorf("expected [REDACTED:credential_access] marker, got: %q", q)
	}
	// Reason field must SURVIVE -- it's the triage context.
	if !strings.Contains(q, "URL contains userinfo") {
		t.Errorf("reason field should survive scrub: %q", q)
	}
}

func TestRecordKeepsCommandWhenNotCredentialAccess(t *testing.T) {
	// The defensive scrub fires only on credential_access -- other
	// rule categories (destructive, etc.) should leave Command
	// intact so ops can see what was flagged.
	runner := &stubRunner{}
	p := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	desc := safety.NewExecAction(safety.SurfaceWorkbench, "rm -rf /tmp/foo",
		safety.CallerContext{})
	p.Record(context.Background(), desc,
		safety.Classification{
			Tier:       safety.TierHigh,
			Categories: []safety.Category{safety.CategoryDestructive},
			Source:     safety.SourceRule,
		},
		safety.DecisionAsk, safety.ModeShadow)
	q := runner.Queries()[0]
	if !strings.Contains(q, "rm -rf /tmp/foo") {
		t.Errorf("destructive-but-not-credential command should survive: %q", q)
	}
	if strings.Contains(q, "[REDACTED:credential_access]") {
		t.Errorf("scrub should NOT fire on destructive-only category: %q", q)
	}
}

func TestRecordRedactsSecretsBeforePersist(t *testing.T) {
	runner := &stubRunner{}
	p := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// Tool descriptor whose args carry a credential -- must NOT
	// land in the persisted argsRedacted blob in cleartext.
	desc := safety.NewToolAction(safety.SurfaceToolWebhook, "callX",
		map[string]any{"apiKey": "secret-token-xyz", "ok": 1},
		safety.CallerContext{})
	p.Record(context.Background(), desc,
		safety.Classification{Tier: safety.TierLow, Source: safety.SourceRule},
		safety.DecisionAllow, safety.ModeShadow)

	q := runner.Queries()[0]
	if strings.Contains(q, "secret-token-xyz") {
		t.Errorf("persisted query leaked a secret: %q", q)
	}
	if !strings.Contains(q, "[REDACTED]") {
		t.Errorf("expected [REDACTED] marker in argsRedacted, got: %q", q)
	}
}

func TestRecordSwallowsRunnerError(t *testing.T) {
	// Recorder contract: a backing-store failure must NOT block the
	// dispatch path. The slog sink running alongside in a fanout
	// captures the same data, so we just log + return.
	runner := &stubRunner{err: errors.New("db down")}
	p := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// Must not panic, must not propagate -- the Record signature is
	// void by design.
	p.Record(context.Background(),
		safety.NewExecAction(safety.SurfaceWorkbench, "ls", safety.CallerContext{}),
		safety.Classification{Tier: safety.TierLow},
		safety.DecisionAllow, safety.ModeShadow)
	if len(runner.Queries()) != 1 {
		t.Errorf("runner should have been called once even on error, got %d", len(runner.Queries()))
	}
}

func TestRecordNilRunnerIsNoop(t *testing.T) {
	// Defensive: a misconfigured PersistingRecorder (nil runner)
	// must not crash the dispatch path. Mirrors the SlogRecorder
	// nil-logger guarantee.
	p := New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.Record(context.Background(),
		safety.NewExecAction(safety.SurfaceWorkbench, "ls", safety.CallerContext{}),
		safety.Classification{}, safety.DecisionAllow, safety.ModeShadow)
	// No assertion beyond "doesn't panic" -- the call returns void.
}

func TestRecordDeterministicQueryOrder(t *testing.T) {
	// Sorted keys keep the query string stable -- valuable for
	// log-line diffing + future cache-key-equivalence checks.
	runner := &stubRunner{}
	p := New(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	desc := safety.NewExecAction(safety.SurfaceWorkbench, "ls", safety.CallerContext{})
	cls := safety.Classification{Tier: safety.TierLow, Source: safety.SourceRule}
	p.Record(context.Background(), desc, cls, safety.DecisionAllow, safety.ModeShadow)
	p.Record(context.Background(), desc, cls, safety.DecisionAllow, safety.ModeShadow)
	qs := runner.Queries()
	if qs[0] != qs[1] {
		t.Errorf("expected stable query for identical inputs:\n%s\n%s", qs[0], qs[1])
	}
}
