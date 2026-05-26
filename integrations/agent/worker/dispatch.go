//go:build agent

// Package worker (agent-side) bridges the agent's tool loop to the
// worker subsystem. It owns:
//
//   - the `workerHost` and `workerComputer` tool handlers,
//   - the dispatch path that routes a tool call from the agent's
//     tool loop to the in-memory Registry on the same node and back,
//   - the Q9 scope check and Q13 kill-switch check,
//   - the per-call telemetry write to v1:worker:invocation,
//   - the audit emission for denied / scope-elevation events.
//
// All worker integration code lives under //go:build agent so the
// other node types (bff, voice, cognition, planner) compile without
// the dependency.
package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/safety"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"google.golang.org/protobuf/types/known/durationpb"
)

// Dispatcher is the agent-side bridge. NewDispatcher wires it up
// from the worker.Service registry + the engine + an audit logger.
type Dispatcher struct {
	logger   *slog.Logger
	registry *workerservice.Registry
	engine   *memqlengine.MemQLEngine
	auditor  workerservice.Auditor
	store    Store
	clock    func() time.Time
}

// Store is the persistence side of the dispatcher (resolve agent
// authorization, plan, user; write invocations).
type Store interface {
	UserPreferences(ctx context.Context, userId string) (Preferences, error)
	AgentAuthorization(ctx context.Context, agentId, ownerUserId string) (*Authorization, error)
	PlanScope(ctx context.Context, planId string) (string, error)
	WriteInvocation(ctx context.Context, row workerservice.InvocationRow) error
}

// Preferences carries the dispatch-relevant user preference fields.
type Preferences struct {
	ComputerUseEnabled bool
}

// Authorization carries the dispatch-relevant agentAuthorization
// row fields.
type Authorization struct {
	ID               string
	AgentId          string
	UserId           string
	ComputerUseScope string
}

// Result is the wire-shape returned to the calling tool loop.
type Result struct {
	OK            bool
	OutputJSON    string
	ErrorCode     string
	ErrorMessage  string
	BytesIn       int
	BytesOut      int
	OutputPreview string
}

// Request carries the inputs from the agent tool loop.
type Request struct {
	Tool          string
	Action        string
	Args          map[string]any
	AgentId       string
	OwnerUserId   string
	PlanId        string
	TaskId        string
	CorrelationId string
	Timeout       time.Duration
}

// Options configures NewDispatcher.
type Options struct {
	Logger   *slog.Logger
	Registry *workerservice.Registry
	Engine   *memqlengine.MemQLEngine
	Auditor  workerservice.Auditor
	Store    Store
	Clock    func() time.Time
}

// NewDispatcher constructs a worker dispatcher.
func NewDispatcher(opts Options) (*Dispatcher, error) {
	if opts.Registry == nil {
		return nil, errors.New("agent.worker: registry required")
	}
	if opts.Logger == nil {
		return nil, errors.New("agent.worker: logger required")
	}
	clock := opts.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &Dispatcher{
		logger:   opts.Logger,
		registry: opts.Registry,
		engine:   opts.Engine,
		auditor:  opts.Auditor,
		store:    opts.Store,
		clock:    clock,
	}, nil
}

// Dispatch is the single entry point. Called from the tool loop
// when a workerHost / workerComputer tool resolves.
func (d *Dispatcher) Dispatch(ctx context.Context, req Request) (Result, error) {
	if d == nil {
		return Result{}, errors.New("agent.worker: dispatcher not initialized")
	}
	startedAt := d.clock()

	gate := d.preDispatchCheck(ctx, req)
	if gate.deny {
		d.recordInvocation(ctx, req, "", startedAt, d.clock(), Result{
			OK:           false,
			ErrorCode:    gate.errorCode,
			ErrorMessage: gate.errorMessage,
		}, gate.outcome)
		d.emitDenied(ctx, req, gate)
		return Result{
			OK:           false,
			ErrorCode:    gate.errorCode,
			ErrorMessage: gate.errorMessage,
		}, nil
	}

	worker, err := d.pickWorker(req, gate.requiredCapability)
	if err != nil {
		d.recordInvocation(ctx, req, "", startedAt, d.clock(), Result{
			OK:           false,
			ErrorCode:    "no_worker_available",
			ErrorMessage: err.Error(),
		}, "no_worker_available")
		return Result{OK: false, ErrorCode: "no_worker_available", ErrorMessage: err.Error()}, nil
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = workerservice.DispatchTimeoutDefault
	}
	dispatchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := worker.Acquire(dispatchCtx, gate.requiredCapability); err != nil {
		d.recordInvocation(ctx, req, worker.RegistrationId, startedAt, d.clock(), Result{
			OK:           false,
			ErrorCode:    "worker_busy",
			ErrorMessage: err.Error(),
		}, "failure")
		return Result{OK: false, ErrorCode: "worker_busy", ErrorMessage: err.Error()}, nil
	}
	defer worker.Release(gate.requiredCapability)

	envelope := buildToolDispatch(req, timeout)
	res, err := worker.Dispatch(dispatchCtx, envelope)
	completedAt := d.clock()

	wireRes := translateResult(envelope.GetCallId(), res, err)
	outcome := wireRes.classifyOutcome()
	d.recordInvocation(ctx, req, worker.RegistrationId, startedAt, completedAt, wireRes, outcome)

	return wireRes, nil
}

