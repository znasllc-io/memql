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

// accountExportMaxIDBytes bounds the id taken from the path before the plug-in
// is asked about it.
const accountExportMaxIDBytes = 128

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

	// The long ceiling, not the Library's 30 minutes: the Library caps a file at
	// 4 GiB and an export has no such cap. The idle write deadline is the same
	// and is what stops a stalled reader. The edge's hop for this route uses
	// the same ceiling (component/edge/proxy.go).
	writer, request, finish, deadlineErr := httpstream.DownloadWithin(w, r, httpstream.LongDownloadLimit)
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
	etag := accountExportETag(file.SHA256)
	// If-Range is what lets a browser resume an interrupted download: only the
	// exact strong ETag lets the Range stand. A different ETag, a date, a weak
	// validator, or any If-Range when no ETag can be made, means the file may
	// have changed, so the Range is ignored and the whole file is answered.
	effectiveRange := rangeAsked
	if ifRange := r.Header.Get("If-Range"); ifRange != "" && (etag == "" || ifRange != etag) {
		effectiveRange = ""
	}
	// The Library's single-range subset: a multi-range or malformed header is
	// answered whole (Range is advisory), a well-formed one past the end is 416.
	// A zero-length suffix (bytes=-0) is unsatisfiable (RFC 9110 14.1.2); the
	// shared parser reads it as malformed, so it is decided here.
	rng, wellFormed := parseRangeHeader(effectiveRange, size)
	if isZeroSuffixRange(effectiveRange) {
		rng, wellFormed = nil, true
	}
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

	announced := size
	if rng != nil {
		announced = length
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "application/octet-stream")
	hdr.Set("Content-Disposition", downloadDisposition(SanitizeLibraryFileName(file.FileName)))
	hdr.Set("Accept-Ranges", "bytes")
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("X-Content-Type-Options", "nosniff")
	if etag != "" {
		hdr.Set("ETag", etag)
	}
	if rng != nil {
		hdr.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", rng.start, rng.end, size))
		hdr.Set("Content-Length", strconv.FormatInt(announced, 10))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		hdr.Set("Content-Length", strconv.FormatInt(announced, 10))
		w.WriteHeader(http.StatusOK)
	}
	// Exactly what was announced: never more, whatever Open hands back. A
	// stored object that ends early, or a copy that fails, is a failed
	// transfer: the headers are gone, so the connection is closed rather than
	// letting a short body read as a complete file.
	if n, err := io.CopyN(w, rc, announced); err != nil {
		h.logger.Warn("account export download interrupted",
			"error", err, "exportId", file.ID, "sent", n, "announced", announced)
		panic(http.ErrAbortHandler)
	}
}

// accountExportETag is the strong validator for an export: its SHA-256, which
// names the file's bytes exactly. Empty when the record carries no usable
// digest, in which case no ETag is sent and no If-Range can match.
func accountExportETag(sha256 string) string {
	if len(sha256) == 0 || len(sha256) > 128 {
		return ""
	}
	for i := 0; i < len(sha256); i++ {
		c := sha256[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return ""
		}
	}
	return `"sha256-` + strings.ToLower(sha256) + `"`
}

// isZeroSuffixRange reports a single suffix range of length zero (bytes=-0,
// bytes=-00), which asks for no bytes and is unsatisfiable.
func isZeroSuffixRange(header string) bool {
	spec, ok := strings.CutPrefix(header, "bytes=")
	if !ok || strings.Contains(spec, ",") {
		return false
	}
	spec = strings.TrimSpace(spec)
	if !strings.HasPrefix(spec, "-") {
		return false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(spec[1:]), 10, 64)
	return err == nil && n == 0
}

// isAccountExportID accepts 1 to accountExportMaxIDBytes bytes of letters,
// digits, "-", "_", "." and ":" (a bare or canonical node id), so nothing odd
// reaches a lookup or a log line.
func isAccountExportID(id string) bool {
	if len(id) == 0 || len(id) > accountExportMaxIDBytes {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == ':':
		default:
			return false
		}
	}
	return true
}

// parseAccountExportContentPath parses /accounts/exports/{id}/content,
// tolerating a leading base prefix. r.URL.Path is already decoded, so an id
// that carried an encoded slash arrives as an extra segment and fails the
// shape. The id is taken exactly as decoded (never trimmed) and must be in the
// closed alphabet of isAccountExportID; "." and a dot-dot anywhere are refused
// too, before the plug-in is asked about it.
func parseAccountExportContentPath(path string) (string, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] != "accounts" || parts[i+1] != "exports" {
			continue
		}
		if i+3 != len(parts)-1 || parts[i+3] != "content" {
			return "", false
		}
		id := parts[i+2]
		if !isAccountExportID(id) || id == "." || strings.Contains(id, "..") {
			return "", false
		}
		return id, true
	}
	return "", false
}
