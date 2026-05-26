package safety

import (
	"context"
	"errors"
)

// screen.go is the OUTPUT-SCREENING half of the safety system
// (memql#233). Parallel to the command-classifier half (gate.go +
// classifier.go), but operates on INCOMING content -- tool outputs,
// HTTP fetch bodies, file reads -- BEFORE the model re-ingests them.
//
// The two halves share the safety package because they share
// taxonomy (RiskTier, Category) + recorder primitives + redaction
// helpers. They DO NOT share the Gate / Decision / Classifier
// types: gating an ACTION ("should this run?") is a different
// shape from screening INGRESS ("should this be re-ingested?").

// ContentType discriminates ingress channels for the OutputGate.
// Lets screening rules + dashboards condition on the channel:
// tool_output from a tightly-scoped local tool may be lower risk
// than http_fetch from the open web.
type ContentType string

const (
	ContentTypeToolOutput    ContentType = "tool_output"
	ContentTypeHTTPFetch     ContentType = "http_fetch"
	ContentTypeFileRead      ContentType = "file_read"
	ContentTypeKnowledgeSeed ContentType = "knowledge_seed"
)

// ScreeningInput is the per-call envelope handed to each Screener.
// Mirrors ActionDescriptor's caller-lineage fields for audit but
// drops the surface/action shape -- screening is about CONTENT, not
// agency.
type ScreeningInput struct {
	ContentType ContentType
	// Content is the text the model is about to re-ingest. Caller
	// is responsible for any encoding choice (utf-8 byte string is
	// the convention).
	Content string
	// Caller carries agent/owner/plan/correlation lineage for the
	// audit trail. Same shape as the command Gate's CallerContext.
	Caller CallerContext
}

// ScreeningVerdict is the OutputGate's final outcome.
type ScreeningVerdict string

const (
	// ScreeningVerdictClean: no injection markers found. Surface
	// passes content through unchanged in every mode.
	ScreeningVerdictClean ScreeningVerdict = "clean"

	// ScreeningVerdictSuspicious: pattern matched but with lower
	// confidence (e.g. a single jailbreak term in isolation). In
	// enforce mode, surface logs + passes -- escalation to
	// "blocked" is a per-surface policy decision the OutputGate
	// surfaces in #235's rollout.
	ScreeningVerdictSuspicious ScreeningVerdict = "suspicious"

	// ScreeningVerdictBlocked: overt injection (instruction
	// override, role-hijack tag, etc.). In enforce mode, surface
	// MUST sanitise -- typically by replacing the content with a
	// stub like "[CONTENT BLOCKED BY OUTPUT SCREENER: <reason>]"
	// so the model still sees that something happened but can't
	// follow the embedded instructions.
	ScreeningVerdictBlocked ScreeningVerdict = "blocked"
)

// ScreenSource is the verdict provenance, mirroring Classification.Source
// for the command side.
type ScreenSource string

const (
	ScreenSourceRule     ScreenSource = "rule"
	ScreenSourceModel    ScreenSource = "model"
	ScreenSourceCache    ScreenSource = "cache"
	ScreenSourceNoop     ScreenSource = "noop"
	ScreenSourceDisabled ScreenSource = "disabled"
)

// ScreeningResult is what each Screener returns. Verdict is the
// final outcome; Tier + Categories let downstream consumers
// (recorder, policy) ladder responses without re-running the
// screener.
type ScreeningResult struct {
	Verdict    ScreeningVerdict
	Tier       RiskTier
	Categories []Category
	Reason     string
	Source     ScreenSource
	RuleID     string
	Confidence float64
	LatencyMs  int64
}

// OutputScreener is implemented by every concrete strategy (rule,
// LLM, cache, etc.). The ChainScreener composes them.
type OutputScreener interface {
	Screen(ctx context.Context, in ScreeningInput) (ScreeningResult, error)
}

// ErrNoScreenerOpinion is returned by a screener that wants to
// abstain so the chain falls through to the next layer. Mirrors
// ErrNoClassifierOpinion on the command side.
var ErrNoScreenerOpinion = errors.New("screener has no opinion")

// NoopScreener returns Clean for everything. The default when no
// screener is wired (and the safe fall-through for an empty chain).
type NoopScreener struct{}

func (NoopScreener) Screen(_ context.Context, _ ScreeningInput) (ScreeningResult, error) {
	return ScreeningResult{
		Verdict:    ScreeningVerdictClean,
		Tier:       TierNone,
		Source:     ScreenSourceNoop,
		Reason:     "no screener configured",
		Confidence: 1.0,
	}, nil
}

// ChainScreener tries each screener in order; the first non-abstain
// result wins. Mirrors ChainClassifier on the command side. The
// canonical chain is RuleScreener (cheap, deterministic) -> LLM
// screener (deferred to a follow-up; the rule layer is sufficient
// substrate for v1).
type ChainScreener struct {
	screeners []OutputScreener
}

// NewChainScreener returns a ChainScreener that consults the given
// screeners in order. Nil entries are dropped so callers can pass
// an optional screener without nil-checking.
func NewChainScreener(screeners ...OutputScreener) *ChainScreener {
	nonNil := make([]OutputScreener, 0, len(screeners))
	for _, s := range screeners {
		if s != nil {
			nonNil = append(nonNil, s)
		}
	}
	return &ChainScreener{screeners: nonNil}
}

// Screen walks the chain. ErrNoScreenerOpinion advances to the
// next; any other error short-circuits (the OutputGate decides
// fail-open vs fail-closed by mode). When every screener abstains
// (or the chain is empty), returns a Clean verdict so the surface
// keeps working -- safe default for an unconfigured chain.
func (c *ChainScreener) Screen(ctx context.Context, in ScreeningInput) (ScreeningResult, error) {
	for _, s := range c.screeners {
		res, err := s.Screen(ctx, in)
		if errors.Is(err, ErrNoScreenerOpinion) {
			continue
		}
		return res, err
	}
	return ScreeningResult{
		Verdict:    ScreeningVerdictClean,
		Tier:       TierNone,
		Source:     ScreenSourceNoop,
		Reason:     "no screener opinion",
		Confidence: 1.0,
	}, nil
}
