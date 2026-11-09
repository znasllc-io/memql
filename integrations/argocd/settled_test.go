package argocd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func requireSettled(t *testing.T, c *Client, intent Intent, want bool) {
	t.Helper()
	settled, err := c.OperationSettled(context.Background(), intent)
	require.NoError(t, err)
	require.Equal(t, want, settled)
}

// A close asks only whether this intent's own operation can still act. Neither
// an Application that never received the write nor one a hand deploy moved on
// holds such an operation; a queued, running or terminating one does.
func TestOperationSettledFollowsOnlyTheIntentsOwnOperation(t *testing.T) {
	f := newFixture(t)
	c := f.client(t)
	intent := fixtureIntent(t, c)
	requireSettled(t, c, intent, true) // The fixture's earlier foreign sync is not this intent's.

	_, err := c.Apply(context.Background(), intent)
	require.NoError(t, err)
	requireSettled(t, c, intent, false) // Queued: Argo has not taken it yet.

	f.change(t, func(obj map[string]any) {
		object(obj, "status")["operationState"] = map[string]any{"phase": "Running", "operation": object(obj, "operation")}
	})
	requireSettled(t, c, intent, false)
	f.change(t, func(obj map[string]any) {
		delete(obj, "operation")
		object(object(obj, "status"), "operationState")["phase"] = "Terminating"
	})
	requireSettled(t, c, intent, false) // Still owned and not at a terminal phase.
	f.change(t, func(obj map[string]any) { object(object(obj, "status"), "operationState")["phase"] = "" })
	requireSettled(t, c, intent, false)

	for _, phase := range []string{"Failed", "Error", "Succeeded"} {
		f.change(t, func(obj map[string]any) { object(object(obj, "status"), "operationState")["phase"] = phase })
		requireSettled(t, c, intent, true)
	}

	// A hand deploy moves the spec on and runs its own sync. Nothing of the
	// intent is pending, though the Application no longer matches it.
	f.change(t, func(obj map[string]any) {
		object(object(obj, "spec"), "source")["targetRevision"] = "9999999999999999999999999999999999999999"
		obj["operation"] = map[string]any{"initiatedBy": map[string]any{"username": "instance.argocd-sync"}, "sync": map[string]any{"revision": "9999999999999999999999999999999999999999"}}
		object(obj, "status")["operationState"] = map[string]any{"phase": "Running", "operation": obj["operation"]}
	})
	_, err = c.Observe(context.Background(), intent)
	require.ErrorIs(t, err, ErrChanged)
	requireSettled(t, c, intent, true)
}

// Ownership errs towards refusing: an operation carrying the intent's digest
// counts as the intent's even when its other fields differ from what Apply
// writes. A replaced Application holds nothing of the intent.
func TestOperationSettledTreatsDigestCarriersAsOwnedAndReplacementsAsSettled(t *testing.T) {
	f := newFixture(t)
	c := f.client(t)
	intent := fixtureIntent(t, c)
	digest, err := intent.Digest()
	require.NoError(t, err)
	f.change(t, func(obj map[string]any) {
		obj["operation"] = map[string]any{"sync": map[string]any{"revision": "9999999999999999999999999999999999999999", "prune": true},
			"info": []any{map[string]any{"name": "other", "value": "x"}, map[string]any{"name": operationInfoName, "value": digest}}}
	})
	requireSettled(t, c, intent, false)
	f.change(t, func(obj map[string]any) {
		obj["operation"] = map[string]any{"sync": map[string]any{"revision": testRevision}, "info": []any{map[string]any{"name": operationInfoName, "value": "memql-id:another"}}}
	})
	requireSettled(t, c, intent, true) // Another intent's operation is not this one's.
	f.change(t, func(obj map[string]any) {
		obj["operation"] = map[string]any{"info": []any{map[string]any{"name": operationInfoName, "value": digest}}}
		object(obj, "metadata")["uid"] = "replacement-application-uid"
	})
	requireSettled(t, c, intent, true)
}

func TestOperationSettledRefusesWhatItCannotRead(t *testing.T) {
	f := newFixture(t)
	c := f.client(t)
	intent := fixtureIntent(t, c)
	elsewhere := intent
	elsewhere.Target.Name = "another-installation"
	_, err := c.OperationSettled(context.Background(), elsewhere)
	require.Error(t, err, "an unreadable Application cannot show its operation finished")
	f.change(t, func(obj map[string]any) { obj["kind"] = "ConfigMap" })
	_, err = c.OperationSettled(context.Background(), intent)
	require.ErrorContains(t, err, "different object")
	invalid := intent
	invalid.Revision = "main"
	_, err = c.OperationSettled(context.Background(), invalid)
	require.Error(t, err)
	var missing *Client
	_, err = missing.OperationSettled(context.Background(), intent)
	require.Error(t, err)
}
