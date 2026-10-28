package pipelinerun

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/pipelines"
)

// The captured installed source and immutable run facts are the only inputs.
// No operation is granted here: repository source cannot opt itself into paid
// capacity, and a replica or bundle change cannot replace an admitted policy.
// Old snapshots lack this optional phase and permit Fleet only for pool work.
func (dr *runDriver) selectComputePlacement(ctx context.Context) (*pipelines.ComputePlacementPolicy, error) {
	if dr.workflow == nil || dr.workflow.snapshot == nil || dr.workflow.snapshot.Phases["pipelineComputePlacementPolicy"] == "" {
		return &pipelines.ComputePlacementPolicy{}, nil
	}
	value, err := runPolicyPhase(ctx, dr.workflow.snapshot, "pipelineComputePlacementPolicy", map[string]any{
		"repository": dr.run.Repository, "pipelineId": dr.run.PipelineID,
		"ownerUserId": dr.run.OwnerUserID, "event": string(dr.run.Event),
	}, workflowhost.Options{})
	if err != nil {
		return nil, err
	}
	return parseComputePlacement(value)
}

func parseComputePlacement(value any) (*pipelines.ComputePlacementPolicy, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("compute policy must return a bounded placement object")
	}
	var fields struct {
		FleetWaitSeconds *int  `json:"fleetWaitSeconds"`
		CloudFallback    *bool `json:"cloudFallback"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fields); err != nil || fields.FleetWaitSeconds == nil || fields.CloudFallback == nil || *fields.FleetWaitSeconds < 0 || *fields.FleetWaitSeconds > 3600 {
		return nil, fmt.Errorf("compute policy must return fleetWaitSeconds (integer 0..3600) and cloudFallback (boolean), with no extra fields")
	}
	return &pipelines.ComputePlacementPolicy{FleetWaitSeconds: *fields.FleetWaitSeconds, CloudFallback: *fields.CloudFallback}, nil
}
