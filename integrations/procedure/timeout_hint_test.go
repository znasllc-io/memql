package procedure

import (
	"reflect"
	"testing"
	"time"
)

// timeout_hint_test.go -- the longest recorded timeout rides beside the
// template, never in it (review finding I2, epic memql#5408).
//
// The app picks a different per-call timeout nearly every time, so a timeout
// generalized into the template was a FREE parameter no goal input supplies
// (f184315de dropped it from the arguments for that reason). Dropped
// entirely, every replayed command ran under the executor's default -- sixty
// seconds on the workbench -- however long the recordings let it run. It is
// kept as a HINT: the longest any recording asked for, per step, outside the
// version's hash, and handed to every dispatch.

// timeoutsOf is a stored payload's per-step timeout hint.
func timeoutsOf(t *testing.T, payload any) []int {
	t.Helper()
	p, err := DecodeProcedure(payload)
	if err != nil {
		t.Fatal(err)
	}
	if p.Hints == nil {
		return nil
	}
	return p.Hints.TimeoutsMs
}

// TestTheLongestRecordedTimeoutRidesBesideTheTemplate: two recordings whose
// command asked for two and five minutes leave a template with no timeout in
// it, a hint of five minutes on that step and none on the write -- and the
// SAME version as recordings that asked for none: a hint is not behaviour a
// person approves.
func TestTheLongestRecordedTimeoutRidesBesideTheTemplate(t *testing.T) {
	recs := twoRecordings()
	recs[0].execTimeoutMs = 120000
	recs[1].execTimeoutMs = 300000
	eng, res := liftFixture(t, recs...)
	payload := argsOf(t, eng.callTo(t, "recordProcedure"))["procedure"]
	if got := timeoutsOf(t, payload); !reflect.DeepEqual(got, []int{300000, 0}) {
		t.Fatalf("hints.timeoutsMs = %v, want the longest recorded per step", got)
	}
	p, _ := DecodeProcedure(payload)
	if _, present := p.Steps[0].Args.At([]string{"timeout"}); present {
		t.Fatal("the timeout is in the template: every recording's value would be a parameter nobody supplies")
	}

	_, plain := liftFixture(t, twoRecordings()...)
	if res.ProcedureHash != plain.ProcedureHash {
		t.Fatalf("a timeout hint changed the version: %s vs %s", res.ProcedureHash, plain.ProcedureHash)
	}
}

// TestAReplayCarriesTheRecordedTimeout: every dispatch is handed its step's
// hint, and a step with none is handed zero -- the executor's own default.
func TestAReplayCarriesTheRecordedTimeout(t *testing.T) {
	w := newReplayWorld(t, "trusted")
	w.withProcedure(t, func(p map[string]any) {
		p["hints"] = map[string]any{"timeoutsMs": []any{float64(300000), float64(0)}}
	})
	if out := w.serve(t, ReplayTrusted, map[string]any{"file": goalFile}); !out.Served {
		t.Fatalf("outcome = %+v", out)
	}
	calls := w.d.recorded()
	if len(calls) != 2 || calls[0].Timeout != 5*time.Minute || calls[1].Timeout != 0 {
		t.Fatalf("dispatched timeouts %v, want five minutes then the default", []time.Duration{calls[0].Timeout, calls[1].Timeout})
	}
}

// TestAnUnchangedVersionLearnsALongerTimeout: a recording that agrees with the
// procedure changes nothing a replay does, so the version is kept -- but if it
// let a command run longer than any before, the hint grows, or the next
// replay is cut off where the app was not. Only the hint is written: the
// source, the preconditions, the provenance and the ladder are the version's.
func TestAnUnchangedVersionLearnsALongerTimeout(t *testing.T) {
	short := twoRecordings()
	short[0].execTimeoutMs, short[1].execTimeoutMs = 60000, 120000
	lifted, _ := liftFixture(t, short...)
	stored := storedConstruct(t, lifted, "trusted")

	long := twoRecordings()
	long[0].execTimeoutMs, long[1].execTimeoutMs = 60000, 600000
	eng, res := relift(t, stored, &passingGate{}, long...)
	if res.Lift != LiftUnchanged {
		t.Fatalf("lift = %s, want the version unchanged", res.Lift)
	}
	if names := writeNames(eng); !reflect.DeepEqual(names, []string{"recordProcedure"}) {
		t.Fatalf("an unchanged version wrote %v, want only the hint", names)
	}
	rp := argsOf(t, eng.callTo(t, "recordProcedure"))
	if got := timeoutsOf(t, rp["procedure"]); !reflect.DeepEqual(got, []int{600000, 0}) {
		t.Fatalf("hints.timeoutsMs = %v, want the longer timeout learned", got)
	}
	if rp["procedureHash"] != stored["procedureHash"] || rp["source"] != stored["source"] {
		t.Fatal("refreshing the hint rewrote the version")
	}
	newFrom, _ := rp["procedure"].(map[string]any)["recordedFrom"]
	oldFrom, _ := stored["procedure"].(map[string]any)["recordedFrom"]
	if !reflect.DeepEqual(newFrom, oldFrom) {
		t.Fatalf("refreshing the hint rewrote the provenance: %v -> %v", oldFrom, newFrom)
	}

	// The same hint again writes nothing at all.
	again, _ := relift(t, copyRow(t, map[string]any{
		"id": stored["id"], "bundleId": stored["bundleId"], "name": stored["name"], "ladder": "trusted",
		"goalSignature": stored["goalSignature"], "procedureHash": stored["procedureHash"],
		"procedure": rp["procedure"], "preconditions": stored["preconditions"], "source": stored["source"],
	}), &passingGate{}, long...)
	if names := writeNames(again); len(names) != 0 {
		t.Fatalf("an unchanged version with an unchanged hint wrote %v", names)
	}
}
