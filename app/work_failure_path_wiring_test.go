package app

// work_failure_path_wiring_test.go -- guards the planner's half of the work
// spine's failure path (memql#5664).
//
// WHY A SOURCE-LEVEL GUARD. wireWorkFailurePath was written, reviewed and
// documented, and nothing called it: every replan and repair wait stayed
// parked for ever, with every test green, because the wiring compiles only
// under the planner build tag and no test lane builds app/ with it
// (app/planner_claimer_test.go records the same constraint). So this guard
// asserts the WIRING -- the call, its place in the phase, and the claim
// before the remedy -- and the behaviour is integrations/work's and
// integrations/planner's tests.

import (
	"os"
	"strings"
	"testing"
)

func TestTheWorkFailurePathIsWiredOnThePlanner(t *testing.T) {
	phase := readAppSource(t, "integrations_planner.go")
	compile := strings.Index(phase, "a.wireWorkCompiler()")
	failure := strings.Index(phase, "a.wireWorkFailurePath()")
	if failure < 0 {
		t.Fatal("integrationsPlanner no longer calls wireWorkFailurePath: no replan or repair wait is served anywhere, and a classified plan or contract miss parks for ever")
	}
	if compile < 0 || failure < compile {
		t.Fatal("wireWorkFailurePath must run after wireWorkCompiler: its run-event subscription is what hands a remedy wait to this node, whichever node holds the sweep's cron lease")
	}

	body := appFunctionBody(t, readAppSource(t, "integrations_work_compile.go"), "func (a *App) wireWorkFailurePath()")
	claim, remedy := strings.Index(body, "SetRunClaimer("), strings.Index(body, "SetRemedy(")
	if remedy < 0 {
		t.Fatal("wireWorkFailurePath no longer installs the remedy")
	}
	if claim < 0 || claim > remedy {
		t.Fatal("wireWorkFailurePath must install the cross-replica claim before the remedy: integrations/work refuses a remedy it cannot claim, and served unclaimed a re-plan is paid for and installed once per planner replica")
	}
	if strings.Contains(body, "TopicPreconditionMissed") {
		t.Fatal("wireWorkFailurePath subscribes the healer: the miss event is broadcast to every replica and the healer takes no claim, so every precondition miss would be proposed against and approved once per planner replica (see wireWorkHealer)")
	}
}

func readAppSource(t *testing.T, name string) string {
	t.Helper()
	src, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(src)
}

// appFunctionBody is the source of one top-level function, from its signature
// to its closing brace at the start of a line.
func appFunctionBody(t *testing.T, src, signature string) string {
	t.Helper()
	start := strings.Index(src, signature)
	if start < 0 {
		t.Fatalf("%q is gone; this guard needs updating", signature)
	}
	rest := src[start:]
	if end := strings.Index(rest, "\n}\n"); end >= 0 {
		return rest[:end]
	}
	return rest
}
