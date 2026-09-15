package parser

import (
	"strings"
	"testing"
)

// TestRetiredExprBuiltinsRejectWithHint is the #2707 retirement gate (the
// #2620 ruling, riding the 2026.08 grammar epoch): the eight zero-use
// expression builtins are recognised only to emit a migration-hint parse
// error, exactly like the retired caller() / now() / timestamp() forms --
// never an opaque "unknown function".
func TestRetiredExprBuiltinsRejectWithHint(t *testing.T) {
	cases := []struct {
		call string
		hint string // a fragment the rejection must carry
	}{
		{"year(f)", "retired"},
		{"quarter(f)", "retired"},
		{"month(f)", "retired"},
		{"dayOfMonth(f)", "retired"},
		{"isAnniversary(f)", "retired"},
		{"isFirstDayOfQuarter(f)", "retired"},
		{"memqlVersion()", "retired"},
		{"subtractTimestamps(a, b)", "addDuration"},
	}
	for _, tc := range cases {
		name := tc.call[:strings.Index(tc.call, "(")]
		t.Run(name, func(t *testing.T) {
			src := "query user probe {\n  filter " + tc.call + " == 1\n}\n"
			normalised, nErr := NormaliseAll(src)
			if nErr != nil {
				t.Fatalf("normalise: %v", nErr)
			}
			_, err := ParseFile(normalised)
			if err == nil {
				t.Fatalf("%s must be rejected (retired under 2026.08, #2707)", tc.call)
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.hint) || !strings.Contains(msg, "#2707") {
				t.Errorf("%s rejection must carry the migration hint (%q) and name #2707, got: %s", tc.call, tc.hint, msg)
			}
		})
	}
}

// TestRetiredExprBuiltinsRejectedInConditionPositions pins the second gate:
// an if-condition is parsed by the v1 call parser (parseV1FunctionCall), not
// the callable dispatch the first gate sits in, so without a gate there a
// retired builtin in an if-condition would be load-green and silently
// constant-false at evaluation, the exact class the retirement must not
// reintroduce.
func TestRetiredExprBuiltinsRejectedInConditionPositions(t *testing.T) {
	body := func(cond string) string {
		return "logic probe {\n  args {\n    a string!\n  }\n  x := args.a ?? \"\"\n  if " + cond + " {\n    y := x + \"!\"\n  }\n  return x\n}\n"
	}
	src := body("year(args.a) == 2026")
	normalised, err := NormaliseAll(src)
	if err != nil {
		t.Fatalf("normalise: %v", err)
	}
	if _, err := ParseFile(normalised); err == nil || !strings.Contains(err.Error(), "#2707") {
		t.Fatalf("retired builtin in an if-condition must fail parse with the migration hint, got: %v", err)
	}
	// A live builtin in the same position still parses.
	src = body(`lower(args.a) == "x"`)
	normalised, err = NormaliseAll(src)
	if err != nil {
		t.Fatalf("normalise: %v", err)
	}
	if _, err := ParseFile(normalised); err != nil {
		t.Fatalf("live builtin in an if-condition must keep parsing: %v", err)
	}
}

// TestRetiredCallsRefusedAtEveryV1Position: the edition-2026 call parser
// keeps the legacy dispatch's refusals at every in-process position, not only
// in an if-condition -- a #2707 builtin and caller() anywhere, and `asOf`
// outside a query -- each with the legacy message, where it would otherwise
// read as a call to a function nothing defines.
func TestRetiredCallsRefusedAtEveryV1Position(t *testing.T) {
	logic := func(stmt string) string {
		return "logic probe {\n  args {\n    a string\n  }\n  " + stmt + "\n}\n"
	}
	for _, tc := range []struct{ name, src, want string }{
		{"a #2707 builtin in a logic return", logic("return month(args.a)"), "#2707"},
		{"caller() in a logic statement", logic("x := caller()\n  return x"), "caller.X is retired"},
		{"asOf in a logic return", logic("return asOf(things, latest)"), "query-only clause and cannot appear in a logic body"},
		{"a #2707 builtin in a mutation value", "mutation thing probe {\n  args {\n    a string\n  }\n  insert {\n    id: args.a\n    n: memqlVersion()\n  }\n}\n", "#2707"},
		{"a #2707 builtin in a call argument", "@trigger(event=\"node.created\", concept=\"v1:probe:thing\")\nautomation probe {\n  args {\n    at any\n  }\n  run := logic f(x: quarter(args.at))\n}\n", "#2707"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseV1Authored(t, tc.src)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want a refusal saying %q, got %v", tc.want, err)
			}
		})
	}
}

