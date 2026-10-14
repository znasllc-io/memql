//go:build agent

package worker

import (
	"context"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	workerservice "github.com/znasllc-io/memql/component/worker"
	permission "github.com/znasllc-io/memql/core/computerpermission"
)

// Test-only bridge keeps the real work integration outside worker's package
// dependency graph while reusing the production forwarding harness.
type PermissionFlowHopForTest struct{ *hop }

func NewPermissionFlowHopForTest(t *testing.T, dispatch workerservice.DispatchFunc, mutate func(*Candidate)) *PermissionFlowHopForTest {
	return &PermissionFlowHopForTest{newHop(t, dispatch, mutate)}
}
func (h *PermissionFlowHopForTest) SetOwner(owner string) {
	h.owner = owner
	h.store.owner = owner
	h.registry.WorkerById("laptop").OwnerUserId = owner
}
func (h *PermissionFlowHopForTest) Origin() *Dispatcher               { return h.dispatch }
func (h *PermissionFlowHopForTest) Registry() *workerservice.Registry { return h.registry }
func (h *PermissionFlowHopForTest) Holder(a permission.Authority) {
	h.link.handler.SetComputerPermissions(a)
}
func (h *PermissionFlowHopForTest) Request() Request { return h.request() }
func PermissionFlowPersonForTest(t *testing.T, owner string) context.Context {
	return authorityCtx(t, owner)
}
func RequestScopeForTest(i *Integration, ctx context.Context, args map[string]any) ([]memorynodes.MemoryNode, error) {
	return i.handleRequestScope(ctx, args, 0)
}
