package pipelinerun

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeAdmission(t *testing.T, path string, allow bool, until time.Time) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"allowNewRuns": allow, "validUntil": until, "reason": "operator budget"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestFileRunAdmissionFailsClosedAndReadsEveryDecision(t *testing.T) {
	if FileRunAdmission("  ") != nil {
		t.Fatal("unset admission must preserve existing behavior")
	}
	path := filepath.Join(t.TempDir(), "admission.json")
	gate := FileRunAdmission(path)
	if allow, err := gate(testNow, false); allow || err == nil {
		t.Fatal("missing configured decision must hold new runs")
	}
	writeAdmission(t, path, true, testNow.Add(90*time.Minute))
	if allow, err := gate(testNow, false); !allow || err != nil {
		t.Fatalf("fresh allowance: %v, %v", allow, err)
	}
	writeAdmission(t, path, false, testNow.Add(90*time.Minute))
	if allow, err := gate(testNow, false); allow || err != nil {
		t.Fatalf("the updated hold must be read without restarting: %v, %v", allow, err)
	}
	writeAdmission(t, path, true, testNow.Add(90*time.Minute))
	if allow, err := gate(testNow.Add(90*time.Minute), false); allow || err == nil {
		t.Fatal("a stopped writer cannot leave an indefinite allowance")
	}
	for _, content := range []string{
		`{}`, `null`, `{"allowNewRuns":true}`, `{"allowNewRuns":"true"}`,
		`{"allowNewRuns":true,"validUntil":"bad"}`, strings.Repeat(" ", admissionMaxBytes+1),
		`{"allowNewRuns":true,"validUntil":"2099-01-01T00:00:00Z"}`,
	} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if allow, err := gate(testNow, false); allow || err == nil {
			t.Fatalf("invalid decision allowed admission: %q", content)
		}
	}
}

func TestAdmissionHoldsAcrossReplicasBeyondRunCeilingAndResumesOnce(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	path := filepath.Join(t.TempDir(), "admission.json")
	writeAdmission(t, path, false, testNow.Add(time.Hour))
	dh.integ.Configure(func(d *Deps) { d.AdmitRun = FileRunAdmission(path) })
	run := dh.openRun(t, prOpening())
	deliver(t, dh.integ, run)

	// A different process has no originating node's memory. A full month
	// passes (well beyond the execution ceiling), and the old decision expires.
	later := testNow.Add(31 * 24 * time.Hour)
	peer := dh.peer("agent-b")
	peer.Configure(func(d *Deps) {
		d.AdmitRun = FileRunAdmission(path)
		d.Now = func() time.Time { return later }
	})
	if err := peer.RecoverRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitDrives(t, peer)
	got, _ := dh.store.run(run.ID)
	if got.Status != StatusQueued || !got.StartedAt.IsZero() || got.DriverNodeID != "" || got.WorkRunID != "" || len(dh.exec.sent()) != 0 {
		t.Fatalf("held run acquired execution state: %+v", got)
	}
	writeAdmission(t, path, true, later.Add(time.Hour))
	for range 2 {
		if err := peer.RecoverRuns(context.Background()); err != nil {
			t.Fatal(err)
		}
		waitDrives(t, peer)
	}
	got, _ = dh.store.run(run.ID)
	if got.Conclusion != ConclusionSuccess || !got.StartedAt.Equal(later) || got.DriverNodeID != "agent-b" {
		t.Fatalf("released run did not complete on its new clock: %+v", got)
	}
	if len(dh.exec.sent()) != 3 || len(dh.work.callsNamed("createWorkRun")) != 1 {
		t.Fatal("resumption duplicated work")
	}
}

func TestAdmissionDoesNotPreventCancellingAnUnstartedClaim(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)
	run := queuedRun(p, shaA)
	run.DriverNodeID = "old-agent"
	run.CancelRequested = true
	h.store.addRun(run)
	h.integ.Configure(func(d *Deps) {
		d.AdmitRun = func(time.Time, bool) (bool, error) {
			t.Fatal("cancellation must not depend on admission")
			return false, nil
		}
	})
	if _, ok, err := h.integ.claim(context.Background(), h.integ.snapshot(), run.ID); !ok || err != nil {
		t.Fatalf("cancel recovery blocked: %v, %v", ok, err)
	}
}

func writeTrainAdmission(t *testing.T, path string, allowRuns, allowTrains bool, until time.Time) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"allowNewRuns": allowRuns, "allowTrainRuns": allowTrains, "validUntil": until, "reason": "train allowance"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

// The cost guard can hold new runs and still admit release trains, which run
// on a schedule and a budget of their own (design record 2026-10-27, 5.3).
func TestAdmissionAdmitsATrainRunUnderTheTrainAllowance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admission.json")
	gate := FileRunAdmission(path)
	writeTrainAdmission(t, path, false, true, testNow.Add(90*time.Minute))
	if allow, err := gate(testNow, true); !allow || err != nil {
		t.Fatalf("a train run under the train allowance: %v, %v", allow, err)
	}
	if allow, err := gate(testNow, false); allow || err != nil {
		t.Fatalf("an ordinary run under the train allowance alone was admitted: %v, %v", allow, err)
	}
}

