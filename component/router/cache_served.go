package router

// cache_served.go -- the decision record for a call a CACHE answered
// (memql#5581).
//
// The engine's exact-hash cache is consulted after the router resolves,
// because its key folds in the resolved provider name. So a hit is a call that
// walked a chain, matched a rule, took a door and then did not reach a
// provider. Before this the resolution was discarded and no row was written,
// which made a warm page and an idle cluster the same picture in the ledger.
//
// WHAT THE ROW SAYS, AND WHAT IT MUST NOT:
//
//   - `cacheKind` names the cache. It is the one field a reader needs to tell
//     a cache hit from a provider call, and it is empty on every row an
//     observer writes.
//   - Every token count and every cost is a REAL ZERO. Nothing was sent and
//     nothing was billed, so the spend figures do not imply a call that did
//     not happen -- a reader summing totalCost over a warm page gets nought,
//     which is the true answer.
//   - `tokensEstimated` is FALSE, and that is the honest value rather than a
//     copy of the observer's `true`: nothing was estimated because there was
//     nothing to estimate. `pricingConfigured` is false for the same reason --
//     it is a claim about a cost that was computed, and none was.
//   - The DECISION is carried whole, `considered` included. It is the part
//     that is evidence about the rules, and keeping it is what stops the
//     calls a cache serves -- which are by definition the ones that repeat --
//     from being the ones the rule corpus can never be checked against.
//
// It reuses recordCall, so a cache row goes through the same bounded queue and
// the same single writer as everything else.

import (
	"context"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/airoute"
)

// RecordCacheServed satisfies memql.AICacheRecorder.
//
// It never blocks and never fails: the answer has already been served, and an
// observability record must not be able to turn a served answer into an error.
func (r *Router) RecordCacheServed(
	ctx context.Context,
	req ResolveRequest,
	resolution airoute.Resolution,
	cacheKind string,
	durationMs int,
) {
	if r == nil || !airoute.ValidCacheKind(cacheKind) {
		return
	}
	// The same attribution belt ResolveFor runs, and for the same reason: this
	// entry point is reached directly from the AI runtime with whatever
	// request the call site built, and a cache row that named nobody would be
	// the defect this epic is about, reintroduced on the one path that skips
	// ResolveFor.
	if strings.TrimSpace(req.UserId) == "" {
		if access, ok := auth.AccessFromContext(ctx); ok && access != nil {
			req.UserId = strings.TrimSpace(access.UserId)
		}
	}
	if strings.TrimSpace(req.CallerKind) == "" {
		req.CallerKind = auth.CallerKindFromContext(ctx)
	}
	r.recordObserved(ctx, cacheServedRecord(r.stampRequestId(req), resolution, cacheKind, durationMs))
}

// cacheServedRecord builds the CallRecord for a cache-served call.
//
// PURE, and separate from the method, so the "no spend on a cache row"
// property is a statement about a function over values rather than something a
// test has to drive a router to observe.
func cacheServedRecord(
	req ResolveRequest,
	resolution airoute.Resolution,
	cacheKind string,
	durationMs int,
) CallRecord {
	if durationMs < 0 {
		durationMs = 0
	}
	return CallRecord{
		RequestId:  req.RequestId,
		Partition:  req.Partition,
		AgentId:    req.AgentId,
		UserId:     req.UserId,
		CallerKind: req.CallerKind,
		PromptName: req.PromptName,

		// The provider the chain picked, which is also the provider whose
		// answer is being replayed: the cache key folds its name in, so a
		// cached answer can only ever be served for the entry that produced
		// it. Naming it is what lets a reader see WHICH model a warm page is
		// no longer paying for.
		Vendor:       resolution.Vendor,
		Model:        resolution.Model,
		ProviderName: resolution.ProviderName,

		CacheKind: cacheKind,

		// NO SPEND, and every zero here is a measured zero rather than an
		// absent measurement. TokensEstimated stays false: the observer sets
		// it because a char-count heuristic produced its numbers, and nothing
		// produced these.
		StartedAt:       time.Now().Add(-time.Duration(durationMs) * time.Millisecond),
		TotalDurationMs: durationMs,
		Outcome:         "ok",

		// THE DECISION, whole. `considered` on a cache row is the same
		// evidence it is on a provider row: what the chain passed over on the
		// way to the entry whose answer is being replayed.
		PolicyName:         resolution.Decision.Policy,
		Level:              string(resolution.Decision.Level),
		RequestedLevel:     string(resolution.Decision.RequestedLevel),
		ServedLevel:        string(resolution.Decision.ServedLevel),
		Degraded:           resolution.Decision.Degraded,
		Rule:               resolution.Decision.Rule,
		Policy:             resolution.Decision.Policy,
		Door:               resolution.Decision.Door,
		Considered:         resolution.Decision.Considered,
		Touches:            resolution.Decision.Touches,
		MinContextTokens:   resolution.Decision.MinContextTokens,
		MachineOwnerUserId: resolution.Decision.MachineOwnerUserId,
	}
}
