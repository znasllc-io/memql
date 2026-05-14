package server

import (
	"context"
	"net/http"
)

type (
	httpContext struct {
		Request *http.Request
		Writer  http.ResponseWriter
	}

	requestContextKey struct{}
)

func injectRequestContext(next StrictHandlerFunc, _ string) StrictHandlerFunc {
	if next == nil {
		return next
	}
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request, request interface{}) (interface{}, error) {
		httpCtx := &httpContext{
			Request: r,
			Writer:  w,
		}
		ctx = context.WithValue(ctx, requestContextKey{}, httpCtx)
		return next(ctx, w, r, request)
	}
}

func requestFromContext(ctx context.Context) (*http.Request, bool) {
	if ctx == nil {
		return nil, false
	}
	if value, ok := ctx.Value(requestContextKey{}).(*httpContext); ok && value != nil {
		return value.Request, value.Request != nil
	}
	return nil, false
}

