package memql

// conformance_probe.go -- the conformance corpus's seam onto today's
// expression engine (epic memql#5356, task memql#5361; D23 of
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md).
//
// The corpus under test/conformance/<edition>/ pins two verdicts that are not
// load outcomes: `lower` (the SQL a filter pushes down) and `evaluate` (what a
// filter decides against one row, in process). Both are the executor's own
// behaviour, and the functions that produce it -- tryCompileCombinedFilter and
// nodeMatchesExpression -- are unexported because nothing outside the executor
// should compose them. These two methods expose exactly that behaviour and
// nothing else, so a corpus case exercises the same code a read does rather
// than a copy of it.
//
// They take a filter in the runtime query grammar and bind no arguments and no
// actor: the corpus seeds its `lower` and `evaluate` cases without either. The
// epic that owns the expression language (dsl-v1-expressions) replaces what
// these call with the one lowering and the one in-process evaluator, and the
// corpus cases hold both to the same answers.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// ProbeLowerFilter returns the SQL predicate the executor pushes down for one
// filter over concept, with `?` placeholders. A filter that does not lower to
// a single predicate is an error: the executor would split it and evaluate part
// of it in process, and a corpus case asking for SQL is asking whether it
// lowers.
func (e *MemQLEngine) ProbeLowerFilter(ctx context.Context, concept, filter string) (string, error) {
	plan, err := e.probeParse(filter)
	if err != nil {
		return "", err
	}
	compiled, ok := e.tryCompileCombinedFilter(ctx, plan.Root, concept)
	if !ok {
		return "", fmt.Errorf("filter %q does not lower to one SQL predicate over %s", filter, concept)
	}
	return compiled.sql, nil
}

// ProbeEvaluateFilter decides one filter against one row of concept the way the
// executor's in-process post-filter does.
func (e *MemQLEngine) ProbeEvaluateFilter(ctx context.Context, concept, filter string, row map[string]any) (bool, error) {
	plan, err := e.probeParse(filter)
	if err != nil {
		return false, err
	}
	payload, err := json.Marshal(row)
	if err != nil {
		return false, fmt.Errorf("row is not JSON: %w", err)
	}
	id, _ := row["id"].(string)
	node := memorynodes.MemoryNode{ID: id, Concept: concept, Payload: payload}
	return nodeMatchesExpression(node, plan.Root, map[string]map[string]any{})
}

func (e *MemQLEngine) probeParse(filter string) (*QueryPlan, error) {
	if strings.TrimSpace(filter) == "" {
		return nil, errors.New("empty filter")
	}
	plan, err := e.Parse(filter)
	if err != nil {
		return nil, err
	}
	if plan.Root == nil {
		return nil, fmt.Errorf("filter %q parsed to no predicate", filter)
	}
	return plan, nil
}

// LoadReport is the account of the last Init's DSL load: what registered, what
// was skipped and why, and what was registered twice. Nil before Init. The
// caller must not modify it.
func (e *MemQLEngine) LoadReport() *LoadReport {
	return e.loadReport
}
