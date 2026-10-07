package installation

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

func promotedReopenFixture(t *testing.T) (context.Context, *preparationJournal, *preparationJournal, preparationRecord, revisionRecord) {
	t.Helper()
	db, peerDB := journalDB(t)
	j, peer := preparationConnection(db), preparationConnection(peerDB)
	ctx, prepared, plan, evidence := promotionFixture(t, j)
	promoted, err := j.promote(ctx, plan.InstallationID, prepared.ID, prepared.Scope.WorkflowDigest, prepared.Scope.ConfigurationDigest, plan, evidence)
	require.NoError(t, err)
	record, err := peer.get(ctx, plan.InstallationID, prepared.ID)
	require.NoError(t, err)
	return ctx, j, peer, record, promoted
}

func promotedReopenFiles(t *testing.T, record preparationRecord, role string) *captureFilesFixture {
	t.Helper()
	spec := record.Scope.Captures[role]
	message := "private-source-commit"
	if role == "rollback" {
		message = "rollback-source-commit"
	}
	_, archive := captureRevisionWithRenderFixture(t, message, &spec.Render)
	capture, err := newSourceCapture(spec)
	require.NoError(t, err)
	_, files := capturePorts(capture, archive)
	require.Equal(t, record.Captures[role].ReceiptDigest, preparationReceiptDigest(files.row), "recreated external storage contains the exact original receipt and bytes")
	return files
}

func TestPromotedSourceReopensAcrossJournalHostsWithoutReopeningPreparation(t *testing.T) {
	ctx, j, peer, record, revision := promotedReopenFixture(t)
	for _, role := range []string{"candidate", "rollback"} {
		files := promotedReopenFiles(t, record, role)
		first, err := j.reopenPromotedCapture(ctx, record.Scope.InstallationID, record.ID, revision.ID, role, files)
		require.NoError(t, err)
		second, err := peer.reopenPromotedCapture(ctx, record.Scope.InstallationID, record.ID, revision.ID, role, files)
		require.NoError(t, err)
		require.Equal(t, record.Captures[role].SourceDigest, first.source.Digest())
		require.Equal(t, first.source.Digest(), second.source.Digest())
		require.Equal(t, first.receipt, second.receipt)
		require.Equal(t, 2, files.opens)
		require.Equal(t, 2, files.closed)
		require.Len(t, files.refs, 2)
		require.Equal(t, files.refs[0], files.refs[1], "reopening only reuses the permanent source consumer")
		_, err = peer.verifyCapture(ctx, record.Scope.InstallationID, record.ID, record.Scope.WorkflowDigest, role, files)
		require.Error(t, err, "the preparing-only verifier remains closed after promotion")
		require.Equal(t, 2, files.opens)
	}
	after, err := peer.get(ctx, record.Scope.InstallationID, record.ID)
	require.NoError(t, err)
	require.Equal(t, record, after, "source reopening does not rewrite captures, acknowledgments or promotion")
}

func TestPromotedSourceRejectsChangedActorIdentityAndRetainedBytes(t *testing.T) {
	ctx, j, peer, record, revision := promotedReopenFixture(t)
	for _, fault := range []string{"public origin", "other requester", "reader", "foreign plan", "foreign role", "changed receipt", "changed bytes"} {
		t.Run(fault, func(t *testing.T) {
			files := promotedReopenFiles(t, record, "rollback")
			caller, plan, role := ctx, revision.ID, "rollback"
			switch fault {
			case "public origin":
				caller = operator(auth.RoleOwner, record.Scope.RequestedBy)
			case "other requester":
				caller = captureOperator(auth.RoleOwner, "another")
			case "reader":
				caller = captureOperator(auth.RoleReader, record.Scope.RequestedBy)
			case "foreign plan":
				plan = "memql-id:" + strings.Repeat("f", 64)
			case "foreign role":
				role = "other"
			case "changed receipt":
				files.row.ETag = "substituted-version"
			case "changed bytes":
				files.body[0] ^= 1
			}
			_, err := peer.reopenPromotedCapture(caller, record.Scope.InstallationID, record.ID, plan, role, files)
			require.Error(t, err)
			require.Equal(t, files.opens, files.closed)
			if fault != "changed receipt" && fault != "changed bytes" {
				require.Zero(t, files.opens)
				require.Empty(t, files.refs)
			}
		})
	}
	revisions := &revisionJournal{db: j.db}
	_, err := revisions.cancel(ctx, record.Scope.InstallationID, revision.ID)
	require.NoError(t, err)
	files := promotedReopenFiles(t, record, "rollback")
	_, err = peer.reopenPromotedCapture(ctx, record.Scope.InstallationID, record.ID, revision.ID, "rollback", files)
	require.Error(t, err)
	require.Zero(t, files.opens)
}

type promotedReopenHook struct {
	sourceCaptureFiles
	afterOpen func()
}

func (f promotedReopenHook) OpenRunFileReceipt(ctx context.Context, scope pipelinesteps.RunFileReceiptScope, intent string) (pipelinesteps.StoredFileReceipt, io.ReadCloser, error) {
	row, body, err := f.sourceCaptureFiles.OpenRunFileReceipt(ctx, scope, intent)
	if err == nil {
		f.afterOpen()
	}
	return row, body, err
}

func TestPromotedSourceRechecksOwnershipAfterExternalRead(t *testing.T) {
	ctx, j, peer, record, revision := promotedReopenFixture(t)
	files := promotedReopenFiles(t, record, "rollback")
	revisions := &revisionJournal{db: peer.db}
	hooked := promotedReopenHook{sourceCaptureFiles: files, afterOpen: func() {
		_, err := revisions.cancel(ctx, record.Scope.InstallationID, revision.ID)
		require.NoError(t, err, "external reads must not hold the installation journal lock")
	}}
	_, err := j.reopenPromotedCapture(ctx, record.Scope.InstallationID, record.ID, revision.ID, "rollback", hooked)
	require.Error(t, err, "a valid closure cannot escape after its parent loses ownership")
	require.Equal(t, 1, files.opens)
	require.Equal(t, 1, files.closed)
	after, err := peer.get(ctx, record.Scope.InstallationID, record.ID)
	require.NoError(t, err)
	require.Equal(t, record, after)
}
