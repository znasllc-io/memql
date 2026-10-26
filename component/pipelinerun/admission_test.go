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
	if allow, err := gate(testNow); allow || err == nil {
		t.Fatal("missing configured decision must hold new runs")
	}
	writeAdmission(t, path, true, testNow.Add(90*time.Minute))
	if allow, err := gate(testNow); !allow || err != nil {
		t.Fatalf("fresh allowance: %v, %v", allow, err)
	}
	writeAdmission(t, path, false, testNow.Add(90*time.Minute))
	if allow, err := gate(testNow); allow || err != nil {
		t.Fatalf("the updated hold must be read without restarting: %v, %v", allow, err)
	}
	writeAdmission(t, path, true, testNow.Add(90*time.Minute))
	if allow, err := gate(testNow.Add(90 * time.Minute)); allow || err == nil {
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
		if allow, err := gate(testNow); allow || err == nil {
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
		d.AdmitRun = func(time.Time) (bool, error) {
			t.Fatal("cancellation must not depend on admission")
			return false, nil
		}
	})
	if _, ok, err := h.integ.claim(context.Background(), h.integ.snapshot(), run.ID); !ok || err != nil {
		t.Fatalf("cancel recovery blocked: %v, %v", ok, err)
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
			for name, gate := range map[string]func(time.Time) (bool, error){"runs": runs, "cloud": cloud} {
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
