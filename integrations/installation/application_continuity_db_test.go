package installation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/integrations/argocd"
)

type journalHistoryAPI struct {
	mu          sync.Mutex
	application map[string]any
	head        string
	events      []map[string]any
	cursors     []string
	unavailable bool
}

func (a *journalHistoryAPI) Do(_ context.Context, method, path, contentType string, body []byte) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	u, err := url.Parse(path)
	if err != nil || method != http.MethodGet || contentType != "" || len(body) != 0 || !strings.HasSuffix(u.Path, "/applications") {
		return nil, errors.New("history fixture refuses writes or unrelated reads")
	}
	q := u.Query()
	if q.Get("fieldSelector") != "metadata.name="+resourceText(resourceMap(a.application, "metadata"), "name") {
		return nil, errors.New("history fixture refuses an unscoped list")
	}
	if q.Get("watch") == "true" {
		a.cursors = append(a.cursors, q.Get("resourceVersion"))
		if a.unavailable {
			return nil, errors.New("watch history expired")
		}
		var out []byte
		for _, event := range a.events {
			b, _ := json.Marshal(event)
			out = append(out, b...)
			out = append(out, '\n')
		}
		return out, nil
	}
	return json.Marshal(map[string]any{"apiVersion": "argoproj.io/v1alpha1", "kind": "ApplicationList", "metadata": map[string]any{"resourceVersion": a.head}, "items": []any{a.application}})
}

func (a *journalHistoryAPI) change(t *testing.T, version, head string, generation int64, revision string) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	meta := resourceMap(a.application, "metadata")
	meta["resourceVersion"] = version
	meta["generation"] = generation
	resourceMap(resourceMap(a.application, "spec"), "source")["targetRevision"] = revision
	a.head = head
	b, err := json.Marshal(a.application)
	require.NoError(t, err)
	var copy map[string]any
	require.NoError(t, json.Unmarshal(b, &copy))
	a.events = append(a.events, map[string]any{"type": "MODIFIED", "object": copy})
}

func historyJournalFixture(t *testing.T) (context.Context, *preparationJournal, *preparationJournal, preparationRecord, *journalHistoryAPI, *argocd.Client) {
	t.Helper()
	db, peerDB := journalDB(t)
	j, peer := preparationConnection(db), preparationConnection(peerDB)
	scope, _ := preparationFixture(t)
	scope.Intent.FormatVersion = 2
	scope.Intent.BeforeResourceVersion = "41"
	scope.Intent.WatchResourceVersion = "100"
	ctx := captureOperator(auth.RoleOwner, scope.RequestedBy)
	r, err := j.reserve(ctx, scope)
	require.NoError(t, err)
	var spec map[string]any
	require.NoError(t, json.Unmarshal(scope.Intent.BeforeSpec, &spec))
	api := &journalHistoryAPI{head: "100", application: map[string]any{"apiVersion": "argoproj.io/v1alpha1", "kind": "Application", "metadata": map[string]any{"namespace": scope.Intent.Target.Namespace, "name": scope.Intent.Target.Name, "uid": scope.Intent.Target.UID, "resourceVersion": "41", "generation": scope.Intent.BeforeGeneration}, "spec": spec}}
	client, err := argocd.New(api)
	require.NoError(t, err)
	return ctx, j, peer, r, api, client
}

func TestApplicationContinuitySurvivesReplicaChangeAndRetainsRefusal(t *testing.T) {
	ctx, j, peer, r, api, client := historyJournalFixture(t)
	_, err := j.advanceContinuity(ctx, r.Scope.InstallationID, r.ID, "", client)
	require.NoError(t, err)
	revision := beforeRevision(r.Scope.Intent.BeforeSpec)
	api.change(t, "42", "200", 43, revision)
	proof, err := peer.advanceContinuity(ctx, r.Scope.InstallationID, r.ID, "", client)
	require.NoError(t, err)
	require.NotNil(t, proof)
	recovered, err := j.get(ctx, r.Scope.InstallationID, r.ID)
	require.NoError(t, err)
	require.Equal(t, "200", recovered.Continuity[""].Cursor)
	require.Equal(t, int64(43), recovered.Continuity[""].Generation)
	require.Equal(t, []string{"100"}, api.cursors)
	api.mu.Lock()
	api.events = nil
	api.mu.Unlock()
	api.change(t, "43", "300", 44, strings.Repeat("e", 40))
	api.change(t, "44", "400", 45, revision)
	proof, err = peer.advanceContinuity(ctx, r.Scope.InstallationID, r.ID, "", client)
	require.ErrorIs(t, err, argocd.ErrChanged)
	require.Nil(t, proof)
	proof, err = j.advanceContinuity(ctx, r.Scope.InstallationID, r.ID, "", client)
	require.ErrorIs(t, err, argocd.ErrChanged)
	require.Nil(t, proof)
	recovered, err = j.get(ctx, r.Scope.InstallationID, r.ID)
	require.NoError(t, err)
	require.True(t, recovered.Continuity[""].Failed)
	require.Equal(t, "200", recovered.Continuity[""].Cursor, "a refused history must not move its boundary past the edit")
	db, err := j.database()
	require.NoError(t, err)
	down, err := os.ReadFile("../../component/database/memory-nodes/migrations/20261108174500_installation_application_continuity.down.sql")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, string(down))
	require.ErrorContains(t, err, "retained Application continuity")
}

