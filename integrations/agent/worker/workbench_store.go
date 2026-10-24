package worker

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/core/id"
)

const workbenchBindingConcept = "v1:workbench:executionBinding"
const workbenchLifetime = time.Hour

type workbenchBinding struct {
	Resource, RegistrationID, MachineID, MachineOwner, RunID, Caller string
	Expires                                                          time.Time
	ID                                                               string
	Version                                                          time.Time
	Generation                                                       uint64
	StateLost                                                        bool
}
type workbenchStore interface {
	CheckWorkbenchRun(context.Context, string, string) error
	WorkbenchBinding(context.Context, string, string, *Candidate) (workbenchBinding, error)
	AdvanceWorkbenchBinding(context.Context, string, string, workbenchBinding, string) (workbenchBinding, error)
}

func (s *EngineStore) CheckWorkbenchRun(ctx context.Context, owner, runID string) error {
	if s == nil || s.Engine == nil {
		return fmt.Errorf("workbench authority is unavailable")
	}
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || owner == "" || !sameSubject(ac.UserId, owner) {
		return fmt.Errorf("workbench requires its run owner's authority")
	}
	if err := s.CheckExecutionDestination(ctx, runID, "workbench"); err != nil {
		return err
	}
	call, _ := langparser.RenderCall("workRunForOwner", map[string]any{"runId": runID})
	res, err := s.Engine.Execute(memqlengine.ContextWithFreshRead(ctx), "query "+call)
	if err != nil {
		return err
	}
	if res == nil {
		return fmt.Errorf("workbench run is unavailable")
	}
	rows := memqlengine.MaterializeRows(res.OutputPayload())
	if len(rows) != 1 || rows[0]["status"] != "running" || rows[0]["cancelRequested"] == true {
		return fmt.Errorf("workbench run is no longer active")
	}
	return nil
}

