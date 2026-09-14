package memql

import (
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// logic_body_load_test.go -- an edition-2026 logic body compiles at LOAD
// (epic memql#5370, task memql#5372): the function loader runs
// compiler.CompileBody over it, so every scope problem and every D14 construct
// rule refuses the load, and a clean body carries its compiled step list.

func TestNativeLogicCompilesAtLoad(t *testing.T) {
	fn, err := tryParseNewFunctionSyntax("routeStatus", "logic", `/// Route by role.
logic routeStatus {
  args {
    role any
  }
  r := args.role ?? ""
  return r == "owner" ? "queued" : "needs_validation"
}`, "forge.logic.memql", newMemoryRegistry(map[string]*memoryNodes.Concept{}))
	if err != nil {
		t.Fatalf("a clean statement body refused the load: %v", err)
	}
	if len(fn.LogicBody) != 2 || fn.LogicBody[0]["id"] != "r" || fn.LogicBody[1]["type"] != "return" {
		t.Fatalf("LogicBody = %v, want the binding and the return, in source order", fn.LogicBody)
	}
	if fn.LogicSteps != nil || fn.Expr != nil {
		t.Fatalf("a statement body has no legacy form: LogicSteps %v, Expr %v", fn.LogicSteps, fn.Expr)
	}
	if clone := fn.clone(); len(clone.LogicBody) != 2 {
		t.Fatalf("the registry's clone dropped LogicBody: %v", clone.LogicBody)
	}
}

func TestNativeLogicProblemsRefuseTheLoad(t *testing.T) {
	cases := []struct {
		name, fn, src, want string
	}{
		{"forward reference", "early", `logic early {
  x := y
  y := 1
  return x
}`, "[body_forward_reference]"},
		{"publish", "announce", `logic announce {
  publish "things.happened" { at: now }
  return 1
}`, "[body_publish_in_logic]"},
		{"an automation call", "sweepAll", `logic sweepAll {
  automation sweep(limit: 10)
  return 1
}`, "[body_call_not_in_logic]"},
		{"no final return", "noReturn", `logic noReturn {
  x := 1
}`, "[body_logic_return]"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			_, err := tryParseNewFunctionSyntax(c.fn, "logic", c.src, "x.logic.memql", newMemoryRegistry(map[string]*memoryNodes.Concept{}))
			if err == nil {
				t.Fatalf("the load accepted it; it should refuse with %s", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not carry %s", err, c.want)
			}
		})
	}
}
