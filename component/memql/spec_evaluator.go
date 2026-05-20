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
// callable predicate. Callable from policy bodies via the `spec(...)`
// builtin (see policy_evaluator.go), and from Go callers that want
// to gate behaviour on an atomic caller-context check.
//
// The evaluation reuses the policy ctx envelope: actor + partition +
// now + config are stamped onto ctx automatically. Specs don't take
// args (no input schema); if you need parameters, write a policy.
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
		return false, fmt.Errorf("spec %q is a row-spec; call it from a query filter instead (concept==X;%s) — only context-specs are callable via EvaluateSpec", name, name)
	}
	if spec.Expr == nil {
		return false, fmt.Errorf("spec %q has no compiled body", name)
	}

	effective := buildSpecCtx(ctx, e)
	val, err := evaluatePolicyExpressionWithCtx(ctx, e, spec.Expr, effective)
	if err != nil {
		return false, fmt.Errorf("evaluate spec %q: %w", name, err)
	}
	return policyTruthy(val), nil
}

// buildSpecCtx assembles the evaluation envelope a context-spec body
// sees. Same shape as buildPolicyCtx minus the input/output/error
// fields — specs don't carry args or produce output.
func buildSpecCtx(ctx context.Context, engine *MemQLEngine) map[string]any {
	out := make(map[string]any, 6)
	actor := map[string]any{}
	if access, ok := auth.AccessFromContext(ctx); ok {
		actor["userId"] = access.UserId
		actor["role"] = string(access.Role)
		actor["identityId"] = access.IdentityId
		actor["isClusterOwner"] = access.IsClusterOwner()
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
