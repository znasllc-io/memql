package memql

// data_verb.go -- CLASSIFICATION ONLY. It answers "what would executing this
// query string do to the data plane", so a caller that IS an enforcement point
// can name the right RBAC verb. It enforces nothing itself, and nothing on the
// Execute path calls it.
//
// That separation is the memql#3179 ruling, not an accident of layering. The
// coarse data-resource capability check was wired at the gRPC HANDLER layer
// rather than at the executor chokepoint, because handlers see only user
// requests: node bootstrap (component/node/bootstrap.go, Engine.Execute on a
// context.Background() with no AccessContext), the authoring promote/demote/
// rearm paths, the authored scheduler, the planner reactive loop and the MCP
// automation runner all reach the engine in-process and never send an
// ExecuteQueryMsg. Putting the decision at the executor would have needed an
// enumerated system-bypass list for exactly those callers; putting it at the
// handler needs none. The accepted cost is that the check is PARTIAL BY
// CONSTRUCTION -- a DSL-callable surface not reached through a handler is not
// covered.

import (
	"context"

	"github.com/znasllc-io/memql/component/auth"
)

// DataVerbFor reports which RBAC data-plane verb executing `query` under `ctx`
// would require: auth.VerbCreate when the query WRITES, auth.VerbRead
// otherwise.
//
// The parse here is byte-for-byte the parse executeWith performs -- same
// function registry, same nil spec overlay, same allowInline=false, and the
// same call origin and ambient envelope read off the SAME context. That is
// deliberate: a classifier that parsed differently from the executor could
// disagree with it, and a disagreement in the read direction is a write
// admitted by mistake. Pass the context the query will actually execute under.
//
// The write verbs (create / update / delete) are not discriminated. The
// consolidated model grants all three on `data` to exactly the same roles
// (component/auth/rbac_model.go: owner / developer / admin / writer hold the
// set, reader holds none), so splitting them would add a distinction with no
// decision behind it. Any write reports auth.VerbCreate.
//
// An unparseable or empty query reports auth.VerbRead. That is deliberate and
// is not a hole: such a query cannot execute either, and Execute is about to
// return the parse error. Reporting a write here would replace a precise
// syntax error with a misleading permission error.
//
// What it recognises, all four of the plan shapes the executor dispatches as
// writes (see executeWith + resolvePlanFunctionsWithAmbient):
//   - a top-level mutation function call (plan.MutationCall) -- including the
//     F.6 shape where a logic function's `return` is a mutation call;
//   - a multi-step logic call (plan.LogicCall), which exists to sequence
//     effects and is treated as a write;
//   - an insert() / update() literal (plan.Mutations);
//   - a mutation/action call surviving anywhere in the query expression
//     (findImpureCall), the engine's own purity predicate.
//
// What it does NOT recognise: a Go-backed builtin whose integration writes as
// a side effect. Builtins declare no verb anywhere in the model, so there is
// nothing to classify them by. Stated rather than papered over.
func (e *MemQLEngine) DataVerbFor(ctx context.Context, query string) string {
	if e == nil {
		return auth.VerbRead
	}
	plan, err := e.parseWithFunctionsAmbient(
		query,
		e.functions,
		nil,   // no authored spec overlay -- matches Execute
		false, // allowInline -- matches Execute
		auth.OriginFromContext(ctx),
		buildAmbientEnvelope(ctx, e),
	)
	if err != nil || plan == nil {
		return auth.VerbRead
	}
	return planDataVerb(plan, e.functions)
}

// planDataVerb is DataVerbFor over an already-parsed plan.
func planDataVerb(plan *QueryPlan, functions *FunctionRegistry) string {
	if plan == nil {
		return auth.VerbRead
	}
	if plan.MutationCall != nil || plan.LogicCall != nil || len(plan.Mutations) > 0 {
		return auth.VerbCreate
	}
	var snapshot map[string]*Function
	if functions != nil {
		snapshot = functions.Snapshot()
	}
	if findImpureCall(plan.Root, snapshot) != "" {
		return auth.VerbCreate
	}
	return auth.VerbRead
}
