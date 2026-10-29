package pipelinerun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/pipelines"
)

// selectComputePlacement reads the run's compute placement policy, where an
// installation writes down how a run's steps are placed and how hard they are
// retried. Its two halves reach differently (pipelines.ComputePlacementPolicy):
// the capacity half only a pool step reads, the retry half every command step,
// so the driver reads the policy for every run that has a command step
// (readPlan), not only one with a pool step.
//
// The captured installed source and immutable run facts are the only inputs.
// No operation is granted here: repository source cannot opt itself into paid
// capacity or into more attempts than the installation allows, and a replica or
// bundle change cannot replace an admitted policy.
//
// A snapshot without this optional phase is a run opened before the role
// existed, or by an installation that never wrote one. It permits Fleet only for
// pool work and spends no cloud, and it takes the portable retry limits: retries
// apply to every step now, and "the installation named no limits" is exactly the
// case the defaults are for. Answering "no retries" instead would make an
// installation that deletes its policy change how its steps fail without anyone
// editing a limit.
//
// Regression tests: TestEveryRunGetsAPolicyEvenWithoutPoolSteps (the legacy
// snapshot, a plan with no pool step) and
// TestComputePlacementUsesCapturedPolicyAcrossReplicas.
func (dr *runDriver) selectComputePlacement(ctx context.Context) (*pipelines.ComputePlacementPolicy, error) {
	if dr.workflow == nil || dr.workflow.snapshot == nil || dr.workflow.snapshot.Phases["pipelineComputePlacementPolicy"] == "" {
		return &pipelines.ComputePlacementPolicy{Retries: pipelines.DefaultRetryPolicy()}, nil
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

// The bounds an installed policy's retry limits must stay inside. They are the
// second fence behind pipelines.MaxStepAttempts, which caps a step's attempts
// whatever a policy says. These keep a policy from asking for a wait longer than
// a quarter hour (a step held that long is no longer being retried, and the
// run's ceiling is spent on the hold) and make a typo (sameMachine: 30) a refusal
// naming the field, instead of a policy that reads as intent and is then
// silently clipped by the cap.
const (
	maxSameMachineRetries = 3
	maxRetryDelays        = 3
	maxRetryDelaySeconds  = 900
)

// computePolicyShape is what a policy must return, said once for every way it
// can fail to.
const computePolicyShape = "compute policy must return fleetWaitSeconds (integer 0..3600) and cloudFallback (boolean), and optionally retries, with no extra fields"

// parseComputePlacement reads the policy a template returned, strictly: every
// field typed exactly, the capacity half required, retries optional, no other
// field. A refusal is the installation's mistake and says which field (the run
// fails with it, before any effect: readPlan), because a policy that is quietly
// rounded into something its author never wrote is a spend or a retry nobody
// chose.
//
// Retries is optional so that every policy installed before it existed, which
// returns only {fleetWaitSeconds, cloudFallback}, keeps working: its absence
// means the portable limits. Once present it must be complete (parseRetryPolicy).
func parseComputePlacement(value any) (*pipelines.ComputePlacementPolicy, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("compute policy must return a bounded placement object")
	}
	var fields struct {
		FleetWaitSeconds *int  `json:"fleetWaitSeconds"`
		CloudFallback    *bool `json:"cloudFallback"`
		// Raw, so that a retries of null can be told from an absent one: the
		// decoder would read both as "no retries" if it decoded them in place.
		Retries json.RawMessage `json:"retries"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fields); err != nil {
		// The decoder's reason names the field: the key that is not part of the
		// policy, or the one whose value is the wrong type.
		return nil, fmt.Errorf("%s (%v)", computePolicyShape, err)
	}
	if fields.FleetWaitSeconds == nil || fields.CloudFallback == nil || *fields.FleetWaitSeconds < 0 || *fields.FleetWaitSeconds > 3600 {
		return nil, errors.New(computePolicyShape)
	}
	retries := pipelines.DefaultRetryPolicy()
	if fields.Retries != nil {
		retries, err = parseRetryPolicy(fields.Retries)
		if err != nil {
			return nil, err
		}
	}
	return &pipelines.ComputePlacementPolicy{FleetWaitSeconds: *fields.FleetWaitSeconds, CloudFallback: *fields.CloudFallback, Retries: retries}, nil
}

// parseRetryPolicy reads a retries object that is present. All three limits are
// required once it is: a partial block would take the rest from the engine's
// defaults, and the policy source would then no longer say how hard the
// installation retries (a block that reads "sameMachine: 0" would still fail
// over to other machines). An installation that wants the portable limits omits
// retries; one that writes it writes all of it. The members are decoded one by
// one, not into a struct, so that a JSON null (which encoding/json would read
// into an int or a bool as zero or false) is a refusal naming its field, and so
// the unknown key a refusal names is the one the policy wrote.
func parseRetryPolicy(raw json.RawMessage) (pipelines.RetryPolicy, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil || members == nil {
		return pipelines.RetryPolicy{}, fmt.Errorf("compute policy retries must be an object with sameMachine, delaysSeconds and otherMachines")
	}
	known := []string{"sameMachine", "delaysSeconds", "otherMachines"}
	for _, key := range slices.Sorted(maps.Keys(members)) {
		if !slices.Contains(known, key) {
			return pipelines.RetryPolicy{}, fmt.Errorf("compute policy retries.%s is not a retry limit; the only fields are %s", key, strings.Join(known, ", "))
		}
	}

	sameMachine, err := retryMember[int](members, "sameMachine", fmt.Sprintf("an integer 0..%d", maxSameMachineRetries))
	if err != nil {
		return pipelines.RetryPolicy{}, err
	}
	if sameMachine < 0 || sameMachine > maxSameMachineRetries {
		return pipelines.RetryPolicy{}, fmt.Errorf("compute policy retries.sameMachine must be an integer 0..%d, got %d", maxSameMachineRetries, sameMachine)
	}

	// Pointers, so that a null inside the list is seen rather than read as zero
	// seconds: a delay nobody wrote must not become "retry at once".
	listed, err := retryMember[[]*int](members, "delaysSeconds", fmt.Sprintf("a list of 1 to %d integers, each 0..%d", maxRetryDelays, maxRetryDelaySeconds))
	if err != nil {
		return pipelines.RetryPolicy{}, err
	}
	if len(listed) < 1 || len(listed) > maxRetryDelays {
		return pipelines.RetryPolicy{}, fmt.Errorf("compute policy retries.delaysSeconds must list 1 to %d delays, got %d", maxRetryDelays, len(listed))
	}
	delays := make([]int, len(listed))
	for i, delay := range listed {
		if delay == nil || *delay < 0 || *delay > maxRetryDelaySeconds {
			return pipelines.RetryPolicy{}, fmt.Errorf("compute policy retries.delaysSeconds[%d] must be an integer 0..%d", i, maxRetryDelaySeconds)
		}
		delays[i] = *delay
	}

	otherMachines, err := retryMember[bool](members, "otherMachines", "a boolean")
	if err != nil {
		return pipelines.RetryPolicy{}, err
	}
	return pipelines.RetryPolicy{SameMachine: sameMachine, DelaysSeconds: delays, OtherMachines: otherMachines}, nil
}

// retryMember decodes one required member of a retries object. The decode goes
// through a pointer because encoding/json reads a JSON null into a plain value
// as that type's zero, with no error: a policy author's null would otherwise
// arrive as "no retries" or "never fail over" and look like a decision.
func retryMember[T any](members map[string]json.RawMessage, name, shape string) (T, error) {
	var zero T
	raw, present := members[name]
	if !present {
		return zero, fmt.Errorf("compute policy retries.%s is required when retries is given: %s", name, shape)
	}
	var value *T
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return zero, fmt.Errorf("compute policy retries.%s must be %s", name, shape)
	}
	return *value, nil
}
