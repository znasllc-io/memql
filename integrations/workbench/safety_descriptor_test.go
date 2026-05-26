package workbench

import (
	"reflect"
	"testing"

	"github.com/znasllc-io/memql/component/safety"
)

func TestBuildSafetyDescriptor_ExecCarriesCommandAndPlanId(t *testing.T) {
	got := buildSafetyDescriptor("exec", "plan-1", map[string]any{"command": "ls -la"})
	if got.Surface != safety.SurfaceWorkbench {
		t.Errorf("surface: got %q", got.Surface)
	}
	if got.Action != safety.ActionExec {
		t.Errorf("action: got %q", got.Action)
	}
	if got.Payload.Command != "ls -la" {
		t.Errorf("command: got %q", got.Payload.Command)
	}
	if got.Caller.PlanID != "plan-1" || got.Caller.Capability != "workbench_use" {
		t.Errorf("caller: %+v", got.Caller)
	}
}

func TestBuildSafetyDescriptor_FSAndHTTP(t *testing.T) {
	read := buildSafetyDescriptor("fs_read", "p", map[string]any{"path": "/etc/shadow"})
	if read.Action != safety.ActionFSRead || !reflect.DeepEqual(read.Payload.Paths, []string{"/etc/shadow"}) {
		t.Errorf("fs_read: %+v", read)
	}
	httpReq := buildSafetyDescriptor("http_fetch", "p", map[string]any{
		"method": "POST", "url": "https://x", "body": "b",
	})
	if httpReq.Action != safety.ActionHTTPFetch || httpReq.Payload.URL != "https://x" || httpReq.Payload.Method != "POST" || httpReq.Payload.Body != "b" {
		t.Errorf("http_fetch: %+v", httpReq)
	}
}

func TestBuildSafetyDescriptor_PathsListShape(t *testing.T) {
	got := buildSafetyDescriptor("fs_read", "p", map[string]any{"paths": []any{"/a", "/b"}})
	if !reflect.DeepEqual(got.Payload.Paths, []string{"/a", "/b"}) {
		t.Errorf("paths: %#v", got.Payload.Paths)
	}
}

func TestBuildSafetyDescriptor_UnknownActionPreservesArgs(t *testing.T) {
	got := buildSafetyDescriptor("weird", "p", map[string]any{"x": 1})
	if string(got.Action) != "weird" {
		t.Errorf("action: got %q", got.Action)
	}
	if got.Payload.Args["x"] != 1 {
		t.Errorf("args dropped on unknown action: %+v", got.Payload.Args)
	}
}
