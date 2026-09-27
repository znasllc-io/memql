package dslgate

import (
	"strconv"
	"strings"
	"testing"
)

// statementGates is what the statement-body gate reports over files: each
// violation as "gate@file:line construct".
func statementGates(files ...SourceFile) []string {
	var out []string
	for _, v := range ScanFiles(files, Options{}) {
		if v.Gate == GateStatementConfigKey || v.Gate == GateStatementUnknownCall {
			out = append(out, string(v.Gate)+"@"+v.File+":"+strconv.Itoa(v.Line)+" "+v.Construct+": "+v.Detail)
		}
	}
	return out
}

const predicatesFile = `use probe.concepts.{ thing }

spec thing isOpen = row => row.status == "open"
trait isActive = row => row.active == true
`

const queriesFile = `query thing getUser {
  args {
    id string
  }
  filter row => row.id == args.id
  paginate 10
}
`

// TestStatementBodiesReadAllowedConfigAndKnownCalls: what the corpus answers
// is not reported -- an allow-listed config key, a catalog function, a spec
// and a trait declared in another file, and a construct call with its kind.
func TestStatementBodiesReadAllowedConfigAndKnownCalls(t *testing.T) {
	logic := `logic fine {
  args {
    id string
    t object
  }
  user := query getUser(id: args.id)
  flag := config.demoMode == true
  return lower("X") + (isOpen(args.t) ? "o" : "") + (isActive(args.t) ? "a" : "") + (flag ? "d" : "")
}
`
	if got := statementGates(SourceFile{"probe/predicates.memql", predicatesFile},
		SourceFile{"probe/queries.memql", queriesFile}, SourceFile{"probe/logic.memql", logic}); len(got) != 0 {
		t.Fatalf("reported what the corpus answers:\n%s", strings.Join(got, "\n"))
	}
}

// TestStatementBodiesRefuseAnUnknownConfigKey: a key the allow-list does not
// hold reads absent at run time, so it is reported at its line -- in a logic
// and in an automation.
func TestStatementBodiesRefuseAnUnknownConfigKey(t *testing.T) {
	src := `// logic.memql -- probe namespace.

logic probeConfig {
  return config.definitelyNotExposed
}

@trigger(schedule="0 0 * * * *")
automation probeConfigToo {
  builtin note(v: config.DemoMode)
}
`
	got := statementGates(SourceFile{"probe/logic.memql", src})
	if len(got) != 2 {
		t.Fatalf("want two reports, got:\n%s", strings.Join(got, "\n"))
	}
	for i, want := range []string{
		"statement-config-key@probe/logic.memql:4 probeConfig: `config.definitelyNotExposed` names no key of the config allow-list",
		// A key is spelled exactly as the allow-list spells it: the envelope
		// is keyed by it, so another casing reads absent too.
		"statement-config-key@probe/logic.memql:9 probeConfigToo: `config.DemoMode` names no key",
	} {
		if !strings.HasPrefix(got[i], want) || !strings.HasSuffix(got[i], "[body_config_unknown]") {
			t.Errorf("report %d = %q, want it to open %q and end with the code", i, got[i], want)
		}
	}
}

// TestStatementBodiesRefuseAnUnknownBareCall: a bare call naming neither a
// catalog function nor a declared predicate fails when it runs, so it is
// reported; one naming a construct is told to call it with its kind.
func TestStatementBodiesRefuseAnUnknownBareCall(t *testing.T) {
	src := `logic probeUnknownCall {
  args {
    id string
  }
  a := getUser(id: args.id).name
  return definitelyNothing(1)
}
`
	got := statementGates(SourceFile{"probe/queries.memql", queriesFile}, SourceFile{"probe/logic.memql", src})
	if len(got) != 2 {
		t.Fatalf("want two reports, got:\n%s", strings.Join(got, "\n"))
	}
	if want := "statement-unknown-call@probe/logic.memql:5 probeUnknownCall: `getUser(...)` is not a function or a predicate known here"; !strings.HasPrefix(got[0], want) ||
		!strings.Contains(got[0], "getUser is a query, which is called with its kind as a statement of its own: `x := query getUser(...)`") {
		t.Errorf("report = %q, want %q and the construct's remedy", got[0], want)
	}
	if want := "statement-unknown-call@probe/logic.memql:6 probeUnknownCall: `definitelyNothing(...)`"; !strings.HasPrefix(got[1], want) ||
		!strings.HasSuffix(got[1], "[body_call_unknown]") {
		t.Errorf("report = %q, want %q and the code", got[1], want)
	}
}

