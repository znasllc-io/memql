// Package reviewstore persists review receipts only after library has admitted
// the current artifact and its backing row under the ORIGINAL caller. It borrows
// only that verified row's owner for comment storage. No artifact/content reads
// or arbitrary query strings can enter this package.
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