func TestAdmissionWithoutTheTrainFieldIsUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admission.json")
	gate := FileRunAdmission(path)
	writeAdmission(t, path, false, testNow.Add(90*time.Minute))
	if allow, err := gate(testNow, true); allow || err != nil {
		t.Fatalf("a hold with no train field held a train run: %v, %v", allow, err)
	}
	writeAdmission(t, path, true, testNow.Add(90*time.Minute))
	if allow, err := gate(testNow, true); !allow || err != nil {
		t.Fatalf("an open window admits a train run too: %v, %v", allow, err)
	}
	if err := os.WriteFile(path, []byte(`{"allowNewRuns":false,"allowTrainRuns":"yes","validUntil":"`+testNow.Add(time.Hour).Format(time.RFC3339)+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if allow, err := gate(testNow, true); allow || err == nil {
		t.Fatalf("a train allowance that is not a boolean was read: %v, %v", allow, err)
	}
}

func TestClaimPassesTheTrainFact(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)
	train := queuedRun(p, shaA)
	train.HeadBranch = "train/2026-10-28-1"
	ordinary := queuedRun(p, shaB)
	ordinary.ID, ordinary.RunKey = "run-ordinary", "k-ordinary"
	h.store.addRun(train)
	h.store.addRun(ordinary)
	var asked []bool
	h.integ.Configure(func(d *Deps) {
		d.AdmitRun = func(_ time.Time, isTrain bool) (bool, error) {
			asked = append(asked, isTrain)
			return false, nil
		}
	})
	for _, id := range []string{train.ID, ordinary.ID} {
		if _, _, err := h.integ.claim(context.Background(), h.integ.snapshot(), id); err != nil {
			t.Fatal(err)
		}
	}
	if len(asked) != 2 || !asked[0] || asked[1] {
		t.Fatalf("admission was asked %v, want [true false]: a train run, then an ordinary one", asked)
	}
	if !IsTrainRun(train) || IsTrainRun(ordinary) {
		t.Errorf("IsTrainRun misreads the head branch")
	}
}

func TestSplitAdmissionKeepsCostAndOperatorHoldsSeparate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admission.json")
	runs, cloud := FileRunAdmission(path), FileCloudAdmission(path)
	if FileCloudAdmission(" ") != nil {
		t.Fatal("unset cloud admission must preserve existing installations")
	}
	for _, tc := range []struct {
		name        string
		decision    map[string]any
		runs, cloud bool
		invalid     bool
	}{
		{"legacy allow", map[string]any{"allowNewRuns": true}, true, true, false},
		{"legacy hold", map[string]any{"allowNewRuns": false}, false, false, false},
		{"cost hold", map[string]any{"version": 2, "allowNewRuns": true, "allowCloud": false}, true, false, false},
		{"operator hold", map[string]any{"version": 2, "allowNewRuns": false, "allowCloud": true}, false, false, false},
		{"allow both", map[string]any{"version": 2, "allowNewRuns": true, "allowCloud": true}, true, true, false},
		{"missing cloud", map[string]any{"version": 2, "allowNewRuns": true}, false, false, true},
		{"missing version", map[string]any{"allowNewRuns": true, "allowCloud": true}, false, false, true},
		{"unknown version", map[string]any{"version": 3, "allowNewRuns": true, "allowCloud": true}, false, false, true},
		{"null version", map[string]any{"version": nil, "allowNewRuns": true}, false, false, true},
		{"unknown field", map[string]any{"version": 2, "allowNewRuns": true, "allowCloud": true, "bypass": true}, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.decision["validUntil"] = testNow.Add(time.Hour)
			data, err := json.Marshal(tc.decision)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			for name, gate := range map[string]func(time.Time) (bool, error){"runs": func(now time.Time) (bool, error) { return runs(now, false) }, "cloud": func(now time.Time) (bool, error) { return cloud(now, false) }} {
				want := tc.runs
				if name == "cloud" {
					want = tc.cloud
				}
				got, err := gate(testNow)
				if got != want || (err != nil) != tc.invalid {
					t.Fatalf("%s: got %v/%v", name, got, err)
				}
				if got, err := gate(testNow.Add(time.Hour)); got || err == nil {
					t.Fatalf("%s accepted expired decision", name)
				}
			}
		})
	}
}

// Release trains and Fleet/cloud placement came from independent branches.
// A train allowance preserves legacy train execution under a new-run hold.
// A separate v2 cloud spending hold still wins for both classes.
func TestTrainAllowancePreservesIndependentCloudHold(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admission.json")
	runs, cloud := FileRunAdmission(path), FileCloudAdmission(path)
	for _, tc := range []struct {
		name                                       string
		decision                                   map[string]any
		ordinary, train, ordinarySpend, trainSpend bool
	}{
		{"legacy train allowance", map[string]any{"allowNewRuns": false, "allowTrainRuns": true}, false, true, false, true},
		{"legacy all held", map[string]any{"allowNewRuns": false, "allowTrainRuns": false}, false, false, false, false},
		{"train during ordinary run hold", map[string]any{"version": 2, "allowNewRuns": false, "allowTrainRuns": true, "allowCloud": true}, false, true, false, true},
		{"train during spending hold", map[string]any{"version": 2, "allowNewRuns": true, "allowTrainRuns": true, "allowCloud": false}, true, true, false, false},
		{"train only during spending hold", map[string]any{"version": 2, "allowNewRuns": false, "allowTrainRuns": true, "allowCloud": false}, false, true, false, false},
		{"both allowances", map[string]any{"version": 2, "allowNewRuns": true, "allowTrainRuns": true, "allowCloud": true}, true, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.decision["validUntil"] = testNow.Add(time.Hour)
			data, err := json.Marshal(tc.decision)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			for _, check := range []struct{ train, want bool }{{false, tc.ordinary}, {true, tc.train}} {
				if got, err := runs(testNow, check.train); got != check.want || err != nil {
					t.Fatalf("run train=%v: got %v/%v, want %v", check.train, got, err, check.want)
				}
			}
			for _, check := range []struct{ train, want bool }{{false, tc.ordinarySpend}, {true, tc.trainSpend}} {
				if got, err := cloud(testNow, check.train); got != check.want || err != nil {
					t.Fatalf("cloud train=%v: got %v/%v, want %v", check.train, got, err, check.want)
				}
			}
		})
	}
}