// Placement is created with an absence fence. Only the native lifecycle below
// can renew its lease or explicitly advance a lost ephemeral generation.
func (s *EngineStore) WorkbenchBinding(ctx context.Context, owner, runID string, selected *Candidate) (workbenchBinding, error) {
	if err := s.CheckWorkbenchRun(ctx, owner, runID); err != nil {
		return workbenchBinding{}, err
	}
	read, _ := langparser.RenderCall("workbenchExecutionForRun", map[string]any{"runId": runID})
	for attempt := 0; attempt < 3; attempt++ {
		res, err := s.Engine.Execute(memqlengine.ContextWithFreshRead(ctx), "query "+read)
		if err != nil {
			return workbenchBinding{}, err
		}
		if res != nil {
			rows := memqlengine.MaterializeRows(res.OutputPayload())
			if len(rows) > 0 {
				return decodeWorkbenchBinding(rows[0], owner, runID)
			}
		}
		binding := workbenchBinding{Resource: "cloud", Caller: owner, RunID: runID, Expires: time.Now().Add(workbenchLifetime)}
		// A workspace created before this routing contract continues on its cloud
		// node. A rollout must not turn an existing file tree into a new empty one.
		cloudRead, _ := langparser.RenderCall("workspaceForRun", map[string]any{"runId": runID})
		cloud, err := s.Engine.Execute(memqlengine.ContextWithFreshRead(ctx), "query "+cloudRead)
		if err != nil {
			return workbenchBinding{}, err
		}
		cloudExists := cloud != nil && len(memqlengine.MaterializeRows(cloud.OutputPayload())) > 0
		if selected != nil && !cloudExists {
			binding.Resource = "fleet"
			binding.RegistrationID = selected.RegistrationId
			binding.MachineID = selected.MachineId
			binding.MachineOwner = selected.OwnerUserId
		}
		key := string(id.New().MustFromMap(map[string]any{"concept": workbenchBindingConcept, "owner": memqlengine.BareShortId(owner), "run": memqlengine.BareShortId(runID)}))[:32]
		call, err := langparser.RenderCall("bindWorkbenchExecution", map[string]any{"bindingId": key, "runId": runID, "resource": binding.Resource, "registrationId": binding.RegistrationID, "machineId": binding.MachineID, "machineOwnerUserId": binding.MachineOwner, "expiresAt": binding.Expires.UTC().Format(time.RFC3339Nano)})
		if err != nil {
			return workbenchBinding{}, err
		}
		fenced := memqlengine.ContextWithRowVersionFence(auth.ContextWithInternalOrigin(ctx), workbenchBindingConcept, key, time.Time{})
		if _, err = s.Engine.Execute(fenced, call); errors.Is(err, memqlengine.ErrRowVersionChanged) {
			continue
		} else if err != nil {
			return workbenchBinding{}, err
		}
		return binding, nil
	}
	return workbenchBinding{}, memqlengine.ErrRowVersionChanged
}
func decodeWorkbenchBinding(row map[string]any, owner, runID string) (workbenchBinding, error) {
	b := workbenchBinding{ID: scopeString(row["id"]), Resource: scopeString(row["resource"]), RegistrationID: scopeString(row["registrationId"]), MachineID: scopeString(row["machineId"]), MachineOwner: scopeString(row["machineOwnerUserId"]), Caller: scopeString(row["ownerUserId"]), RunID: scopeString(row["runId"])}
	switch at := row["expiresAt"].(type) {
	case time.Time:
		b.Expires = at
	case string:
		b.Expires, _ = time.Parse(time.RFC3339Nano, at)
	}
	b.Version = workbenchTime(row["createdAt"])
	if v := row["generation"]; v != nil {
		var err error
		b.Generation, err = strconv.ParseUint(fmt.Sprint(v), 10, 53)
		if err != nil {
			return b, fmt.Errorf("invalid workbench generation")
		}
	}
	if v := row["stateLost"]; v != nil {
		var ok bool
		b.StateLost, ok = v.(bool)
		if !ok {
			return b, fmt.Errorf("invalid workbench state")
		}
	}
	if !sameSubject(b.Caller, owner) || !sameSubject(b.RunID, runID) || (b.Resource != "fleet" && b.Resource != "cloud") {
		return b, fmt.Errorf("workbench execution binding is invalid")
	}
	if b.Resource == "fleet" && (b.RegistrationID == "" || b.MachineID == "" || b.MachineOwner == "" || b.Expires.IsZero()) {
		return b, fmt.Errorf("fleet workbench binding is incomplete")
	}
	return b, nil
}

func workbenchTime(value any) time.Time {
	if at, ok := value.(time.Time); ok {
		return at
	}
	at, _ := time.Parse(time.RFC3339Nano, scopeString(value))
	return at
}

// AdvanceWorkbenchBinding re-reads authority and placement, then fences the
// exact generation. A stale recovery cannot replace a newer filesystem.
func (s *EngineStore) AdvanceWorkbenchBinding(ctx context.Context, owner, runID string, expected workbenchBinding, action string) (workbenchBinding, error) {
	for attempt := 0; attempt < 3; attempt++ {
		b, err := s.WorkbenchBinding(ctx, owner, runID, nil)
		if err != nil {
			return b, err
		}
		if b.Resource != "fleet" || b.Generation != expected.Generation || b.RegistrationID != expected.RegistrationID || b.MachineID != expected.MachineID || !sameSubject(b.MachineOwner, expected.MachineOwner) || b.ID == "" || b.Version.IsZero() {
			return b, fmt.Errorf("workbench generation or placement changed; inspect its current state")
		}
		// A failed receiver receipt may be recorded after sharing was revoked.
		// Renewal/recovery, however, always need fresh sharing and enrollment.
		if action != "lost" {
			c, err := admittedWorkbench(ctx, s, owner, b.RegistrationID)
			if err != nil {
				return b, err
			}
			if c.MachineId != b.MachineID || !sameSubject(c.OwnerUserId, b.MachineOwner) {
				return b, fmt.Errorf("the workbench's enrolled machine changed")
			}
		}
		now := time.Now().UTC()
		next, err := nextWorkbenchBinding(b, action, now)
		if err != nil {
			return b, err
		}
		if next == b {
			return b, nil
		}
		version := now
		if !version.After(b.Version) {
			version = b.Version.Add(time.Microsecond)
		}
		call, err := langparser.RenderCall("advanceWorkbenchExecution", map[string]any{"bindingId": b.ID, "expiresAt": next.Expires.UTC().Format(time.RFC3339Nano), "generation": next.Generation, "stateLost": next.StateLost, "versionTime": version.Format(time.RFC3339Nano)})
		if err != nil {
			return b, err
		}
		fenced := memqlengine.ContextWithRowVersionFence(auth.ContextWithInternalOrigin(ctx), workbenchBindingConcept, b.ID, b.Version)
		if _, err = s.Engine.Execute(fenced, call); errors.Is(err, memqlengine.ErrRowVersionChanged) {
			continue
		} else if err != nil {
			return b, err
		}
		next.Version = version
		return next, nil
	}
	return workbenchBinding{}, memqlengine.ErrRowVersionChanged
}

