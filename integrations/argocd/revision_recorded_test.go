package argocd

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// laterSync replaces the intent's own finished operation in operationState
// with an operator's hand sync, the way Argo keeps only its last operation.
func laterSync(revision string) func(map[string]any) {
	return func(obj map[string]any) {
		source := object(object(obj, "spec"), "source")
		object(obj, "status")["operationState"] = map[string]any{
			"phase":      "Succeeded",
			"operation":  map[string]any{"initiatedBy": map[string]any{"username": "admin"}, "sync": map[string]any{"revision": revision}},
			"syncResult": map[string]any{"revision": revision, "source": source},
		}
	}
}

// succeededThenSyncedAgain applies one intent, finishes its own operation
// Succeeded, and then lets a hand sync of the same revision replace it.
func succeededThenSyncedAgain(t *testing.T) (*apiFixture, *Client, Intent) {
	t.Helper()
	f := newFixture(t)
	c := f.client(t)
	intent := fixtureIntent(t, c)
	_, err := c.Apply(context.Background(), intent)
	require.NoError(t, err)
	f.change(t, func(obj map[string]any) { finishOperation(obj, "Succeeded") })
	facts, err := c.Observe(context.Background(), intent)
	require.NoError(t, err)
	require.True(t, facts.OperationSucceeded, "the plan's own success is what a journal records")
	f.change(t, laterSync(testRevision))
	return f, c, intent
}

func recorded(t *testing.T, intents ...Intent) context.Context {
	t.Helper()
	ctx, err := WithRecordedSuccess(context.Background(), intents...)
	require.NoError(t, err)
	return ctx
}

// Fix for completion stranding after a later sync: Argo keeps only the last
// operation, so one hand sync of the same revision erased the plan's own
// success from operationState. A journal's record of that observed success,
// with the Application still holding the plan's intent, revision and source,
// lets the success count again; without the record it does not.
func TestRecordedOwnSuccessSurvivesALaterSyncOfTheSameRevision(t *testing.T) {
	_, c, intent := succeededThenSyncedAgain(t)
	facts, err := c.Observe(context.Background(), intent)
	require.NoError(t, err)
	require.False(t, facts.OperationSucceeded, "operationState no longer holds the plan's operation")
	require.Empty(t, facts.OperationPhase)

	facts, err = c.Observe(recorded(t, intent), intent)
	require.NoError(t, err)
	require.True(t, facts.OperationSucceeded)
	require.Equal(t, "Succeeded", facts.OperationPhase)
	require.True(t, facts.IntentObserved && facts.RevisionObserved && facts.Synced && facts.Healthy)
	require.False(t, facts.OperationPending)

	other := intent
	other.RequestID = "another-plan"
	facts, err = c.Observe(recorded(t, other), intent)
	require.NoError(t, err)
	require.False(t, facts.OperationSucceeded, "a record of another intent is no evidence for this one")
}

// movedOnCases are states a record must not vouch for: the Application moved
// to another revision or spec, its later sync is queued, running, failed or
// synced another revision or source, or Argo compares anything but the plan's
// revision and source. Each leaves the plan's own success unproven.
func movedOnCases() map[string]struct {
	change func(map[string]any)
	err    error
} {
	return map[string]struct {
		change func(map[string]any)
		err    error
	}{
		"spec moved to another revision": {change: func(obj map[string]any) {
			object(object(obj, "spec"), "source")["targetRevision"] = strings.Repeat("c", 40)
		}, err: ErrChanged},
		"one-off sync to another commit": {change: laterSync(strings.Repeat("c", 40))},
		"later sync of the same revision from another source": {change: func(obj map[string]any) {
			result := object(object(object(obj, "status"), "operationState"), "syncResult")
			result["source"] = map[string]any{"repoURL": "https://github.com/example/elsewhere.git", "path": "deploy/instance", "targetRevision": testRevision}
		}},
		"later sync failed at the same revision": {change: func(obj map[string]any) {
			object(object(obj, "status"), "operationState")["phase"] = "Failed"
		}},
		"later sync reported no result": {change: func(obj map[string]any) {
			delete(object(object(obj, "status"), "operationState"), "syncResult")
		}},
		"later sync still queued": {change: func(obj map[string]any) {
			obj["operation"] = map[string]any{"initiatedBy": map[string]any{"username": "admin"}, "sync": map[string]any{"revision": testRevision}}
		}, err: ErrBusy},
		"later sync running": {change: func(obj map[string]any) {
			object(object(obj, "status"), "operationState")["phase"] = "Running"
		}},
		"later sync has no phase yet": {change: func(obj map[string]any) {
			delete(object(object(obj, "status"), "operationState"), "phase")
		}},
		"Argo compares another revision": {change: func(obj map[string]any) {
			object(object(obj, "status"), "sync")["revision"] = strings.Repeat("c", 40)
		}},
		"Argo compares another source": {change: func(obj map[string]any) {
			object(object(object(obj, "status"), "sync"), "comparedTo")["source"] = map[string]any{"repoURL": "elsewhere"}
		}},
		"another intent's marker": {change: func(obj map[string]any) {
			object(object(obj, "metadata"), "annotations")[intentAnnotation] = "another-intent"
		}, err: ErrChanged},
	}
}

