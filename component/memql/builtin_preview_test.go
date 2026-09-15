package memql

import (
	"context"
	"strings"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

func TestBuiltinPreviewRefusesEffectsBeforeHandler(t *testing.T) {
	for _, executor := range []string{"integration.mail.send", BuiltinExecutorFleetModelPull, "unknown.executor", BuiltinExecutorContentId} {
		t.Run(executor, func(t *testing.T) {
			reached := 0
			engine := &MemQLEngine{functions: newFunctionRegistry(), builtinExecutorHandlers: map[string]builtinExecutorHandler{
				executor: func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error) {
					reached++
					return nil, nil
				},
			}}
			call := &BuiltinFunctionExpression{Name: "probe", Executor: executor}
			ctx, cancel := context.WithCancel(WithBuiltinPreview(context.Background()))
			defer cancel()
			_, err := engine.evaluateBuiltinFunctionExpression(ctx, call, 0)
			if executor == BuiltinExecutorContentId {
				if err != nil || reached != 1 {
					t.Fatalf("pure builtin: reached=%d, err=%v", reached, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "dry-run refused") || reached != 0 {
				t.Fatalf("effect reached=%d, err=%v", reached, err)
			}
			before := reached
			if _, err := engine.evaluateBuiltinFunctionExpression(context.Background(), call, 0); err != nil {
				t.Fatal(err)
			}
			if reached != before+1 {
				t.Fatal("ordinary execution did not reach the handler")
			}
		})
	}
}
