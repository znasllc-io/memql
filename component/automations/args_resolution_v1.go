package automations

// args_resolution_v1.go -- the load-time name rules (epic memql#5363,
// memql#5367).
//
// An automation's expressions are already parsed (PrepareExpressions runs
// first), so the rules are checked on the nodes, never on expression text:
// the free names of every expression -- the IdentExprs no enclosing lambda
// binds -- are exactly the names the run's scope must answer. A token scan
// could not do this: a lambda parameter (`r` in `rows.where(r => r.active)`)
// is a token that resolves to nothing it knows, and a quoted string and a
// reference are told apart only by quote-stripping heuristics.
//
// The rules apply to every expression position, value leaves included (a
// string value is a literal and a reference is a node, so nothing is
// ambiguous):
//
//   - for every automation: a step id or a loop variable may not be a
//     reserved root (RunScope would answer the root, never the step);
//   - for an args-block automation: an args field may not shadow a reserved
//     root, a step id or loop variable may not shadow an args field, and a
//     free name must be a reserved root, a loop variable in scope, a step id
//     or an args field.
//
// An automation without an args block keeps the runtime's implicit `event.`
// retry for an unknown root (any key of the event envelope), which no load
// check can see, so its free names are resolved at run time, where an unknown
// one is an unknown_name error.

import (
	"fmt"
	"sort"

	"github.com/znasllc-io/memql/component/language/ast"
)

// validateArgsResolutionV1 checks the name rules above.
func validateArgsResolutionV1(a *Automation) error {
	stepIDs := map[string]bool{}
	collectStepIDs(a.Steps, stepIDs)
	for _, id := range sortedKeys(stepIDs) {
		if reservedAutomationRoots[id] {
			return fmt.Errorf("automation %q: step %q has the name of a reserved root -- rename the step (a v1 expression reads %q as the root, never as the step)", a.Name, id, id)
		}
	}
	fields := declaredArgsSet(a)
	for _, name := range sortedKeys(fields) {
		if reservedAutomationRoots[name] {
			return fmt.Errorf("automation %q: args field %q shadows a reserved engine name -- rename the field (reserved: now, actor, partition, config, trace, event, args, steps, ...)", a.Name, name)
		}
	}
	for _, id := range sortedKeys(stepIDs) {
		if fields[id] {
			return fmt.Errorf("automation %q: step %q shadows the args field of the same name -- rename one of them (bare %q must resolve unambiguously; explicit args.%s always works)", a.Name, id, id, id)
		}
	}
	w := &v1NameWalk{automation: a, fields: fields, stepIDs: stepIDs}
	if a.Trigger != nil && a.Trigger.FilterLambda != nil {
		lam := a.Trigger.FilterLambda
		if err := w.check("trigger filter", lam, nil); err != nil {
			return err
		}
	}
	for _, pc := range a.Preconditions {
		if pc != nil {
			if err := w.check("precondition "+pc.ID, pc.checkExpr, nil); err != nil {
				return err
			}
		}
	}
	return w.steps(a.Steps, nil)
}

// v1NameWalk checks the free names of a v1 automation's expressions.
type v1NameWalk struct {
	automation *Automation
	fields     map[string]bool // nil: no args block, so free names are not checked
	stepIDs    map[string]bool
}

func (w *v1NameWalk) steps(steps []*Step, loopVars map[string]bool) error {
	for _, s := range steps {
		if s == nil {
			continue
		}
		if err := w.step(s, loopVars); err != nil {
			return err
		}
	}
	return nil
}

