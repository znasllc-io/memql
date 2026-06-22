//go:build agent || planner

package app

import (
	memqlgrpc "github.com/znasllc-io/memql/component/grpc"
	"github.com/znasllc-io/memql/component/node"
)

// attachAgentForwarderToCognition is a no-op on binaries that don't
// include the cognition integration (agent, planner). Real wiring lives
// in cognition_forwarder_wiring.go under the complementary build tag.
func (a *App) attachAgentForwarderToCognition(_ *memqlgrpc.AiForwardRouter) {
}

// attachClientToolResultClientToCognition is a no-op on binaries without the
// cognition integration (agent, planner). Real wiring lives under the
// complementary build tag.
func (a *App) attachClientToolResultClientToCognition(_ *node.ClientToolResultClient) {
}
