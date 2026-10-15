//go:build agent

package worker

import (
	"context"
	"strings"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

func TestWorkerBuiltinDoesNotCompleteAFailedForwardedEffect(t *testing.T) {
	for _, code := range []string{"denied_no_per_task_approval", "fs_list_failed", ""} {
		t.Run(code, func(t *testing.T) {
			h := newHop(t, func(_ context.Context, d *memqlv1.ToolDispatch, _ func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
				if code == "" {
					return okResult(d.GetCallId()), nil
				}
				return &memqlv1.ToolResult{CallId: d.GetCallId(), Payload: &memqlv1.ToolResult_Failure{Failure: &memqlv1.Failure{ErrorCode: code, ErrorMessage: "operation did not run"}}}, nil
			}, nil)
			i := NewIntegration(h.dispatch, h.registry, nil, testLogger())
			args := dispatchArgs(h.owner, "fs_list")
			nodes, err := i.handleDispatchHost(authorityCtx(t, h.owner), args, 0)
			if code == "" {
				if err != nil || len(nodes) != 1 {
					t.Fatalf("successful operation lost its result: %v %v", nodes, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), code) || len(nodes) != 1 {
				t.Fatalf("failed effect became a successful builtin result: %v %v", nodes, err)
			}
		})
	}
}
