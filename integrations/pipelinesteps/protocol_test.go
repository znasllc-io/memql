package pipelinesteps

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// TestStepRunNeverPrintsItsSecrets (ruling R17): a StepRun carries resolved
// secret values because the runner needs them, so a log line or an error that
// prints one leaks them. Printed with any fmt verb, or recorded through slog,
// a StepRun shows each secret's NAME -- so a reader can tell it was there --
// and never its value. JSON is the forward's wire and keeps the values: that
// is the one place they travel.
func TestStepRunNeverPrintsItsSecrets(t *testing.T) {
	run := testRun()
	check := func(t *testing.T, how, out string) {
		t.Helper()
		for name, value := range run.Secrets {
			if strings.Contains(out, value) {
				t.Errorf("%s prints the value of secret %s:\n%s", how, name, out)
			}
			if !strings.Contains(out, name) {
				t.Errorf("%s does not name secret %s, so a reader cannot tell it was there:\n%s", how, name, out)
			}
		}
	}

	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
		check(t, "fmt "+verb, fmt.Sprintf(verb, run))
		check(t, "fmt "+verb+" of a pointer", fmt.Sprintf(verb, &run))
	}
	check(t, "fmt inside a struct", fmt.Sprintf("%+v", struct{ Run StepRun }{run}))

	var text, structured bytes.Buffer
	slog.New(slog.NewTextHandler(&text, nil)).Info("step", "run", run)
	slog.New(slog.NewJSONHandler(&structured, nil)).Info("step", "run", run)
	check(t, "slog text", text.String())
	check(t, "slog JSON", structured.String())

	raw, err := json.Marshal(run)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for name, value := range run.Secrets {
		if !strings.Contains(string(raw), value) {
			t.Errorf("the forward's JSON lost the value of secret %s: the runner could not put it in the step's Secret", name)
		}
	}
}