// step checks one step's expressions and recurses into the steps it holds,
// with a forEach's loop variable (and `index`) in scope inside its body.
func (w *v1NameWalk) step(s *Step, loopVars map[string]bool) error {
	x := s.Exprs
	if x == nil {
		return nil
	}
	at := func(pos string) string { return fmt.Sprintf("%s of step %s", pos, s.ID) }
	positions := []struct {
		name string
		node ast.ExpressionNode
	}{
		{"condition", x.Condition}, {"query", x.Query}, {"subject", x.Subject},
		{"id", x.ID}, {"parent", x.Parent}, {"aliasOf", x.AliasOf},
		{"url", x.URL}, {"topic", x.Topic},
		{"cardType", x.CardType}, {"partitionId", x.PartitionID}, {"conceptRef", x.ConceptRef},
	}
	for _, p := range positions {
		if err := w.check(at(p.name), p.node, loopVars); err != nil {
			return err
		}
	}
	for _, k := range sortedKeys(x.Headers) {
		if err := w.check(at("header "+k), x.Headers[k], loopVars); err != nil {
			return err
		}
	}
	var values []map[string]any
	switch {
	case s.Function != nil:
		values = append(values, s.Function.Args)
	case s.Action != nil:
		values = append(values, s.Action.Args)
	case s.Automation != nil:
		values = append(values, s.Automation.Args)
	case s.Mutation != nil:
		values = append(values, s.Mutation.Payload)
	case s.Event != nil:
		values = append(values, s.Event.Payload)
	case s.Webhook != nil:
		values = append(values, s.Webhook.Body)
	case s.EmitConceptCard != nil:
		values = append(values, s.EmitConceptCard.Data)
	}
	for _, m := range values {
		if err := w.values(at("arguments"), m, loopVars); err != nil {
			return err
		}
	}
	switch {
	case s.ForEach != nil:
		if err := w.check(at("forEach source"), x.Source, loopVars); err != nil {
			return err
		}
		loopVar := s.ForEach.As
		if loopVar == "" {
			loopVar = "item"
		}
		if reservedAutomationRoots[loopVar] && loopVar != "item" {
			return fmt.Errorf("automation %q: forEach loop variable %q (step %q) has the name of a reserved root -- rename it", w.automation.Name, loopVar, s.ID)
		}
		if w.fields[loopVar] {
			return fmt.Errorf("automation %q: forEach loop variable %q (step %q) shadows the args field of the same name -- rename one of them", w.automation.Name, loopVar, s.ID)
		}
		inner := cloneSet(loopVars)
		inner[loopVar] = true
		inner["index"] = true
		if err := w.check(at("forEach filter"), x.Filter, inner); err != nil {
			return err
		}
		return w.steps(s.ForEach.Do, inner)
	case s.Shape != nil, s.DetectLeadSignal != nil:
		return w.check(at("source"), x.Source, loopVars)
	case s.Parallel != nil:
		return w.steps(s.Parallel.Branches, loopVars)
	case s.Switch != nil:
		for _, k := range sortedCaseKeys(s.Switch.Cases) {
			if err := w.steps(caseSteps(s.Switch.Cases[k]), loopVars); err != nil {
				return err
			}
		}
		return w.steps(caseSteps(s.Switch.Default), loopVars)
	}
	return nil
}

// values checks the expression leaves of a prepared value map.
func (w *v1NameWalk) values(where string, v any, loopVars map[string]bool) error {
	switch x := v.(type) {
	case *ExprLeaf:
		return w.check(where, x.Node, loopVars)
	case map[string]any:
		for _, k := range sortedKeys(x) {
			if err := w.values(where, x[k], loopVars); err != nil {
				return err
			}
		}
	case []any:
		for _, el := range x {
			if err := w.values(where, el, loopVars); err != nil {
				return err
			}
		}
	}
	return nil
}

// check refuses the first free name in n that the run's scope cannot answer.
// It is a no-op for an automation without an args block (see the file
// comment).
func (w *v1NameWalk) check(where string, n ast.ExpressionNode, loopVars map[string]bool) error {
	if n == nil || w.fields == nil {
		return nil
	}
	var unknown []string
	v1FreeNames(n, nil, func(id *ast.IdentExpr) {
		name := id.Name
		if reservedAutomationRoots[name] || loopVars[name] || w.stepIDs[name] || w.fields[name] {
			return
		}
		unknown = append(unknown, name)
	})
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return fmt.Errorf("automation %q: unknown name %q in %s (`%s`) -- not a reserved root, loop variable, step name, or args field. Declare it in the args { } block, or reference it explicitly (args.X / steps.X)",
		w.automation.Name, unknown[0], where, ast.FormatExpr(n))
}

// v1FreeNames calls visit for every IdentExpr in n that no enclosing lambda
// binds: the names n reads from its scope. A callee name is not an IdentExpr
// (CallExpr.Name), and neither is a member or a map key, so none of those is
// visited.
func v1FreeNames(n ast.ExpressionNode, bound map[string]bool, visit func(*ast.IdentExpr)) {
	switch e := n.(type) {
	case nil:
	case *ast.IdentExpr:
		if !bound[e.Name] {
			visit(e)
		}
	case *ast.LambdaExpr:
		inner := cloneSet(bound)
		for _, p := range e.Params {
			inner[p] = true
		}
		v1FreeNames(e.Body, inner, visit)
	case *ast.MemberExpr:
		v1FreeNames(e.Object, bound, visit)
	case *ast.CallExpr:
		v1FreeNames(e.Receiver, bound, visit)
		for _, a := range e.Args {
			v1FreeNames(a, bound, visit)
		}
		for _, na := range e.Named {
			v1FreeNames(na.Value, bound, visit)
		}
	case *ast.UnaryExpr:
		v1FreeNames(e.Operand, bound, visit)
	case *ast.BinaryExpr:
		v1FreeNames(e.Left, bound, visit)
		v1FreeNames(e.Right, bound, visit)
	case *ast.ListExpr:
		for _, el := range e.Elems {
			v1FreeNames(el, bound, visit)
		}
	case *ast.MapExpr:
		for _, en := range e.Entries {
			v1FreeNames(en.Value, bound, visit)
		}
	case *ast.ParenExpr:
		v1FreeNames(e.Inner, bound, visit)
	case *ast.TernaryExpr:
		v1FreeNames(e.Condition, bound, visit)
		v1FreeNames(e.Then, bound, visit)
		v1FreeNames(e.Else, bound, visit)
	}
}