// TestStatementBodiesPassOverARefusedBody: a body the parser refuses -- here
// a retired form, body_block_retired -- is the loader's to report; the gates
// pass over it rather than reporting a read they could not make.
func TestStatementBodiesPassOverARefusedBody(t *testing.T) {
	// memqlmigrate:keep -- the retired form is the case.
	src := `logic legacy {
  body {
    return config.definitelyNotExposed
  }
}
`
	if got := statementGates(SourceFile{"probe/logic.memql", src}); len(got) != 0 {
		t.Fatalf("reported a refused body:\n%s", strings.Join(got, "\n"))
	}
}

// TestStatementBodiesReadIsTheGatesCoverage: the coverage names each body the
// gates read, and not one they passed over.
func TestStatementBodiesReadIsTheGatesCoverage(t *testing.T) {
	// memqlmigrate:keep -- the refused construct is the one the coverage must not name.
	src := `logic native {
  return 1
}

logic legacy {
  body {
    return 1
  }
}

@trigger(schedule="0 0 * * * *")
automation nativeToo {
  builtin note(v: 1)
}
`
	got := StatementBodiesRead([]SourceFile{{"probe/logic.memql", src}})
	if want := []string{"probe/logic.memql native", "probe/logic.memql nativeToo"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("read %q, want %q", got, want)
	}
}

// TestStatementGatesReadAnAutomationsHeaderLambdas (memql#5426): an
// automation's trigger @filter and @loop's until are evaluated in process by
// the evaluator its conditions use, so a bare call there is held to the same
// rule -- a catalog function or a declared predicate -- and reported at the
// line it is written on, where it used to load and refuse every fire. A known
// function and a declared predicate pass.
func TestStatementGatesReadAnAutomationsHeaderLambdas(t *testing.T) {
	src := `/// Filters on names nothing declares.
@trigger(event="node.updated", concept="v1:probe:thing")
@filter(row => frobnicate(row.status))
automation probeFilterCall {
  builtin note(v: 1)
}

@trigger(event="node.updated", concept="v1:probe:thing")
@filter(row => row.status != "done" && isNoSuchSpec(row))
@loop(maxDepth=3, until=row => definitelyNothing(row))
automation probeHeaderCalls {
  builtin note(v: 1)
}

@trigger(event="node.updated", concept="v1:probe:thing")
@filter(row => lower(row.status) == "open" && isOpen(row) && isActive(row))
automation probeFilterKnown {
  builtin note(v: 1)
}
`
	got := statementGates(SourceFile{"probe/predicates.memql", predicatesFile}, SourceFile{"probe/automations.memql", src})
	want := []string{
		"statement-unknown-call@probe/automations.memql:3 probeFilterCall: `frobnicate(...)` is not a function or a predicate known here",
		"statement-unknown-call@probe/automations.memql:9 probeHeaderCalls: `isNoSuchSpec(...)` is not a function or a predicate known here",
		"statement-unknown-call@probe/automations.memql:10 probeHeaderCalls: `definitelyNothing(...)` is not a function or a predicate known here",
	}
	if len(got) != len(want) {
		t.Fatalf("want %d reports, got:\n%s", len(want), strings.Join(got, "\n"))
	}
	for i := range want {
		if !strings.HasPrefix(got[i], want[i]) || !strings.HasSuffix(got[i], "[body_call_unknown]") {
			t.Errorf("report %d = %q, want it to open %q and end with the code", i, got[i], want[i])
		}
	}
}

