// Package tools holds the cockpit-side tool implementations the
// worker dispatches against. Headless tools (workerHost.*) are
// always built. computer-use tools (workerComputer.*) live behind a build
// tag and reduce to "Unimplemented" on the default build.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql-cockpit/internal/worker/consent"
)

// ConsentGate is the narrow interface the dispatcher uses to ask
// the consent manager whether a tool dispatch may run right now.
// Pulled out as an interface so tests can wire a stub without
// pulling in the full consent package surface.
//
// AllowsAt carries the cursor position so the strict-mode region
// exemption (memql-cockpit#131) can fire for workerComputer.mouse_click.
// Allows is the no-cursor convenience for every other call.
type ConsentGate interface {
	Allows(tool, action string) consent.Decision
	AllowsAt(tool, action string, cursor consent.CursorPoint) consent.Decision
	AllowsDirectoryRead(action string) consent.Decision
}

// Dispatcher is the cockpit-side tool dispatcher. Implements
// internal/worker.ToolDispatcher.
type Dispatcher struct {
	logger       *slog.Logger
	policy       *Policy
	consent      ConsentGate
	pipelineHome string
}

// NewHomeDispatcher binds cache reuse to this locally configured enrollment.
// A request cannot select another enrollment's cache by inventing an argument.
func NewHomeDispatcher(logger *slog.Logger, policy *Policy, consentGate ConsentGate, home string) *Dispatcher {
	d := NewDispatcher(logger, policy, consentGate)
	d.pipelineHome = home
	return d
}

// NewDispatcher constructs a Dispatcher. The policy controls
// allow/deny decisions for shell exec / fs / http tools.
//
// Legacy unscoped calls retain their nil-gate behavior; scoped receipts
// require a gate. Ordinary calls and exact commands need a control window.
// Validated directory receipts use the independent local-stop gate.
func NewDispatcher(logger *slog.Logger, policy *Policy, consentGate ConsentGate) *Dispatcher {
	if logger == nil {
		logger = slog.Default()
	}
	if policy == nil {
		policy = DefaultPolicy()
	}
	return &Dispatcher{logger: logger, policy: policy, consent: consentGate}
}

// Dispatch routes a ToolDispatch to its handler.
//
// Verbose telemetry per call: entry log carries tool + action +
// args-byte count + a redacted args preview; exit log carries
// outcome (success / errorCode) + duration + result-bytes when
// successful. This is the operator's primary surface for
// "what is the worker actually doing right now?" Until we
// upgrade the cockpit to a TUI dashboard, the slog stream IS
// the dashboard.
//
// Panic recovery: robotgo (and any cgo-backed tool path) can
// panic on permission-denial / environment-mismatch corner
// cases. Without recovery the panic propagates up the worker's
// goroutine and tears down the gRPC stream, surfacing on the
// agent side as the cryptic `worker_disconnected` error. The
// recover here turns a panic into a structured Failure with
// the stack on the cockpit's stderr so the operator can see
// what went wrong without the worker dying.
//
// Dispatch has nowhere to put output while a call runs; a call that
// streams it (workerHost.pipeline_step) runs here with its output
// dropped. The worker's runner uses DispatchStream.
func (d *Dispatcher) Dispatch(ctx context.Context, dispatch *memqlv1.ToolDispatch) (*memqlv1.Success, *memqlv1.Failure) {
	return d.DispatchStream(ctx, dispatch, nil)
}

