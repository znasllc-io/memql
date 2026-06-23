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
	attachment   *AttachmentRow // returned by GetAttachment
	getErr       error
}

func (s *stubAttachmentStore) CallerOwnsSpace(_ context.Context, _ string) (bool, error) {
	return s.owns, s.ownsErr
}

func (s *stubAttachmentStore) GetAttachment(_ context.Context, _, _ string) (*AttachmentRow, error) {
	return s.attachment, s.getErr
}

// stubDownloader returns canned bytes for the download-path tests.
type stubDownloader struct {
	data []byte
	err  error
}

func (d *stubDownloader) DownloadURL(_ context.Context, _ string) ([]byte, error) {
	return d.data, d.err
}

// authedGet builds a GET request carrying the synthetic auth identity.
func authedGet(t *testing.T, path, actor string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	ctx := auth.ContextWithClaims(req.Context(), map[string]any{"sub": actor, "role": "writer"})
	return req.WithContext(ctx)
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

// --- download path (memql#804) ---

const dlPath = "/spaces/v1:cognition:space:s1/attachments/v1:cognition:space:s1:att1"

// Cross-tenant download is 404 (same opacity as upload) and never reaches the row.
func TestAttachmentDownload_RejectsCrossTenant(t *testing.T) {
	store := &stubAttachmentStore{owns: false, attachment: &AttachmentRow{ID: "att1", PartitionId: "v1:cognition:space:s1"}}
	handler := NewAttachmentHandler(AttachmentHandlerOptions{
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:      store,
		Downloader: &stubDownloader{data: []byte("x")},
	})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, authedGet(t, dlPath, "v1:identity:user:alice"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// Unknown attachment id (no row) is 404.
func TestAttachmentDownload_NotFoundWhenRowMissing(t *testing.T) {
	store := &stubAttachmentStore{owns: true, attachment: nil}
	handler := NewAttachmentHandler(AttachmentHandlerOptions{
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:      store,
		Downloader: &stubDownloader{data: []byte("x")},
	})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, authedGet(t, dlPath, "v1:identity:user:alice"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// Happy path: owned space + matching row + bytes -> 200 with the file bytes,
// Content-Type, and an attachment Content-Disposition.
func TestAttachmentDownload_ServesBytes(t *testing.T) {
	store := &stubAttachmentStore{
		owns: true,
		attachment: &AttachmentRow{
			ID: "att1", FileName: "birds.md", MimeType: "text/markdown",
			BlobUrl: "https://acct.blob.core.windows.net/c/spaces/s1/attachments/x/birds.md",
			PartitionId: "v1:cognition:space:s1",
		},
	}
	handler := NewAttachmentHandler(AttachmentHandlerOptions{
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:      store,
		Downloader: &stubDownloader{data: []byte("# Ten birds")},
	})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, authedGet(t, dlPath, "v1:identity:user:alice"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "# Ten birds" {
		t.Fatalf("body = %q, want the file bytes", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/markdown" {
		t.Errorf("Content-Type = %q, want text/markdown", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "birds.md") {
		t.Errorf("Content-Disposition = %q, want it to name birds.md", cd)
	}
}

// A row whose partitionId doesn't match the path is 404 (no cross-space probing).
func TestAttachmentDownload_SpaceMismatchIs404(t *testing.T) {
	store := &stubAttachmentStore{
		owns:       true,
		attachment: &AttachmentRow{ID: "att1", PartitionId: "v1:cognition:space:OTHER"},
	}
	handler := NewAttachmentHandler(AttachmentHandlerOptions{
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:      store,
		Downloader: &stubDownloader{data: []byte("x")},
	})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, authedGet(t, dlPath, "v1:identity:user:alice"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// Unauthenticated download is 401.
func TestAttachmentDownload_Unauthenticated(t *testing.T) {
	handler := NewAttachmentHandler(AttachmentHandlerOptions{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:  &stubAttachmentStore{owns: true},
	})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, dlPath, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
