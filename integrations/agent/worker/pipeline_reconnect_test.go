//go:build agent

package worker

import (
	"testing"
	"time"

	workerservice "github.com/znasllc-io/memql/component/worker"
)

func TestPipelineWaitsOnlyForAnOtherwiseEligibleOfflineWorker(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Candidate, *Request)
		wait bool
	}{
		{"eligible", func(*Candidate, *Request) {}, true},
		{"revoked", func(c *Candidate, _ *Request) { c.RevokedAt = time.Now() }, false},
		{"missing capability", func(c *Candidate, _ *Request) { c.Capabilities = nil }, false},
		{"consent withdrawn", func(c *Candidate, _ *Request) { delete(c.Labels, PipelinesLabel) }, false},
		{"repository withdrawn", func(c *Candidate, _ *Request) { c.RepositoryScopes = nil }, false},
		{"old contract", func(c *Candidate, _ *Request) { c.ActionContracts = nil }, false},
		{"wrong native platform", func(c *Candidate, r *Request) {
			c.NativePlatform = "linux/amd64"
			r.Args["execution"], r.Args["platform"] = "native", "darwin/arm64"
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := machine("ci-box", withLabels(pipelineLabels()))
			c.ConnectedNodeId = ""
			req := pipelineRequest()
			tc.edit(&c, &req)
			f := newPipelineFleet(t, c)
			f.store.machines[0].ConnectedNodeId = ""
			result, err := f.d.Dispatch(asPipelineExecutor(), req)
			if err != nil || result.OK || !result.RefusedBeforeStart || result.RefusedByGate || result.WaitForConnection != tc.wait || f.total() != 0 {
				t.Fatalf("offline routing: %+v, err=%v, executions=%d", result, err, f.total())
			}
		})
	}
}

func TestPipelineReconnectRoutesAcrossReplicaWithoutLocalStream(t *testing.T) {
	h, ran := recordingHop(t)
	req := ownersPipelineRequest(h)
	candidate := &h.store.machines[0]
	connected := candidate.ConnectedNodeId
	candidate.ConnectedNodeId = ""
	result, err := h.dispatch.Dispatch(asPipelineAcrossTheMesh(t, h), req)
	if err != nil || !result.WaitForConnection || !result.RefusedBeforeStart || len(*ran) != 0 {
		t.Fatalf("offline result=%+v err=%v executions=%d", result, err, len(*ran))
	}
	// A fresh registration read now names the other replica. No stream or
	// remembered worker on the originating replica is needed for this retry.
	candidate.ConnectedNodeId = connected
	result, err = h.dispatch.Dispatch(asPipelineAcrossTheMesh(t, h), req)
	h.link.wg.Wait()
	if err != nil || !result.OK || result.WaitForConnection || len(*ran) != 1 || result.NodeId != nodeB {
		t.Fatalf("reconnected result=%+v err=%v executions=%d", result, err, len(*ran))
	}
	if h.dispatch.registry.WorkerById(candidate.RegistrationId) != nil {
		t.Fatal("test did not exercise the hop")
	}
}

func TestOfflinePlanStillNarrowsByEveryPipelineRequirement(t *testing.T) {
	c := machine("ci-box", withLabels(pipelineLabels()))
	c.NativePlatform = "darwin/arm64"
	p := RoutePlan{OfflineCandidates: []Candidate{c}}
	p.RequireRepository("workerHost.pipeline_step", "o/r")
	p.RequireActionContract("workerHost.pipeline_step", PipelineStepContract)
	p.RequireNativePlatform("darwin/arm64")
	if len(p.Candidates) != 0 || len(p.OfflineCandidates) != 1 {
		t.Fatalf("eligible disconnected worker: %+v", p)
	}
	p.OfflineCandidates[0].ActionContracts = workerservice.ActionContracts{}
	p.RequireActionContract("workerHost.pipeline_step", PipelineStepContract)
	if len(p.OfflineCandidates) != 0 {
		t.Fatal("withdrawn contract kept a disconnected worker eligible")
	}
}
