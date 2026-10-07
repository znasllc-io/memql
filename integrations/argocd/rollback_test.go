package argocd

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func rollbackFixture(t *testing.T) (*apiFixture, *Client, Intent) {
	t.Helper()
	f := newFixture(t)
	f.change(t, func(obj map[string]any) {
		object(object(obj, "spec"), "source")["targetRevision"] = strings.Repeat("b", 40)
	})
	c := f.client(t)
	prior := fixtureIntent(t, c)
	_, err := c.Apply(context.Background(), prior)
	require.NoError(t, err)
	return f, c, prior
}

func TestRollbackPlansExactTerminalReversalAndRecoversLostReply(t *testing.T) {
	for _, phase := range []string{"Succeeded", "Failed", "Error"} {
		t.Run(phase, func(t *testing.T) {
			f, c, prior := rollbackFixture(t)
			f.change(t, func(obj map[string]any) { finishOperation(obj, phase) })
			rollback, err := c.PlanRollback(context.Background(), prior, "explicit-rollback")
			require.NoError(t, err)
			require.NoError(t, ValidateRollback(prior, rollback))
			require.Equal(t, strings.Repeat("b", 40), rollback.Revision)
			require.Equal(t, 1, f.writeCount(), "planning must not write")
			f.configure(true, false)
			_, err = c.Apply(context.Background(), rollback)
			require.Error(t, err)
			body, err := json.Marshal(rollback)
			require.NoError(t, err)
			restored, err := DecodeIntent(body)
			require.NoError(t, err)
			fresh := f.client(t)
			facts, err := fresh.Apply(context.Background(), restored)
			require.NoError(t, err)
			require.True(t, facts.IntentObserved)
			require.Equal(t, 2, f.writeCount())
			_, err = c.Apply(context.Background(), prior)
			require.ErrorIs(t, err, ErrChanged)
			require.Equal(t, 2, f.writeCount())
		})
	}
}

func TestRollbackRefusesUnownedUncertainOrChangedApplication(t *testing.T) {
	for _, fault := range []string{"pending", "running", "terminating", "missing state", "foreign state", "unknown phase", "changed source", "changed uid", "changed marker", "mutable baseline", "same request"} {
		t.Run(fault, func(t *testing.T) {
			f, c, prior := rollbackFixture(t)
			f.change(t, func(obj map[string]any) {
				if fault != "pending" {
					finishOperation(obj, "Succeeded")
				}
				state := object(object(obj, "status"), "operationState")
				switch fault {
				case "running":
					state["phase"] = "Running"
				case "terminating":
					state["phase"] = "Terminating"
				case "missing state":
					delete(object(obj, "status"), "operationState")
				case "foreign state":
					object(object(state, "operation"), "sync")["revision"] = strings.Repeat("f", 40)
				case "unknown phase":
					state["phase"] = "Unknown"
				case "changed source":
					object(object(obj, "spec"), "source")["path"] = "foreign"
				case "changed uid":
					object(obj, "metadata")["uid"] = "replacement"
				case "changed marker":
					object(object(obj, "metadata"), "annotations")[intentAnnotation] = "foreign"
				case "mutable baseline":
					before, _ := decodeObject(prior.BeforeSpec)
					object(before, "source")["targetRevision"] = "main"
					prior.BeforeSpec, _ = canonical(before)
				}
			})
			request := "explicit-rollback"
			if fault == "same request" {
				request = prior.RequestID
			}
			_, err := c.PlanRollback(context.Background(), prior, request)
			require.Error(t, err)
			require.Equal(t, 1, f.writeCount())
		})
	}
}

func TestRollbackRelationRefusesSubstitution(t *testing.T) {
	f, c, prior := rollbackFixture(t)
	f.change(t, func(obj map[string]any) { finishOperation(obj, "Succeeded") })
	reversal, err := c.PlanRollback(context.Background(), prior, "explicit-rollback")
	require.NoError(t, err)
	for _, fault := range []string{"target", "source", "marker", "prune", "revision", "generation"} {
		t.Run(fault, func(t *testing.T) {
			changed := reversal
			switch fault {
			case "target":
				changed.Target.Name = "other"
			case "source":
				before, _ := decodeObject(changed.BeforeSpec)
				object(before, "source")["path"] = "other"
				changed.BeforeSpec, _ = canonical(before)
			case "marker":
				changed.BeforeMarker = "other"
			case "prune":
				changed.Prune = !changed.Prune
			case "revision":
				changed.Revision = strings.Repeat("c", 40)
			case "generation":
				changed.BeforeGeneration = prior.BeforeGeneration - 1
			}
			require.Error(t, ValidateRollback(prior, changed))
		})
	}
}
