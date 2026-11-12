package packages

import (
	"strings"
	"testing"
	"testing/fstest"
)

// An automation is registered by its NAME across the whole cluster: the
// scheduler, a call by name, a sub-automation step and a resume all find it by
// that name alone. So a package cannot declare an automation that takes the
// name of one of the engine's own: at boot the engine's keeps the name and the
// package's is left out, and the person who can rename it is the one reading
// this analysis.

// automationPackage is validPackage plus one automation in its domain.
func automationPackage(name string) fstest.MapFS {
	p := validPackage()
	p["dsl/acme/automations.memql"] = file("/// An acme automation.\n" +
		"@trigger(schedule=\"0 0 3 * * *\")\n" +
		"automation " + name + " {\n" +
		"  run := logic acmeNoop()\n" +
		"}\n")
	p["dsl/acme/logic.memql"] = file("/// Does nothing.\nlogic acmeNoop {\n  return true\n}\n")
	return p
}

func TestAPackageAutomationTakingAnEngineAutomationsNameIsRefused(t *testing.T) {
	// The control first: the same package with a name of its own deploys, so
	// the refusal below is about the name and not about the fixture.
	rep, err := Analyze(automationPackage("acmeNightlySweep"), Options{})
	if err != nil || !rep.OK {
		t.Fatalf("a package automation with its own name was refused: %v (problems: %+v)", err, rep.Problems)
	}

	const engineName = "emailStatisticsRollupSweep" // dsl/campaigns/automations.memql
	rep, err = Analyze(automationPackage(engineName), Options{})
	if err == nil || rep.OK {
		t.Fatalf("a package automation named %s, an automation of the engine's own, was accepted (problems: %+v)", engineName, rep.Problems)
	}
	var found *Problem
	for i := range rep.Problems {
		if strings.Contains(rep.Problems[i].Message, engineName) {
			found = &rep.Problems[i]
		}
	}
	if found == nil {
		t.Fatalf("no problem names the automation %s: %+v", engineName, rep.Problems)
	}
	if !found.Fatal || !strings.Contains(found.Message, "engine") {
		t.Fatalf("the problem must be fatal and say the name is the engine's: %+v", *found)
	}
	if found.Code != CodeDslAutomationNameTaken || RefusalCode(err) != CodeDslAutomationNameTaken {
		t.Errorf("want code %s on the problem and the refusal, got %q and %q", CodeDslAutomationNameTaken, found.Code, RefusalCode(err))
	}
	if !strings.Contains(found.Message, "campaigns/automations.memql") {
		t.Errorf("the problem must name the engine's automation it collides with: %q", found.Message)
	}
	if found.Scope != "dsl/acme/automations.memql" {
		t.Errorf("the problem must name the file that declares the automation, got scope %q", found.Scope)
	}
}