// DispatchStream is Dispatch for a caller that can carry a call's output
// to the cluster while the call runs: send receives it as ToolStream
// frames, stdout and stderr apart, each carrying the call id, in order
// per stream and ALL BEFORE DispatchStream returns -- the engine relays a
// chunk only while its call is pending, and drops one that arrives after
// the result. Only workerHost.pipeline_step streams today. send may be
// nil.
func (d *Dispatcher) DispatchStream(ctx context.Context, dispatch *memqlv1.ToolDispatch, send func(*memqlv1.ToolStream) error) (success *memqlv1.Success, failure *memqlv1.Failure) {
	if dispatch == nil {
		return nil, &memqlv1.Failure{ErrorCode: "bad_request", ErrorMessage: "nil dispatch"}
	}

	tool := dispatch.GetTool()
	action := dispatch.GetAction()
	rawArgs := dispatch.GetArgsJson()
	callId := dispatch.GetCallId()
	startedAt := time.Now()

	defer func() {
		if rec := recover(); rec != nil {
			stack := string(debug.Stack())
			d.logger.Error("worker tool dispatch PANIC -- recovered to keep the worker stream alive",
				"tool", tool,
				"action", action,
				"call_id", callId,
				"panic", fmt.Sprintf("%v", rec),
				"stack", stack,
			)
			success = nil
			failure = &memqlv1.Failure{
				ErrorCode:    "tool_panic",
				ErrorMessage: fmt.Sprintf("worker tool %s.%s panicked: %v", tool, action, rec),
			}
		}
	}()

	d.logger.Info("worker tool dispatch START",
		"tool", tool,
		"action", action,
		"call_id", callId,
		"args_bytes", len(rawArgs),
		"args_preview", redactedArgsPreview(rawArgs),
	)

	args, err := decodeArgs(rawArgs, action)
	if err != nil {
		d.logger.Warn("worker tool dispatch FAIL (bad_request: decode args)",
			"tool", tool,
			"action", action,
			"call_id", callId,
			"error", err,
		)
		return nil, &memqlv1.Failure{ErrorCode: "bad_request", ErrorMessage: err.Error()}
	}

	if dispatch.GetTool() == "workerWorkbench" || dispatch.GetWorkbench() != nil {
		return d.dispatchWorkbench(ctx, dispatch, args)
	}

	// A scoped receipt is a separate, explicit owner decision. Validate it
	// before choosing its consent gate; directory reads do not expire with
	// the broader computer-control window. Local revocation still wins.
	if dispatch.GetComputerPermission() != nil {
		return d.dispatchComputerPermission(ctx, dispatch, args)
	}

	// Consent gate (memql-cockpit#64). Every unscoped workerHost /
	// workerComputer call passes through the per-host consent
	// manager. Without an active operator-granted window the call
	// short-circuits with `consent_required` carrying the gate's
	// hint about how to open one. When d.consent == nil (no gate
	// wired, e.g. legacy tests) we behave the pre-#64 way.
	//
	// workerHost.pipeline_step does not ask the window (memql#5494):
	// the owner's consent to it is STANDING, policy.yaml's pipelines
	// block, which the step checks before it touches anything. Every
	// other call asks exactly as before.
	if d.consent != nil && !consent.AdmittedByPolicy(tool, action) {
		// Target-addressed clicks do not use the live cursor. Do not grant
		// the old cursor-region exemption for a different explicit target.
		var dec consent.Decision
		if tool == "workerComputer" && action == "mouse_click" {
			dec = d.consent.AllowsAt(tool, action, consent.CursorPoint{Known: false})
		} else {
			dec = d.consent.Allows(tool, action)
		}
		if !dec.Allowed {
			elapsedMs := time.Since(startedAt).Milliseconds()
			d.logger.Warn("worker tool dispatch FAIL (consent_required)",
				"tool", tool,
				"action", action,
				"call_id", callId,
				"reason", dec.Reason,
				"elapsed_ms", elapsedMs,
			)
			return nil, &memqlv1.Failure{
				ErrorCode:    "consent_required",
				ErrorMessage: dec.Reason,
			}
		}
	}

	switch tool {
	case "workerHost":
		success, failure = d.dispatchHost(ctx, action, args, dispatch.GetAgentId(), d.streamTo(callId, send), func(data []byte) error {
			if send == nil {
				return fmt.Errorf("artifact streaming is unavailable")
			}
			return send(&memqlv1.ToolStream{CallId: callId, Payload: &memqlv1.ToolStream_DataChunk{DataChunk: data}})
		})
	case "workerComputer":
		// `capabilities` is build-agnostic introspection
		// (memql-cockpit#162): route it BEFORE the per-build
		// dispatchComputer so the headless build answers honestly
		// (computerUseAvailable=false, actions=[]) instead of rejecting
		// with computeruse_unavailable. It is the one workerComputer action
		// that must work on every build variant.
		//
		// Every other workerComputer action runs the per-call
		// platform preflights first (memql-cockpit#164): macOS TCC
		// (Accessibility for input, Screen Recording for screenshot)
		// and the Linux display-server check. A failed preflight
		// short-circuits with a structured remediation hint instead
		// of letting RobotGo fail or panic downstream.
		// `wait` (memql-cockpit#166) is the second build-agnostic
		// action: a bounded sleep that needs no computer-use backend and no
		// platform permission, so it routes alongside `capabilities`,
		// before the per-build router AND before the preflights.
		if action == computerCapabilitiesAction {
			success, failure = runComputerCapabilities()
		} else if action == computerWaitAction {
			success, failure = runComputerWait(ctx, args)
		} else if fail := preflightComputerAction(action); fail != nil {
			failure = fail
		} else {
			success, failure = d.dispatchComputer(context.WithValue(ctx, inputScopeContextKey{}, inputScope{dispatcher: d, run: dispatch.GetRunId()}), action, args)
		}
	default:
		failure = &memqlv1.Failure{ErrorCode: "unknown_tool", ErrorMessage: fmt.Sprintf("unknown tool %q", tool)}
	}

	elapsedMs := time.Since(startedAt).Milliseconds()
	if failure != nil {
		d.logger.Warn("worker tool dispatch FAIL",
			"tool", tool,
			"action", action,
			"call_id", callId,
			"error_code", failure.GetErrorCode(),
			"error_message", failure.GetErrorMessage(),
			"elapsed_ms", elapsedMs,
		)
	} else if success != nil {
		d.logger.Info("worker tool dispatch OK",
			"tool", tool,
			"action", action,
			"call_id", callId,
			"result_bytes", len(success.GetResultJson()),
			"output_preview", success.GetOutputPreview(),
			"elapsed_ms", elapsedMs,
		)
	}
	return success, failure
}

