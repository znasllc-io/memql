package safety

import (
	"context"
	"errors"
	"time"
)

// Classifier judges a proposed action. Implementations are expected
// to be cheap (rule-based ones run in microseconds); the LLM-backed
// classifier (#230) wraps a structured-output provider and is only
// reached when a rule layer escalates.
//
// Returning a non-nil error means the classifier itself failed (e.g.
// the LLM timed out). The Gate's fail-safe handling -- which surfaces
// fail-closed vs fail-open on classifier error -- is the decision
// policy's responsibility (#231), not the classifier's.
type Classifier interface {
	Classify(ctx context.Context, desc ActionDescriptor) (Classification, error)
}

// NoopClassifier returns a "no opinion" verdict for every action. It
// is the default when no other classifier is configured -- the Gate
// composes it with shadow mode and the always-allow decision stub so
// Phase 0 is a true no-op on live behavior.
type NoopClassifier struct{}

// Classify always returns TierNone with Source=SourceNoop. Latency
// is recorded so observability metrics work uniformly across
// classifier implementations.
func (NoopClassifier) Classify(_ context.Context, _ ActionDescriptor) (Classification, error) {
	start := time.Now()
	return Classification{
		Tier:       TierNone,
		Confidence: 1.0,
		Source:     SourceNoop,
		Reason:     "no classifier configured",
		LatencyMs:  since(start),
	}, nil
}

// ErrNoClassifierOpinion is the sentinel a chained classifier returns
// when it has no opinion and the next link in the chain should be
// consulted. It is NOT a failure -- the chain consumes it silently.
// A rule engine that can't decide returns this; an LLM classifier
// that returns it would be a bug (the LLM is the last link).
var ErrNoClassifierOpinion = errors.New("safety: classifier has no opinion; escalate")

// ChainClassifier consults each classifier in order, returning the
// first non-escalation verdict. It is the shape used for the real
// pipeline: a fast rule classifier (#228) escalates ambiguous cases
// to the LLM semantic classifier (#230) via ErrNoClassifierOpinion;
// the chain returns whichever decides first. If every link
// escalates, the chain returns a TierNone verdict with Source=Noop
// rather than an error -- "no one had an opinion" is a valid
// outcome the decision policy handles.
type ChainClassifier struct {
	links []Classifier
}

// NewChainClassifier returns a ChainClassifier over the given links
// in priority order. A zero-link chain behaves like NoopClassifier.
func NewChainClassifier(links ...Classifier) *ChainClassifier {
	return &ChainClassifier{links: links}
}

// Classify walks the chain. A real verdict (any non-escalation
// return) short-circuits; an ErrNoClassifierOpinion advances to the
// next link; any other error short-circuits with the error (the
// Gate's fail-safe path takes over). LatencyMs on the returned
// classification reflects total chain time.
func (c *ChainClassifier) Classify(ctx context.Context, desc ActionDescriptor) (Classification, error) {
	start := time.Now()
	for _, link := range c.links {
		cls, err := link.Classify(ctx, desc)
		if errors.Is(err, ErrNoClassifierOpinion) {
			continue
		}
		if err != nil {
			return Classification{LatencyMs: since(start)}, err
		}
		cls.LatencyMs = since(start)
		return cls, nil
	}
	return Classification{
		Tier:       TierNone,
		Confidence: 1.0,
		Source:     SourceNoop,
		Reason:     "no classifier in the chain had an opinion",
		LatencyMs:  since(start),
	}, nil
}
