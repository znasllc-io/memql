package server

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/bus"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// AutomationLister loads automation definitions for validation (e.g., checking
// if an automation exists and is enabled before triggering).
type AutomationLister interface {
	LoadAll() ([]*automations.Automation, error)
	LoadByName(name string) (*automations.Automation, error)
}

// AutomationTrigger allows triggering automations by name.
type AutomationTrigger interface {
	TriggerAutomation(ctx context.Context, name string) (*automations.AutomationExecution, error)
	TriggerAutomationWithEvent(ctx context.Context, name string, event *events.Event) (*automations.AutomationExecution, error)
	GetAutomations() []*automations.Automation
	ResumeAutomation(ctx context.Context, executionId string, fromStep string) (*automations.AutomationExecution, error)
}

type (
	Server struct {
		*NetHTTP
		automationScheduler AutomationTrigger
		automationLoader    AutomationLister
		memoryEngine        *memql.MemQLEngine
		wiring              *bus.Wiring
	}
)

const (
	ComponentName = common.ComponentName("server")
)

var (
	loadServerEnvOptionsFn = loadDefaultServerEnvOptions
	envOptionsToArgsFn     = EnvOptionsToArgs
)

func NewServer(args ...ServerArg) (*Server, error) {
	envArgs, err := loadServerArgs()
	if err != nil {
		return nil, err
	}

	srv := &Server{}

	argList := append([]ServerArg{}, envArgs...)
	argList = append(argList, WithStrictServer(srv))
	argList = append(argList, args...)

	netHTTP, err := NewNetHTTP(ComponentName, argList...)
	if err != nil {
		return nil, err
	}

	srv.NetHTTP = netHTTP
	return srv, nil
}

func loadServerArgs() ([]ServerArg, error) {
	opts, err := loadServerEnvOptionsFn()
	if err != nil {
		return nil, err
	}
	return envOptionsToArgsFn(opts)
}

func loadDefaultServerEnvOptions() (ServerEnvOptions, error) {
	return LoadServerEnvOptions(ServerEnvLoader{Prefix: serverEnvPrefix})
}

func (*Server) GetHealthz(ctx context.Context, request GetHealthzRequestObject) (GetHealthzResponseObject, error) {
	_ = ctx
	_ = request
	return buildHealthResponse(), nil
}

func (s *Server) SetAutomationScheduler(scheduler AutomationTrigger) {
	if s == nil {
		return
	}
	s.automationScheduler = scheduler
}

func (s *Server) SetAutomationLoader(loader AutomationLister) {
	if s == nil {
		return
	}
	s.automationLoader = loader
}

func (s *Server) SetMemoryEngine(engine *memql.MemQLEngine) {
	if s == nil {
		return
	}
	s.memoryEngine = engine
}

func (s *Server) PostAutomationTrigger(ctx context.Context, request PostAutomationTriggerRequestObject) (PostAutomationTriggerResponseObject, error) {
	name := request.AutomationName
	if name == "" {
		return PostAutomationTrigger404JSONResponse{
			Error: "automation name required",
		}, nil
	}

	if s == nil || s.automationScheduler == nil {
		return PostAutomationTrigger404JSONResponse{
			Error: fmt.Sprintf("automation %q not found", name),
		}, nil
	}

	// First, check if the automation exists and is enabled using the loader
	// This allows us to return proper 404/409 before attempting to trigger
	if s.automationLoader != nil {
		automation, loadErr := s.automationLoader.LoadByName(name)
		if loadErr != nil {
			return PostAutomationTrigger404JSONResponse{
				Error: fmt.Sprintf("automation %q not found", name),
			}, nil
		}
		if automation != nil && !automation.IsEnabled() {
			return PostAutomationTrigger409JSONResponse{
				Error: fmt.Sprintf("automation %q is disabled", name),
			}, nil
		}
	}

	// Try to trigger the automation
	// TriggerAutomation executes asynchronously-ish but returns an execution object
	// Even if execution fails, we return 202 since the automation was found and triggered
	execution, err := s.automationScheduler.TriggerAutomation(ctx, name)
	if err != nil {
		// Check if this is a "not found" error from the scheduler (automation not in its map)
		if execution == nil {
			return PostAutomationTrigger404JSONResponse{
				Error: fmt.Sprintf("automation %q not found", name),
			}, nil
		}
		// Automation was found and started, but execution had an error
		// Still return 202 - the automation was triggered (execution errors are logged separately)
	}

	// Return 202 - automation was found and triggered
	// Check execution status to return appropriate response
	execId := ""
	initialChainHead := ""
	chainHead := ""
	status := "queued"
	message := "Automation triggered"

	if execution != nil {
		execId = execution.ID
		initialChainHead = execution.InitialChainHead
		chainHead = execution.ChainHead

		// If execution was skipped (e.g., dedup detected duplicate), report honestly
		if execution.Status == "skipped" {
			status = "skipped"
			if execution.Error != "" {
				message = execution.Error
			} else {
				message = "Execution skipped"
			}
		}
	}

	return PostAutomationTrigger202JSONResponse{
		ExecutionId:      execId,
		Status:           status,
		Message:          message,
		InitialChainHead: initialChainHead,
		ChainHead:        chainHead,
	}, nil
}