// redactedArgsPreview returns a short, redacted preview of the raw
// args JSON for log lines. Strips known secret-shaped fields
// (token / password / authorization headers, anything under the
// "secret" key family) and clamps the rest to 256 bytes so the
// log line stays readable. Use this in entry-logs only -- the
// full args land in the policy layer's per-action audit if needed.
func redactedArgsPreview(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		// Not JSON or malformed -- return a clamped raw string.
		s := string(raw)
		if len(s) > 256 {
			s = s[:256] + "...(truncated)"
		}
		return s
	}
	redactSecrets(probe)
	out, _ := json.Marshal(probe)
	if len(out) > 256 {
		return string(out[:256]) + "...(truncated)"
	}
	return string(out)
}

// redactSecrets walks a decoded JSON map in place and replaces any
// value under a known-secret-shaped key with "[REDACTED]". Conservative
// match: case-insensitive substring on token / password / authorization
// / secret / api_key.
func redactSecrets(m map[string]any) {
	for k, v := range m {
		if isSecretKey(k) {
			m[k] = "[REDACTED]"
			continue
		}
		switch nested := v.(type) {
		case map[string]any:
			redactSecrets(nested)
		case []any:
			for _, item := range nested {
				if im, ok := item.(map[string]any); ok {
					redactSecrets(im)
				}
			}
		}
	}
}

