package memql

import (
	"strings"
	"testing"
)

// TestValidateAutomationAnnotations pins the #2712 gate: automations now run
// through the same allow-list + retired-name gate the function kinds use.
// @schedule is LIVE (folds to the honored AutomationDef.Schedule) and must be
// accepted; the six dead behavior-promise annotations and the retired/buried
// names must be rejected with the pointed message.
func TestValidateAutomationAnnotations(t *testing.T) {
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
		if err := ValidateAutomationAnnotations(body(p)); err != nil {
			t.Errorf("accepted annotation rejected: %q -> %v", strings.TrimSpace(p), err)
		}
	}

	// Dead behavior-promises -- rejected as unknown (not in the allow-list).
	for _, name := range []string{"retry", "audit", "async", "deprecated", "version", "timeout"} {
		err := ValidateAutomationAnnotations(body("@" + name + "\n"))
		if err == nil {
			t.Errorf("dead @%s must be rejected on an automation", name)
		}
	}

	// Retired / buried -- rejected with the pointed ticket message.
	for name, ticket := range map[string]string{"internal": "#2708", "role": "#2709", "permission": "#2713"} {
		err := ValidateAutomationAnnotations(body("@" + name + "\n"))
		if err == nil || !strings.Contains(err.Error(), ticket) {
			t.Errorf("retired @%s must carry %s on an automation, got: %v", name, ticket, err)
		}
	}

	// Typo -- rejected.
	if err := ValidateAutomationAnnotations(body("@triggr(event=\"x\")\n")); err == nil {
		t.Error("typo'd @triggr must be rejected")
	}
}
