package memql

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/annotations"
)

// TestAutomationAnnotationGate pins the #2712 gate on the path it now runs on:
// an automation's annotations are held to the Automation receiver at PARSE
// time by the annotation registry (memql#5359), the same check every
// construct runs, rather than by a load-time text scan of their names.
// @trigger(schedule=...) is LIVE (folds to the honored AutomationDef.Schedule)
// and must be accepted, while @schedule, its retired synonym, is refused by
// name (D15); the dead behavior-promise annotations and the retired/buried
// names must be refused, the latter with the pointed message.
func TestAutomationAnnotationGate(t *testing.T) {
	body := func(preamble string) string {
		return preamble + "automation probe {\n  run := logic doThing(event: event)\n}\n"
	}
	accept := []string{
		"@trigger(event=\"x\", concept=\"v1:a:b\")\n",
		"@filter(row => row.a == 1)\n",
		"@enabled\n",
		"@description(\"d\")\n",
		"@trigger(schedule=\"0 5 9 * * *\")\n", // LIVE -- must stay accepted
	}
	for _, p := range accept {
		if err := runReceiverGate(annotations.Automation, body(p)); err != nil {
			t.Errorf("accepted annotation rejected: %q -> %v", strings.TrimSpace(p), err)
		}
	}

	// memqlmigrate:keep -- the retired synonym is the case.
	schedule := body("@schedule(cron=\"0 5 9 * * *\")\n")
	if err := runReceiverGate(annotations.Automation, schedule); err == nil || !strings.Contains(err.Error(), "[trigger_schedule_synonym_retired]") {
		t.Errorf("@schedule must be refused by name on an automation, got: %v", err)
	}

	// Dead behavior-promises -- not on the Automation receiver. @version is
	// live on a concept and a seed, so it is refused as misplaced; @deprecated
	// is unknown.
	for _, name := range []string{"deprecated", "version"} {
		err := runReceiverGate(annotations.Automation, body("@"+name+"\n"))
		if code := refusalCode(err); code != annotations.CodeUnknown && code != annotations.CodeMisplaced {
			t.Errorf("dead @%s must be refused on an automation, got code %q: %v", name, code, err)
		}
	}

	// Retired / buried -- refused with the pointed ticket message. The #989
	// removals and @async carry their history since memql#5360.
	for name, ticket := range map[string]string{
		"internal": "#2708", "role": "#2709", "permission": "#2713",
		"retry": "memql#989", "audit": "memql#989", "timeout": "memql#989", "async": "memql#2712",
	} {
		err := runReceiverGate(annotations.Automation, body("@"+name+"\n"))
		if refusalCode(err) != annotations.CodeRetired || !strings.Contains(err.Error(), ticket) {
			t.Errorf("retired @%s must carry %s on an automation, got: %v", name, ticket, err)
		}
	}

	// Typo -- refused, and the refusal suggests the name.
	err := runReceiverGate(annotations.Automation, body("@triggr(event=\"x\")\n"))
	if refusalCode(err) != annotations.CodeUnknown || !strings.Contains(err.Error(), "did you mean @trigger") {
		t.Errorf("typo'd @triggr must be refused with a did-you-mean, got: %v", err)
	}
}
