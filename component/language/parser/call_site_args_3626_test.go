package parser

import (
	"strings"
	"testing"
)

// call_site_args_3626_test.go -- memql#3626, the two call-site siblings.
//
// A call's named arguments accumulate into a map[string]any. That map is where
// two different declarations stopped meaning what they said, and both were
// silent:
//
//   - a repeated name COLLAPSED last-wins, so the value a reader sees is not
//     the value the engine uses (the memql#2968 mechanism, one construct over);
//   - a reserved engine name BOUND, even though the args block on the other end
//     of the same contract refuses exactly those names -- so the value could
//     never be declared, never be read, and was guaranteed to be dropped.

func parseExprSafe(t *testing.T, src string) error {
	t.Helper()
	_, err := ParseExpression(src)
	return err
}

// A repeated argument name, in each of the three named-argument spellings.
func TestDuplicateCallArgumentIsRejected(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"colon form", `mutation m(a: 1, a: 2)`},
		{"equals form", `mutation m(a = 1, a = 2)`},
		{"mixed colon and equals", `mutation m(a: 1, a = 2)`},
		{"bare call", `publishEvent(topic: "x", topic: "y")`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := parseExprSafe(t, tc.src)
			if err == nil {
				t.Fatalf("a repeated argument was accepted in %s. Arguments collapse into one "+
					"map, so the first value is discarded with no signal -- the value a reader "+
					"sees left to right is not the one the engine uses.\n  source: %s", tc.name, tc.src)
			}
			if !strings.Contains(err.Error(), "duplicate argument") {
				t.Errorf("the error must say it is a duplicate; got: %v", err)
			}
			if !strings.Contains(err.Error(), `"a"`) && !strings.Contains(err.Error(), `"topic"`) {
				t.Errorf("the error must NAME the repeated argument, or the author cannot find "+
					"it in a long call; got: %v", err)
			}
		})
	}
}

// A reserved engine name in argument position. The args BLOCK already refuses
// exactly this set, which is what makes the value undeliverable rather than
// merely unusual.
func TestReservedEngineNameIsRejectedAsCallArgument(t *testing.T) {
	for _, name := range []string{"now", "actor", "partition", "config", "trace"} {
		t.Run(name, func(t *testing.T) {
			err := parseExprSafe(t, `mutation m(`+name+`: 1)`)
			if err == nil {
				t.Fatalf("an argument named %q was accepted. An args block may not declare that "+
					"name (reservedArgsNames), so it can never bind and is silently dropped -- "+
					"the two ends of one contract disagreed.", name)
			}
			if !strings.Contains(err.Error(), "reserved engine name") {
				t.Errorf("the error must say the name is reserved, matching the args-block "+
					"wording an author has already seen; got: %v", err)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("the error must name the offending argument; got: %v", err)
			}
		})
	}
}

// A reserved name reached the map through the PUNNED form too
// (`action f(now)` == `now: now`), which is a separate assignment site.
func TestReservedEngineNameIsRejectedWhenPunned(t *testing.T) {
	err := parseExprSafe(t, `logic l(now)`)
	if err == nil {
		t.Fatal("a punned reserved name was accepted. Punning binds `now: now`, which is the " +
			"same undeliverable argument written a third way.")
	}
	if !strings.Contains(err.Error(), "reserved engine name") {
		t.Errorf("got: %v", err)
	}
}

// The two ends must now agree in BOTH directions, which is the actual claim.
// Pinned as one test so a future change that relaxes either end fails here
// rather than re-opening the gap quietly.
func TestArgsBlockAndCallSiteAgreeOnReservedNames(t *testing.T) {
	for _, name := range []string{"now", "actor", "partition", "config", "trace"} {
		if _, err := parseArgsSafe("args { " + name + " string @required }"); err == nil {
			t.Errorf("the args block must still refuse %q -- if it stops, the call-site rule "+
				"below loses its reason", name)
		}
		if err := parseExprSafe(t, `mutation m(`+name+`: 1)`); err == nil {
			t.Errorf("the call site must refuse %q for the same reason the args block does", name)
		}
	}
}

// The direction that keeps the change honest: distinct, non-reserved argument
// names still parse and still carry their values.
func TestDistinctCallArgumentsStillParse(t *testing.T) {
	for _, src := range []string{
		`mutation m(a: 1, b: 2)`,
		`mutation m(a = 1, b = 2)`,
		`query q(spaceId: "s", limit: 10)`,
		`publishEvent(topic: "session.created", payload: { sessionId: "x" })`,
		// Two different calls may each use the same argument name; the
		// duplicate check is per-call, because the map is allocated per-call.
		// A fix that hoisted the seen-set out of parseFunctionCallWithKind
		// would break every nested call that reuses a name.
		`mutation m(x: someCall(a: 1), y: otherCall(a: 2))`,
		// An object-literal KEY is not a call argument and shares neither
		// rule: it may repeat a name used as an argument, and (being the
		// payload's own field) it is not measured against the reserved set.
		`mutation m(a: 1, payload: { a: 2 })`,
	} {
		if err := parseExprSafe(t, src); err != nil {
			t.Errorf("a valid call must still parse: %s\n  got: %v", src, err)
		}
	}
}
