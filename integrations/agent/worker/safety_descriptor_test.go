//go:build agent

package worker

import (
	"reflect"
	"testing"

	"github.com/znasllc-io/memql/component/safety"
)

func TestBuildSafetyDescriptor_ExecSurfaceMapping(t *testing.T) {
	// workerHost -> headless; workerComputer -> embodied.
	for _, tc := range []struct {
		tool string
		want safety.Surface
	}{
		{"workerHost", safety.SurfaceComputerUseHeadless},
		{"workerComputer", safety.SurfaceComputerUseEmbodied},
	} {
		req := Request{Tool: tc.tool, Action: "exec", Args: map[string]any{"command": "ls"}}
		got := buildSafetyDescriptor(req, "interact", "computer_use_headless")
		if got.Surface != tc.want {
			t.Errorf("tool=%q: surface=%q, want %q", tc.tool, got.Surface, tc.want)
		}
		if got.Action != safety.ActionExec || got.Payload.Command != "ls" {
			t.Errorf("tool=%q: expected exec/ls, got %+v", tc.tool, got)
		}
	}
}

func TestBuildSafetyDescriptor_CallerContextPopulated(t *testing.T) {
	req := Request{
		Tool: "workerHost", Action: "exec", Args: map[string]any{"command": "ls"},
		AgentId: "a1", OwnerUserId: "u1", PlanId: "p1", TaskId: "t1", CorrelationId: "c1",
	}
	got := buildSafetyDescriptor(req, "full", "workerHost")
	want := safety.CallerContext{
		AgentID: "a1", OwnerUserID: "u1", PlanID: "p1", TaskID: "t1",
		CorrelationID: "c1", Scope: "full", Capability: "workerHost",
	}
	if got.Caller != want {
		t.Errorf("caller context mismatch:\n got  %+v\n want %+v", got.Caller, want)
	}
}

func TestBuildSafetyDescriptor_FSActions(t *testing.T) {
	for _, action := range []string{"fs_read", "fs_write", "fs_list", "fs_stat"} {
		// Single `path` form.
		req := Request{Tool: "workerHost", Action: action, Args: map[string]any{"path": "/tmp/x"}}
		got := buildSafetyDescriptor(req, "interact", "workerHost")
		if string(got.Action) != action {
			t.Errorf("action=%q: got %q", action, got.Action)
		}
		if !reflect.DeepEqual(got.Payload.Paths, []string{"/tmp/x"}) {
			t.Errorf("action=%q paths: got %#v", action, got.Payload.Paths)
		}
	}
	// `paths` []any form (the shape an LLM-shaped JSON args map
	// would arrive as).
	req := Request{Tool: "workerHost", Action: "fs_read", Args: map[string]any{
		"paths": []any{"/a", "/b"},
	}}
	got := buildSafetyDescriptor(req, "interact", "workerHost")
	if !reflect.DeepEqual(got.Payload.Paths, []string{"/a", "/b"}) {
		t.Errorf("paths []any: got %#v", got.Payload.Paths)
	}
}

func TestBuildSafetyDescriptor_HTTPFetch(t *testing.T) {
	req := Request{Tool: "workerHost", Action: "http_fetch", Args: map[string]any{
		"method": "POST",
		"url":    "https://example.com/x",
		"body":   `{"k":"v"}`,
	}}
	got := buildSafetyDescriptor(req, "interact", "workerHost")
	if got.Action != safety.ActionHTTPFetch {
		t.Errorf("action: got %v", got.Action)
	}
	if got.Payload.Method != "POST" || got.Payload.URL != "https://example.com/x" || got.Payload.Body != `{"k":"v"}` {
		t.Errorf("http payload mismatch: %+v", got.Payload)
	}
}

func TestBuildSafetyDescriptor_UnknownActionPassesArgs(t *testing.T) {
	// An unknown action must NOT lose the args -- the recorder still
	// needs to see what was attempted. The classifier's rules
	// escalate on unknown actions (no opinion), so this lands as a
	// shadow-mode noop verdict.
	req := Request{Tool: "workerHost", Action: "weird_new_action", Args: map[string]any{
		"x": 1, "y": "z",
	}}
	got := buildSafetyDescriptor(req, "interact", "workerHost")
	if string(got.Action) != "weird_new_action" {
		t.Errorf("action: got %q", got.Action)
	}
	if got.Payload.Args["x"] != 1 || got.Payload.Args["y"] != "z" {
		t.Errorf("args dropped on unknown action: %+v", got.Payload.Args)
	}
}
