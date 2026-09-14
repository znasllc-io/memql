package parser

// v1_positions_test.go -- the positions that parse the edition-2026
// expression grammar (task memql#5364, Task 3 of the DSL v1 expressions plan),
// in both modes of the transition: Options.ExpressionsV1 off (today's grammar,
// with the pushdown positions accepting their v1 spellings beside the legacy
// ones) and on (every position v1, the legacy predicate forms refused).

import (
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
)

var (
	v1Off = Options{}
	v1On  = Options{ExpressionsV1: true}
)

// parseV1Authored runs authored source through the path the engine uses --
// the struct-form rewriter, then the parser -- under o.
func parseV1Authored(t *testing.T, src string, o Options) (*File, error) {
	t.Helper()
	normalised, err := NormaliseAll(src)
	if err != nil {
		return nil, err
	}
	return ParseFileWithOptions(normalised, o)
}

func mustParseV1Authored(t *testing.T, src string, o Options) *File {
	t.Helper()
	f, err := parseV1Authored(t, src, o)
	if err != nil {
		t.Fatalf("parse (ExpressionsV1=%v):\n%s\nerror: %v", o.ExpressionsV1, src, err)
	}
	return f
}

// onlyFunction returns the file's one FunctionDef.
func onlyFunction(t *testing.T, f *File) *FunctionDef {
	t.Helper()
	var fns []*FunctionDef
	for _, d := range f.Definitions {
		if fn, ok := d.(*FunctionDef); ok {
			fns = append(fns, fn)
		}
	}
	if len(fns) != 1 {
		t.Fatalf("want one function definition, got %d (%d definitions)", len(fns), len(f.Definitions))
	}
	return fns[0]
}

// TestExpressionsV1MarksDefinitions: every definition parsed with the option
// on says so, on the FunctionDef and on an automation or logic body, because
// that flag is what the compiler and the automation runtime key their v1 path
// on; with the option off nothing is marked.
func TestExpressionsV1MarksDefinitions(t *testing.T) {
	sources := map[string]string{
		"logic": `logic probe {
  args {
    x string @required
  }
  body {
    return args.x
  }
}`,
		"automation": `@trigger(event="node.created", concept="v1:probe:thing")
automation probe {
  step first {
    logic other(x: 1)
  }
}`,
	}
	for name, src := range sources {
		t.Run(name, func(t *testing.T) {
			for _, o := range []Options{v1Off, v1On} {
				fn := onlyFunction(t, mustParseV1Authored(t, src, o))
				if fn.ExpressionsV1 != o.ExpressionsV1 {
					t.Errorf("ExpressionsV1=%v: FunctionDef.ExpressionsV1 = %v", o.ExpressionsV1, fn.ExpressionsV1)
				}
				auto, ok := fn.Body.(*ast.AutomationDef)
				if !ok {
					t.Fatalf("want an AutomationDef body, got %T", fn.Body)
				}
				if auto.ExpressionsV1 != o.ExpressionsV1 {
					t.Errorf("ExpressionsV1=%v: AutomationDef.ExpressionsV1 = %v", o.ExpressionsV1, auto.ExpressionsV1)
				}
			}
		})
	}
	if DefaultOptions.ExpressionsV1 {
		t.Error("DefaultOptions.ExpressionsV1 is on before the tree is migrated")
	}
}
