package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/httpstream"
)

// GET /accounts/exports/{id}/content -- the download of a finished account
// export (account export design, section G; owner-approved HTTP exception
// 2026-11-12, recorded in the root CLAUDE.md table). A browser can only save
// bytes that arrive over HTTP, and the stream carries no answer that large.
//
// The handler owns the narrow interface below; the accounts plug-in implements
// it and the bff's wiring injects it, so this package never reads an export
// row or a blob key itself.

// accountExportPart is the part every export act requires (design section H).
const accountExportPart = "app:accounts/export"

// accountExportFallbackName is the saved name when the record's own name
// sanitizes to nothing.
const accountExportFallbackName = "account-export"

// AccountExportFile is a finished export as the route needs it.
type AccountExportFile struct {
	ID, AccountID, FileName, ObjectKey, SHA256 string
	SizeBytes                                  int64
}

// AccountExportDownloads is what the route needs from the accounts plug-in.
type AccountExportDownloads interface {
	// ResolveForDownload answers ok=false for an unknown id and for an export
	// that is not complete or was removed.
	ResolveForDownload(ctx context.Context, exportID string) (f AccountExportFile, ok bool, err error)
	// Open streams the stored object from offset. length < 0 means to the end.
	Open(ctx context.Context, f AccountExportFile, offset, length int64) (io.ReadCloser, error)
	// RecordDownload writes the audit event for one request. rangeAsked is the
	// Range header as sent, or "".
	RecordDownload(ctx context.Context, f AccountExportFile, userID, rangeAsked string) error
}

// AccountExportHandlerOptions carries the handler's collaborators.
type AccountExportHandlerOptions struct {
	Logger    *slog.Logger
	Downloads AccountExportDownloads
}

// AccountExportHandler serves GET /accounts/exports/{id}/content on the bff.
type AccountExportHandler struct {
	logger    *slog.Logger
	downloads AccountExportDownloads
}

// NewAccountExportHandler builds the handler. The app mounts it on the bff as
// "GET <path>" for every path AccountExportPaths() returns.
func NewAccountExportHandler(opts AccountExportHandlerOptions) *AccountExportHandler {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &AccountExportHandler{logger: logger, downloads: opts.Downloads}
}

// notFound is the one refusal. Every reason a request is not served -- no
// person, no part, an unknown id, a draft, a removed file -- answers exactly
// this, so the route says nothing about which exports exist.
func (h *AccountExportHandler) notFound(w http.ResponseWriter) {
	http.Error(w, "not found", http.StatusNotFound)
}

func (h *AccountExportHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// "GET <pattern>" on ServeMux also admits HEAD. HEAD is not offered: it
	// would answer a size without an audit event for a byte sent.
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	exportID, ok := parseAccountExportContentPath(r.URL.Path)
	if !ok || h.downloads == nil {
		h.notFound(w)
		return
	}
	ctx := r.Context()

	// A person's own session first, then the part for that person. The order
	// does not leak anything (both refuse with the same 404); it keeps the
	// capability question from ever being asked for a machine credential.
	userID, err := auth.RequirePersonSession(ctx)
	if err != nil {
		h.notFound(w)
		return
	}
	subject, ok := auth.SubjectFromContext(ctx)
	if !ok || !auth.CapableFor(ctx, subject, auth.VerbExecute, accountExportPart) {
		h.notFound(w)
		return
	}

	file, found, err := h.downloads.ResolveForDownload(ctx, exportID)
	if err != nil {
		h.logger.Error("account export lookup failed", "error", err, "exportId", exportID)
		http.Error(w, "lookup failed", http.StatusInternalServerError)
		return
	}
	if !found || file.SizeBytes < 0 {
		h.notFound(w)
		return
	}

	writer, request, finish, deadlineErr := httpstream.Download(w, r)
	if deadlineErr != nil {
		http.Error(w, "download unavailable", http.StatusServiceUnavailable)
		return
	}
	defer finish()
	w, r = writer, request
	ctx = r.Context()

	rangeAsked := r.Header.Get("Range")
	// The audit event is written for every request that reached the file,
	// an unsatisfiable range included, and BEFORE any byte: a failed write
	// fails the request, so there is no download without its record.
	if err := h.downloads.RecordDownload(ctx, file, userID, rangeAsked); err != nil {
		h.logger.Error("account export download audit failed; refusing to serve",
			"error", err, "exportId", file.ID)
		http.Error(w, "download unavailable", http.StatusInternalServerError)
		return
	}

	size := file.SizeBytes
	// The Library's single-range subset: a multi-range or malformed header is
	// answered whole (Range is advisory), a well-formed one past the end is 416.
	rng, wellFormed := parseRangeHeader(rangeAsked, size)
	if wellFormed && rng == nil {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		http.Error(w, "requested range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
		return
	}

	offset, length := int64(0), int64(-1)
	if rng != nil {
		offset, length = rng.start, rng.end-rng.start+1
	}
	rc, err := h.downloads.Open(ctx, file, offset, length)
	if err != nil {
		h.logger.Error("account export object unavailable", "error", err, "exportId", file.ID)
		http.Error(w, "download unavailable", http.StatusInternalServerError)
		return
	}
	defer func() { _ = rc.Close() }()

	name := SanitizeLibraryFileName(file.FileName)
	if name == "" {
		name = accountExportFallbackName
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "application/octet-stream")
	hdr.Set("Content-Disposition", downloadDisposition(name))
	hdr.Set("Accept-Ranges", "bytes")
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("X-Content-Type-Options", "nosniff")
	if rng != nil {
		hdr.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", rng.start, rng.end, size))
		hdr.Set("Content-Length", strconv.FormatInt(length, 10))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		hdr.Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
	}
	if _, err := io.Copy(w, rc); err != nil {
		// The headers are gone; the log line is all that is left.
		h.logger.Warn("account export download interrupted", "error", err, "exportId", file.ID)
	}
}

// parseAccountExportContentPath parses /accounts/exports/{id}/content,
// tolerating a leading base prefix. r.URL.Path is already decoded, so an id
// that carried an encoded slash arrives as an extra segment and fails the
// shape; a dot-dot anywhere in the id is refused outright, before the plug-in
// is asked about it.
func parseAccountExportContentPath(path string) (string, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] != "accounts" || parts[i+1] != "exports" {
			continue
		}
		if i+3 != len(parts)-1 || parts[i+3] != "content" {
			return "", false
		}
		id := strings.TrimSpace(parts[i+2])
		if id == "" || id == "." || strings.Contains(id, "..") || strings.ContainsAny(id, `/\`) {
			return "", false
		}
		return id, true
	}
	return "", false
}
