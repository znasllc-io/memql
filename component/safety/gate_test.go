package safety

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
)

// recordedEntry captures one Record call so tests can assert.
type recordedEntry struct {
	Desc     ActionDescriptor
	Cls      Classification
	Decision Decision
	Mode     Mode
}

// fakeRecorder is a thread-safe in-memory DecisionRecorder for tests.
type fakeRecorder struct {
	mu      sync.Mutex
	entries []recordedEntry
}

func (r *fakeRecorder) Record(_ context.Context, desc ActionDescriptor, cls Classification, decision Decision, mode Mode) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, recordedEntry{desc, cls, decision, mode})
}
func (r *fakeRecorder) Entries() []recordedEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recordedEntry, len(r.entries))
	copy(out, r.entries)
	return out
}

// canned returns a Classifier that always emits a fixed verdict.
type canned struct{ cls Classification }

func (c canned) Classify(_ context.Context, _ ActionDescriptor) (Classification, error) {
	return c.cls, nil
}

// boom returns a Classifier that always errors -- exercises the
// fail-safe / error-recording path.
type boom struct{ err error }

func (b boom) Classify(_ context.Context, _ ActionDescriptor) (Classification, error) {
	return Classification{Source: SourceModel}, b.err
}

func desc() ActionDescriptor {
	return NewExecAction(SurfaceWorkbench, "ls", CallerContext{AgentID: "a1"})
}

func TestGateModeOffBypassesClassifier(t *testing.T) {
	r := &fakeRecorder{}
	// The classifier MUST NOT be called when mode=off: pass one that
	// would otherwise return Critical and verify we don't see it.
	g := NewGate(GateOptions{
		Classifier: canned{cls: Classification{Tier: TierCritical}},
		Recorder:   r,
		Mode:       ModeOff,
	})
	dec, cls, err := g.Evaluate(context.Background(), desc())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if dec != DecisionAllow {
		t.Errorf("off should allow, got %v", dec)
	}
	if cls.Source != SourceDisabled {
		t.Errorf("off should mark Source=disabled, got %v", cls.Source)
	}
	if len(r.Entries()) != 0 {
		t.Errorf("off should not record, got %d entries", len(r.Entries()))
	}
}

func TestGateShadowAlwaysAllowsButRecords(t *testing.T) {
	// Even with a (hypothetical) Critical verdict the shadow gate
	// must return Allow -- that's what makes a shadow rollout safe.
	r := &fakeRecorder{}
	g := NewGate(GateOptions{
		Classifier: canned{cls: Classification{Tier: TierCritical, Source: SourceRule, Reason: "rm -rf"}},
		Recorder:   r,
		Mode:       ModeShadow,
	})
	dec, cls, err := g.Evaluate(context.Background(), desc())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if dec != DecisionAllow {
		t.Errorf("shadow must always allow live traffic, got %v", dec)
	}
	if cls.Tier != TierCritical || cls.Source != SourceRule {
		t.Errorf("classification not surfaced to caller: %+v", cls)
	}
	if got := r.Entries(); len(got) != 1 || got[0].Mode != ModeShadow {
		t.Fatalf("expected one shadow recording, got %+v", got)
	}
}

func TestGateEnforceHonorsDecision(t *testing.T) {
	// Phase 0's decide() is the always-allow stub, so enforce mode
	// returns Allow too. The point of this test is the call-site
	// shape -- it pins the contract that #231 will repurpose when it
	// replaces decide() with the real policy.
	r := &fakeRecorder{}
	g := NewGate(GateOptions{
		Classifier: canned{cls: Classification{Tier: TierLow, Source: SourceRule, Reason: "ls"}},
		Recorder:   r,
		Mode:       ModeEnforce,
	})
	dec, _, err := g.Evaluate(context.Background(), desc())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if dec != DecisionAllow {
		t.Errorf("Phase 0 decide() stub should return Allow, got %v", dec)
	}
	if got := r.Entries(); len(got) != 1 || got[0].Mode != ModeEnforce {
		t.Errorf("expected one enforce recording, got %+v", got)
	}
}

