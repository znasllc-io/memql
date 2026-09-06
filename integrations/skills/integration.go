package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// integration.go -- the DSL surface: `runScript` and `captureScript`, and the
// adapters that let one composition drive two dispatchers it does not import.
//
// ===========================================================================
// THE SURFACES ARE CAPABILITY HANDLERS, NOT PACKAGES
// ===========================================================================
// The workbench dispatcher and the fleet dispatcher already exist, each
// behind its own integration, each with its own gates -- the environment
// hint, the safety classifier, the scope check, the exec allowlist, the
// router. This package drives them through the ONE thing they both publish:
// the `dispatchHost` capability handler off `Capabilities()`.
//
// That is what keeps `runScript` a composition rather than a third dispatcher.
// It cannot skip a gate, because it does not reach past the same entry point
// every other caller uses; and it needs neither package as an import, so the
// agent-only fleet integration and the everywhere workbench one can be wired
// independently by whichever binary has them.

// CapabilityHandler is the shape of `memql.IntegrationCapability.Handler`.
// Declared here rather than imported because the field's type is unexported;
// Go func types are structural, so the assignment is exact.
type CapabilityHandler func(ctx context.Context, args map[string]any, target int) ([]memorynodes.MemoryNode, error)

// Integration is the DSL-facing provider.
type Integration struct {
	runner *Runner
	logger *slog.Logger
}

func NewIntegration(runner *Runner, logger *slog.Logger) *Integration {
	if logger == nil {
		logger = slog.Default()
	}
	return &Integration{runner: runner, logger: logger}
}

func (i *Integration) IntegrationName() string { return "skills" }

func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "runScript",
			Description: "Run a skill's script, shipped by content hash and verified on the far side before it executes.",
			Handler:     i.handleRunScript,
			ArgsSchema: map[string]string{
				"skillId":          "string (required) -- the v1:skills:skill whose script to run",
				"scriptArtifactId": "string -- pin one entry; empty picks by platform",
				"args":             "array -- arguments appended to the entry, quoted individually",
				"planId":           "string (required) -- the workspace / per-task approval scope",
				"agentId":          "string -- the calling agent, for the fleet's gate",
				"ownerUserId":      "string -- whose fleet to route on",
				"stepId":           "string -- the v1:work:step to stamp the receipt onto",
				"runId":            "string -- the v1:work:run the step belongs to",
				"environment":      "object -- {os, needs[]}, passed through to the surface unchanged",
				"requireLabels":    "object -- labels that MUST match; a non-empty map moves the call to the fleet",
				"timeoutSec":       "integer -- exec timeout; absent takes the surface's default",
			},
		},
		{
			Name:        "captureScript",
			Description: "Copy a script a run discovered on a surface into the Library and record it on the skill by artifact rather than by path.",
			Handler:     i.handleCaptureScript,
			ArgsSchema: map[string]string{
				"skillId":       "string (required) -- the skill to record it on",
				"path":          "string (required) -- where the file is on the far side",
				"platform":      "string -- linux / darwin / windows / any; empty records `any`",
				"entry":         "string -- the command line, {script} standing for the shipped path",
				"name":          "string -- the Library file name; empty uses the base name",
				"planId":        "string (required) -- scopes the surface it is read from",
				"agentId":       "string",
				"ownerUserId":   "string",
				"requireLabels": "object -- read it from a fleet machine rather than the workbench",
				"environment":   "object",
			},
		},
	}
}

func (i *Integration) handleRunScript(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if i == nil || i.runner == nil {
		return nil, fmt.Errorf("skills.runScript: no script runner is wired on this node")
	}
	req := requestFromArgs(args)
	receipt, err := i.runner.Run(ctx, req)
	if err != nil {
		var refusal Refusal
		if asRefusal(err, &refusal) {
			// A REFUSAL IS A RESULT, not a Go error. Every one of them means
			// nothing ran, and the caller -- an agent's tool loop, or the
			// reroute -- decides what to do from the CODE. Returning an error
			// here would flatten `denied_by_scope` and `script_hash_mismatch`
			// into one unreadable string.
			return refusalNode(receipt, refusal), nil
		}
		return nil, err
	}
	return receiptNode(receipt), nil
}

func (i *Integration) handleCaptureScript(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if i == nil || i.runner == nil {
		return nil, fmt.Errorf("skills.captureScript: no script runner is wired on this node")
	}
	req := CaptureRequest{
		Request:  requestFromArgs(args),
		Path:     asString(args["path"]),
		Platform: asString(args["platform"]),
		Entry:    asString(args["entry"]),
		Name:     asString(args["name"]),
	}
	captured, err := i.runner.Capture(ctx, req)
	if err != nil {
		var refusal Refusal
		if asRefusal(err, &refusal) {
			return errorNode("captureScript", refusal.Code, refusal.Message), nil
		}
		return nil, err
	}
	return okNode("captureScript", captured), nil
}

func requestFromArgs(args map[string]any) Request {
	return Request{
		SkillID:          asString(args["skillId"]),
		ScriptArtifactID: asString(args["scriptArtifactId"]),
		Args:             asStringList(args["args"]),
		PlanID:           asString(args["planId"]),
		AgentID:          asString(args["agentId"]),
		OwnerID:          asString(args["ownerUserId"]),
		StepID:           asString(args["stepId"]),
		RunID:            asString(args["runId"]),
		Environment:      asObject(args["environment"]),
		RequireLabels:    asLabels(args["requireLabels"]),
		TimeoutSec:       asInt(args["timeoutSec"]),
	}
}

