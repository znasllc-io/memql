package workbench

import "context"

type FleetRequest struct {
	RunID, AgentID, Action string
	Args                   map[string]any
}
type FleetResult struct {
	OK                      bool
	Payload                 any
	ErrorCode, ErrorMessage string
}
type FleetDispatcher interface {
	DispatchFleetWorkbench(context.Context, FleetRequest) (FleetResult, bool)
}

func (i *Integration) SetFleetDispatcher(d FleetDispatcher) { i.fleet = d }
