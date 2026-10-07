package pipelinesteps

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"github.com/znasllc-io/memql/integrations/workbench"
)

func TestReceiptCleanupRequiresPodAbsence(t *testing.T) {
	absent := kubeStatus(404, "NotFound", "gone")
	for _, tc := range []struct {
		name      string
		podAnswer kubeAnswer
	}{
		{"terminating pod", kubeOK(200, PodList{Items: []Pod{{Metadata: ObjectMeta{Name: "still-terminating", DeletionTimestamp: time.Now()}}}})},
		{"unreadable pods", kubeStatus(503, "Unavailable", "cannot confirm")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kube, _ := newKubeFake(t, map[string]kubeAnswer{
				"POST " + kubeSecrets: kubeOK(201, Secret{}),
				"GET " + kubeSecrets + "/" + kubeJobName + retirementSuffix: kubeOK(200, Secret{Metadata: ObjectMeta{UID: "marker", Labels: map[string]string{LabelManagedBy: ManagedBy}}}),
				"GET " + kubeJobs + "/" + kubeJobName:                       absent,
				"GET " + kubeSecrets + "/" + SecretName(kubeJobName):        absent,
				"GET " + kubePods: tc.podAnswer,
			})
			r := NewRunner(Config{}, kube, nil, nil, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			if err := r.Ack(ctx, AckRequest{JobName: kubeJobName}); err == nil {
				t.Fatal("acknowledged resources without proving absence")
			}
		})
	}
}

func TestReceiptCleanupPreservesUnownedNamesake(t *testing.T) {
	h := newRunnerHarness(t)
	job := h.existingJob(t, rtRun(), nil)
	delete(job.Metadata.Labels, LabelManagedBy)
	h.c.putJob(job, rtRunningScript(testJobName))
	if err := h.r.Ack(context.Background(), AckRequest{JobName: testJobName}); err == nil {
		t.Fatal("cleanup accepted an unowned namesake")
	}
	if !h.c.hasJob(testJobName) || len(h.c.requestsFor(http.MethodDelete, testJobName)) != 0 {
		t.Fatal("cleanup deleted an unowned namesake")
	}
}

func TestReceiptCleanupReturnsFailureAndHonorsCancellation(t *testing.T) {
	wb := &scriptedWorkbench{ack: func(*exForward) (*nodev1.WorkbenchForwardResponse, string, error) {
		return nil, "", workbench.ErrNoWorkbenchPeer
	}}
	e := newTestExecutor(wb, nil)
	if err := e.AcknowledgeReceipt(context.Background(), exRequest()); err == nil {
		t.Fatal("exhausted cleanup retries returned success")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := len(wb.sent(workbench.PipelineAckAction))
	if err := e.AcknowledgeReceipt(ctx, exRequest()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled cleanup = %v", err)
	}
	if len(wb.sent(workbench.PipelineAckAction)) != before {
		t.Fatal("cancelled cleanup sent more work")
	}
}
