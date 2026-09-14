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
