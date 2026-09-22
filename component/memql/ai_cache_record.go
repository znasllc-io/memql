package memql

// ai_cache_record.go -- the seam a CACHE HIT reaches the decision ledger
// through (memql#5581).
//
// # What was measured
//
// The exact-hash cache is consulted AFTER the router has resolved, because
// its key folds in the RESOLVED provider name -- that is the property that
// invalidates a cached answer when routing changes, and moving the lookup
// earlier would serve one model's answer for a call that now routes to
// another. So on a hit the chain walk has already happened: a rule matched, a
// policy was expanded, a door was taken, and a door report exists.
//
// What did not happen is a provider call. And because the v1:router:call rows
// are written by the OBSERVERS that wrap a provider, a hit produced no row at
// all -- so a warm page and an idle cluster were the same picture in the
// ledger, and the resolution the hit paid for was thrown away.
//
// # The decision
//
// A HIT IS A DECISION AND IT IS RECORDED. The row carries the full decision
// the resolution made -- rule, policy, door, considered -- and `cacheKind`
// naming which cache answered, with every token and cost figure a real zero.
// That is what lets a reader tell a cache hit from a provider call in the
// ledger rather than by its absence, and it is why "N decisions, no spend"
// now reads as the cache working: the rows say so.
//
// The alternative -- record nothing, on the ground that no provider was
// called -- keeps the ledger smaller and leaves the reader with silence to
// interpret. It was rejected for that reason: the decision the resolution made
// is evidence about the rules, and `considered` is kept on success precisely
// so that the decisions a rule MADE can be read back. Dropping it for the
// calls a cache served would hide exactly the ones that repeat.
//
// # Why an optional interface
//
// component/router imports this package and not the other way round, so the
// recorder is declared here and satisfied there, exactly as AIResolver is. It
// is a SEPARATE interface rather than a method on AIResolver so that every
// existing implementation -- and every test fake -- keeps compiling and simply
// records nothing.

import (
	"context"
	"time"

	"github.com/znasllc-io/memql/core/airoute"
)

// AICacheRecorder writes the one v1:router:call row a cache-served call
// produces. component/router implements it.
type AICacheRecorder interface {
	// RecordCacheServed records a call that a cache answered. The request and
	// the resolution are the ones the router just made; cacheKind is one of
	// airoute.CacheKinds. It never blocks and never returns an error: the
	// ledger is observability, and a call already answered must not fail
	// because its record could not be queued.
	RecordCacheServed(ctx context.Context, req airoute.ResolveRequest, resolution airoute.Resolution, cacheKind string, durationMs int)
}

// recordCacheServed writes the decision record for a cache hit, when the
// installed resolver can.
//
// A NIL OR NON-RECORDING RESOLVER IS A WORKING STATE, unlike an unwired
// resolver on the call path: a node that cannot record a cache hit still
// serves it correctly, and refusing here would turn an observability gap into
// a failed answer.
func (e *MemQLEngine) recordCacheServed(ctx context.Context, req airoute.ResolveRequest, resolution airoute.Resolution, cacheKind string, startedAt time.Time) {
	if e == nil || !airoute.ValidCacheKind(cacheKind) {
		return
	}
	recorder, ok := e.aiResolver.get().(AICacheRecorder)
	if !ok || recorder == nil {
		return
	}
	recorder.RecordCacheServed(ctx, req, resolution, cacheKind, int(time.Since(startedAt).Milliseconds()))
}