// TestRetiredExprBuiltinsGoneFromCallableSet pins the introspection surface:
// the retired names moved from CallableBuiltins (the dslspec-pinned editor
// set) to CallableRetiredNames.
func TestRetiredExprBuiltinsGoneFromCallableSet(t *testing.T) {
	retired := []string{"year", "quarter", "month", "dayofmonth", "isanniversary", "isfirstdayofquarter", "memqlversion", "subtracttimestamps"}
	inSet := func(set []string, name string) bool {
		for _, s := range set {
			if s == name {
				return true
			}
		}
		return false
	}
	for _, name := range retired {
		if inSet(CallableBuiltins, name) {
			t.Errorf("%s must leave CallableBuiltins (retired, #2707)", name)
		}
		if !inSet(CallableRetiredNames, name) {
			t.Errorf("%s must join CallableRetiredNames (#2707)", name)
		}
	}
	// The kept toolbox stays.
	for _, name := range []string{"lower", "upper", "trim", "tostring", "daysbetween", "contains", "addduration"} {
		if !inSet(CallableBuiltins, name) {
			t.Errorf("%s must STAY in CallableBuiltins (keep-toolbox, #2620 ruling)", name)
		}
	}
}

// TestRetiredInternalAnnotationOnDeclarativeKinds pins the #2708 pointed
// hint on the declarative kinds (builtin / shape / prompt / ...), whose
// annotations the parser holds to the annotation registry (memql#5359) -- the
// retirement message, not the generic unknown-annotation error.
func TestRetiredInternalAnnotationOnDeclarativeKinds(t *testing.T) {
	src := "@internal\n@executor(\"help\")\nbuiltin probeInternal {\n}\n"
	_, err := ParseBuiltinDecl(src)
	if err == nil || !strings.Contains(err.Error(), "#2708") {
		t.Fatalf("@internal on a builtin must carry the retirement hint, got: %v", err)
	}
}

// TestRetiredRoleAnnotationOnDeclarativeKinds is the #2709 twin: the buried
// @role must also carry the pointed message through the registry check, on
// both a builtin and a shape (the two dedicated-loader kinds).
func TestRetiredRoleAnnotationOnDeclarativeKinds(t *testing.T) {
	if _, err := ParseBuiltinDecl("@role(\"admin\")\n@executor(\"help\")\nbuiltin probeRole {\n}\n"); err == nil || !strings.Contains(err.Error(), "#2709") {
		t.Fatalf("@role on a builtin must carry the bury hint, got: %v", err)
	}
	if _, err := ParseShapeDecl("@role(\"admin\")\nshape ProbeRole {\n  a string\n}\n"); err == nil || !strings.Contains(err.Error(), "#2709") {
		t.Fatalf("@role on a shape must carry the bury hint, got: %v", err)
	}
}

// TestRetiredPermissionAnnotationOnDeclarativeKinds is the #2713 twin of the
// above: buried @permission must carry the pointed message through the
// declarative-kind validator too.
func TestRetiredPermissionAnnotationOnDeclarativeKinds(t *testing.T) {
	if _, err := ParseBuiltinDecl("@permission(\"read\")\n@executor(\"help\")\nbuiltin probePerm {\n}\n"); err == nil || !strings.Contains(err.Error(), "#2713") {
		t.Fatalf("@permission on a builtin must carry the bury hint, got: %v", err)
	}
	if _, err := ParseShapeDecl("@permission(\"read\")\nshape ProbePerm {\n  a string\n}\n"); err == nil || !strings.Contains(err.Error(), "#2713") {
		t.Fatalf("@permission on a shape must carry the bury hint, got: %v", err)
	}
}
