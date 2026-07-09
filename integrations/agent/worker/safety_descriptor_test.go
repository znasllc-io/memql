//go:build agent

package worker

import (
	"context"
	"errors"
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

func TestBuildSafetyDescriptor_ComputerUseV2Actions(t *testing.T) {
	// The v2 workerComputer additions (cockpit #166) have no typed
	// safety payload, so they ride the default branch like the
	// pre-existing computer-use input actions: embodied surface, verbatim
	// action name, args preserved for the recorder.
	cases := []struct {
		action string
		args   map[string]any
	}{
		{"capabilities", nil},
		{"wait", map[string]any{"ms": 250}},
		{"key_hold", map[string]any{"key": "shift", "ms": 500}},
		{"mouse_down", map[string]any{"x": 10, "y": 20, "button": "left"}},
		{"mouse_up", map[string]any{"x": 10, "y": 20, "button": "left"}},
		{"mouse_click", map[string]any{"x": 10, "y": 20, "count": 3}},
	}
	for _, tc := range cases {
		req := Request{Tool: "workerComputer", Action: tc.action, Args: tc.args}
		got := buildSafetyDescriptor(req, "full", "COMPUTERUSE")
		if got.Surface != safety.SurfaceComputerUseEmbodied {
			t.Errorf("%s: surface=%q, want embodied", tc.action, got.Surface)
		}
		if string(got.Action) != tc.action {
			t.Errorf("%s: action passthrough broken, got %q", tc.action, got.Action)
		}
		for k, v := range tc.args {
			if got.Payload.Args[k] != v {
				t.Errorf("%s: arg %q dropped/mutated: got %v want %v", tc.action, k, got.Payload.Args[k], v)
			}
		}
		if tc.args == nil && got.Payload.Args != nil {
			t.Errorf("%s: nil args must stay nil, got %v", tc.action, got.Payload.Args)
		}
	}
}

func TestComputerUseV2Actions_RuleLayerEscalates(t *testing.T) {
	// Decision-path proof: the curated rule layer has no opinion on
	// the new actions (they are not exec/fs/http shapes), so the
	// classifier escalates via ErrNoClassifierOpinion -- the same
	// path the pre-existing mouse_* / key_* input actions take. A
	// rule mis-firing on the new action names or args would fail
	// here.
	rc := safety.NewRuleClassifier(safety.DefaultRules()...)
	for _, action := range []string{"capabilities", "wait", "key_hold", "mouse_down", "mouse_up"} {
		req := Request{Tool: "workerComputer", Action: action, Args: map[string]any{
			"key": "shift", "x": 1, "y": 2,
		}}
		desc := buildSafetyDescriptor(req, "full", "COMPUTERUSE")
		_, err := rc.Classify(context.Background(), desc)
		if !errors.Is(err, safety.ErrNoClassifierOpinion) {
			t.Errorf("%s: expected rule-layer escalation, got err=%v", action, err)
		}
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
