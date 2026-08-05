package memql

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	busv1 "github.com/znasllc-io/memql/component/bus/gen"
	"github.com/znasllc-io/memql/component/config"
)

// EvaluateSpec runs a context-spec (Kind == SpecKindContext) and
// returns its boolean result. Row-specs (Kind == SpecKindRow) are
// rejected here — they belong in a SQL filter expression, not as a
// callable predicate. Callable from another spec body via the
// `spec(...)` builtin (see expression_evaluator.go), and from Go
// callers that want to gate behaviour on an atomic caller-context
// check.
//
// The evaluation stamps a small ctx envelope: actor + partition +
// now + config are placed onto ctx automatically. Specs don't take
// args (no input schema); they are atomic boolean predicates.
//
// Errors:
//   - unknown spec
//   - calling a row-spec via this entry point
//   - the body referencing a field the evaluator can't resolve
func (e *MemQLEngine) EvaluateSpec(ctx context.Context, name string) (bool, error) {
	if e == nil || e.specs == nil {
		return false, fmt.Errorf("spec registry is not initialised")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return false, fmt.Errorf("spec name is required")
	}
	spec, err := e.specs.Get(name)
	if err != nil {
		return false, err
	}
	if spec.Kind != SpecKindContext {
		// Role split (#2034 / C4): a data/state predicate (row-kind) is a
		// `trait` and belongs in a query `filter`, not in the
		// authorization / `requires` slot this entry point serves.
		kindLabel := "row-spec"
		if spec.IsTrait {
			kindLabel = "trait"
		}
		return false, fmt.Errorf("%s %q is a data/state predicate and cannot be used as an authorization gate -- reference it in a query `filter` instead (e.g. `filter ... && %s`). Only an authorization `spec` (one that reads actor.*) is callable via a `requires` slot / EvaluateSpec", kindLabel, name, name)
	}
	if spec.Expr == nil {
		return false, fmt.Errorf("spec %q has no compiled body", name)
	}

	effective := buildSpecCtx(ctx, e)
	val, err := evaluateSpecExpression(ctx, e, spec.Expr, effective)
	if err != nil {
		return false, fmt.Errorf("evaluate spec %q: %w", name, err)
	}
	return specTruthy(val), nil
}

// buildSpecCtx assembles the evaluation envelope a context-spec body
// sees. Specs don't carry args or produce output. Thin alias for
// buildAmbientEnvelope, kept under this name because the spec-side
// tests guard the #2801 fail-closed behaviour through it.
func buildSpecCtx(ctx context.Context, engine *MemQLEngine) map[string]any {
	return buildAmbientEnvelope(ctx, engine)
}

// buildAmbientEnvelope assembles the ambient evaluation envelope: the
// resolved actor, the active partition, the engine timestamp, and the
// allow-listed config surface. Those are the reserved top-level names
// a DSL body may read besides `args` (CLAUDE.md, "Argument
// resolution").
//
// ONE canonical envelope (#2623), shared by both surfaces that need
// it: context-specs (EvaluateSpec) and cond-predicate arg expansion
// (memql#3024). A second builder that could drift from this one is
// precisely the failure #2623 and #2801 were about -- the same gate
// answering differently depending on which surface evaluated it.
func buildAmbientEnvelope(ctx context.Context, engine *MemQLEngine) map[string]any {
	out := make(map[string]any, 6)
	// One canonical envelope (#2623), built UNCONDITIONALLY (#2801).
	//
	// Seeding an empty map when auth is absent introduced a third
	// nil-representation -- absent keys -- alongside the envelope's two.
	// specEqual short-circuits on nil (`a == nil || b == nil` -> `a == b`,
	// expression_evaluator.go), so a NEGATED actor predicate diverged from
	// the envelope's deny answer: `isClusterOwner != false` evaluated TRUE
	// on an absent context, which is the same fail-open the envelope's own
	// default was fixed for. AccessFromContext already returns nil when
	// !ok, so passing it straight through yields the denying envelope with
	// every key present.
	access, _ := auth.AccessFromContext(ctx)
	out["actor"] = auth.ActorEnvelopeMap(access)
	out["partition"] = currentPartitionFromContext(ctx)
	out["now"] = time.Now().UTC().Format(time.RFC3339Nano)
	var snapshot *busv1.ConfigSnapshot
	if engine != nil {
		if raw := engine.ConfigSnapshot(); raw != nil {
			snapshot, _ = raw.(*busv1.ConfigSnapshot)
		}
	}
	out["config"] = config.BuildPolicyConfigCtx(snapshot)
	return out
}