// TestStatementGatesRefuseAMethodNoValueHas (memql#5426): the evaluator
// resolves `x.name()` against the methods a list or a string has before it
// evaluates the receiver, so a name neither has fails on every run; the gate
// refuses it at load -- in a condition, in a logic's return and in a trigger
// filter -- and passes a catalog method.
func TestStatementGatesRefuseAMethodNoValueHas(t *testing.T) {
	src := `@trigger(event="node.updated", concept="v1:probe:thing")
@filter(row => row.status.frob())
automation probeMethods {
  args {
    region  string
    tags    []string
  }
  if args.region.includes("-") && args.tags.count() > 0 {
    builtin note(v: 1)
  }
  if args.region.frobnicate() {
    builtin note(v: 2)
  }
}

logic probeLogicMethod {
  args {
    region  string
  }
  return args.region.shout()
}
`
	got := statementGates(SourceFile{"probe/automations.memql", src})
	want := []string{
		"statement-unknown-call@probe/automations.memql:2 probeMethods: `.frob()` is not a method of a list or a string",
		"statement-unknown-call@probe/automations.memql:11 probeMethods: `.frobnicate()` is not a method of a list or a string",
		"statement-unknown-call@probe/automations.memql:20 probeLogicMethod: `.shout()` is not a method of a list or a string",
	}
	if len(got) != len(want) {
		t.Fatalf("want %d reports, got:\n%s", len(want), strings.Join(got, "\n"))
	}
	for i := range want {
		if !strings.HasPrefix(got[i], want[i]) || !strings.HasSuffix(got[i], "[body_call_unknown]") {
			t.Errorf("report %d = %q, want it to open %q and end with the code", i, got[i], want[i])
		}
	}
}

// TestStatementGatesReadAMultiLineHeaderLambda (memql#5426 review): the
// declaration is parsed from the first line of its preamble, and a filter
// whose lambda spans lines is part of that preamble. The walk used to stop at
// the filter's closing line, so the declaration was parsed without it and the
// method it calls was never checked.
func TestStatementGatesReadAMultiLineHeaderLambda(t *testing.T) {
	src := `@trigger(event="node.updated", concept="v1:probe:thing")
@filter(row =>
  row.status.frob()
)
automation probeMultiLineFilter {
  builtin note(v: 1)
}
`
	got := statementGates(SourceFile{"probe/automations.memql", src})
	want := "statement-unknown-call@probe/automations.memql:3 probeMultiLineFilter: `.frob()` is not a method of a list or a string"
	if len(got) != 1 || !strings.HasPrefix(got[0], want) {
		t.Fatalf("want one report opening %q, got:\n%s", want, strings.Join(got, "\n"))
	}
}

// TestStatementGatesRefuseACallOfTheWrongShape (memql#5426 review): the
// evaluator checks a call's arguments against the catalog entry before it
// evaluates the receiver, so a call of the wrong shape fails on every run
// whatever the data -- and `"abc".sum()` calls a list method on a value the
// source already says is a string. Both are refused at load, as is a catalog
// function called with the wrong count; a name a list and a string both have
// fits when it fits either, and a lambda's parameter count, which the catalog
// does not state, is not judged here.
func TestStatementGatesRefuseACallOfTheWrongShape(t *testing.T) {
	src := `logic probeShapes {
  args {
    items  []object
    name   string
  }
  a := args.items.any()
  b := "abc".sum()
  c := args.items.where(1)
  d := lower()
  e := args.items.any(x => x.active == true)
  f := args.name.count() + args.items.count()
  g := args.items.reduce(0, (acc, x) => acc + x.n)
  h := "abc".includes("b")
  return lower(args.name)
}
`
	got := statementGates(SourceFile{"probe/logic.memql", src})
	want := []string{
		"statement-unknown-call@probe/logic.memql:6 probeShapes: list.any(pred lambda) bool takes 1 argument(s), and `args.items.any()` passes 0",
		"statement-unknown-call@probe/logic.memql:7 probeShapes: `.sum()` is a list method and `\"abc\"` is a string",
		"statement-unknown-call@probe/logic.memql:8 probeShapes: argument 1 of list.where(",
		"statement-unknown-call@probe/logic.memql:9 probeShapes: lower(",
	}
	if len(got) != len(want) {
		t.Fatalf("want %d reports, got:\n%s", len(want), strings.Join(got, "\n"))
	}
	for i := range want {
		if !strings.HasPrefix(got[i], want[i]) || !strings.HasSuffix(got[i], "[body_call_unknown]") {
			t.Errorf("report %d = %q, want it to open %q and end with the code", i, got[i], want[i])
		}
	}
}
