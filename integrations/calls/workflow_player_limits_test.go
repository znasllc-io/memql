package calls

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
)

func TestWorkflowExtensionFitsFundedRemainingTime(t *testing.T) {
	now := time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name                       string
		duration, elapsed, seconds int64
		remaining                  time.Duration
		invalid, refused           bool
	}{
		{name: "exact boundary", duration: 120, elapsed: 20, seconds: 30, remaining: 130 * time.Second},
		{name: "one second beyond", duration: 120, elapsed: 20, seconds: 31, remaining: 130 * time.Second, refused: true},
		{name: "fraction rounded down", duration: 120, elapsed: 20, seconds: 31, remaining: 130*time.Second + 999*time.Millisecond, refused: true},
		{name: "reduces overtime", duration: 120, elapsed: 200, seconds: 70, remaining: time.Second},
		{name: "overtime to boundary", duration: 120, elapsed: 200, seconds: 81, remaining: time.Second},
		{name: "deadline passed", duration: 120, elapsed: 200, seconds: 1, remaining: 0, refused: true},
		{name: "maximum step", duration: 14399, elapsed: 14399, seconds: 2, remaining: time.Hour, invalid: true},
		{name: "bounded request", duration: 0, seconds: 3601, remaining: 2 * time.Hour, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := workflowValidateExtension(tc.duration, tc.elapsed, tc.seconds, now.Add(tc.remaining), now)
			if tc.invalid {
				require.ErrorIs(t, err, callrules.ErrInvalid)
			} else if tc.refused {
				require.ErrorContains(t, err, "workflow_extension_exceeds_call_deadline")
			} else {
				require.NoError(t, err)
			}
		})
	}
	require.ErrorContains(t, workflowValidateExtension(0, 0, 1, time.Time{}, now), "workflow_extension_exceeds_call_deadline")
}

func TestWorkflowChangeCheckpointFreezesEveryClock(t *testing.T) {
	now := time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)
	r := callrules.WorkflowRun{ID: "change-run", Definition: workflowParallelDefinition(), Revision: 4, ControlEpoch: 2}
	workflowEnter(&r, "fork", now)
	oldVisit := r.VisitID
	workflowPauseForChange(&r, false, now.Add(12*time.Second))
	require.Equal(t, "paused", r.Status)
	require.Equal(t, "human", r.Hold)
	require.EqualValues(t, 5, r.Revision)
	require.EqualValues(t, 3, r.ControlEpoch)
	require.Equal(t, oldVisit, r.VisitID)
	for _, b := range r.Parallel.Branches {
		require.True(t, b.AnchorAt.IsZero())
		require.EqualValues(t, 12, workflowBranchElapsed(&r, b, now.Add(time.Hour)))
	}
	require.Equal(t, "change_paused", r.Events[len(r.Events)-1].Type)
}

func TestWorkflowBranchOvertimeActivityStablePrivateAndReadOnly(t *testing.T) {
	now := time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)
	r := callrules.WorkflowRun{ID: "overtime-run", Definition: workflowParallelDefinition(), Bindings: map[string]string{"speaker": "guest"}}
	workflowEnter(&r, "fork", now)
	before, err := json.Marshal(r)
	require.NoError(t, err)
	overtime := func(v callrules.WorkflowRun) []callrules.WorkflowEvent {
		var events []callrules.WorkflowEvent
		for _, e := range v.Events {
			if e.Type == "overtime" {
				events = append(events, e)
			}
		}
		return events
	}
	host := overtime(workflowProjection(r, seatRow{Host: true}, false, now.Add(45*time.Second)))
	require.Len(t, host, 2)
	later := overtime(workflowProjection(r, seatRow{Host: true}, false, now.Add(46*time.Second)))
	require.Len(t, later, 2)
	for i := range host {
		require.Equal(t, host[i].ID, later[i].ID)
		require.Equal(t, r.VisitID, host[i].ParallelVisitID)
		require.Equal(t, r.Parallel.Branches[i].ID, host[i].BranchID)
	}
	guest := overtime(workflowProjection(r, seatRow{ID: "guest"}, false, now.Add(45*time.Second)))
	require.Len(t, guest, 1, "host-only branch overtime must not leak through Activity")
	require.Equal(t, "second", guest[0].BranchID)
	after, err := json.Marshal(r)
	require.NoError(t, err)
	require.Equal(t, before, after)
	workflowFreeze(&r, now.Add(45*time.Second))
	r.Status = "paused"
	frozen := overtime(workflowProjection(r, seatRow{Host: true}, false, now.Add(time.Hour)))
	require.Equal(t, host[1].Message, frozen[1].Message)
	r.Parallel.Branches[1].DurationSeconds = 60
	require.Len(t, overtime(workflowProjection(r, seatRow{Host: true}, false, now.Add(time.Hour))), 1, "extension removes resolved advisory overtime")
}