func nextWorkbenchBinding(b workbenchBinding, action string, now time.Time) (workbenchBinding, error) {
	switch action {
	case "lost":
		b.StateLost = true
	case "renew":
		if b.StateLost || !now.Before(b.Expires) {
			return b, fmt.Errorf("workbench generation %d is unavailable; use workspace_recover with that generation; no operation was replayed", b.Generation)
		}
		if b.Expires.Sub(now) < workbenchLifetime/2 {
			b.Expires = now.Add(workbenchLifetime)
		}
	case "recover":
		if !b.StateLost && now.Before(b.Expires) {
			return b, fmt.Errorf("the current workspace has not reported state loss; recovery would discard live files")
		}
		if b.Generation >= 1<<53-1 {
			return b, fmt.Errorf("workbench generation is exhausted")
		}
		b.Generation++
		b.StateLost = false
		b.Expires = now.Add(workbenchLifetime)
	default:
		return b, fmt.Errorf("unknown workbench lifecycle action")
	}
	return b, nil
}

// Fresh consent is independent of inference sharing. Group membership is read
// from the installed source on the receiving replica, without a cache.
func admittedWorkbench(ctx context.Context, store FleetStore, owner, registration string) (Candidate, error) {
	if store == nil {
		return Candidate{}, fmt.Errorf("fleet workbench consent store is unavailable")
	}
	ctx = memqlengine.ContextWithFreshRead(ctx)
	own, err := store.WorkersForOwner(ctx, owner)
	if err != nil {
		return Candidate{}, err
	}
	all := own
	if shared, ok := store.(interface {
		SharedInferenceWorkers(context.Context) ([]Candidate, error)
	}); ok {
		rows, readErr := shared.SharedInferenceWorkers(ctx)
		if readErr != nil && registration != "" {
			found := false
			for _, c := range own {
				if sameSubject(c.RegistrationId, registration) {
					found = true
				}
			}
			if !found {
				return Candidate{}, readErr
			}
		}
		all = append(all, rows...)
	}
	person := workerservice.NewPerson(ctx, owner, nil)
	for _, c := range all {
		if registration != "" && !sameSubject(c.RegistrationId, registration) {
			continue
		}
		if c.MachineId == "" || c.OwnerUserId == "" || !workerservice.StreamHeld(c.ConnectedNodeId, c.RevokedAt) || !c.SupportsCapability(WorkbenchCapability) || !c.ActionContracts.Supports(WorkbenchTool, WorkbenchContract) {
			continue
		}
		if sameSubject(c.OwnerUserId, owner) || (c.WorkbenchSharing.Mode == workerservice.SharingModePeople && c.WorkbenchSharing.Admits(person)) {
			return c, nil
		}
	}
	return Candidate{}, fmt.Errorf("no connected fleet workbench is available under current sharing permissions")
}
