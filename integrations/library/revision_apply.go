package library

import (
	"context"
	"fmt"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/work"
)

// RevisionFileWriter is a bounded compare-and-swap on existing stored bytes.
// The adapter shares upload quotas, immutable blobs and file version history
// with ordinary file saves. It reconciles this operation before retrying writes.
type RevisionFileWriter interface {
	SaveRevision(context.Context, RevisionFileWrite) error
}
type RevisionFileWrite struct {
	ArtifactID, FileID, BlobURL, OperationID string
	Version                                  int
	Content                                  []byte
}

func (i *Integration) SetRevisionFiles(writer RevisionFileWriter) { i.revisionFiles = writer }

func (i *Integration) applyRevision(ctx context.Context, ids work.ReviewGoalReceipt, proposal map[string]any) ([]memorynodes.MemoryNode, error) {
	ctx = memql.ContextWithFreshRead(ctx)
	artifactID := asString(proposal["artifactId"])
	doc, release, err := i.lockReviewDocument(ctx, artifactID)
	if err != nil {
		return nil, err
	}
	defer func() {
		if release != nil {
			release()
		}
	}()
	if err := i.validateCurrentRevision(ctx, proposal); err != nil {
		return nil, err
	}
	version, valid := intArg(proposal["version"])
	if !valid || doc.kind != proposal["documentKind"] || memql.BareShortId(doc.source) != proposal["sourceId"] {
		return nil, fmt.Errorf("the revision no longer identifies this document")
	}
	content := asString(proposal["revisedContent"])
	operationID := "revision-" + ids.RunID
	result := map[string]any{"applied": true, "artifactId": artifactID, "newVersion": version + 1, "summary": proposal["summary"]}
	if doc.kind == "file" {
		if i.revisionFiles == nil {
			return nil, fmt.Errorf("document version storage is unavailable on this node")
		}
		// The adapter reconciles its immutable operation blob before rejecting a
		// stale version. It owns the final shared lock through the head update.
		blobURL := asString(proposal["blobURL"])
		if doc.version == version {
			original, err := i.revisionContent(ctx, doc)
			if err != nil {
				return nil, err
			}
			if doc.revision != proposal["revision"] || original != proposal["content"] {
				return nil, fmt.Errorf("the document changed; submit feedback on its current revision")
			}
			blobURL = stringField(doc.backing, "blobUrl")
		}
		release()
		release = nil
		if err = i.revisionFiles.SaveRevision(ctx, RevisionFileWrite{ArtifactID: artifactID, FileID: doc.source, BlobURL: blobURL, OperationID: operationID, Version: version, Content: []byte(content)}); err != nil {
			return nil, err
		}
		return reviewResult(result)
	}
	// The version identity is also the receipt. A crash between history and
	// backing content recovers the same version, never increments it twice.
	stored, err := i.revisionRow(ctx, "documentVersionById", map[string]any{"versionId": operationID})
	if err != nil {
		return nil, err
	}
	if stored != nil {
		n, _ := intArg(stored["versionNumber"])
		if stored["content"] != content || n != version+1 || memql.BareShortId(asString(stored["documentId"])) != memql.BareShortId(doc.source) || memql.BareShortId(asString(stored["producedByRunId"])) != ids.RunID {
			return nil, fmt.Errorf("the revision receipt does not match the approved changes")
		}
		// Confirm our exact head before acknowledging a lost response. A later
		// edit is preserved and reported as a conflict, never overwritten.
		if doc.revision != proposal["revision"] {
			if doc.backing["body"] == content && doc.version == version+1 {
				return reviewResult(result)
			}
			return nil, fmt.Errorf("a later edit changed the document; the approved version is preserved in history")
		}
	} else {
		if doc.version != version || doc.revision != proposal["revision"] || doc.backing["body"] != proposal["content"] {
			return nil, fmt.Errorf("the document changed; submit feedback on its current revision")
		}
		latest, _, err := i.latestVersion(ctx, doc.source)
		if err != nil {
			return nil, err
		}
		latest, err = i.ensureInitialVersion(ctx, doc.backing, latest)
		if err != nil {
			return nil, err
		}
		versionAt := nextDocumentTime(doc.backing, latest)
		if err = i.appendVersion(ctx, appendArgs{versionAt: versionAt, versionId: operationID, documentId: doc.source, versionNumber: version + 1, content: content, authorKind: "assistant", note: asString(proposal["summary"]), parentVersionId: stringField(latest, "id"), producedByRunId: ids.RunID, partitionId: stringField(doc.backing, "partitionId")}); err != nil {
			return nil, err
		}
		stored, err = i.revisionRow(memql.ContextWithFreshRead(ctx), "documentVersionById", map[string]any{"versionId": operationID})
		if err != nil || stored == nil {
			return nil, fmt.Errorf("could not confirm the saved document version: %v", err)
		}
	}
	if doc.backing["body"] != proposal["content"] {
		return nil, fmt.Errorf("the original document changed before the approved version could be saved")
	}
	if err = i.updateBackingContent(ctx, doc.backing, content, stringField(doc.backing, "attachmentId"), timestampField(stored, "createdAt")); err != nil {
		return nil, err
	}
	i.touchArtifact(ctx, doc.backing)
	return reviewResult(result)
}
