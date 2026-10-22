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
