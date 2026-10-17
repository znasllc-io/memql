//go:build agent

package worker

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/integrations/workbench"
)

type workbenchHopStore struct {
	*fakeStore
	mu                sync.Mutex
	candidate         Candidate
	binding           workbenchBinding
	paused, cancelled bool
	read              chan struct{}
}

func (s *workbenchHopStore) WorkersForOwner(context.Context, string) ([]Candidate, error) {
	return nil, nil
}
func (s *workbenchHopStore) SharedInferenceWorkers(context.Context) ([]Candidate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.candidate
	c.WorkbenchSharing.Paused = s.paused
	if s.read != nil {
		select {
		case s.read <- struct{}{}:
		default:
		}
	}
	return []Candidate{c}, nil
}
func (s *workbenchHopStore) CheckWorkbenchRun(_ context.Context, owner, run string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancelled || !sameSubject(owner, s.binding.Caller) || run != s.binding.RunID {
		return fmt.Errorf("goal is not active")
	}
	return nil
}
func (s *workbenchHopStore) WorkbenchBinding(ctx context.Context, owner, run string, _ *Candidate) (workbenchBinding, error) {
	if err := s.CheckWorkbenchRun(ctx, owner, run); err != nil {
		return workbenchBinding{}, err
	}
	return s.binding, nil
}

func newWorkbenchHop(t *testing.T, call workerservice.DispatchFunc) (*Dispatcher, *workbenchHopStore, *workerservice.Worker) {
	t.Helper()
	candidate := machine("shared-workbench")
	candidate.OwnerUserId = sharedHopOwner
	candidate.MachineId = "physical-machine"
	candidate.ConnectedNodeId = nodeB
	candidate.Capabilities = []string{workerservice.CapabilityHeadless, WorkbenchCapability}
	candidate.ActionContracts = workerservice.ActionContracts{WorkbenchTool: WorkbenchContract}
	candidate.WorkbenchSharing = workerservice.Sharing{Mode: workerservice.SharingModePeople, UserIds: []string{sharedHopCaller}}
	store := &workbenchHopStore{fakeStore: &fakeStore{fakeFleet: &fakeFleet{}}, candidate: candidate, binding: workbenchBinding{Resource: "fleet", RegistrationID: candidate.RegistrationId, MachineID: candidate.MachineId, MachineOwner: sharedHopOwner, Caller: sharedHopCaller, RunID: "goal-run", Expires: time.Now().Add(time.Hour)}}
	reg := workerservice.NewRegistry(testLogger(), fleetNow)
	w := &workerservice.Worker{RegistrationId: candidate.RegistrationId, OwnerUserId: sharedHopOwner, Capabilities: candidate.Capabilities, Concurrency: map[string]uint32{WorkbenchCapability: 1}, CapabilityDescriptor: &workerservice.CapabilityDescriptor{ActionContracts: candidate.ActionContracts}}
	w.SetDispatchFunc(call, func() {})
	reg.Add(w)
	link := &meshLink{t: t, reachable: true}
	link.handler = NewForwardHandler(reg, store, testLogger())
	link.router = newForwardRouter(link, func() (string, string) { return nodeA, "agent" }, testLogger())
	d, err := NewDispatcher(Options{Logger: testLogger(), Store: store, Registry: workerservice.NewRegistry(testLogger(), fleetNow), SelfNodeId: nodeA, Remote: link.router})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { link.wg.Wait() })
	return d, store, w
}
func TestBorrowedWorkbenchCrossesReplicaWithoutOpeningHostTools(t *testing.T) {
	var calls atomic.Int32
	d, store, _ := newWorkbenchHop(t, func(_ context.Context, dispatch *memqlv1.ToolDispatch, _ func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
		c := dispatch.GetWorkbench()
		if c == nil || c.GetOwnerUserId() != sharedHopOwner || c.GetCallerUserId() != sharedHopCaller || c.GetMachineId() != "physical-machine" || c.GetRunId() != "goal-run" || dispatch.GetComputerPermission() != nil {
			t.Errorf("invalid receiver-issued constraint: %v", dispatch)
		}
		calls.Add(1)
		return okResult(dispatch.CallId), nil
	})
	req := workbench.FleetRequest{RunID: "goal-run", Action: "fs_list", Args: map[string]any{"path": "."}}
	result, handled := d.DispatchFleetWorkbench(authorityCtx(t, sharedHopCaller), req)
	if !handled || !result.OK || calls.Load() != 1 {
		t.Fatalf("shared workbench did not cross replica: %+v", result)
	}
	store.mu.Lock()
	store.paused = true
	store.mu.Unlock()
	result, _ = d.DispatchFleetWorkbench(authorityCtx(t, sharedHopCaller), req)
	if result.OK || calls.Load() != 1 {
		t.Fatal("paused sharing admitted a new call")
	}
	store.mu.Lock()
	store.paused = false
	store.mu.Unlock()
	result, _ = d.DispatchFleetWorkbench(authorityCtx(t, sharedHopCaller), req)
	if !result.OK || calls.Load() != 2 {
		t.Fatalf("resume did not re-admit: %+v", result)
	}
	for _, tool := range []string{"workerHost", "workerComputer"} {
		if code, _ := purposeBinding(PurposeWorkbench, tool, "exec", ""); code == "" {
			t.Fatal("workbench purpose widened to host tools")
		}
	}
	if code, _ := purposeBinding("", WorkbenchTool, "exec", ""); code == "" {
		t.Fatal("host purpose admitted workbench without its own authority")
	}
}
func TestQueuedWorkbenchRechecksSharingOnReceivingReplica(t *testing.T) {
	var calls atomic.Int32
	d, store, w := newWorkbenchHop(t, func(_ context.Context, dispatch *memqlv1.ToolDispatch, _ func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
		calls.Add(1)
		return okResult(dispatch.CallId), nil
	})
	if err := w.Acquire(context.Background(), WorkbenchCapability); err != nil {
		t.Fatal(err)
	}
	store.read = make(chan struct{}, 16)
	done := make(chan Result, 1)
	go func() {
		r, _, _ := d.remote.ForwardDispatch(authorityCtx(t, sharedHopCaller), nodeB, Request{Tool: WorkbenchTool, Action: "fs_list", Purpose: PurposeWorkbench, OwnerUserId: sharedHopCaller, RunId: "goal-run", Args: map[string]any{"path": "."}}, store.candidate.RegistrationId, WorkbenchCapability, 2*time.Second)
		done <- r
	}()
	select {
	case <-store.read:
	case <-time.After(time.Second):
		t.Fatal("receiver did not read consent")
	}
	store.mu.Lock()
	store.paused = true
	store.mu.Unlock()
	w.Release(WorkbenchCapability)
	select {
	case result := <-done:
		if result.OK || calls.Load() != 0 {
			t.Fatalf("queued work ignored revoked sharing: %+v", result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued work did not finish")
	}
}
