package release

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

// The two actual fixture streams retain their individual scope/byte checks.
// Only their local transport is multiplexed; candidate and effect journals are real.
type overlayCombinedLibrary []*candidateLibraryFixture

func (l overlayCombinedLibrary) selected(scope pipelinesteps.RunFileReceiptScope) (*candidateLibraryFixture, error) {
	for _, item := range l {
		if receiptScope(item.receipt) == scope {
			return item, nil
		}
	}
	return nil, errors.New("unbound combined fixture receipt")
}
func (l overlayCombinedLibrary) PinRunFileReceipts(ctx context.Context, ref pipelinesteps.RunFileReference) ([]pipelinesteps.StoredFileReceipt, error) {
	item, err := l.selected(ref.Scope)
	if err != nil {
		return nil, err
	}
	return item.PinRunFileReceipts(ctx, ref)
}
func (l overlayCombinedLibrary) ReleaseRunFileReference(ctx context.Context, ref pipelinesteps.RunFileReference) error {
	item, err := l.selected(ref.Scope)
	if err != nil {
		return err
	}
	return item.ReleaseRunFileReference(ctx, ref)
}
func (l overlayCombinedLibrary) RetireRunFile(ctx context.Context, scope pipelinesteps.RunFileReceiptScope, key string) (pipelinesteps.RetiredFileReceipt, error) {
	item, err := l.selected(scope)
	if err != nil {
		return pipelinesteps.RetiredFileReceipt{}, err
	}
	return item.RetireRunFile(ctx, scope, key)
}
func (l overlayCombinedLibrary) OpenRunFileReceipt(ctx context.Context, scope pipelinesteps.RunFileReceiptScope, key string) (pipelinesteps.StoredFileReceipt, io.ReadCloser, error) {
	item, err := l.selected(scope)
	if err != nil {
		return pipelinesteps.StoredFileReceipt{}, nil, err
	}
	return item.OpenRunFileReceipt(ctx, scope, key)
}

func TestInstallationOverlayCombinedCandidateSurvivesAnotherReplica(t *testing.T) {
	s, h, _, companion := overlayNativeJournalFixture(t, true)
	require.Equal(t, s.request.CandidateID, companion)
	_, err := s.nativeEvidence(ownerCtx(), nil)
	require.NoError(t, err)
	require.Len(t, s.nativeCompanions, 1)
	chosen := s.nativeCompanions[companion]
	require.Equal(t, s.nativePrimary.Candidate, chosen.Candidate)
	require.Equal(t, "docs", chosen.File.Name)
	require.Len(t, s.nativePrimary.Runs, 2)
	require.Equal(t, s.nativePrimary.Runs, chosen.Runs)
	_, err = s.bindNativePublication(ownerCtx(), map[string]any{"candidateId": companion, "catalogDigest": chosen.Candidate.CatalogDigest})
	require.NoError(t, err)
	_, err = s.build(ownerCtx(), nil)
	require.NoError(t, err)
	_, err = s.reserve(ownerCtx(), nil)
	require.NoError(t, err)
	saved, err := s.load(ownerCtx(), s.id)
	require.NoError(t, err)
	require.Equal(t, saved.Record.NativeEvidence.Primary.Candidate, saved.Record.NativeEvidence.Companion.Candidate)
	require.Equal(t, s.nativePrimary.Runs, saved.Record.NativeEvidence.Primary.Runs)

	peerIntegration := &Integration{resolver: s.integration.resolver, store: NewStore(assemblyEngine(t, s.db)), candidateDB: s.integration.candidateDB}
	peerIntegration.installationOverlay.Store(s.integration.installationOverlay.Load())
	peerIntegration.installation.Store(s.integration.installation.Load())
	peer := &overlayScope{integration: peerIntegration, db: s.db, configuration: s.configuration, request: s.request, owner: s.owner, id: s.id, snapshot: saved.Record.Snapshot, record: &saved.Record, source: s.source}
	_, err = peer.nativeEvidence(ownerCtx(), nil)
	require.NoError(t, err)
	_, err = peer.bindNativePublication(ownerCtx(), map[string]any{"candidateId": companion, "catalogDigest": chosen.Candidate.CatalogDigest})
	require.NoError(t, err)
	_, err = peer.build(ownerCtx(), nil)
	require.NoError(t, err)
	_, err = peer.reserve(ownerCtx(), nil)
	require.NoError(t, err)
	_, err = peer.propose(ownerCtx(), nil)
	require.NoError(t, err)
	_, err = peer.propose(ownerCtx(), nil)
	require.NoError(t, err)
	require.Equal(t, 1, h.refs)
	require.Equal(t, 1, h.prs)
	for _, field := range []string{"approval", "workflow", "catalog", "source"} {
		t.Run(field, func(t *testing.T) {
			altered := chosen
			switch field {
			case "approval":
				altered.Candidate.ApprovalID += "changed"
			case "workflow":
				altered.Candidate.WorkflowDigest += "changed"
			case "catalog":
				altered.Candidate.CatalogDigest += "changed"
			case "source":
				altered.Commit += "changed"
			}
			require.False(t, overlayCompanionMatches(*s.nativePrimary, altered))
		})
	}
}
