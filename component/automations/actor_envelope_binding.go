package automations

import (
	"context"

	"github.com/znasllc-io/memql/component/auth"
)

// bindActorEnvelope binds the canonical actor envelope onto an evaluator
// (memql#2801).
//
// Every evaluator that can reach an `actor.*` read must bind this, and
// must bind it UNCONDITIONALLY. Leaving `actor` unbound was not neutral:
// the string evaluator rendered an unresolved dotted path as its own path
// TEXT, so `actor.isClusterOwner != false` evaluated TRUE -- fail-open, on
// the one field that gates admin work -- and under the v1 absence table an
// ABSENT actor would do the same. auth.ActorEnvelopeMap denies on a nil
// context (owner bits false, identity empty), so binding always is both
// safer and simpler than guarding on presence. RunScope answers an
// unseeded `actor` with that same denying envelope, as the second guard
// (run_scope.go).
//
// One helper rather than four call-site copies: the bug this closes was
// four evaluators each inventing their own representation of "no actor"
// (an empty map, an unbound root, absent keys, the envelope), so the
// answer depended on which one ran.
func bindActorEnvelope(ctx context.Context, evaluator *Evaluator) {
	ac, _ := auth.AccessFromContext(ctx)
	evaluator.SetCustom("actor", auth.ActorEnvelopeMap(ac))
}

// bindNoCallerActorEnvelope binds the denying envelope where there is
// provably no caller -- a background event trigger or scheduler tick is
// not acting on anyone's behalf. Spelled separately so the absence is a
// stated fact at the call site rather than an incidental
// context.Background().
func bindNoCallerActorEnvelope(evaluator *Evaluator) {
	evaluator.SetCustom("actor", auth.ActorEnvelopeMap(nil))
}
