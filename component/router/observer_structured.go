package router

// observer_structured.go -- the ledger row for a STRUCTURED call.
//
// The structured surface serves the calls a person never sees answered: goal
// triage, the symptom classifier, the answer validator, compose, suggestions.
// It had no observer, so none of them reached v1:router:call -- the rows
// Fleet History reads -- and on a cluster whose route puts an app door first
// that hid exactly the two things a person needed to see there: an app door a
// structured call PASSED (the reason is in the decision's `considered`), and
// an app session a structured call OPENED AND LOST (the call's error). Live on
// 2026-09-28 the cluster made 18 model calls in an hour and wrote 6 decision
// rows, every one of them an agent reply.
//
// This is the chat observer's shape (observedChat) for the structured
// interface, and it records exactly what that one records: one row per call,
// ok or error, with the decision that chose the provider. It is not a fallback
// wrapper -- the structured surface still resolves one provider and a failure
// is returned to the caller.

import (
	"context"
	"time"

	"github.com/znasllc-io/memql/core/common"
)

// observedStructured wraps a ChatStructuredProvider and writes one ledger row
// per call.
//
// IT ANSWERS THE USAGE INTERFACE TOO, and must: the engine's structured path
// asks the provider for its measured usage (component/memql
// callStructuredWithUsage), and the work spine's modelCall row and the run's
// spend are read from it. A wrapper that answered only CallChatStructured
// would turn every measured call into an unmeasured one without a single
// failing assertion anywhere else.
type observedStructured struct {
	inner    common.ChatStructuredProvider
	router   *Router
	resolved Resolved
	req      ResolveRequest
}

var (
	_ common.ChatStructuredProvider      = (*observedStructured)(nil)
	_ common.ChatStructuredUsageProvider = (*observedStructured)(nil)
)

// CallChatStructured implements common.ChatStructuredProvider.
func (o *observedStructured) CallChatStructured(ctx context.Context, messages []common.ChatMessage, schema common.StructuredSchema) (string, error) {
	text, _, err := o.CallChatStructuredWithUsage(ctx, messages, schema)
	return text, err
}

// CallChatStructuredWithUsage implements common.ChatStructuredUsageProvider.
// A provider that reports no usage answers Reported=false, which is the same
// answer the absence of the interface gives.
func (o *observedStructured) CallChatStructuredWithUsage(ctx context.Context, messages []common.ChatMessage, schema common.StructuredSchema) (string, common.ChatUsage, error) {
	start := time.Now()
	ctx = startObservation(ctx, o.req, o.resolved, start)

	var (
		text  string
		usage common.ChatUsage
		err   error
	)
	if withUsage, ok := o.inner.(common.ChatStructuredUsageProvider); ok {
		text, usage, err = withUsage.CallChatStructuredWithUsage(ctx, messages, schema)
	} else {
		text, err = o.inner.CallChatStructured(ctx, messages, schema)
	}

	// MEASURED WHEN THE PROVIDER SAID, ESTIMATED OTHERWISE -- and the row
	// says which. Pricing is computed from whichever counts the row carries.
	inputTokens, outputTokens := EstimateMessageTokens(messages), EstimateTokensFromChars(len(text))
	if usage.Reported {
		inputTokens, outputTokens = int(usage.InputTokens), int(usage.OutputTokens)
	}
	rec := buildRecord(o.req, o.resolved, o.inner, inputTokens, outputTokens, 0, start, time.Time{}, time.Now(), false, err, ctx.Err())
	rec.TokensEstimated = !usage.Reported
	o.router.recordObserved(ctx, rec)
	return text, usage, err
}

// resolveStructured resolves a structured call and wraps the winner in its
// observer. ResolveFor and ResolveStructured both come through here, so a
// structured call writes its row whichever entry point it used.
func (r *Router) resolveStructured(ctx context.Context, req ResolveRequest) (any, Resolved, error) {
	client, resolved, err := r.resolveDirect(ctx, req, modalityStructured)
	if err != nil {
		return nil, Resolved{}, err
	}
	inner, ok := client.(common.ChatStructuredProvider)
	if !ok {
		// resolveDirect interface-checked the modality, so this is the two
		// disagreeing; hand back what resolved rather than a nil client, and
		// let the caller's own assertion say so.
		return client, resolved, nil
	}
	return &observedStructured{inner: inner, router: r, resolved: resolved, req: r.stampRequestId(req)}, resolved, nil
}