// ---------------------------------------------------------------------------
// Surfaces
// ---------------------------------------------------------------------------

// NewWorkbenchSurface adapts the workbench's `dispatchHost` capability.
func NewWorkbenchSurface(handler CapabilityHandler, platform string) Surface {
	return &capabilitySurface{
		name:     SurfaceWorkbench,
		platform: platform,
		handler:  handler,
		envelope: func(req Request, action string, args map[string]any) map[string]any {
			out := map[string]any{
				"action":  action,
				"args":    args,
				"planId":  req.PlanID,
				"agentId": req.AgentID,
			}
			if len(req.Environment) > 0 {
				out["environment"] = req.Environment
			}
			return out
		},
		// The workbench's node payload is {ok, action, payload, errorCode,
		// errorMessage}.
		read: func(body map[string]any) CallResult {
			return CallResult{
				OK:        body["ok"] == true,
				ErrorCode: asString(body["errorCode"]),
				ErrorMsg:  asString(body["errorMessage"]),
				Payload:   asObject(body["payload"]),
			}
		},
	}
}

// NewFleetSurface adapts the agent worker's `dispatchHost` capability.
//
// The PLATFORM IS NOT KNOWN before the call. A fleet dispatch is routed by
// label across machines that may run different operating systems, and asking
// "which platform" before the router has picked would be answering for a
// machine nobody has chosen yet. So it reports "", and `PickScript` treats
// that as "only an `any` script" -- which is the fail-closed direction: a
// caller that needs a platform-specific script on a machine must say so with
// a label, and then the script for that platform is what a template pins.
func NewFleetSurface(handler CapabilityHandler) Surface {
	return &capabilitySurface{
		name:     SurfaceMachine,
		platform: "",
		handler:  handler,
		envelope: func(req Request, action string, args map[string]any) map[string]any {
			out := map[string]any{
				"action":      action,
				"args":        args,
				"planId":      req.PlanID,
				"agentId":     req.AgentID,
				"ownerUserId": req.OwnerID,
			}
			if len(req.RequireLabels) > 0 {
				out["requireLabels"] = toAnyMap(req.RequireLabels)
			}
			return out
		},
		// The worker's node payload is {ok, output, errorCode, errorMessage,
		// ...}, and `output` is the cockpit's own JSON for the action -- the
		// same key set the workbench answers with, which is what the identical
		// six verbs buy.
		read: func(body map[string]any) CallResult {
			return CallResult{
				OK:        body["ok"] == true,
				ErrorCode: asString(body["errorCode"]),
				ErrorMsg:  asString(body["errorMessage"]),
				Payload:   asObject(body["output"]),
			}
		},
	}
}

type capabilitySurface struct {
	name     string
	platform string
	handler  CapabilityHandler
	envelope func(req Request, action string, args map[string]any) map[string]any
	read     func(body map[string]any) CallResult
}

func (s *capabilitySurface) Name() string { return s.name }

func (s *capabilitySurface) Platform(context.Context, Request) string { return s.platform }

func (s *capabilitySurface) Call(ctx context.Context, req Request, action string, args map[string]any) (CallResult, error) {
	if s.handler == nil {
		return CallResult{}, refuse(ErrNoSurface, "the %s surface is wired without a dispatcher", s.name)
	}
	nodes, err := s.handler(ctx, s.envelope(req, action, args), 1)
	if err != nil {
		return CallResult{}, err
	}
	if len(nodes) == 0 {
		return CallResult{}, fmt.Errorf("skills: the %s surface answered nothing for %s", s.name, action)
	}
	var body map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &body); err != nil {
		return CallResult{}, fmt.Errorf("skills: the %s surface's answer to %s was unreadable: %w", s.name, action, err)
	}
	return s.read(body), nil
}

// ---------------------------------------------------------------------------
// Nodes
// ---------------------------------------------------------------------------

func receiptNode(receipt Receipt) []memorynodes.MemoryNode {
	return okNode("runScript", map[string]any{"ok": true, "receipt": receipt})
}

func refusalNode(receipt Receipt, refusal Refusal) []memorynodes.MemoryNode {
	// The receipt travels WITH the refusal. A hash mismatch that names the
	// artifact and the surface it was refused on is actionable; a bare code
	// is not, and the caller has no second read that could recover it.
	return okNode("runScript", map[string]any{
		"ok":           false,
		"errorCode":    refusal.Code,
		"errorMessage": refusal.Message,
		"receipt":      receipt,
	})
}

func errorNode(action, code, message string) []memorynodes.MemoryNode {
	return okNode(action, map[string]any{"ok": false, "errorCode": code, "errorMessage": message})
}

func okNode(action string, body any) []memorynodes.MemoryNode {
	payload, err := json.Marshal(body)
	if err != nil {
		payload = []byte(`{"ok":false,"errorCode":"encode_failed"}`)
	}
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("skills:%s:%d", action, time.Now().UnixNano()),
		Concept:   "integration:skills:result",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payload,
	}}
}

// ---------------------------------------------------------------------------

func asRefusal(err error, out *Refusal) bool {
	r, ok := err.(Refusal)
	if ok {
		*out = r
	}
	return ok
}

func asString(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func asObject(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asStringList(v any) []string {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		out = append(out, fmt.Sprint(item))
	}
	return out
}

func asLabels(v any) map[string]string {
	raw, ok := v.(map[string]any)
	if !ok || len(raw) == 0 {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, val := range raw {
		out[k] = fmt.Sprint(val)
	}
	return out
}

func toAnyMap(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func asInt(v any) int {
	return intFrom(v)
}
