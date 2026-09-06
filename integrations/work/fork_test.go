package work

import (
	"strings"
	"testing"
)

// sourceRun is the row workRunForOwner answers with in this file.
func sourceRun(owner string) map[string]any {
	return map[string]any{
		"id":                  "v1:work:run:r-source",
		"ownerUserId":         owner,
		"goalId":              "v1:work:goal:g1",
		"automationName":      "reconcileInvoices",
		"templateFingerprint": "fp-abc",
		"input":               map[string]any{"month": "2026-09"},
		"inputFingerprint":    "in-abc",
		"status":              runStatusSucceeded,
	}
}

// TestForkAndReplayDeriveARunAndLeaveTheSourceAlone.
func TestForkAndReplayDeriveARunAndLeaveTheSourceAlone(t *testing.T) {
	cases := []struct {
		name        string
		args        map[string]any
		call        func(i *Integration, args map[string]any) (map[string]any, error)
		wantMode    string
		wantForkKey string
	}{
		{
			name:     "fork",
			args:     map[string]any{"runId": "v1:work:run:r-source", "atStepKey": "step-4"},
			wantMode: modeFork, wantForkKey: "step-4",
		},
		{
			name:     "replay",
			args:     map[string]any{"runId": "v1:work:run:r-source"},
			wantMode: modeReplay,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			i, eng := newTestIntegration(t)
			eng.reply("workRunForOwner", sourceRun("u-alice"))

			var err error
			if tc.wantMode == modeFork {
				_, err = i.handleForkRun(callerContext("u-alice"), tc.args, 0)
			} else {
				_, err = i.handleReplayRun(callerContext("u-alice"), tc.args, 0)
			}
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}

			create := eng.callTo(t, "createWorkRun")
			args := create.Args(t)
			if args["mode"] != tc.wantMode {
				t.Errorf("mode = %v, want %v", args["mode"], tc.wantMode)
			}
			// LINEAGE. Without forkedFromRunId a derived run came from
			// nowhere and nothing could find the journal it must serve from.
			if args["forkedFromRunId"] != "v1:work:run:r-source" {
				t.Errorf("forkedFromRunId = %v", args["forkedFromRunId"])
			}
			if tc.wantForkKey != "" && args["forkAtStepKey"] != tc.wantForkKey {
				t.Errorf("forkAtStepKey = %v, want %v", args["forkAtStepKey"], tc.wantForkKey)
			}
			// The template identity is INHERITED. A derived run that
			// re-compiled would not be a replay of anything.
			if args["automationName"] != "reconcileInvoices" || args["templateFingerprint"] != "fp-abc" {
				t.Errorf("the template identity was not inherited: %v / %v", args["automationName"], args["templateFingerprint"])
			}
			if args["goalId"] != "v1:work:goal:g1" {
				t.Errorf("goalId = %v; journal serving never crosses goals, so a derived run must stay on its source's goal", args["goalId"])
			}
			// The SOURCE is untouched.
			if n := len(eng.callsTo("updateWorkRun")); n != 0 {
				t.Errorf("the source run was written to (%d updates); a fork and a replay leave it alone", n)
			}
			newId, _ := args["runId"].(string)
			if newId == "v1:work:run:r-source" || !strings.HasPrefix(newId, runConcept+":") {
				t.Errorf("derived run id %q", newId)
			}
		})
	}
}

// TestDerivedRunInheritsTheSourcesOwnerNotTheCallers.
//
// A cluster owner's fork of somebody else's run stays owned by that somebody:
// the new run belongs to the person whose work it continues, not to whoever
// pressed the button. The owner is expressed through the ACTOR, because
// ownerUserId is @serverSet.
func TestDerivedRunInheritsTheSourcesOwnerNotTheCallers(t *testing.T) {
	i, eng := newTestIntegration(t)
	eng.reply("workRunForOwner", sourceRun("u-bob"))

	if _, err := i.handleForkRun(clusterOwnerContext("u-operator"), map[string]any{
		"runId": "v1:work:run:r-source", "atStepKey": "step-2",
	}, 0); err != nil {
		t.Fatalf("forkRun: %v", err)
	}
	create := eng.callTo(t, "createWorkRun")
	if create.Actor != "u-bob" {
		t.Errorf("the derived run was written under actor %q, want the SOURCE run's owner u-bob -- the write guard ignores the clusterOwner arm, so an owned row must be written as its owner", create.Actor)
	}
	if _, present := create.Args(t)["ownerUserId"]; present {
		t.Error("createWorkRun was called with an ownerUserId argument; the field is @serverSet")
	}
}

// TestForkAndReplayRefuseWhatTheyCannotRecord.
//
// The refusals are the point. createWorkRun accepts neither `variables` nor
// `replayPolicy` today, so accepting either would run the derived work on
// values the caller did not ask for while telling them otherwise. A silent
// drop of an argument that changes what the work DOES is exactly the class of
// bug these refusals exist to prevent.
func TestForkAndReplayRefuseWhatTheyCannotRecord(t *testing.T) {
	cases := []struct {
		name     string
		run      func(i *Integration) error
		wantWord string
	}{
		{
			name: "fork with variable overrides",
			run: func(i *Integration) error {
				_, err := i.handleForkRun(callerContext("u-alice"), map[string]any{
					"runId": "v1:work:run:r-source", "atStepKey": "s", "variables": map[string]any{"x": 1},
				}, 0)
				return err
			},
			wantWord: "variables",
		},
		{
			name: "replay with a permissive policy",
			run: func(i *Integration) error {
				_, err := i.handleReplayRun(callerContext("u-alice"), map[string]any{
					"runId": "v1:work:run:r-source", "policy": "permissive",
				}, 0)
				return err
			},
			wantWord: "replayPolicy",
		},
		{
			name: "replay with an unknown policy",
			run: func(i *Integration) error {
				_, err := i.handleReplayRun(callerContext("u-alice"), map[string]any{
					"runId": "v1:work:run:r-source", "policy": "loose",
				}, 0)
				return err
			},
			wantWord: "strict or permissive",
		},
		{
			name: "fork with no divergence point",
			run: func(i *Integration) error {
				_, err := i.handleForkRun(callerContext("u-alice"), map[string]any{"runId": "v1:work:run:r-source"}, 0)
				return err
			},
			wantWord: "atStepKey",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			i, eng := newTestIntegration(t)
			eng.reply("workRunForOwner", sourceRun("u-alice"))
			err := tc.run(i)
			if err == nil {
				t.Fatal("the argument was accepted")
			}
			if !strings.Contains(err.Error(), tc.wantWord) {
				t.Errorf("error %q does not name %q, so a caller cannot tell what to do about it", err, tc.wantWord)
			}
			if n := len(eng.callsTo("createWorkRun")); n != 0 {
				t.Errorf("a run was derived anyway (%d creates)", n)
			}
		})
	}
}

// TestForkRefusesARunTheCallerCannotRead. The owned read IS the check.
func TestForkRefusesARunTheCallerCannotRead(t *testing.T) {
	i, eng := newTestIntegration(t)
	// workRunForOwner answers nothing, which is what the read gate does for
	// somebody else's run: zero rows, no error.
	_, err := i.handleForkRun(callerContext("u-mallory"), map[string]any{
		"runId": "v1:work:run:not-mine", "atStepKey": "s",
	}, 0)
	if err == nil {
		t.Fatal("forked a run that did not come back from the owned read")
	}
	if n := len(eng.callsTo("createWorkRun")); n != 0 {
		t.Errorf("a run was derived anyway (%d creates)", n)
	}
}
