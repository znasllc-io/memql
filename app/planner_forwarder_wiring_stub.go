//go:build !planner

package app

import memqlgrpc "github.com/znasllc-io/memql/component/grpc"

// attachAgentForwarderToPlanner is a no-op on binaries that don't
// include the planner integration. The real wiring lives in
// planner_forwarder_wiring.go under the complementary build tag.
func (a *App) attachAgentForwarderToPlanner(_ *memqlgrpc.SIForwardRouter) {
}