type delayedHistoryAPI struct {
	base          argocd.API
	seen, release chan struct{}
	once          sync.Once
}

func (a *delayedHistoryAPI) Do(ctx context.Context, method, path, contentType string, body []byte) ([]byte, error) {
	raw, err := a.base.Do(ctx, method, path, contentType, body)
	a.once.Do(func() {
		close(a.seen)
		select {
		case <-a.release:
		case <-ctx.Done():
		}
	})
	return raw, err
}

func TestApplicationContinuityLateReaderCannotEraseFailureOrCancellation(t *testing.T) {
	for _, outcome := range []string{"changed", "cancelled"} {
		t.Run(outcome, func(t *testing.T) {
			ctx, j, peer, r, api, client := historyJournalFixture(t)
			_, err := j.advanceContinuity(ctx, r.Scope.InstallationID, r.ID, "", client)
			require.NoError(t, err)
			delayed := &delayedHistoryAPI{base: api, seen: make(chan struct{}), release: make(chan struct{})}
			slow, err := argocd.New(delayed)
			require.NoError(t, err)
			done := make(chan error, 1)
			go func() { _, err := j.advanceContinuity(ctx, r.Scope.InstallationID, r.ID, "", slow); done <- err }()
			select {
			case <-delayed.seen:
			case <-time.After(5 * time.Second):
				t.Fatal("reader did not reach the protocol barrier")
			}
			if outcome == "changed" {
				api.change(t, "42", "200", 43, strings.Repeat("e", 40))
				api.change(t, "43", "300", 44, beforeRevision(r.Scope.Intent.BeforeSpec))
				_, err = peer.advanceContinuity(ctx, r.Scope.InstallationID, r.ID, "", client)
				require.Error(t, err)
			} else {
				_, err = peer.cancelBeforeCapture(ctx, r.Scope.InstallationID, r.ID, r.Scope.WorkflowDigest)
				require.NoError(t, err)
			}
			close(delayed.release)
			select {
			case err = <-done:
				require.Error(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("late reader failed to finish")
			}
			saved, err := peer.get(ctx, r.Scope.InstallationID, r.ID)
			require.NoError(t, err)
			if outcome == "changed" {
				require.True(t, saved.Continuity[""].Failed)
			} else {
				require.Equal(t, "cancelled", saved.State)
			}
		})
	}
}

func TestApplicationContinuityUnavailableHistoryKeepsOriginalBoundary(t *testing.T) {
	ctx, j, peer, r, api, client := historyJournalFixture(t)
	_, err := j.advanceContinuity(ctx, r.Scope.InstallationID, r.ID, "", client)
	require.NoError(t, err)
	api.change(t, "42", "200", 43, beforeRevision(r.Scope.Intent.BeforeSpec))
	api.unavailable = true
	_, err = peer.advanceContinuity(ctx, r.Scope.InstallationID, r.ID, "", client)
	require.ErrorIs(t, err, argocd.ErrHistoryUnavailable)
	saved, err := j.get(ctx, r.Scope.InstallationID, r.ID)
	require.NoError(t, err)
	require.Equal(t, "100", saved.Continuity[""].Cursor)
	api.unavailable = false
	_, err = peer.advanceContinuity(ctx, r.Scope.InstallationID, r.ID, "", client)
	require.NoError(t, err)
	require.Equal(t, []string{"100", "100"}, api.cursors)
}

func TestApplicationContinuityGuardCancelsAndJoinsItsOwningCall(t *testing.T) {
	ctx, j, _, r, api, client := historyJournalFixture(t)
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	guard, err := startApplicationContinuity(ctx, cancel, j, client, r, nil)
	require.NoError(t, err)
	defer func() { _ = guard.stop() }()
	api.change(t, "42", "200", 43, strings.Repeat("e", 40))
	select {
	case <-ctx.Done():
		require.ErrorIs(t, context.Cause(ctx), argocd.ErrChanged)
	case <-time.After(15 * time.Second):
		t.Fatal("long preparation was not interrupted after a real edit")
	}
	require.ErrorIs(t, guard.stop(), argocd.ErrChanged)
}
