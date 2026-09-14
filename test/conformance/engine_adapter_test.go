package conformance

// engine_adapter_test.go -- the one place the corpus runner reaches the
// expression engine for its `lower` and `evaluate` verdicts (epic memql#5356,
// task memql#5361).
//
// This file is the seam the expression-language epic (dsl-v1-expressions)
// swaps: it replaces the bodies of lower and evaluate with the one lowering and
// the one in-process evaluator, per position, and every expression case in the
// corpus holds the new engine to the answers the old one gave. Nothing else in
// the runner knows which engine answers.
//
// Today both route through the engine's conformance probe
// (component/memql/conformance_probe.go), which is the executor's own
// pushdown compiler and post-filter. That engine serves the filter grammar at
// two positions, a query filter and a spec body; the other positions have no
// seam onto today's evaluators, which are separate string scanners the record
// retires, so a case at one of them is refused here by name rather than
// answered by a guess.

import (
	"context"
	"errors"
	"fmt"

	"github.com/znasllc-io/memql/component/language/tiers"
	"github.com/znasllc-io/memql/component/memql"
)

// ExprEnv is what an expression is lowered or evaluated against.
//
// (Not `Env`: that name belongs to the MCP conformance harness in this
// package.)
type ExprEnv struct {
	// Row is the row the expression is decided against (evaluate).
	Row map[string]any
	// Args and Actor are the values `args.*` and `actor.*` read.
	Args  map[string]any
	Actor map[string]any
	// Concept is the canonical id of the concept the expression is over.
	Concept string
	// Engine is a booted engine with the case directory's fixture mounted.
	Engine *memql.MemQLEngine
}

// lower returns the SQL an expression at a position pushes down.
func lower(position tiers.Position, src string, env ExprEnv) (string, error) {
	if err := probeSupports(position, env); err != nil {
		return "", err
	}
	return env.Engine.ProbeLowerFilter(context.Background(), env.Concept, src)
}

// evaluate returns what an expression at a position decides against env.Row.
func evaluate(position tiers.Position, src string, env ExprEnv) (any, error) {
	if err := probeSupports(position, env); err != nil {
		return nil, err
	}
	return env.Engine.ProbeEvaluateFilter(context.Background(), env.Concept, src, env.Row)
}

func probeSupports(position tiers.Position, env ExprEnv) error {
	switch position {
	case tiers.PositionQueryFilter, tiers.PositionSpecBody:
	default:
		return fmt.Errorf("position %s has no seam onto today's evaluators; dsl-v1-expressions adds one", position)
	}
	if len(env.Args) > 0 || len(env.Actor) > 0 {
		return errors.New("today's probe binds no args and no actor; dsl-v1-expressions adds binding")
	}
	if env.Engine == nil {
		return errors.New("no engine booted for this case")
	}
	return nil
}
