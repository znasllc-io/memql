// Package reviewstore persists review receipts only after library has admitted
// the current artifact and its backing row under the ORIGINAL caller. It borrows
// only that verified row's owner for comment and version-receipt storage. The
// fixed approval lookup reads decisions across collaborators, scoped to that
// artifact. No arbitrary query strings or caller-chosen authority enter here.
package reviewstore

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

func execute(ctx context.Context, engine memql.IntegrationEngineAccess, owner, kind, call string) (*memql.ExecuteResult, error) {
	if owner == "" {
		return nil, fmt.Errorf("review document has no owner")
	}
	scoped := auth.ContextWithUserActor(ctx, owner)
	return engine.Execute(auth.ContextWithInternalOrigin(scoped), kind+" "+call)
}
func List(ctx context.Context, engine memql.IntegrationEngineAccess, owner, artifact string) (*memql.ExecuteResult, error) {
	call, err := langparser.RenderCall("documentCommentsForArtifact", map[string]any{"artifactId": artifact})
	if err != nil {
		return nil, err
	}
	return execute(ctx, engine, owner, "query", call)
}
func ByID(ctx context.Context, engine memql.IntegrationEngineAccess, owner, comment string) (*memql.ExecuteResult, error) {
	call, err := langparser.RenderCall("documentCommentById", map[string]any{"commentId": comment})
	if err != nil {
		return nil, err
	}
	return execute(ctx, engine, owner, "query", call)
}
func Append(ctx context.Context, engine memql.IntegrationEngineAccess, owner string, args map[string]any) (*memql.ExecuteResult, error) {
	call, err := langparser.RenderCall("appendDocumentComment", args)
	if err != nil {
		return nil, err
	}
	return execute(ctx, engine, owner, "mutation", call)
}

func Notes(ctx context.Context, engine memql.IntegrationEngineAccess, owner, artifact, author string) (*memql.ExecuteResult, error) {
	call, err := langparser.RenderCall("documentNotesForArtifact", map[string]any{"artifactId": artifact, "authorUserId": author})
	if err != nil {
		return nil, err
	}
	return execute(ctx, engine, owner, "query", call)
}

func Remove(ctx context.Context, engine memql.IntegrationEngineAccess, owner, comment string) (*memql.ExecuteResult, error) {
	call, err := langparser.RenderCall("removeDocumentAnnotation", map[string]any{"commentId": comment})
	if err != nil {
		return nil, err
	}
	return execute(ctx, engine, owner, "mutation", call)
}

func Removed(ctx context.Context, engine memql.IntegrationEngineAccess, owner, artifact string, ids []string) (*memql.ExecuteResult, error) {
	call, err := langparser.RenderCall("removedDocumentComments", map[string]any{"artifactId": artifact, "commentIds": ids})
	if err != nil {
		return nil, err
	}
	return execute(ctx, engine, owner, "query", call)
}

// ApprovedRevisions reads audit decisions, never document bodies. Library must
// admit BOTH the artifact and backing document under the original caller first.
// The system identity is confined to this one server-only, artifact-scoped read.
func ApprovedRevisions(ctx context.Context, engine memql.IntegrationEngineAccess, artifact, cursor string, versions []int) (*memql.ExecuteResult, error) {
	if artifact == "" {
		return nil, fmt.Errorf("review document has no artifact")
	}
	call, err := langparser.RenderCall("workDocumentApprovedRevisions", map[string]any{"artifactId": artifact, "versions": versions})
	if err != nil {
		return nil, err
	}
	ctx = memql.ContextWithCursor(ctx, cursor)
	return engine.Execute(auth.ContextWithInternalOrigin(auth.ContextWithSystemActor(ctx, "library-review-history")), "query "+call)
}

func VersionReceipt(ctx context.Context, engine memql.IntegrationEngineAccess, owner, source string, version int, file bool) (*memql.ExecuteResult, error) {
	name := "libraryDocumentVersionByNumber"
	args := map[string]any{"documentId": memql.BareShortId(source), "documentIdAlias": "v1:library:generatedOutput:" + memql.BareShortId(source), "versionNumber": version}
	if file {
		name = "libraryFileVersionByNumber"
		args = map[string]any{"fileId": memql.BareShortId(source), "versionNumber": version}
	}
	call, err := langparser.RenderCall(name, args)
	if err != nil {
		return nil, err
	}
	return execute(ctx, engine, owner, "query", call)
}
