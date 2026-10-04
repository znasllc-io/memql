package app

import (
	"github.com/znasllc-io/memql/core/logger"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

// currentPipelinesLineSink is the log store, as the node installed it when a
// step's capture opens; nil before boot installs one.
//
// Untagged, because both of a step's runners capture through it: the
// workbench's runner for a cluster step (pipelines_runner_adapter.go) and the
// agent's fleet path for a step run on a machine
// (pipelines_executor_agent.go). One function, so a step's lines reach the
// same store whichever surface ran it.
func currentPipelinesLineSink() pipelinesteps.LineSink {
	if s := logger.CurrentSink(); s != nil {
		return s
	}
	return nil
}