type gateResult struct {
	deny               bool
	requiredCapability string
	requiredScope      string
	errorCode          string
	errorMessage       string
	outcome            string
	// classification carries the safety-classifier verdict on the
	// `denied_by_classifier` path so emitDenied can surface
	// {tier, ruleId, reason, categories} in the audit row. nil for
	// every other denial path.
	classification *safety.Classification
}

// preDispatchCheck enforces Q9 (scope) + Q13 (kill switch) BEFORE
// any wire traffic.
//
// Also enforces per-task approval as a hard server-side gate: every
// workerHost / workerComputer dispatch MUST carry a PlanId
// referencing an approved scope-elevation Plan. Without this check
// an agent that REASONS AROUND the prompt's "always ask first" rule
// (e.g. "the user already granted full standing scope, so I can
// just run this") would silently bypass the user's per-task consent
// surface. The prompt is best-effort guidance for the LLM; this
// gate is the structural guarantee the user actually sees a canvas
// card before any tool runs on their machine.
//
// workerStatus is exempt -- it's the cheap connectivity probe and
// has no side effects on the user's machine.
func (d *Dispatcher) preDispatchCheck(ctx context.Context, req Request) gateResult {
	required := actionRequiredScope(req.Tool, req.Action)
	if required.Capability == "" || required.Scope == "" {
		return gateResult{
			deny:         true,
			errorCode:    "unknown_action",
			errorMessage: fmt.Sprintf("unknown worker action %q on %q", req.Action, req.Tool),
			outcome:      "failure",
		}
	}

	// Per-task approval gate. workerHost / workerComputer require a
	// PlanId; workerStatus and any future read-only probes don't.
	// The PlanId arrives via agentContextStamps.StampPlanId in
	// streaming.go, populated from msg.Hints["plan_id"] on
	// post-approval turns. If the agent dispatched without going
	// through requestComputerUseScope first, PlanId is empty and we
	// deny here -- the agent's reply text then surfaces "I tried to
	// dispatch without asking permission first; please re-ask".
	if req.Tool == "workerHost" || req.Tool == "workerComputer" {
		if strings.TrimSpace(req.PlanId) == "" {
			return gateResult{
				deny:               true,
				requiredCapability: required.Capability,
				requiredScope:      required.Scope,
				errorCode:          "denied_no_per_task_approval",
				errorMessage:       "per-task approval required: call requestComputerUseScope first and end your turn; the user clicks Allow on the canvas card and the planner will re-dispatch this turn",
				outcome:            "denied_by_policy",
			}
		}
	}

	if d.store != nil {
		prefs, err := d.store.UserPreferences(ctx, req.OwnerUserId)
		if err != nil {
			d.logger.Warn("user preferences lookup failed; treating as kill-switch enabled",
				"owner_user_id", req.OwnerUserId,
				"error", err,
			)
		} else if !prefs.ComputerUseEnabled {
			return gateResult{
				deny:               true,
				requiredCapability: required.Capability,
				requiredScope:      required.Scope,
				errorCode:          "kill_switch_engaged",
				errorMessage:       "computer use is currently disabled by the user",
				outcome:            "kill_switch_engaged",
			}
		}

		auth, err := d.store.AgentAuthorization(ctx, req.AgentId, req.OwnerUserId)
		if err != nil {
			d.logger.Warn("agent authorization lookup failed; denying call",
				"agent_id", req.AgentId,
				"error", err,
			)
			return gateResult{
				deny:         true,
				errorCode:    "authorization_lookup_failed",
				errorMessage: err.Error(),
				outcome:      "failure",
			}
		}
		standingScope := ""
		if auth != nil {
			standingScope = auth.ComputerUseScope
		}

		effectiveScope := standingScope
		if req.PlanId != "" {
			planScope, err := d.store.PlanScope(ctx, req.PlanId)
			if err != nil {
				d.logger.Warn("plan scope lookup failed; falling back to standing scope",
					"plan_id", req.PlanId,
					"error", err,
				)
			} else if planScope != "" {
				if !scopeIsNarrowerOrEqual(planScope, standingScope) {
					return gateResult{
						deny:               true,
						requiredCapability: required.Capability,
						requiredScope:      required.Scope,
						errorCode:          "denied_by_scope",
						errorMessage:       "plan scope wider than agent's standing scope",
						outcome:            "denied_by_scope",
					}
				}
				effectiveScope = planScope
			}
		}

		if !scopeAllows(effectiveScope, required.Scope) {
			return gateResult{
				deny:               true,
				requiredCapability: required.Capability,
				requiredScope:      required.Scope,
				errorCode:          "denied_by_scope",
				errorMessage:       fmt.Sprintf("action requires scope %q, agent has %q", required.Scope, effectiveScope),
				outcome:            "denied_by_scope",
			}
		}

		// Safety classifier (memql#229). Runs LAST in preDispatchCheck
		// so the structural gates above (per-task approval, kill
		// switch, scope) have already done their work and we know the
		// agent is otherwise allowed to dispatch. Computer-use is the
		// highest blast radius -- fail-closed on classifier error in
		// enforce mode. Gate.EnforceDecision honours shadow mode end
		// to end: a classifier error in shadow is logged + swallowed,
		// never blocks live traffic. #235's rollout flips enforce.
		desc := buildSafetyDescriptor(req, effectiveScope, required.Capability)
		gate := safety.DefaultGate()
		decision, cls, classErr := gate.Evaluate(ctx, desc)
		if proceed, reason := gate.EnforceDecision(decision, cls, classErr, true); !proceed {
			return gateResult{
				deny:               true,
				requiredCapability: required.Capability,
				requiredScope:      required.Scope,
				errorCode:          "denied_by_classifier",
				errorMessage:       reason,
				outcome:            "denied_by_classifier",
				classification:     &cls,
			}
		}
	}

	return gateResult{requiredCapability: required.Capability, requiredScope: required.Scope}
}