func isSecretKey(k string) bool {
	lower := strings.ToLower(k)
	for _, needle := range []string{"token", "password", "authorization", "secret", "api_key", "apikey"} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

// streamTo turns the runner's ToolStream sender into the output a running
// call writes to: each piece becomes one frame carrying the call id. A
// failed send is logged once and the call carries on -- the stream it
// would report on is gone, and losing it is what ends the call (the runner
// cancels every call in flight on a lost stream), not a chunk that did not
// arrive.
func (d *Dispatcher) streamTo(callID string, send func(*memqlv1.ToolStream) error) outputEmitter {
	if send == nil {
		return nil
	}
	var failed atomic.Bool
	return func(stderr bool, data []byte) {
		chunk := &memqlv1.ToolStream{CallId: callID}
		if stderr {
			chunk.Payload = &memqlv1.ToolStream_StderrChunk{StderrChunk: data}
		} else {
			chunk.Payload = &memqlv1.ToolStream_StdoutChunk{StdoutChunk: data}
		}
		if err := send(chunk); err != nil && !failed.Swap(true) {
			d.logger.Warn("worker tool output could not be streamed to the cluster",
				"call_id", callID,
				"error", err,
			)
		}
	}
}

// dispatchHost routes a workerHost call. agentID is the envelope's agent_id,
// which pipeline_step refuses when set: a step comes only from the cluster's
// pipeline runner, which dispatches with no agent.
func (d *Dispatcher) dispatchHost(ctx context.Context, action string, args map[string]any, agentID string, emit outputEmitter, sendArtifact artifactEmitter) (*memqlv1.Success, *memqlv1.Failure) {
	switch action {
	case "pipeline_step":
		return runPipelineStep(context.WithValue(ctx, pipelineCacheHomeKey{}, d.pipelineHome), agentID, args, d.policy, emit, sendArtifact)
	case "exec":
		return runExec(ctx, args, d.policy)
	case "fs_read":
		return runFsRead(ctx, args, d.policy)
	case "fs_write":
		return runFsWrite(ctx, args, d.policy)
	case "fs_list":
		return runFsList(ctx, args, d.policy)
	case "fs_stat":
		return runFsStat(ctx, args, d.policy)
	case "http_fetch":
		return runHTTPFetch(ctx, args, d.policy)
	}
	return nil, &memqlv1.Failure{
		ErrorCode:    "unknown_action",
		ErrorMessage: fmt.Sprintf("workerHost.%s not implemented", action),
	}
}

// decodeArgs unwraps the discriminated-union envelope:
//
//	{"action": "exec", "exec": { ... }}
//
// and returns the inner per-action map. Falls back to the outer
// object when the inner key is absent.
func decodeArgs(raw []byte, action string) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var outer map[string]any
	if err := json.Unmarshal(raw, &outer); err != nil {
		return nil, fmt.Errorf("decode args: %w", err)
	}
	if inner, ok := outer[action].(map[string]any); ok {
		return inner, nil
	}
	// Treat the outer map (minus the action discriminator) as the args.
	out := make(map[string]any, len(outer))
	for k, v := range outer {
		if k == "action" {
			continue
		}
		out[k] = v
	}
	return out, nil
}

func argString(args map[string]any, key string) string {
	if v, ok := args[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func argStringList(args map[string]any, key string) []string {
	v, ok := args[key]
	if !ok {
		return nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func argInt(args map[string]any, key string, def int) int {
	v, ok := args[key]
	if !ok {
		return def
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return def
}

func argMap(args map[string]any, key string) map[string]any {
	v, ok := args[key]
	if !ok {
		return nil
	}
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

func successJSON(payload map[string]any, exit int, bytesIn, bytesOut int, preview string) *memqlv1.Success {
	body, _ := json.Marshal(payload)
	return &memqlv1.Success{
		ResultJson:    body,
		ExitCode:      int32(exit),
		BytesIn:       uint64(bytesIn),
		BytesOut:      uint64(bytesOut),
		OutputPreview: clampPreview(preview),
	}
}

func failure(code, message string) *memqlv1.Failure {
	return &memqlv1.Failure{ErrorCode: code, ErrorMessage: message}
}

func clampPreview(s string) string {
	const limit = 1024
	if len(s) <= limit {
		return s
	}
	return s[:limit]
}

func cleanString(s string) string { return strings.TrimSpace(s) }
