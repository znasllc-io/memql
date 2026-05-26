package safety

import "testing"

func TestNewExecAction(t *testing.T) {
	d := NewExecAction(SurfaceWorkbench, "ls -la", CallerContext{AgentID: "a1", PlanID: "p1"})
	if d.Surface != SurfaceWorkbench || d.Action != ActionExec {
		t.Errorf("wrong surface/action: %+v", d)
	}
	if d.Payload.Command != "ls -la" {
		t.Errorf("command not set: %q", d.Payload.Command)
	}
	if d.Caller.AgentID != "a1" || d.Caller.PlanID != "p1" {
		t.Errorf("caller context lost: %+v", d.Caller)
	}
}

func TestNewHTTPAction(t *testing.T) {
	d := NewHTTPAction(SurfaceComputerUseHeadless, "POST", "https://e.com", `{"k":"v"}`, CallerContext{})
	if d.Action != ActionHTTPFetch {
		t.Errorf("wrong action: %v", d.Action)
	}
	if d.Payload.Method != "POST" || d.Payload.URL != "https://e.com" || d.Payload.Body != `{"k":"v"}` {
		t.Errorf("payload not set: %+v", d.Payload)
	}
}

func TestNewFSActionPathsCopied(t *testing.T) {
	paths := []string{"/a", "/b"}
	d := NewFSAction(SurfaceWorkbench, ActionFSRead, paths, CallerContext{})
	if d.Action != ActionFSRead || len(d.Payload.Paths) != 2 {
		t.Fatalf("paths not carried: %+v", d.Payload)
	}
	// Mutating the input slice must not affect the descriptor (and vice versa).
	paths[0] = "/MUTATED"
	if d.Payload.Paths[0] == "/MUTATED" {
		t.Error("descriptor aliases caller's slice; should have copied")
	}
}

func TestNewToolActionArgsCopiedShallow(t *testing.T) {
	args := map[string]any{"k": "v"}
	d := NewToolAction(SurfaceToolWebhook, "myTool", args, CallerContext{})
	if d.Payload.ToolName != "myTool" {
		t.Errorf("tool name not set: %q", d.Payload.ToolName)
	}
	// Top-level isolation: mutating the input map must not affect
	// the descriptor's args.
	args["k"] = "MUTATED"
	if d.Payload.Args["k"] == "MUTATED" {
		t.Error("descriptor aliases caller's args map; top-level should be copied")
	}
}

func TestNewToolActionNilArgs(t *testing.T) {
	d := NewToolAction(SurfaceToolWebhook, "myTool", nil, CallerContext{})
	if d.Payload.Args != nil {
		t.Errorf("nil args should stay nil, got %v", d.Payload.Args)
	}
}