func (d *Dispatcher) pickWorker(req Request, capability string) (*workerservice.Worker, error) {
	if d.registry == nil {
		return nil, workerservice.ErrNoWorkerAvailable
	}
	w, err := d.registry.PickWorker(req.OwnerUserId, capability, nil)
	if err != nil {
		return nil, err
	}
	return w, nil
}

func buildToolDispatch(req Request, timeout time.Duration) *memqlv1.ToolDispatch {
	args, _ := json.Marshal(map[string]any{
		"action":   req.Action,
		req.Action: req.Args,
	})
	return &memqlv1.ToolDispatch{
		CallId:        newCallId(),
		PlanId:        req.PlanId,
		TaskId:        req.TaskId,
		AgentId:       req.AgentId,
		CorrelationId: req.CorrelationId,
		Tool:          req.Tool,
		Action:        req.Action,
		ArgsJson:      args,
		Timeout:       durationpb.New(timeout),
	}
}

func translateResult(callId string, res *memqlv1.ToolResult, err error) Result {
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return Result{OK: false, ErrorCode: "timeout", ErrorMessage: "worker call timed out"}
		}
		return Result{OK: false, ErrorCode: "worker_disconnected", ErrorMessage: err.Error()}
	}
	if res == nil {
		return Result{OK: false, ErrorCode: "worker_disconnected", ErrorMessage: "empty result"}
	}
	switch payload := res.GetPayload().(type) {
	case *memqlv1.ToolResult_Success:
		return Result{
			OK:            true,
			OutputJSON:    string(payload.Success.GetResultJson()),
			BytesIn:       int(payload.Success.GetBytesIn()),
			BytesOut:      int(payload.Success.GetBytesOut()),
			OutputPreview: payload.Success.GetOutputPreview(),
		}
	case *memqlv1.ToolResult_Failure:
		return Result{
			OK:           false,
			ErrorCode:    payload.Failure.GetErrorCode(),
			ErrorMessage: payload.Failure.GetErrorMessage(),
		}
	}
	return Result{OK: false, ErrorCode: "unknown_result", ErrorMessage: "worker returned unknown result shape"}
}

func (r Result) classifyOutcome() string {
	if r.OK {
		return "success"
	}
	switch r.ErrorCode {
	case "timeout":
		return "timeout"
	case "denied_by_scope":
		return "denied_by_scope"
	case "denied_by_policy":
		return "denied_by_policy"
	case "kill_switch_engaged":
		return "kill_switch_engaged"
	case "no_worker_available":
		return "no_worker_available"
	case "":
		return "failure"
	default:
		return "failure"
	}
}

