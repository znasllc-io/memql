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
// @schedule is LIVE (folds to the honored AutomationDef.Schedule) and must be
// accepted; the dead behavior-promise annotations and the retired/buried
// names must be refused, the latter with the pointed message.
func TestAutomationAnnotationGate(t *testing.T) {
	body := func(preamble string) string {
		return preamble + "automation probe {\n  step run {\n    logic doThing { event: event }\n  }\n}\n"
	}
	accept := []string{
		"@trigger(event=\"x\", concept=\"v1:a:b\")\n",
		"@filter(a == 1)\n",
		"@enabled\n",
		"@description(\"d\")\n",
		"@schedule(cron=\"0 5 9 * * *\")\n", // LIVE -- must stay accepted
	}
	for _, p := range accept {
		if err := runReceiverGate(annotations.Automation, body(p)); err != nil {
			t.Errorf("accepted annotation rejected: %q -> %v", strings.TrimSpace(p), err)
		}
	}

	// Dead behavior-promises -- not on the Automation receiver. @version is
	// live on a concept and a seed, so it is refused as misplaced; the rest
	// are unknown.
	for _, name := range []string{"retry", "audit", "async", "deprecated", "version", "timeout"} {
		err := runReceiverGate(annotations.Automation, body("@"+name+"\n"))
		if code := refusalCode(err); code != annotations.CodeUnknown && code != annotations.CodeMisplaced {
			t.Errorf("dead @%s must be refused on an automation, got code %q: %v", name, code, err)
		}
	}

	// Retired / buried -- refused with the pointed ticket message.
	for name, ticket := range map[string]string{"internal": "#2708", "role": "#2709", "permission": "#2713"} {
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
