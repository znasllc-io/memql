package calls

import (
	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	"testing"
	"time"
)

func TestWorkflowMetadataProjectionKeepsDeclarationsHostOnly(t *testing.T) {
	d := workflowReadinessDefinition()
	d.Languages = []string{"en"}
	d.Materials = []callrules.WorkflowMaterial{{ID: "private", Label: "Private interviewer rubric"}}
	d.Inputs = []callrules.WorkflowField{{ID: "topic", Label: "Private topic", Type: "text"}}
	d.Outputs = []callrules.WorkflowField{{ID: "decision", Label: "Private permitted output", Type: "boolean"}}
	r := callrules.WorkflowRun{ID: "metadata-run", Definition: d, Revision: 1, ControlEpoch: 1}
	now := time.Now()
	workflowEnter(&r, "check", now)
	guest := workflowProjection(r, seatRow{ID: "guest"}, false, now)
	require.Equal(t, []string{"en"}, guest.Definition.Languages)
	require.Nil(t, guest.Definition.Materials)
	require.Nil(t, guest.Definition.Inputs)
	require.Nil(t, guest.Definition.Outputs)
	host := workflowProjection(r, seatRow{ID: "host", Host: true}, false, now)
	require.Equal(t, d.Materials, host.Definition.Materials)
	require.Equal(t, d.Inputs, host.Definition.Inputs)
	require.Equal(t, d.Outputs, host.Definition.Outputs)
	require.Equal(t, d.Materials, r.Definition.Materials, "guest projection must not mutate persisted metadata")
}