// The record never outweighs the current Application: in every moved-on
// state the plan's own operation reads as unproven, with no phase at all.
func TestRecordedOwnSuccessRefusesAnApplicationThatMovedOn(t *testing.T) {
	for name, tc := range movedOnCases() {
		t.Run(name, func(t *testing.T) {
			f, c, intent := succeededThenSyncedAgain(t)
			f.change(t, tc.change)
			facts, err := c.Observe(recorded(t, intent), intent)
			if tc.err != nil {
				require.ErrorIs(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			require.False(t, facts.OperationSucceeded, "a record cannot vouch for a different current state")
			require.Empty(t, facts.OperationPhase)
			require.Empty(t, facts.SuccessEvidence)
		})
	}
}

// Planning a reversal needs no Synced status, so the same states must refuse
// it on their own conditions.
func TestPlanRollbackRefusesARecordedSuccessOverAnApplicationThatMovedOn(t *testing.T) {
	for name, tc := range movedOnCases() {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.change(t, func(obj map[string]any) {
				object(object(obj, "spec"), "source")["targetRevision"] = strings.Repeat("b", 40)
			})
			c := f.client(t)
			prior := fixtureIntent(t, c)
			_, err := c.Apply(context.Background(), prior)
			require.NoError(t, err)
			f.change(t, func(obj map[string]any) { finishOperation(obj, "Succeeded") })
			f.change(t, laterSync(testRevision))
			_, history := historyClient(t, f)
			_, err = history.PlanRollback(recorded(t, prior), prior, "explicit-rollback")
			require.NoError(t, err, "the unchanged state is reversible with the record")
			f.change(t, tc.change)
			_, err = history.PlanRollback(recorded(t, prior), prior, "explicit-rollback")
			require.Error(t, err)
			require.Equal(t, 1, f.writeCount(), "planning writes nothing")
		})
	}
}

// The OutOfSync and unhealthy states keep their own meaning: the record says
// only that the plan's operation succeeded, never that the Application is
// converged now.
func TestRecordedOwnSuccessDoesNotReportConvergence(t *testing.T) {
	f, c, intent := succeededThenSyncedAgain(t)
	f.change(t, func(obj map[string]any) {
		object(object(obj, "status"), "sync")["status"] = "OutOfSync"
		object(object(obj, "status"), "health")["status"] = "Progressing"
	})
	facts, err := c.Observe(recorded(t, intent), intent)
	require.NoError(t, err)
	require.True(t, facts.OperationSucceeded)
	require.False(t, facts.Synced)
	require.False(t, facts.Healthy)
}

