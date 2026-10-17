package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/integrations/workbench"
)

const (
	PurposeWorkbench    = "workbench"
	WorkbenchTool       = "workerWorkbench"
	WorkbenchCapability = "WORKBENCH"
	WorkbenchContract   = 1
)

func workbenchAction(action string) bool {
	switch action {
	case "exec", "fs_read", "fs_write", "fs_list", "fs_stat":
		return true
	}
	return false
}

// Bounded on every originating replica; the machine's two global reservations
// remain authoritative across replicas, processes and enrolled clusters.
var fleetWorkbenchQueue = struct {
	sync.Mutex
	active map[string]int
}{active: map[string]int{}}

func enterWorkbenchQueue(machine string) (func(), bool) {
	fleetWorkbenchQueue.Lock()
	defer fleetWorkbenchQueue.Unlock()
	if fleetWorkbenchQueue.active[machine] >= 32 {
		return nil, false
	}
	fleetWorkbenchQueue.active[machine]++
	return func() {
		fleetWorkbenchQueue.Lock()
		defer fleetWorkbenchQueue.Unlock()
		fleetWorkbenchQueue.active[machine]--
		if fleetWorkbenchQueue.active[machine] == 0 {
			delete(fleetWorkbenchQueue.active, machine)
		}
	}, true
}

// DispatchFleetWorkbench runs only after the workbench integration's safety and
// destination gates. It never routes a host command or grants computer access.
func (d *Dispatcher) DispatchFleetWorkbench(ctx context.Context, req workbench.FleetRequest) (workbench.FleetResult, bool) {
	fail := func(code, message string) (workbench.FleetResult, bool) {
		return workbench.FleetResult{ErrorCode: code, ErrorMessage: message}, true
	}
	store, ok := d.store.(workbenchStore)
	if !ok {
		return fail("workbench_authority_unavailable", "Fleet workbench authority is unavailable.")
	}
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac.UserId == "" {
		return fail("workbench_authority_unavailable", "A workbench requires its goal owner's authority.")
	}
	var selected *Candidate
	if workbenchAction(req.Action) {
		if c, err := admittedWorkbench(ctx, d.store, ac.UserId, ""); err == nil {
			selected = &c
		}
	}
	binding, err := store.WorkbenchBinding(ctx, ac.UserId, req.RunID, selected)
	if err != nil {
		return fail("workbench_binding_refused", err.Error())
	}
	if binding.Resource == "cloud" || req.Action == "http_fetch" {
		return workbench.FleetResult{}, false
	}
	if !workbenchAction(req.Action) {
		return fail("environment_mismatch", "This fleet workbench provides isolated files, Python and shell commands without network access. This action needs a different execution resource; ask permission before switching.")
	}
	leave, ok := enterWorkbenchQueue(binding.RegistrationID)
	if !ok {
		return fail("workbench_queue_full", "The fleet workbench queue is full. Try later or ask permission to use another execution resource.")
	}
	defer leave()
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	started := d.clock()
	for {
		cand, err := admittedWorkbench(callCtx, d.store, ac.UserId, binding.RegistrationID)
		if err != nil {
			return fail("workbench_sharing_refused", err.Error())
		}
		if cand.MachineId != binding.MachineID || !sameSubject(cand.OwnerUserId, binding.MachineOwner) {
			return fail("workbench_binding_refused", "The enrolled machine changed. Start a new goal; no work was replayed.")
		}
		request := Request{Tool: WorkbenchTool, Action: req.Action, Purpose: PurposeWorkbench, Args: req.Args, AgentId: req.AgentID, OwnerUserId: ac.UserId, RunId: req.RunID, Timeout: 10 * time.Minute}
		res, outcome := d.attempt(callCtx, request, WorkbenchCapability, cand, 10*time.Minute)
		if outcome == ForwardRefusedBeforeStart && res.ErrorCode == "workbench_capacity_busy" {
			timer := time.NewTimer(time.Second)
			select {
			case <-callCtx.Done():
				timer.Stop()
				return fail("cancelled_before_start", "The wait for fleet workbench capacity ended before execution.")
			case <-timer.C:
				continue
			}
		}
		d.recordInvocation(ctx, request, cand.RegistrationId, started, d.clock(), res, "", RoutingRecord{Attempts: 1, SelectedBy: SelectedByOnlyCandidate})
		if !res.OK {
			return fail(res.ErrorCode, res.ErrorMessage)
		}
		var payload any
		if err := json.Unmarshal([]byte(res.OutputJSON), &payload); err != nil {
			return fail("workbench_invalid_result", "The machine returned an unreadable result; the operation was not replayed.")
		}
		return workbench.FleetResult{OK: true, Payload: payload}, true
	}
}

// The receiver derives every constraint from fresh durable state AFTER capacity
// admission. Sender-supplied metadata never stands in for borrowed consent.
type workbenchAdmission struct {
	Constraint *memqlv1.WorkbenchConstraint
	Subjects   workerservice.SharingSubjects
}

func admitWorkbenchDispatch(ctx context.Context, store FleetStore, w *workerservice.Worker, req Request) (*workbenchAdmission, error) {
	authority, ok := store.(workbenchStore)
	if !ok || w == nil || req.Purpose != PurposeWorkbench || req.Tool != WorkbenchTool || !workbenchAction(req.Action) {
		return nil, fmt.Errorf("fleet workbench authority is unavailable")
	}
	if err := authority.CheckWorkbenchRun(ctx, req.OwnerUserId, req.RunId); err != nil {
		return nil, err
	}
	candidate, err := admittedWorkbench(ctx, store, req.OwnerUserId, w.RegistrationId)
	if err != nil {
		return nil, err
	}
	binding, err := authority.WorkbenchBinding(ctx, req.OwnerUserId, req.RunId, nil)
	if err != nil {
		return nil, err
	}
	if binding.Resource != "fleet" || !sameSubject(binding.RegistrationID, w.RegistrationId) || binding.MachineID != candidate.MachineId || !sameSubject(binding.MachineOwner, w.OwnerUserId) || !sameSubject(candidate.OwnerUserId, w.OwnerUserId) || w.CapabilityDescriptor == nil || !w.CapabilityDescriptor.ActionContracts.Supports(WorkbenchTool, WorkbenchContract) {
		return nil, fmt.Errorf("fleet workbench does not match its goal's enrolled machine")
	}
	subjects := workerservice.SharingSubjects{}
	if !sameSubject(req.OwnerUserId, w.OwnerUserId) {
		subjects = candidate.WorkbenchSharing.SubjectsFor(workerservice.NewPerson(ctx, req.OwnerUserId, nil))
	}
	return &workbenchAdmission{Subjects: subjects, Constraint: &memqlv1.WorkbenchConstraint{OwnerUserId: w.OwnerUserId, CallerUserId: req.OwnerUserId, RegistrationId: w.RegistrationId, MachineId: candidate.MachineId, RunId: req.RunId, ExpiresUnix: time.Now().Add(time.Minute).Unix(), WorkspaceExpiresUnix: binding.Expires.Unix()}}, nil
}
func workbenchRefusedBeforeStart(purpose string, result Result, err error) bool {
	return purpose == PurposeWorkbench && err == nil && !result.OK && (result.ErrorCode == "workbench_capacity_busy" || result.ErrorCode == "workbench_not_enabled" || result.ErrorCode == "workbench_constraint_refused")
}
