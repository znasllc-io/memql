package installation

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/integrations/argocd"
)

// Build a history-backed promoted record through the ordinary reservation,
// capture, verification, acknowledgement and promotion gates. The original
// promotion fixture supplies fresh native preservation/artifact observations;
// a separate journal owns this format-2 intent, with no journal rewriting.
func promotedHistoryReopenFixture(t *testing.T) (context.Context, *preparationJournal, *preparationJournal, preparationRecord, revisionRecord, *journalHistoryAPI, *argocd.Client) {
	t.Helper()
	fixtureDB, _ := journalDB(t)
	ctx, original, plan, evidence := promotionFixture(t, preparationConnection(fixtureDB))
	db, peerDB := journalDB(t)
	j, peer := preparationConnection(db), preparationConnection(peerDB)
	scope := original.Scope
	scope.Intent.FormatVersion = 2
	scope.Intent.BeforeResourceVersion = "41"
	scope.Intent.WatchResourceVersion = "100"
	plan.Intent = scope.Intent
	r, err := j.reserve(ctx, scope)
	require.NoError(t, err)
	for _, role := range []string{"candidate", "rollback"} {
		message := "private-source-commit"
		if role == "rollback" {
			message = "rollback-source-commit"
		}
		spec := scope.Captures[role]
		_, archive := captureRevisionWithRenderFixture(t, message, &spec.Render)
		capture, err := newSourceCapture(spec)
		require.NoError(t, err)
		executor, files := capturePorts(capture, archive)
		_, err = j.capture(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, role, executor, "")
		require.NoError(t, err)
		proof, err := j.verifyCapture(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, role, files)
		require.NoError(t, err)
		r, err = j.acknowledgeCapture(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, role, executor)
		require.NoError(t, err)
		if role == "candidate" {
			evidence.candidateSource = proof
		} else {
			evidence.rollbackSource = proof
		}
	}
	promoted, err := j.promote(ctx, plan.InstallationID, r.ID, scope.WorkflowDigest, scope.ConfigurationDigest, plan, evidence)
	require.NoError(t, err)
	var spec map[string]any
	require.NoError(t, json.Unmarshal(scope.Intent.BeforeSpec, &spec))
	api := &journalHistoryAPI{head: "100", application: map[string]any{"apiVersion": "argoproj.io/v1alpha1", "kind": "Application", "metadata": map[string]any{"namespace": scope.Intent.Target.Namespace, "name": scope.Intent.Target.Name, "uid": scope.Intent.Target.UID, "resourceVersion": "41", "generation": scope.Intent.BeforeGeneration}, "spec": spec}}
	client, err := argocd.New(api)
	require.NoError(t, err)
	_, err = peer.advanceContinuity(ctx, scope.InstallationID, r.ID, "", client)
	require.NoError(t, err)
	r, err = j.get(ctx, scope.InstallationID, r.ID)
	require.NoError(t, err)
	return ctx, j, peer, r, promoted, api, client
}

func TestPromotedSourceAllowsVerifiedContinuityProgressDuringExternalRead(t *testing.T) {
	for _, progress := range []string{"list cursor only", "status generation"} {
		t.Run(progress, func(t *testing.T) {
			ctx, j, peer, record, revision, api, client := promotedHistoryReopenFixture(t)
			files := promotedReopenFiles(t, record, "rollback")
			hooked := promotedReopenHook{sourceCaptureFiles: files, afterOpen: func() {
				if progress == "list cursor only" {
					api.mu.Lock()
					api.head = "200"
					api.mu.Unlock()
				} else {
					api.change(t, "42", "200", record.Scope.Intent.BeforeGeneration+1, beforeRevision(record.Scope.Intent.BeforeSpec))
				}
				_, err := peer.advanceContinuity(ctx, record.Scope.InstallationID, record.ID, "", client)
				require.NoError(t, err)
			}}
			proof, err := j.reopenPromotedCapture(ctx, record.Scope.InstallationID, record.ID, revision.ID, "rollback", hooked)
			require.NoError(t, err, "valid history progress is not a retained source change")
			require.Equal(t, record.Captures["rollback"].SourceDigest, proof.source.Digest())
			after, err := j.get(ctx, record.Scope.InstallationID, record.ID)
			require.NoError(t, err)
			require.Equal(t, "200", after.Continuity[""].Cursor)
			require.Equal(t, record.Captures, after.Captures)
			require.Equal(t, record.Scope, after.Scope)
			require.Equal(t, 1, files.closed)
		})
	}
}

func TestPromotedSourceRejectsContinuityFailureAndBindingChangesDuringExternalRead(t *testing.T) {
	for _, fault := range []string{"away and back", "missing history", "foreign intent", "generation regression", "changed capture"} {
		t.Run(fault, func(t *testing.T) {
			ctx, j, peer, record, revision, api, client := promotedHistoryReopenFixture(t)
			if fault == "generation regression" {
				api.change(t, "42", "200", record.Scope.Intent.BeforeGeneration+2, beforeRevision(record.Scope.Intent.BeforeSpec))
				_, err := peer.advanceContinuity(ctx, record.Scope.InstallationID, record.ID, "", client)
				require.NoError(t, err)
				record, err = j.get(ctx, record.Scope.InstallationID, record.ID)
				require.NoError(t, err)
			}
			files := promotedReopenFiles(t, record, "rollback")
			hooked := promotedReopenHook{sourceCaptureFiles: files, afterOpen: func() {
				if fault == "away and back" {
					api.change(t, "42", "200", record.Scope.Intent.BeforeGeneration+1, strings.Repeat("e", 40))
					api.change(t, "43", "300", record.Scope.Intent.BeforeGeneration+2, beforeRevision(record.Scope.Intent.BeforeSpec))
					_, err := peer.advanceContinuity(ctx, record.Scope.InstallationID, record.ID, "", client)
					require.ErrorIs(t, err, argocd.ErrChanged)
					return
				}
				_, err := peer.withRecordState(ctx, record.Scope.InstallationID, record.ID, record.Scope.WorkflowDigest, "promoted", func(current *preparationRecord) error {
					state := current.Continuity[""]
					switch fault {
					case "missing history":
						delete(current.Continuity, "")
					case "foreign intent":
						state.Intent = "memql-id:" + strings.Repeat("f", 64)
						current.Continuity[""] = state
					case "generation regression":
						state.Generation--
						current.Continuity[""] = state
					case "changed capture":
						capture := current.Captures["rollback"]
						capture.SourceDigest = "memql-id:" + strings.Repeat("f", 64)
						current.Captures["rollback"] = capture
					}
					return nil
				})
				if fault == "foreign intent" {
					require.Error(t, err, "journal reread already refuses foreign history")
				} else {
					require.NoError(t, err)
				}
			}}
			_, err := j.reopenPromotedCapture(ctx, record.Scope.InstallationID, record.ID, revision.ID, "rollback", hooked)
			// The journal rolls a malformed foreign-intent write back before it
			// can become evidence. Other faults are retained and must refuse.
			if fault == "foreign intent" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.Equal(t, 1, files.closed)
		})
	}
}