// Restoring needs the forward operation's terminal outcome too. A later sync
// of the same revision used to refuse PlanRollback forever; with the record it
// plans the exact reversal, and a later sync elsewhere still refuses.
func TestPlanRollbackAcceptsARecordedSuccessAfterALaterSync(t *testing.T) {
	f := newFixture(t)
	f.change(t, func(obj map[string]any) {
		object(object(obj, "spec"), "source")["targetRevision"] = strings.Repeat("b", 40)
	})
	c := f.client(t)
	prior := fixtureIntent(t, c)
	_, err := c.Apply(context.Background(), prior)
	require.NoError(t, err)
	f.change(t, func(obj map[string]any) { finishOperation(obj, "Succeeded") })
	f.change(t, laterSync(testRevision))
	_, history := historyClient(t, f)
	_, err = history.PlanRollback(context.Background(), prior, "explicit-rollback")
	require.ErrorContains(t, err, "terminal outcome", "without a record the replaced operation proves nothing")

	reversal, err := history.PlanRollback(recorded(t, prior), prior, "explicit-rollback")
	require.NoError(t, err)
	require.NoError(t, ValidateRollback(prior, reversal))
	require.Equal(t, strings.Repeat("b", 40), reversal.Revision)

	f.change(t, laterSync(strings.Repeat("c", 40)))
	_, err = history.PlanRollback(recorded(t, prior), prior, "explicit-rollback")
	require.Error(t, err, "a later sync to another commit is not the plan's state to reverse")
	require.Equal(t, 1, f.writeCount(), "planning writes nothing")
}

func TestRecordedSuccessRequiresValidIntents(t *testing.T) {
	_, err := WithRecordedSuccess(context.Background(), Intent{})
	require.Error(t, err)
	ctx, err := WithRecordedSuccess(context.Background())
	require.NoError(t, err)
	require.Empty(t, successEvidenceIn(ctx))
	_, err = WithDeploymentHistory(context.Background(), time.Time{}, Intent{})
	require.Error(t, err, "history needs the intent's start time")
}

// deployed appends one entry to status.history, as Argo does for each sync that
// Succeeded, with the source the Application held.
func deployed(id int, revision string, at time.Time) func(map[string]any) {
	return func(obj map[string]any) {
		status := object(obj, "status")
		history, _ := status["history"].([]any)
		source := object(object(obj, "spec"), "source")
		status["history"] = append(history, map[string]any{"id": json.Number(strconv.Itoa(id)), "revision": revision, "source": source, "deployedAt": at.UTC().Format(time.RFC3339)})
	}
}

// A reversal whose own sync Succeeded and was then replaced by a hand sync
// before any read saw it can still be proved, from Argo's deployment history:
// the newest deployment, after the reversal started, is exactly its revision
// and source. Without that evidence the success stays unproven.
func TestDeploymentHistoryProvesAReplacedSuccess(t *testing.T) {
	start := time.Now().Add(-time.Minute)
	setup := func(t *testing.T) (*apiFixture, *Client, Intent) {
		f := newFixture(t)
		c := f.client(t)
		intent := fixtureIntent(t, c)
		_, err := c.Apply(context.Background(), intent)
		require.NoError(t, err)
		f.change(t, func(obj map[string]any) { finishOperation(obj, "Succeeded") })
		f.change(t, deployed(7, testRevision, start.Add(10*time.Second)))
		f.change(t, laterSync(testRevision))
		f.change(t, deployed(8, testRevision, start.Add(20*time.Second)))
		return f, c, intent
	}
	history := func(t *testing.T, intent Intent, since time.Time) context.Context {
		ctx, err := WithDeploymentHistory(context.Background(), since, intent)
		require.NoError(t, err)
		return ctx
	}
	t.Run("proved", func(t *testing.T) {
		_, c, intent := setup(t)
		facts, err := c.Observe(context.Background(), intent)
		require.NoError(t, err)
		require.False(t, facts.OperationSucceeded, "no evidence, no success")
		facts, err = c.Observe(history(t, intent, start), intent)
		require.NoError(t, err)
		require.True(t, facts.OperationSucceeded)
		require.Equal(t, "Succeeded", facts.OperationPhase)
		require.Equal(t, SuccessDeployed, facts.SuccessEvidence)
		facts, err = c.Observe(recorded(t, intent), intent)
		require.NoError(t, err)
		require.Equal(t, SuccessRecorded, facts.SuccessEvidence)
	})
	for name, change := range map[string]func(map[string]any){
		"newest deployment is another revision": deployed(9, strings.Repeat("c", 40), start.Add(30*time.Second)),
		"newest deployment is another source": func(obj map[string]any) {
			deployed(9, testRevision, start.Add(30*time.Second))(obj)
			history := object(obj, "status")["history"].([]any)
			history[len(history)-1].(map[string]any)["source"] = map[string]any{"repoURL": "elsewhere"}
		},
		"newest deployment predates the start": func(obj map[string]any) {
			object(obj, "status")["history"] = []any{map[string]any{"id": json.Number("1"), "revision": testRevision, "source": object(object(obj, "spec"), "source"), "deployedAt": start.Add(-time.Hour).UTC().Format(time.RFC3339)}}
		},
		"no history":                 func(obj map[string]any) { delete(object(obj, "status"), "history") },
		"later sync failed":          func(obj map[string]any) { object(object(obj, "status"), "operationState")["phase"] = "Failed" },
		"history entry is no object": func(obj map[string]any) { object(obj, "status")["history"] = []any{"deployed"} },
	} {
		t.Run(name, func(t *testing.T) {
			f, c, intent := setup(t)
			f.change(t, change)
			facts, err := c.Observe(history(t, intent, start), intent)
			require.NoError(t, err)
			require.False(t, facts.OperationSucceeded)
			require.Empty(t, facts.OperationPhase)
		})
	}
	t.Run("another intent's history", func(t *testing.T) {
		_, c, intent := setup(t)
		other := intent
		other.RequestID = "another-reversal"
		facts, err := c.Observe(history(t, other, start), intent)
		require.NoError(t, err)
		require.False(t, facts.OperationSucceeded)
	})
}

