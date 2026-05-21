package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// stubAttachmentStore lets a test drive only the bits of the
// AttachmentStore interface that the ownership-check path exercises.
type stubAttachmentStore struct {
	owns         bool
	ownsErr      error
	createErr    error
	createdCount int
}

func (s *stubAttachmentStore) CallerOwnsSpace(_ context.Context, _ string) (bool, error) {
	return s.owns, s.ownsErr
}

func (s *stubAttachmentStore) CreateAttachment(_ context.Context, _ AttachmentCreateParams) (json.RawMessage, error) {
	s.createdCount++
	if s.createErr != nil {
		return nil, s.createErr
	}
	return json.RawMessage(`{"id":"v1:common:attachment:stub"}`), nil
}

// authedRequest builds an HTTP request whose context already carries
// the synthetic auth identity the handler reads via auth.ActorFromContext.
func authedRequest(t *testing.T, path, actor string, body io.Reader) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, body)
	claims := map[string]any{
		"sub":  actor,
		"role": "writer",
	}
	ctx := auth.ContextWithClaims(req.Context(), claims)
	return req.WithContext(ctx)
}

// TestAttachmentHandler_RejectsCrossTenantBeforeUpload asserts the F10
// defense-in-depth ownership gate: when the caller does not own the
// space, the handler returns 404 and CreateAttachment is never reached.
//
// The 404 (rather than 403) is intentional -- probing for spaces the
// caller does not own should be indistinguishable from probing for
// non-existent spaces.
func TestAttachmentHandler_RejectsCrossTenantBeforeUpload(t *testing.T) {
	store := &stubAttachmentStore{owns: false}
	handler := NewAttachmentHandler(AttachmentHandlerOptions{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:  store,
	})

	// Note: we send no multipart body. If the ownership check fires
	// FIRST (as it should), the handler returns 404 without ever
	// trying to ParseMultipartForm. If the ownership check were
	// missing or ordered wrong, the empty body would surface as
	// "invalid multipart form" 400.
	req := authedRequest(t, "/spaces/v1:cognition:space:foreign/attachments", "v1:identity:user:alice", strings.NewReader(""))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body=%q)", rec.Code, rec.Body.String())
	}
	if store.createdCount != 0 {
		t.Fatalf("CreateAttachment was called %d time(s) despite ownership rejection; expected 0", store.createdCount)
	}
}

// TestAttachmentHandler_OwnershipCheckErrorIs500 verifies that an
// engine-side failure in the ownership lookup surfaces as 500 (not
// silently fall-open to upload).
func TestAttachmentHandler_OwnershipCheckErrorIs500(t *testing.T) {
	store := &stubAttachmentStore{ownsErr: errors.New("engine offline")}
	handler := NewAttachmentHandler(AttachmentHandlerOptions{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:  store,
	})

	req := authedRequest(t, "/spaces/v1:cognition:space:any/attachments", "v1:identity:user:alice", strings.NewReader(""))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body=%q)", rec.Code, rec.Body.String())
	}
	if store.createdCount != 0 {
		t.Fatalf("CreateAttachment was called %d time(s) despite ownership-lookup error; expected 0", store.createdCount)
	}
}

// TestAttachmentHandler_UnauthenticatedIs401 confirms the
// pre-existing 401 path still fires when the caller has no actor on
// the context. The ownership check runs only after this, so an
// unauthenticated caller can't reach the store at all.
func TestAttachmentHandler_UnauthenticatedIs401(t *testing.T) {
	store := &stubAttachmentStore{owns: true}
	handler := NewAttachmentHandler(AttachmentHandlerOptions{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:  store,
	})

	req := httptest.NewRequest(http.MethodPost, "/spaces/v1:cognition:space:any/attachments", strings.NewReader(""))
	// No auth context on this request.
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body=%q)", rec.Code, rec.Body.String())
	}
}
