// Package versionstore limits internal origin to the two document history writes.
// Ownership reads stay in library under the original caller. The stamp is local
// to Execute and cannot be reused by a request. Neither mutation accepts an owner.
package versionstore

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

func Execute(ctx context.Context, engine memql.IntegrationEngineAccess, query string) (*memql.ExecuteResult, error) {
	if !strings.HasPrefix(query, "mutation appendDocumentVersion(") && !strings.HasPrefix(query, "mutation updateGeneratedOutputContent(") {
		return nil, fmt.Errorf("unsupported document version write")
	}
	return engine.Execute(auth.ContextWithInternalOrigin(ctx), query)
}

// Append and UpdateContent render only these two server-authored writes.
// The renderer validates field names; neither mutation accepts an owner.
func Append(ctx context.Context, engine memql.IntegrationEngineAccess, args map[string]any) (*memql.ExecuteResult, error) {
	call, err := langparser.RenderCall("appendDocumentVersion", args)
	if err != nil {
		return nil, err
	}
	return Execute(ctx, engine, "mutation "+call)
}
func UpdateContent(ctx context.Context, engine memql.IntegrationEngineAccess, args map[string]any) (*memql.ExecuteResult, error) {
	call, err := langparser.RenderCall("updateGeneratedOutputContent", args)
	if err != nil {
		return nil, err
	}
	return Execute(ctx, engine, "mutation "+call)
}