func (d *Dispatcher) recordInvocation(
	ctx context.Context,
	req Request,
	workerId string,
	startedAt, completedAt time.Time,
	res Result,
	outcome string,
) {
	if d.store == nil {
		return
	}
	row := workerservice.InvocationRow{
		ID:            newInvocationId(),
		OwnerUserId:   req.OwnerUserId,
		WorkerId:      workerId,
		AgentId:       req.AgentId,
		PlanId:        req.PlanId,
		TaskId:        req.TaskId,
		CorrelationId: req.CorrelationId,
		Tool:          req.Tool,
		Action:        req.Action,
		ArgsRedacted:  redactArgs(req.Args),
		StartedAt:     startedAt,
		CompletedAt:   completedAt,
		DurationMs:    int(completedAt.Sub(startedAt).Milliseconds()),
		Outcome:       outcome,
		BytesIn:       res.BytesIn,
		BytesOut:      res.BytesOut,
		OutputPreview: clampPreview(res.OutputPreview),
		ErrorCode:     res.ErrorCode,
		ErrorMessage:  res.ErrorMessage,
	}
	if err := d.store.WriteInvocation(ctx, row); err != nil {
		d.logger.Warn("worker invocation persistence failed",
			"call_id", req.CorrelationId,
			"error", err,
		)
	}
}

func (d *Dispatcher) emitDenied(ctx context.Context, req Request, gate gateResult) {
	if d.auditor == nil {
		return
	}
	switch gate.outcome {
	case "denied_by_scope":
		d.auditor.Emit(ctx, workerservice.AuditEvent{
			Action:        "scope_elevation_requested",
			Actor:         "agent:" + req.AgentId,
			Target:        req.AgentId,
			TargetType:    "agent",
			OwnerUserId:   req.OwnerUserId,
			CorrelationId: req.CorrelationId,
			Detail: map[string]any{
				"requestedScope":      gate.requiredScope,
				"requestedCapability": gate.requiredCapability,
				"errorMessage":        gate.errorMessage,
				"planId":              req.PlanId,
				"taskId":              req.TaskId,
			},
			Timestamp: d.clock(),
		})
	case "kill_switch_engaged":
		d.auditor.Emit(ctx, workerservice.AuditEvent{
			Action:        "worker_call_blocked_by_kill_switch",
			Actor:         "agent:" + req.AgentId,
			Target:        req.OwnerUserId,
			TargetType:    "user",
			OwnerUserId:   req.OwnerUserId,
			CorrelationId: req.CorrelationId,
			Timestamp:     d.clock(),
		})
	case "denied_by_policy":
		d.auditor.Emit(ctx, workerservice.AuditEvent{
			Action:        "worker_call_denied_by_policy",
			Actor:         "agent:" + req.AgentId,
			Target:        req.AgentId,
			TargetType:    "agent",
			OwnerUserId:   req.OwnerUserId,
			CorrelationId: req.CorrelationId,
			Detail: map[string]any{
				"errorMessage": gate.errorMessage,
			},
			Timestamp: d.clock(),
		})
	case "denied_by_classifier":
		// memql#229. The gate's per-decision SlogRecorder line is
		// the lightweight observability sink; this auditEvent is the
		// persistent "this command was blocked" record.
		detail := map[string]any{
			"errorMessage": gate.errorMessage,
			"tool":         req.Tool,
			"action":       req.Action,
		}
		if cls := gate.classification; cls != nil {
			detail["tier"] = cls.Tier.String()
			detail["ruleId"] = cls.RuleID
			detail["reason"] = cls.Reason
			detail["confidence"] = cls.Confidence
			cats := make([]string, 0, len(cls.Categories))
			for _, c := range cls.Categories {
				cats = append(cats, string(c))
			}
			detail["categories"] = cats
		}
		d.auditor.Emit(ctx, workerservice.AuditEvent{
			Action:        "command_blocked",
			Actor:         "agent:" + req.AgentId,
			Target:        req.AgentId,
			TargetType:    "agent",
			OwnerUserId:   req.OwnerUserId,
			CorrelationId: req.CorrelationId,
			Detail:        detail,
			Timestamp:     d.clock(),
		})
	}
}

func newCallId() string {
	return randomHex(12)
}

func newInvocationId() string {
	return randomHex(12)
}

func randomHex(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

func clampPreview(s string) string {
	const limit = 1024
	if len(s) <= limit {
		return s
	}
	return s[:limit]
}

// redactArgs scrubs secret-shaped fields from the args before
// persistence. Top-level scrubbing only -- nested secrets are best-
// effort. Tokens / passwords / Authorization headers / SECRET-shaped
// env-var keys are replaced with "[REDACTED]".
func redactArgs(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		if isSecretKey(k) {
			out[k] = "[REDACTED]"
			continue
		}
		if nested, ok := v.(map[string]any); ok {
			out[k] = redactArgs(nested)
			continue
		}
		out[k] = v
	}
	return out
}

func isSecretKey(k string) bool {
	upper := strings.ToUpper(k)
	if upper == "AUTHORIZATION" {
		return true
	}
	for _, frag := range []string{"TOKEN", "SECRET", "PASSWORD", "PASSWD", "API_KEY", "APIKEY", "PRIVATE_KEY", "PRIVATEKEY"} {
		if strings.Contains(upper, frag) {
			return true
		}
	}
	return false
}
