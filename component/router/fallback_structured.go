package router

// fallback_structured.go -- a structured call walks the route and writes its
// decision record, like every chat surface already did (synthesis2 fix 8).
//
// ResolveStructured used to hand back the WINNER'S RAW CLIENT. Two things
// followed, and both were invisible:
//
//   - A preferred source that failed at call time failed the whole call. The
//     route named a second source and nothing tried it, so the planner's
//     triage moving from a fleet model to an app source it cannot open would
//     have turned a working call into a failing one with no fallback.
//   - No v1:router:call row was written. Every classifier, the planner's
//     triage and the compile pass reached a model through this surface, and
//     the sources each passed over -- an app source this node cannot open,
//     a machine that was asleep -- were never readable anywhere.
//
// The wrapper here is the synchronous chat wrapper's loop, shared rather than
// copied (fallbackWalk), around an observer that records the structured row.

import (
	"context"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

// fallbackStructured walks a route on the structured-output surface.
type fallbackStructured struct {
	router   *Router
	chain    []string
	req      ResolveRequest
	resolved Resolved
	served   servedTracker
}

func (f *fallbackStructured) CallChatStructured(ctx context.Context, messages []common.ChatMessage, schema common.StructuredSchema) (string, error) {
	text, _, err := f.CallChatStructuredWithUsage(ctx, messages, schema)
	return text, err
}

// CallChatStructuredWithUsage keeps the source's own usage report, which is
// the only real token count the journal ever receives
// (common.ChatStructuredUsageProvider). A source that reports none answers a
// zero usage with Reported=false, exactly as calling it directly did.
func (f *fallbackStructured) CallChatStructuredWithUsage(ctx context.Context, messages []common.ChatMessage, schema common.StructuredSchema) (string, common.ChatUsage, error) {
	var text string
	var usage common.ChatUsage
	err := f.router.fallbackWalk(ctx, f.req, f.resolved, f.chain, modalityStructured, &f.served,
		func(ctx context.Context, client any, resolved Resolved) (string, error) {
			observed := &observedStructured{
				inner:    client.(common.ChatStructuredProvider),
				router:   f.router,
				resolved: resolved,
				req:      f.req,
			}
			var err error
			text, usage, err = observed.CallChatStructuredWithUsage(ctx, messages, schema)
			return observed.callId, err
		})
	return text, usage, err
}

// LastServed reports the source the most recent call ended on.
func (f *fallbackStructured) LastServed() (airoute.Served, bool) {
	if f == nil {
		return airoute.Served{}, false
	}
	return f.served.last()
}

// observedStructured records one structured attempt.
type observedStructured struct {
	inner    common.ChatStructuredProvider
	router   *Router
	resolved Resolved
	req      ResolveRequest
	// callId is the full id of the row this attempt wrote, once it returns.
	callId string
}

func (o *observedStructured) CallChatStructured(ctx context.Context, messages []common.ChatMessage, schema common.StructuredSchema) (string, error) {
	text, _, err := o.CallChatStructuredWithUsage(ctx, messages, schema)
	return text, err
}

func (o *observedStructured) CallChatStructuredWithUsage(ctx context.Context, messages []common.ChatMessage, schema common.StructuredSchema) (string, common.ChatUsage, error) {
	start := time.Now()
	ctx = startObservation(ctx, o.req, o.resolved, start)

	var text string
	var usage common.ChatUsage
	var err error
	if withUsage, ok := o.inner.(common.ChatStructuredUsageProvider); ok {
		text, usage, err = withUsage.CallChatStructuredWithUsage(ctx, messages, schema)
	} else {
		text, err = o.inner.CallChatStructured(ctx, messages, schema)
	}

	// THE SOURCE'S OWN COUNT WHEN IT GAVE ONE. The chat observers estimate
	// because their surfaces return a bare string; this one can do better, and
	// a row that says `tokensEstimated: false` has to mean somebody measured.
	inputTokens := EstimateMessageTokens(messages) + EstimateTokensFromChars(len(schema.Schema))
	outputTokens := EstimateTokensFromChars(len(text))
	if usage.Reported {
		inputTokens, outputTokens = int(usage.InputTokens), int(usage.OutputTokens)
	}
	rec := buildRecord(o.req, o.resolved, o.inner, inputTokens, outputTokens, 0, start, time.Time{}, time.Now(), false, err, ctx.Err())
	rec.TokensEstimated = !usage.Reported
	o.callId = o.router.recordAttempt(ctx, rec)
	return text, usage, err
}

// servedTracker keeps the last attempt a wrapper made, for LastServed.
//
// The wrapper re-resolves every attempt by name, so the client that answered
// is built inside the call and is not the one the resolution handed out --
// which is why this is read back rather than known up front, the same shape
// fallbackWithTools.LastSession has.
type servedTracker struct {
	mu     sync.Mutex
	served airoute.Served
	ok     bool
}

func (s *servedTracker) note(resolved Resolved, callId string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.served = airoute.Served{
		RouterCallId: callId,
		ProviderName: resolved.ProviderName,
		Vendor:       resolved.Vendor,
		Model:        resolved.Model,
		Decision:     resolved.Decision,
	}
	s.ok = true
}

func (s *servedTracker) last() (airoute.Served, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.served, s.ok
}

// fallbackWalk is the loop the synchronous wrappers share: try the route's
// sources in order, record a fallback_used row for each one that failed, and
// stop at the first that serves.
//
// try runs ONE attempt through that surface's observer and returns the full id
// of the row the observer wrote, which the tracker keeps with the attempt.
//
// THE CEILING IS ASKED BEFORE A VENDOR HOP (fallbackHopRefusal). The walk asks
// it at resolution time, but only for the entries it reached: a local source
// that WON and then failed at call time sends the wrapper on to the vendor
// behind it, and that hop is exactly the paid fall-back the ceiling governs.
func (r *Router) fallbackWalk(
	ctx context.Context,
	req ResolveRequest,
	selection Resolved,
	chain []string,
	mod providerModality,
	served *servedTracker,
	try func(ctx context.Context, client any, resolved Resolved) (string, error),
) error {
	var lastErr error
	var lastFailed Resolved
	localSeen := selection.localSeen

	for _, name := range chain {
		client, resolved, ok := r.providerLookup(ctx, req, name, mod)
		if !ok {
			continue
		}
		resolved = resolved.withDecisionFrom(selection)

		if lastErr != nil {
			// A cancelled caller is not a reason to try the next source.
			if err := ctx.Err(); err != nil {
				return err
			}
			if refusal := r.fallbackHopRefusal(ctx, req, resolved, localSeen, lastFailed, lastErr); refusal != nil {
				return refusal
			}
			r.recordObserved(ctx, fallbackRecord(req, lastFailed, lastErr))
		}
		if isLocalDoor(resolved.Decision.Door) {
			localSeen = true
		}

		callId, err := try(ctx, client, resolved)
		served.note(resolved, callId)
		if err == nil {
			return nil
		}
		lastErr = err
		lastFailed = resolved
	}

	if lastErr != nil {
		return lastErr
	}
	return errNoChainEntryAvailable
}

// isLocalDoor reports whether a door runs on somebody's own machine: a local
// model, an app answering a turn, or an app holding a whole step.
func isLocalDoor(door string) bool {
	switch door {
	case DoorLocal, DoorApp, DoorSession:
		return true
	}
	return false
}

// fallbackHopRefusal answers the cost ceiling for a wrapper about to hop to a
// VENDOR after a local source: nil when the hop may go ahead, the typed
// ceiling refusal when it may not.
//
// The condition is the walk's (walkChain): the ceiling governs falling BACK to
// paid inference, not choosing it, so a route that starts at a vendor -- an
// operator's decision -- is never refused here.
func (r *Router) fallbackHopRefusal(
	ctx context.Context,
	req ResolveRequest,
	next Resolved,
	localSeen bool,
	failed Resolved,
	failure error,
) error {
	if !localSeen || next.Decision.Door != DoorFederation {
		return nil
	}
	reason, reached := r.ceilingReached(ctx)
	if !reached {
		return nil
	}
	report := &doorReporter{}
	report.note(failed.ProviderName, "failed when it was called: "+TruncateError(errOrString(failure), 200))
	report.note(next.ProviderName, "the cost ceiling for this process has been reached")
	decision := next.Decision
	decision.Considered = append(append([]airoute.ConsideredEntry(nil), next.Decision.Considered...), report.entries()...)
	refusal := report.refusal(work.RefusalCeilingReached, next.PolicyName, reason)
	decision.Outcome = work.RefusalCeilingReached
	refusal.Decision = decision
	return refusal
}
