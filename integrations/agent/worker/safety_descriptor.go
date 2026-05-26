//go:build agent

package worker

import (
	"github.com/znasllc-io/memql/component/safety"
)

// buildSafetyDescriptor lowers a worker dispatch Request into the
// surface-agnostic safety.ActionDescriptor the classifier consumes.
// Lives in the worker package so we don't pollute component/safety
// with a worker.Request import (and to avoid a cycle).
//
// Surface mapping:
//   - workerHost    -> computer_use_headless
//   - workerComputer -> computer_use_embodied
//   - everything else -> computer_use_headless (defensive; the
//     dispatch path's scope check already rejects unknown tools, but
//     the descriptor still needs a surface).
//
// Action mapping follows the worker action vocabulary (exec /
// fs_read / fs_write / fs_list / fs_stat / http_fetch / gui_input /
// screenshot) one-to-one. The Payload pulls just what the rules
// inspect; everything else (timeouts, sub-args) stays in `Args`.
func buildSafetyDescriptor(req Request, effectiveScope, capability string) safety.ActionDescriptor {
	caller := safety.CallerContext{
		AgentID:       req.AgentId,
		OwnerUserID:   req.OwnerUserId,
		PlanID:        req.PlanId,
		TaskID:        req.TaskId,
		CorrelationID: req.CorrelationId,
		Scope:         effectiveScope,
		Capability:    capability,
	}

	surface := safety.SurfaceComputerUseHeadless
	if req.Tool == "workerComputer" {
		surface = safety.SurfaceComputerUseEmbodied
	}

	switch req.Action {
	case "exec":
		cmd, _ := req.Args["command"].(string)
		return safety.NewExecAction(surface, cmd, caller)
	case "fs_read":
		return safety.NewFSAction(surface, safety.ActionFSRead, pathsFromArgs(req.Args), caller)
	case "fs_write":
		return safety.NewFSAction(surface, safety.ActionFSWrite, pathsFromArgs(req.Args), caller)
	case "fs_list":
		return safety.NewFSAction(surface, safety.ActionFSList, pathsFromArgs(req.Args), caller)
	case "fs_stat":
		return safety.NewFSAction(surface, safety.ActionFSStat, pathsFromArgs(req.Args), caller)
	case "http_fetch":
		method, _ := req.Args["method"].(string)
		url, _ := req.Args["url"].(string)
		body, _ := req.Args["body"].(string)
		return safety.NewHTTPAction(surface, method, url, body, caller)
	default:
		// Unknown action: build a descriptor anyway so the recorder
		// captures the attempt. Rules return ErrNoClassifierOpinion
		// for actions they don't recognise, so the classifier
		// escalates (chain falls through to noop, decision=Allow in
		// shadow).
		return safety.ActionDescriptor{
			Surface: surface,
			Action:  safety.Action(req.Action),
			Payload: safety.Payload{Args: copyArgsForSafety(req.Args)},
			Caller:  caller,
		}
	}
}

// pathsFromArgs extracts the path(s) the FS action would touch.
// Accepts either a single `path` string or a `paths` []string /
// []any so the rule layer sees every path the dispatch would visit.
func pathsFromArgs(args map[string]any) []string {
	if p, ok := args["path"].(string); ok && p != "" {
		return []string{p}
	}
	switch v := args["paths"].(type) {
	case []string:
		return append([]string(nil), v...)
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			if s, ok := x.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// copyArgsForSafety produces a shallow copy of the args map so the
// descriptor doesn't alias the caller's. Nested values pass through
// unchanged -- secret redaction happens on the recorder side via
// safety.RedactedPayload.
func copyArgsForSafety(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