// PostAutomationResume resumes a failed automation from a checkpoint.
func (s *Server) PostAutomationResume(ctx context.Context, request PostAutomationResumeRequestObject) (PostAutomationResumeResponseObject, error) {
	if request.Body == nil {
		return PostAutomationResume400JSONResponse{
			Error: "request body required",
		}, nil
	}

	// AUTHORIZATION (memql#2908). Resuming re-executes a checkpoint's
	// remaining steps through the PRODUCTION step registry, under the
	// automation's system actor -- ResumeFrom calls contextWithSystemActor,
	// which REPLACES whatever caller identity reached this handler. So the
	// caller's identity is not merely unchecked downstream; it is discarded.
	// That makes resume an operator action, and it is gated as one, matching
	// component/grpc/node_maintenance_handlers.go's owner-or-admin gate.
	//
	// The check lives HERE, not only in middleware, and that is deliberate
	// (memql#2937): the identity binary skips the verifier middleware
	// entirely (app/config.go's early return on !verifierRequired) while
	// still registering these routes against a real scheduler, so a
	// middleware-only gate leaves the behaviour dependent on per-binary
	// wiring. With no AccessContext there is no actor, isOwnerOrAdmin is
	// false, and this fails closed on every tier.
	//
	// Per-execution ownership ("you may resume what YOU triggered") is not
	// expressible today: the checkpoint records triggerContext.triggeredBy as
	// a KIND ("schedule" / "event" / "manual"), and the row's createdBy is
	// the automation's own system actor, not the human. Narrowing this to the
	// triggering principal needs a field first -- noted on memql#2908.
	if ac, _ := auth.AccessFromContext(ctx); !isOwnerOrAdmin(ac) {
		return PostAutomationResume403JSONResponse{
			Error: "resuming an automation execution requires the owner or admin role",
		}, nil
	}

	executionId := request.Body.ExecutionId
	if executionId == "" {
		return PostAutomationResume400JSONResponse{
			Error: "executionId is required",
		}, nil
	}

	if s == nil || s.automationScheduler == nil {
		return PostAutomationResume404JSONResponse{
			Error: "automation scheduler not configured",
		}, nil
	}

	// Resume the automation
	execution, err := s.automationScheduler.ResumeAutomation(ctx, executionId, request.Body.FromStep)
	if err != nil {
		// Map specific errors to HTTP status codes
		errMsg := err.Error()

		// Check for checkpoint not found
		if containsError(errMsg, "checkpoint not found") {
			return PostAutomationResume404JSONResponse{
				Error: fmt.Sprintf("checkpoint for execution %q not found", executionId),
			}, nil
		}

		// Check for checkpoint expired
		if containsError(errMsg, "expired") {
			return PostAutomationResume410JSONResponse{
				Error: fmt.Sprintf("checkpoint for execution %q has expired", executionId),
			}, nil
		}

		// Check for automation definition changed
		if containsError(errMsg, "definition changed") || containsError(errMsg, "automation changed") {
			return PostAutomationResume409JSONResponse{
				Error: "automation definition has changed since checkpoint was saved",
			}, nil
		}

		// Check for non-retryable step error
		if containsError(errMsg, "not safely retryable") || containsError(errMsg, "side effect") {
			return PostAutomationResume400JSONResponse{
				Error: errMsg,
			}, nil
		}

		// Generic error - return as 400
		return PostAutomationResume400JSONResponse{
			Error: errMsg,
		}, nil
	}

	// Calculate skipped steps (steps restored from checkpoint)
	stepsSkipped := 0
	resumedFrom := ""
	if execution != nil {
		// Count steps that were in the checkpoint (before resume point)
		for _, step := range execution.Steps {
			// Steps restored from checkpoint won't have Duration set
			if step.Duration == 0 && step.Status == "success" {
				stepsSkipped++
			}
		}
		// Get the step we resumed from
		if request.Body.FromStep != "" {
			resumedFrom = request.Body.FromStep
		}
	}

	execId := ""
	chainHead := ""
	status := "running"

	if execution != nil {
		execId = execution.ID
		chainHead = execution.ChainHead
		if execution.Status != "" {
			status = execution.Status
		}
	}

	return PostAutomationResume202JSONResponse{
		ExecutionId:  execId,
		ResumedFrom:  resumedFrom,
		StepsSkipped: stepsSkipped,
		Status:       status,
		ChainHead:    chainHead,
	}, nil
}

// containsError checks if an error message contains a substring (case-insensitive).
func containsError(errMsg, substr string) bool {
	return len(errMsg) > 0 && len(substr) > 0 &&
		(errMsg == substr || len(errMsg) > len(substr) &&
			(errMsg[:len(substr)] == substr || errMsg[len(errMsg)-len(substr):] == substr ||
				findSubstring(errMsg, substr)))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func (s *Server) Order() int {
	if s == nil || s.NetHTTP == nil {
		return 100
	}
	return s.NetHTTP.Order()
}

func (s *Server) ComponentName() common.ComponentName {
	return ComponentName
}

// isOwnerOrAdmin reports whether the resolved AccessContext is a cluster owner
// or admin. Mirrors component/grpc/node_maintenance_handlers.go's gate for
// operator actions.
//
// NIL IS FALSE, and that is the load-bearing property (memql#2908, #2937): on a
// node that installs no auth middleware there is no AccessContext at all, and
// this must refuse rather than wave the request through.
func isOwnerOrAdmin(ac *auth.AccessContext) bool {
	if ac == nil {
		return false
	}
	return ac.Role == auth.RoleOwner || ac.Role == auth.RoleAdmin
}