// Admit is the write-free form of Apply's precondition. A format 1 intent
// keeps its strict fence there, so a refusal can be met before anything is
// recorded as about to be written.
func TestAdmitChecksTheStrictFenceWithoutWriting(t *testing.T) {
	f := newFixture(t)
	c := f.client(t)
	intent := fixtureIntent(t, c)
	proof, err := c.Admit(context.Background(), intent)
	require.NoError(t, err)
	require.Nil(t, proof, "a format 1 intent's patch needs no history proof")
	f.change(t, func(obj map[string]any) { object(obj, "metadata")["generation"] = json.Number("8") })
	_, err = c.Admit(context.Background(), intent)
	require.ErrorIs(t, err, ErrChanged, "any generation change refuses a format 1 intent")
	f.change(t, func(obj map[string]any) {
		object(obj, "metadata")["generation"] = json.Number("7")
		obj["operation"] = map[string]any{"sync": map[string]any{}}
	})
	_, err = c.Admit(context.Background(), intent)
	require.ErrorIs(t, err, ErrBusy)
	f.change(t, func(obj map[string]any) { delete(obj, "operation") })
	_, err = c.Apply(context.Background(), intent)
	require.NoError(t, err)
	proof, err = c.Admit(context.Background(), intent)
	require.NoError(t, err, "an applied intent is reconciled, not refused")
	require.Nil(t, proof)
	require.Equal(t, 1, f.writeCount(), "admitting writes nothing")
}

// Superseded proves a format 1 intent can never land: its baseline is intact
// but the generation its patch tests is gone. Anything else is not proof.
func TestSupersededNeedsTheExactBaselineAtALaterGeneration(t *testing.T) {
	f := newFixture(t)
	c := f.client(t)
	intent := fixtureIntent(t, c)
	superseded, err := c.Superseded(context.Background(), intent)
	require.NoError(t, err)
	require.False(t, superseded, "the intent can still land at its own generation")
	f.change(t, func(obj map[string]any) { object(obj, "metadata")["generation"] = json.Number("8") })
	superseded, err = c.Superseded(context.Background(), intent)
	require.NoError(t, err)
	require.True(t, superseded)
	f.change(t, func(obj map[string]any) { object(object(obj, "spec"), "source")["path"] = "deploy/elsewhere" })
	superseded, err = c.Superseded(context.Background(), intent)
	require.NoError(t, err)
	require.False(t, superseded, "a changed baseline is not this intent's to account for")

	_, history := historyClient(t, newFixture(t))
	snapshot, err := history.ReadPlanning(context.Background(), testTarget)
	require.NoError(t, err)
	planned, err := PlanRevision(snapshot, "history-request", testRevision, false)
	require.NoError(t, err)
	superseded, err = history.Superseded(context.Background(), planned)
	require.NoError(t, err)
	require.False(t, superseded, "a history-backed intent's patch version is not retained")
}
