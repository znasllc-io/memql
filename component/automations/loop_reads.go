package automations

import (
	"encoding/json"
	"reflect"
	"sort"

	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/work"
)

// computeReads narrows only an event-pure body. Unknown constructs and storage
// reads retain the full event so a changing external input cannot be hidden.
func computeReads(a *Automation, source FunctionSource) []string {
	if source == nil {
		return nil
	}
	fields := map[string]bool{"id": true, "nodeId": true}
	if a.Args != nil {
		for _, f := range a.Args.Fields {
			if f != nil {
				fields[f.Name] = true
			}
		}
	}
	pure := true
	checkExpr := func(n ast.ExpressionNode, row string) {
		ast.WalkV1(n, func(e ast.ExpressionNode) bool {
			if root, path, ok := ast.MemberPath(e); ok {
				switch root {
				case "event":
					if len(path) < 2 || path[0] != "payload" {
						pure = false
					} else {
						fields[path[1]] = true
					}
				case "actor", "system", "now", "config", "partition":
					pure = false
				case "args":
					if len(path) == 0 {
						pure = false
					}
					if len(path) > 0 {
						fields[path[0]] = true
					}
				default:
					if row != "" && root == row && len(path) == 0 {
						pure = false
					}
					if row != "" && root == row && len(path) > 0 {
						fields[path[0]] = true
					}
				}
				return false
			}
			if id, ok := e.(*ast.IdentExpr); ok {
				switch id.Name {
				case "event", "actor", "system", "now", "config", "partition":
					pure = false
				}
			}
			if call, ok := e.(*ast.CallExpr); ok && call.Receiver == nil {
				switch call.Name {
				case "var", "systemVar", "secret", "systemSecret":
					pure = false
				}
			}
			return true
		})
	}
	if a.Trigger != nil && a.Trigger.FilterLambda != nil {
		checkExpr(a.Trigger.FilterLambda.Body, a.Trigger.FilterLambda.Params[0])
	}
	// Prepared expression leaves and StepExprs occur in nested maps and blocks.
	var scan func(reflect.Value)
	scan = func(v reflect.Value) {
		if !v.IsValid() || ((v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) && v.IsNil()) {
			return
		}
		if v.CanInterface() {
			if n, ok := v.Interface().(ast.ExpressionNode); ok {
				checkExpr(n, "")
				return
			}
		}
		switch v.Kind() {
		case reflect.Interface, reflect.Pointer:
			if !v.IsNil() {
				scan(v.Elem())
			}
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				if v.Type().Field(i).IsExported() {
					scan(v.Field(i))
				}
			}
		case reflect.Map:
			for _, k := range v.MapKeys() {
				scan(v.MapIndex(k))
			}
		case reflect.Slice:
			for i := 0; i < v.Len(); i++ {
				scan(v.Index(i))
			}
		}
	}
	scan(reflect.ValueOf(a.Steps))
	if len(a.Preconditions) > 0 {
		return nil
	} // preconditions may consult external state
	reg := source.Registry()
	visiting := map[string]bool{}
	var callsPure func(*Automation) bool
	var functionPure func(string) bool
	functionPure = func(name string) bool {
		target, ok := reg[name]
		if !ok {
			return false
		}
		if target.ConstructKind == work.ConstructMutation {
			return true
		}
		if target.ConstructKind != work.ConstructLogic || visiting[name] {
			return false
		}
		src, ok := source.(registrySource)
		if !ok || src.functions == nil {
			return false
		}
		fn := src.functions[name]
		if fn == nil {
			return false
		}
		raw, err := json.Marshal(fn.LogicBody)
		if err != nil {
			return false
		}
		body := &Automation{}
		if json.Unmarshal(raw, &body.Steps) != nil || PrepareExpressions(body) != nil {
			return false
		}
		visiting[name] = true
		defer delete(visiting, name)
		// Logic arguments are supplied by the caller; only reject ambient event
		// access here. Keeping extra argument names in Reads is conservative.
		scan(reflect.ValueOf(body.Steps))
		return pure && callsPure(body)
	}
	callsPure = func(body *Automation) bool {
		facts := collectStepFacts(body)
		if len(facts.subs) > 0 || len(facts.opaque) > 0 {
			return false
		}
		for _, call := range facts.calls {
			if !functionPure(call.name) {
				return false
			}
		}
		return true
	}
	if !pure || !callsPure(a) || !pure {
		return nil
	}
	out := make([]string, 0, len(fields))
	for f := range fields {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}
