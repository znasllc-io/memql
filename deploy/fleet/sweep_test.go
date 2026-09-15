package fleet

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// The scheduled sweeps (epic memql#3852, task memql#3856).
//
// # The failure this file exists for
//
// A sweep's candidate query is deliberately UNWINDOWED. `runningInstances`
// returns every running instance in the fleet, because a filter cannot call
// `addDuration` and the bracket therefore has to be applied per row, in the
// automation's `for ... if` filter.
//
// Which means the filter is the only thing between a sweep and the whole
// fleet. Drop it from `teardownAfterGrace` -- in a refactor, in a merge
// resolution, while "simplifying" -- and the next 04:00 UTC run tears down
// every suspended tenant we have, having taken a final backup of each and
// reported complete success. There is no error, no partial state, and no
// second chance: the Applications are gone and the finalizer has cascaded.
//
// A filter is a small thing to lose and an unrecoverable thing to have lost,
// so it gets a gate.

var (
	fleetConstructHeader = regexp.MustCompile(`^(automation|logic)\s+(\w+)\s*\{`)
	scheduleTrigger      = regexp.MustCompile(`@trigger\(schedule=`)
)

// fleetConstruct is one logic or automation of a fleet file: its source, with
// the doc and annotation lines above it, and its parsed statement body.
type fleetConstruct struct {
	kind, name, src string
	body            *ast.Body
}

// fleetConstructs parses every logic and automation of a fleet file. The
// gates below read the parsed body, so a statement written in a way they did
// not anticipate is still the statement it is.
func fleetConstructs(t *testing.T, file string) []fleetConstruct {
	t.Helper()
	lines := strings.Split(fleetFile(t, file), "\n")
	var out []fleetConstruct
	var preamble []string
	for i := 0; i < len(lines); i++ {
		m := fleetConstructHeader.FindStringSubmatch(lines[i])
		if m == nil {
			if strings.TrimSpace(lines[i]) == "" {
				preamble = nil
			} else {
				preamble = append(preamble, lines[i])
			}
			continue
		}
		block := append(append([]string{}, preamble...), lines[i])
		depth := strings.Count(lines[i], "{") - strings.Count(lines[i], "}")
		for i+1 < len(lines) && depth > 0 {
			i++
			block = append(block, lines[i])
			depth += strings.Count(lines[i], "{") - strings.Count(lines[i], "}")
		}
		preamble = nil
		src := strings.Join(block, "\n")
		pf, err := languageParser.ParseFile(src)
		if err != nil {
			t.Fatalf("%s: %s %s does not parse: %v", file, m[1], m[2], err)
		}
		var body *ast.Body
		for _, d := range pf.Definitions {
			if fn, ok := d.(*ast.FunctionDef); ok && fn.Name == m[2] {
				if auto, ok := fn.Body.(*ast.AutomationDef); ok {
					body = auto.Body
				}
			}
		}
		if body == nil {
			t.Fatalf("%s: %s %s did not parse to a statement body", file, m[1], m[2])
		}
		out = append(out, fleetConstruct{kind: m[1], name: m[2], src: src, body: body})
	}
	return out
}

// TestEverySweepNarrowsItsCandidates.
//
// Every `for` over a sweep's candidate set must carry an `if` filter. The
// candidate queries return everything in a status by design, so a bare `for`
// acts on the entire fleet -- and the sweep that would do the most damage is
// also the one whose action cannot be undone.
func TestEverySweepNarrowsItsCandidates(t *testing.T) {
	constructs := fleetConstructs(t, "trial.memql")
	if len(constructs) == 0 {
		t.Fatal("parsed no automations from trial.memql -- either the file moved or this parse stopped matching, and either way this gate is watching nothing")
	}

	var checked int
	for _, c := range constructs {
		if c.kind != "automation" || !scheduleTrigger.MatchString(c.src) {
			continue
		}
		checked++

		var loops []*ast.ForStatement
		ast.WalkBody(c.body.Statements, func(s ast.BodyStatement) bool {
			if f, ok := s.(*ast.ForStatement); ok {
				loops = append(loops, f)
			}
			return true
		})
		if len(loops) == 0 {
			t.Errorf("scheduled automation %s has no `for`; a sweep that acts on nothing is a sweep that silently does not run", c.name)
			continue
		}
		for _, loop := range loops {
			if loop.Filter == nil {
				t.Errorf("scheduled automation %s has a `for %s in ...` with NO `if` filter.\n"+
					"Its candidate query is unwindowed by design -- it returns every row in a status -- so this loop acts on the whole fleet. For teardownAfterGrace that is every suspended tenant destroyed, with a successful-looking run and nothing to recover from.",
					c.name, loop.Var)
			}
		}
	}
	if checked == 0 {
		t.Fatal("found no scheduled automations, so this gate checked nothing")
	}
}

// TestTheDestructiveSweepIsTheOnlyTeardownPath.
//
// `requestInstanceTeardown` is the one mutation in this domain whose
// consequence is irreversible. It must be reachable from exactly one place: the
// grace-expiry sweep.
//
// The property being protected is a SEQUENCE, not a permission. Nothing a
// customer or an operator does in one action destroys data; the path is pause,
// then fourteen days, then this. A second caller -- a convenience mutation, an
// Orbit action, a "delete my account" flow that seemed obviously fine -- removes
// the fourteen days without removing anything that looks like a safeguard.
func TestTheDestructiveSweepIsTheOnlyTeardownPath(t *testing.T) {
	const teardown = "requestInstanceTeardown"

	// Every logic and automation of the bundle: a logic may call a mutation
	// too, so a second path could be in any of them.
	files, err := filepath.Glob(filepath.Join(bundleRoot(t), "fleet", "*.memql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no fleet .memql files (%v): this gate would watch nothing", err)
	}
	var callers []string // "<file> <construct>", one per call
	for _, path := range files {
		file := filepath.Base(path)
		for _, c := range fleetConstructs(t, file) {
			ast.WalkBody(c.body.Statements, func(s ast.BodyStatement) bool {
				if call := statementCall(s); call != nil && call.Kind == "mutation" && call.Name == teardown {
					callers = append(callers, file+" "+c.name)
				}
				return true
			})
		}
	}

	switch {
	case len(callers) == 0:
		t.Fatalf("%s is called from nowhere -- the grace-expiry sweep does not destroy anything, so a torn-down tenant is one we keep paying for", teardown)
	case len(callers) > 1:
		t.Errorf("%s is called from %d places (%v). It must have exactly one caller: the grace-expiry sweep. What protects a customer's data here is a SEQUENCE -- pause, fourteen days, teardown -- and a second caller removes the fourteen days without removing anything that looks like a safeguard.", teardown, len(callers), callers)
	case callers[0] != "trial.memql teardownAfterGrace":
		t.Errorf("%s's single caller is %s; the only path to it is the grace-expiry sweep, trial.memql teardownAfterGrace", teardown, callers[0])
	}
}

// statementCall is the construct call a statement makes itself, or nil.
func statementCall(s ast.BodyStatement) *ast.ConstructCall {
	switch st := s.(type) {
	case *ast.CallStatement:
		return st.Call
	case *ast.AssignStatement:
		return st.Call
	case *ast.ReturnStatement:
		return st.Call
	}
	return nil
}
