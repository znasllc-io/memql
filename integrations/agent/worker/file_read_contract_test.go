//go:build agent

package worker

import (
	"context"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	workerservice "github.com/znasllc-io/memql/component/worker"
	permission "github.com/znasllc-io/memql/core/computerpermission"
)

func TestExtendedReadContractRecheckedAcrossReplicaHop(t *testing.T) {
	for _, advertised := range []bool{false, true} {
		for _, connected := range []bool{false, true} {
			sent := false
			h := newHop(t, func(_ context.Context, d *memqlv1.ToolDispatch, _ func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
				sent = true
				return okResult(d.GetCallId()), nil
			}, func(c *Candidate) {
				if advertised {
					c.ActionContracts = workerservice.ActionContracts{permission.ReadContract: permission.ReadVersion}
				}
			})
			if connected {
				h.registry.WorkerById("laptop").CapabilityDescriptor = &workerservice.CapabilityDescriptor{ActionContracts: workerservice.ActionContracts{permission.ReadContract: permission.ReadVersion}}
			}
			req := h.request()
			req.Tool = "workerHost"
			req.Action = "fs_read"
			req.Args = map[string]any{"path": "/docs/test.go", "startLine": 2, "lineCount": 4}
			result, err := h.dispatch.Dispatch(authorityCtx(t, h.owner), req)
			if err != nil {
				t.Fatal(err)
			}
			want := advertised && connected
			if sent != want || result.OK != want {
				t.Fatalf("advertised=%v connected=%v sent=%v result=%+v", advertised, connected, sent, result)
			}
		}
	}
}
