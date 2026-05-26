package safety

import (
	"context"
	"errors"
	"testing"
)

func TestNoopClassifierReturnsNone(t *testing.T) {
	cls, err := NoopClassifier{}.Classify(context.Background(), ActionDescriptor{})
	if err != nil {
		t.Fatalf("noop error: %v", err)
	}
	if cls.Tier != TierNone || cls.Source != SourceNoop {
		t.Errorf("unexpected verdict: %+v", cls)
	}
}

// fakeClassifier returns a canned verdict (or error). Used by the
// chain tests below.
type fakeClassifier struct {
	cls Classification
	err error
}

func (f fakeClassifier) Classify(_ context.Context, _ ActionDescriptor) (Classification, error) {
	return f.cls, f.err
}

func TestChainShortCircuitsOnFirstVerdict(t *testing.T) {
	first := fakeClassifier{cls: Classification{Tier: TierHigh, Source: SourceRule, Reason: "rule-1"}}
	second := fakeClassifier{cls: Classification{Tier: TierLow, Source: SourceModel, Reason: "model-2"}}
	chain := NewChainClassifier(first, second)
	cls, err := chain.Classify(context.Background(), ActionDescriptor{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if cls.Tier != TierHigh || cls.Source != SourceRule {
		t.Errorf("expected first link to win, got %+v", cls)
	}
}

func TestChainAdvancesOnEscalation(t *testing.T) {
	first := fakeClassifier{err: ErrNoClassifierOpinion}
	second := fakeClassifier{cls: Classification{Tier: TierMedium, Source: SourceModel, Reason: "model"}}
	chain := NewChainClassifier(first, second)
	cls, err := chain.Classify(context.Background(), ActionDescriptor{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if cls.Source != SourceModel || cls.Tier != TierMedium {
		t.Errorf("expected second link to fire, got %+v", cls)
	}
}

func TestChainAllEscalateReturnsNoop(t *testing.T) {
	chain := NewChainClassifier(
		fakeClassifier{err: ErrNoClassifierOpinion},
		fakeClassifier{err: ErrNoClassifierOpinion},
	)
	cls, err := chain.Classify(context.Background(), ActionDescriptor{})
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	if cls.Tier != TierNone || cls.Source != SourceNoop {
		t.Errorf("expected noop fallthrough, got %+v", cls)
	}
}

func TestChainPropagatesRealError(t *testing.T) {
	boom := errors.New("provider down")
	chain := NewChainClassifier(
		fakeClassifier{err: ErrNoClassifierOpinion},
		fakeClassifier{err: boom},
	)
	_, err := chain.Classify(context.Background(), ActionDescriptor{})
	if !errors.Is(err, boom) {
		t.Errorf("expected real error to propagate, got %v", err)
	}
}

func TestEmptyChainBehavesLikeNoop(t *testing.T) {
	cls, err := NewChainClassifier().Classify(context.Background(), ActionDescriptor{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if cls.Tier != TierNone || cls.Source != SourceNoop {
		t.Errorf("expected noop, got %+v", cls)
	}
}
