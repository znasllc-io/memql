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
// sees: the resolved actor, the active partition, the engine
// timestamp, and the allow-listed config surface. Specs don't carry
// args or produce output.
func buildSpecCtx(ctx context.Context, engine *MemQLEngine) map[string]any {
	out := make(map[string]any, 6)
	actor := map[string]any{}
	if access, ok := auth.AccessFromContext(ctx); ok {
		// One canonical envelope (#2623).
		actor = auth.ActorEnvelopeMap(access)
	}
	out["actor"] = actor
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