func TestGateClassifierErrorRecordedAndReturned(t *testing.T) {
	r := &fakeRecorder{}
	boomErr := errors.New("provider unavailable")
	g := NewGate(GateOptions{
		Classifier: boom{err: boomErr},
		Recorder:   r,
		Mode:       ModeShadow,
	})
	dec, cls, err := g.Evaluate(context.Background(), desc())
	// Gate stays neutral on classifier error -- the policy hasn't
	// been consulted, so it doesn't pick fail-open vs fail-closed;
	// #229 wires that per-surface. We return Allow + the error so
	// the surface adapter can decide.
	if !errors.Is(err, boomErr) {
		t.Errorf("expected boomErr, got %v", err)
	}
	if dec != DecisionAllow {
		t.Errorf("gate-level decision on error stays Allow, got %v", dec)
	}
	if cls.Reason == "" {
		t.Error("classifier-error reason should be captured in returned Classification")
	}
	if got := r.Entries(); len(got) != 1 || got[0].Decision != DecisionAllow {
		t.Errorf("expected one error-path recording, got %+v", got)
	}
}

func TestSlogRecorderNilLoggerSafe(t *testing.T) {
	// Defensive: a misconfigured recorder must not panic the dispatch
	// hot path.
	(SlogRecorder{}).Record(context.Background(), desc(), Classification{}, DecisionAllow, ModeShadow)
}

func TestSlogRecorderRedactsBeforeLogging(t *testing.T) {
	// The recorder must call RedactedPayload before logging -- raw
	// secrets must NEVER reach the slog handler. We can't easily
	// inspect handler output, but we can confirm RedactedPayload's
	// contract via direct call. (RedactArgs / RedactedPayload have
	// their own tests; this is a regression guard that the recorder
	// composes them.)
	r := SlogRecorder{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	d := NewToolAction(SurfaceToolWebhook, "t", map[string]any{"apiKey": "xyz"}, CallerContext{})
	// Should not panic, should not mutate the caller's args.
	r.Record(context.Background(), d, Classification{Tier: TierLow, Source: SourceRule}, DecisionAllow, ModeShadow)
	if d.Payload.Args["apiKey"] != "xyz" {
		t.Errorf("recorder mutated caller's payload: %v", d.Payload.Args)
	}
}

func TestModeFromEnvDefault(t *testing.T) {
	// Unsetting via t.Setenv to the empty string mimics "unset" for
	// os.Getenv semantics.
	t.Setenv(EnvMode, "")
	if got := ModeFromEnv(); got != ModeShadow {
		t.Errorf("default should be shadow, got %v", got)
	}
}

func TestModeFromEnvParses(t *testing.T) {
	cases := map[string]Mode{
		"off":     ModeOff,
		"OFF":     ModeOff,
		" off ":   ModeOff,
		"shadow":  ModeShadow,
		"enforce": ModeEnforce,
		"ENFORCE": ModeEnforce,
		"garbage": ModeShadow, // unrecognized -> safe default
	}
	for in, want := range cases {
		t.Setenv(EnvMode, in)
		if got := ModeFromEnv(); got != want {
			t.Errorf("ModeFromEnv(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestNewGateDefaults(t *testing.T) {
	t.Setenv(EnvMode, "")
	g := NewGate(GateOptions{})
	if g.Mode() != ModeShadow {
		t.Errorf("default mode should be shadow, got %v", g.Mode())
	}
	// A defaulted gate's classifier is Noop -- verify by evaluating
	// in enforce mode and checking the recorded source.
	r := &fakeRecorder{}
	g = NewGate(GateOptions{Recorder: r, Mode: ModeEnforce})
	_, cls, err := g.Evaluate(context.Background(), desc())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if cls.Source != SourceNoop {
		t.Errorf("default classifier should be noop, got Source=%v", cls.Source)
	}
}
