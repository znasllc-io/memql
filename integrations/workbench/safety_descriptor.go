package workbench

import (
	"github.com/znasllc-io/memql/component/safety"
)

// buildSafetyDescriptor lowers a workbench dispatch into the
// surface-agnostic safety.ActionDescriptor the classifier consumes.
// Surface is always SurfaceWorkbench (the per-Plan sandbox).
//
// Caller context carries the planId; agent / owner / task fields
// aren't exposed to the workbench dispatch path today, so they stay
// empty in the descriptor (the gate's recorder logs what's
// available; #234's persistence concept stores the same).
func buildSafetyDescriptor(action, planId string, innerArgs map[string]any) safety.ActionDescriptor {
	caller := safety.CallerContext{
		PlanID:     planId,
		Capability: "workbench_use",
	}
	switch action {
	case "exec":
		cmd, _ := innerArgs["command"].(string)
		return safety.NewExecAction(safety.SurfaceWorkbench, cmd, caller)
	case "fs_read":
		return safety.NewFSAction(safety.SurfaceWorkbench, safety.ActionFSRead, workbenchPaths(innerArgs), caller)
	case "fs_write":
		return safety.NewFSAction(safety.SurfaceWorkbench, safety.ActionFSWrite, workbenchPaths(innerArgs), caller)
	case "fs_list":
		return safety.NewFSAction(safety.SurfaceWorkbench, safety.ActionFSList, workbenchPaths(innerArgs), caller)
	case "fs_stat":
		return safety.NewFSAction(safety.SurfaceWorkbench, safety.ActionFSStat, workbenchPaths(innerArgs), caller)
	case "http_fetch":
		method, _ := innerArgs["method"].(string)
		url, _ := innerArgs["url"].(string)
		body, _ := innerArgs["body"].(string)
		return safety.NewHTTPAction(safety.SurfaceWorkbench, method, url, body, caller)
	default:
		// Unknown action: the dispatch path's own switch already
		// returns an error for these; build a descriptor anyway so
		// the recorder sees the attempt.
		return safety.ActionDescriptor{
			Surface: safety.SurfaceWorkbench,
			Action:  safety.Action(action),
			Payload: safety.Payload{Args: copyArgsForSafety(innerArgs)},
			Caller:  caller,
		}
	}
}

// workbenchPaths extracts the path(s) the FS action would touch from
// the inner args map. Accepts a single `path` string or a `paths`
// []string / []any so the rule layer sees every path the action
// would visit.
func workbenchPaths(args map[string]any) []string {
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
