package memql

// Row authorization on GRAPH SUBSCRIPTIONS (memql#4309).
//
// THE FINDING. Subscriptions were the one egress of rows that never asked.
// A signed-in stream could subscribe to any concept's change feed and
// receive every user's rows: `handleBusEvent` matched the event topic
// against each subscription's patterns and sent the whole flattened
// payload, `handleSubscribe` had no gate, and the events bus has "no
// AccessContext and no authorization hook of any kind"
// (executor_mutation.go). For the concepts that declare a tier that is a
// real leak -- the read denies the row and the subscription delivers it.
// Neither per-row-authz-audit.md nor the threat model listed subscriptions
// as an egress at all, so the gap was UNRECORDED rather than accepted.
//
// THE SEAM IS THE EXISTING FUNCTION, NOT A NEW ONE (design D2). This file
// adapts rowAuthzAdmits to the fan-out's shape and does not decide
// anything itself. A subscription that is stricter or looser than a read is
// a second authorization implementation, and a second implementation drifts
// from the first -- which is the failure this whole area keeps being filed
// for. So: undeclared admits, declared enforces, exactly as on a read (D1).
// The hole closes concept by concept as tiers are declared, and what a
// subscription can see is never a separate question from what a read can.
//
// WHY IT LIVES IN component/memql RATHER THAN BESIDE THE FAN-OUT.
// rowAuthzAdmits and the tier registry are unexported here, and the
// decision belongs next to the rule it defers to: a gate that can be moved
// away from its rule is one that will be. component/grpc holds the wiring
// and its own test that the wiring is live.

import (
	"context"

	"github.com/znasllc-io/memql/component/auth"
)

// SubscriptionAdmission is what the row gate concluded about one graph
// event on its way to one subscribed stream.
type SubscriptionAdmission int

const (
	// SubscriptionAdmit -- send the event as it stands.
	SubscriptionAdmit SubscriptionAdmission = iota
	// SubscriptionDeny -- drop it. The stream is not told, because being
	// told a row exists that you may not read is itself a disclosure.
	SubscriptionDeny
	// SubscriptionIdOnly -- send {concept, id, action, createdAt} with
	// payload_omitted set, and let the client re-read through the
	// authorized read path.
	//
	// This is the `granted` tier, whose predicate is a relationship spec:
	// deciding it needs the join a FILTER performs, and a row on its own
	// does not carry the answer. Silently dropping such an event instead
	// would make a future granted concept's live feed die without a trace
	// (design D3) -- the failure mode where a feature stops working and
	// nothing anywhere says so.
	SubscriptionIdOnly
)

// String renders an admission for a log line or a test failure.
func (a SubscriptionAdmission) String() string {
	switch a {
	case SubscriptionAdmit:
		return "admit"
	case SubscriptionDeny:
		return "deny"
	case SubscriptionIdOnly:
		return "id-only"
	default:
		return "unknown"
	}
}

// subscriptionReadContext builds the context the row gate reads a
// subscription's admission from.
//
// TWO STAMPS, and both are load-bearing.
//
//  1. The stream's AccessContext, which is where rowAuthzActorUserId and
//     rowAuthzIsClusterOwner read the caller from. A nil access is passed
//     through as an EMPTY ACTOR rather than refused here: the owned tier
//     already answers "no identity, no rows" (memql#3172 finding 4), and
//     letting one rule answer it keeps the fan-out from growing a second
//     opinion about anonymous callers.
//
//  2. An UNBOUND row-authz binding. A subscription is the generic-browse
//     shape -- a client reading a concept's rows with no named construct
//     behind it -- which is exactly what rowAuthzPIIUnboundDenies exists
//     for. Its first clause returns false for a read that is not unbound,
//     so WITHOUT this stamp a subscription to v1:identity:user would hand
//     every user's @pii fields to any signed-in stream: memql#3350's
//     generic-browse hole arriving through a different door, on a concept
//     that declares no tier and so trips no other gate.
//
// The base context is deliberately the caller's own (Background at the
// fan-out) rather than the gRPC stream's: the only identity that may
// decide this is the one the stream RESOLVED, and inheriting an ambient
// actor or an internal-origin stamp from the transport context would let
// something other than the subscriber's identity answer the question. In
// particular a client egress must never read as OriginClient's opposite --
// internal origin skips the PII narrowing entirely.
func subscriptionReadContext(ctx context.Context, access *auth.AccessContext) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = auth.ContextWithAccess(ctx, access)
	return contextWithRowAuthzBinding(ctx, "")
}

// AdmitSubscriptionRow decides whether one `graph.node.*` event may reach a
// stream whose resolved access context is access.
//
// It is called once per event per stream at fan-out, with the row's own
// concept, id and stored payload. `access` may be nil -- a stream that has
// not resolved an identity yet -- and is then treated as an empty actor.
//
// Non-graph topics do not come here: they carry no row to decide about and
// are gated at SUBSCRIBE time instead (memql#4311).
// SubscriptionRankContext returns a context carrying a RESOLVED-ONCE rank
// scope, for a fan-out to reuse across every event on one stream (epic
// memql#4832).
//
// WITHOUT IT THE RANK BRANCH WITHHOLDS ON EVERY SUBSCRIPTION. The branch is a
// disjunct that declines to widen when no scope is installed -- the safe
// direction, and here the WRONG answer: a concept declaring `rankVisible`
// would serve a peer's row on a read and drop the live event for the same
// row, which is the "correct on load, frozen after" shape clients/os/README.md
// warns about by name.
//
// IT IS CACHED BY THE CALLER, PER STREAM, and that is deliberate rather than
// lazy. Resolving the scope reads the principal table, and the fan-out runs
// once per event PER STREAM -- resolving there would put two table scans on
// every graph event. The stream already caches WHO the caller is
// (streamSession.currentAccess); caching what they outrank alongside it is the
// same granularity and the same staleness, which is the honest place to put
// it: a role change reaches both facts together, on the next stream.
func (e *MemQLEngine) SubscriptionRankContext(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return contextWithRankScopeMemo(ctx, e)
}

func AdmitSubscriptionRow(ctx context.Context, access *auth.AccessContext, conceptName, id string, payload []byte) SubscriptionAdmission {
	switch rowAuthzAdmits(subscriptionReadContext(ctx, access), conceptName, id, payload) {
	case rowAuthzAdmit:
		return SubscriptionAdmit
	case rowAuthzUndecided:
		return SubscriptionIdOnly
	default:
		// rowAuthzDeny, and anything a future admission value could add.
		// Defaulting to deny is the direction an authorization switch has
		// to fail in: a new outcome nobody taught this function about must
		// not become "send the row".
		return SubscriptionDeny
	}
}
