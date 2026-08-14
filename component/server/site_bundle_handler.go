package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/identity/verifier"
)

const (
	// maxBundleFileBytes caps a single file within a bundle. Same order of
	// magnitude as attachment_handler.go's maxAttachmentBytes -- a large
	// single asset (video, a big image) is the realistic ceiling for one
	// file in a static-site build.
	maxBundleFileBytes = 25 * 1024 * 1024 // 25 MB

	// maxBundleTotalBytes caps the WHOLE request body, enforced by wrapping
	// r.Body in http.MaxBytesReader before ParseMultipartForm ever reads it
	// -- unlike a single-file upload, a bundle's danger is the SUM of many
	// files, not any one of them, so the cap has to apply to the request as
	// a whole rather than per-part. Generous enough for a real SPA build
	// (assets, source maps, images) while bounding worst-case storage/
	// memory abuse from a compromised or misconfigured CI credential.
	maxBundleTotalBytes = 500 * 1024 * 1024 // 500 MB

	// maxBundleFileCount bounds the number of files, independent of their
	// size -- a degenerate bundle of hundreds of thousands of near-empty
	// files would stay under maxBundleTotalBytes while still doing an
	// enormous number of individual blob Put calls in Publisher.Publish.
	maxBundleFileCount = 20000

	// maxBundleMultipartMemory is the in-memory buffering threshold passed
	// to ParseMultipartForm (mirrors attachment_handler.go's
	// maxMultipartMemory); parts larger than this spill to temp files,
	// which is why ServeHTTP defers MultipartForm.RemoveAll.
	maxBundleMultipartMemory = 32 * 1024 * 1024 // 32 MB
)

// BundlePublisher is the narrow surface SiteBundleHandler needs from
// component/edge.Publisher, declared ENTIRELY in this package's own types
// (a plain map, SiteBundlePublishResponse) rather than component/edge's
// (Bundle, Result) -- an interface rather than the concrete type so a test
// can fake Publish without constructing a real BlobWriter/SiteStore pair,
// the same reason every other handler in this package (FileUploader,
// AttachmentStore, ...) takes interfaces instead of concrete dependencies.
//
// The own-types choice is NOT just style here: component/server is a tiered
// module (the memql module split, memql#3228, docs/ci-design.md section D3)
// with its own go.mod, and component/edge has no go.mod of its own -- it
// lives directly in the unsplit root module, which none of this tier's
// existing relative-path replace directives reach (unlike
// component/identity, component/auth, and the other sibling tier modules
// this package already depends on). Importing component/edge.Bundle /
// component/edge.Result here directly would make `go mod tidy` inside
// component/server/ fail outright (confirmed: it does, with "module
// github.com/znasllc-io/memql/component/edge not found"). app/transport_sites.go
// -- itself part of the unsplit root module, so it can import both sides --
// is where the adapter between edge.Bundle/edge.Result and these types
// lives.
type BundlePublisher interface {
	Publish(ctx context.Context, siteID string, files map[string][]byte) (SiteBundlePublishResponse, error)
}

// SiteBundlePublishResponse is the JSON body a successful publish returns.
type SiteBundlePublishResponse struct {
	Version   string `json:"version"`
	BundleRef string `json:"bundleRef"`
}

// SiteBundleHandlerOptions configures a SiteBundleHandler.
type SiteBundleHandlerOptions struct {
	Logger    *slog.Logger
	Publisher BundlePublisher
}

// SiteBundleHandler serves POST /sites/{id}/bundles (memql#3713): the
// atomic bundle-publish endpoint, mounted on the bff per the epic's
// controller ruling (component/edge is a library; see the package comment
// on component/edge/publish.go's Publisher type for why the edge node
// itself does not mount this).
//
// This IS the endpoint the endpoint-protocol exception records: multipart
// in (a CI job's arbitrary, variable-shaped file tree), {version, bundleRef}
// JSON out. A declared-but-unserved route is worse than no route at all --
// CLAUDE.md's exception table and HandlerAuthorizedPaths() both describe
// this handler, so this file is what makes those descriptions true rather
// than aspirational.
//
// # Wire shape: the multipart FIELD NAME carries the bundle path, not filename
//
// Every file rides its own multipart part, named for its path WITHIN the
// bundle -- "index.html", "assets/app.js" -- via
// Content-Disposition: form-data; name="assets/app.js". A CI step
// constructs this with `-F "assets/app.js=@dist/assets/app.js"` per file.
//
// This is deliberately NOT carried in the filename parameter, even though
// that reads as the more obvious choice. mime/multipart's OWN parser runs
// every filename through filepath.Base (Part.FileName's documented
// behaviour, mime/multipart/multipart.go) as a stdlib hardening measure --
// so "assets/app.js" silently becomes "app.js" and "../evil.html" silently
// becomes "evil.html" before this handler ever sees either value. Relying
// on filename would have both corrupted every nested bundle path (colliding
// "assets/app.js" and "css/app.js" into the same "app.js" key) and made the
// fs.ValidPath traversal check below unreachable dead code, since
// filepath.Base already strips every ".." and leading "/" it would have
// been checking for. The form NAME parameter carries no such sanitization
// (Part.FormName returns the Content-Disposition "name" value verbatim),
// which is exactly why it is the field this handler reads.
type SiteBundleHandler struct {
	logger    *slog.Logger
	publisher BundlePublisher
}

var _ http.Handler = (*SiteBundleHandler)(nil)

// NewSiteBundleHandler creates a SiteBundleHandler.
func NewSiteBundleHandler(opts SiteBundleHandlerOptions) *SiteBundleHandler {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &SiteBundleHandler{logger: logger, publisher: opts.Publisher}
}

// ServeHTTP handles POST /sites/{id}/bundles. Route must be registered on a
// ServeMux with a prefix that captures {id} (server.SitesBundlePaths()); the
// id itself is parsed out of the full path by parseSiteBundlePublishPath,
// mirroring how AttachmentHandler parses {partitionId} out of the /spaces/
// prefix rather than relying on mux wildcard capture.
func (h *SiteBundleHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, r, http.MethodPost)
		return
	}

	siteID, ok := parseSiteBundlePublishPath(r.URL.Path)
	if !ok {
		http.Error(w, "site id required", http.StatusBadRequest)
		return
	}

	// AUTHORIZATION FIRST, before any multipart parsing -- the same
	// "before any expensive work" ordering AttachmentHandler's ownership
	// gate uses, and doubly warranted here since a bundle upload is
	// potentially hundreds of megabytes.
	//
	// This is what HandlerAuthorizedPaths() membership means for this
	// route: the handler enforces the class="service_account" credential
	// ITSELF. On the bff (where this route is mounted) the ordinary bearer
	// verifier middleware has already run ahead of this handler and
	// requires SOME valid identity-issued credential -- callerCredentialClass
	// reads what it attached to the context rather than re-verifying the
	// token, mirroring callerIsOwnerOrAdmin's (server.go) ambient-context
	// read, which is what lets this fail closed with no credentials at all
	// (an absent claims map, exactly as on a binary with no verifier
	// installed) as well as reject a present-but-wrong-class one.
	//
	// 401 vs. 403 is a real distinction here, not decoration: ok is false
	// ONLY when no claims are attached at all (genuinely unauthenticated).
	// class defaults to ClassUser when claims ARE present but carry no
	// explicit "class" -- an ordinary user JWT commonly has none -- so a
	// real, logged-in user hitting this endpoint correctly gets 403
	// (authenticated, wrong credential) rather than 401 (unauthenticated).
	switch class, ok := callerCredentialClass(r.Context()); {
	case !ok:
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	case class != verifier.ClassServiceAccount:
		h.logger.Warn("site bundle publish: wrong credential class",
			"siteId", siteID, "class", class)
		http.Error(w, "forbidden: this endpoint requires a service-account credential", http.StatusForbidden)
		return
	}

	// Cap the ENTIRE request body before ParseMultipartForm ever reads a
	// byte of it. A single maxAttachmentBytes-style per-part cap isn't
	// enough for a bundle: the danger is the SUM of many files, so the
	// limit has to apply to the request as a whole.
	r.Body = http.MaxBytesReader(w, r.Body, maxBundleTotalBytes)

	if err := r.ParseMultipartForm(maxBundleMultipartMemory); err != nil {
		// ABORTED / OVERSIZED UPLOAD, HANDLED HERE. Publish's atomicity
		// story assumes it receives a complete Bundle; this is where an
		// incomplete request would otherwise become one, so every error
		// path from here down returns BEFORE h.publisher.Publish is ever
		// called -- there is no partial Bundle for Publish to (correctly)
		// upload as if it were a deliberate new version.
		if bodyTooLargeError(err) {
			http.Error(w, fmt.Sprintf("bundle too large: max %d bytes total", maxBundleTotalBytes), http.StatusRequestEntityTooLarge)
			return
		}
		h.logger.Warn("site bundle publish: multipart parse failed", "error", err, "siteId", siteID)
		http.Error(w, "invalid multipart form: the upload may have been interrupted", http.StatusBadRequest)
		return
	}
	// ParseMultipartForm may have spilled parts larger than
	// maxBundleMultipartMemory to temp files; clean them up unconditionally
	// once this request is done, success or failure.
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	if len(r.MultipartForm.File) == 0 {
		http.Error(w, "at least one file is required in the multipart body", http.StatusBadRequest)
		return
	}
	if bundleFileCountExceeded(len(r.MultipartForm.File)) {
		http.Error(w, fmt.Sprintf("bundle carries %d files, max %d", len(r.MultipartForm.File), maxBundleFileCount), http.StatusRequestEntityTooLarge)
		return
	}

	// Iterating a Go map is order-randomized; that's fine here -- the FIRST
	// problem found still rejects the whole request either way (Publish
	// never sees a partial Bundle regardless of which invalid entry this
	// loop happens to reach first), and Publish itself sorts names before
	// hashing/uploading, so bundle content never depends on insertion order.
	bundle := make(map[string][]byte, len(r.MultipartForm.File))
	for name, headers := range r.MultipartForm.File {
		name = strings.TrimSpace(name)
		// THE SAME BOUNDARY blob.go's blobFS.Open enforces on the read
		// side, applied on the write side: fs.ValidPath rejects "..",
		// leading slashes and empty segments outright. A refusal, not a
		// sanitising rewrite -- there is no legitimate bundle entry that
		// needs repairing, and every key this handler accepts must stay
		// openable by exactly the same rule later.
		if !fs.ValidPath(name) {
			http.Error(w, fmt.Sprintf("invalid file path %q in bundle", name), http.StatusBadRequest)
			return
		}
		// Two parts sharing the same form NAME land under one map key here
		// (r.MultipartForm.File is keyed by name, len(headers) counts the
		// parts that shared it) -- the client sent the same bundle path
		// twice, which is the only way a "duplicate" can arise once the
		// path itself comes from the (necessarily unique) map key rather
		// than from a value that could repeat across keys.
		if len(headers) != 1 {
			http.Error(w, fmt.Sprintf("duplicate file path %q in bundle", name), http.StatusBadRequest)
			return
		}

		// Checked from the ALREADY-KNOWN header size before opening or
		// reading anything: by the time this loop runs, ParseMultipartForm
		// has already fully consumed this part off the wire (into memory or
		// a temp file), so Size reflects bytes actually read, not a
		// client-supplied claim -- trustworthy, and cheap to check first.
		// Skips the wasted Open+ReadAll (and the possibly-large allocation
		// it would produce) for the common oversized-file case, rather than
		// reading the whole thing just to reject it.
		if bundleFileTooLarge(headers[0].Size) {
			http.Error(w, fmt.Sprintf("file %q too large: max %d bytes", name, maxBundleFileBytes), http.StatusRequestEntityTooLarge)
			return
		}

		data, err := readBundlePart(headers[0])
		if err != nil {
			h.logger.Warn("site bundle publish: reading a file failed", "error", err, "siteId", siteID, "file", name)
			http.Error(w, fmt.Sprintf("failed to read file %q: the upload may have been interrupted", name), http.StatusBadRequest)
			return
		}
		// Defensive backstop, not the primary gate: readBundlePart's own
		// io.LimitReader caps at maxBundleFileBytes+1, so this only fires
		// if the header Size ever lied -- which the comment above explains
		// it structurally cannot, but a reader shouldn't have to trust that
		// without something enforcing it.
		if bundleFileTooLarge(int64(len(data))) {
			http.Error(w, fmt.Sprintf("file %q too large: max %d bytes", name, maxBundleFileBytes), http.StatusRequestEntityTooLarge)
			return
		}
		bundle[name] = data
	}

	// Handler-level fail-fast: refuse a bundle with no index.html BEFORE
	// any blob upload happens, in addition to Publish's own identical
	// check. Deliberately duplicated rather than left to Publish alone --
	// Publish's version is the one that MUST hold (it is reachable from any
	// future caller, not just this handler), but checking here too means a
	// bad bundle fails in milliseconds instead of after uploading however
	// many megabytes of assets first.
	if _, ok := bundle["index.html"]; !ok {
		http.Error(w, "bundle has no index.html", http.StatusBadRequest)
		return
	}

	res, err := h.publisher.Publish(r.Context(), siteID, bundle)
	if err != nil {
		h.logger.Error("site bundle publish: Publish failed", "error", err, "siteId", siteID)
		http.Error(w, fmt.Sprintf("failed to publish bundle: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(res)
}

// readBundlePart reads one multipart file part fully, capped one byte past
// maxBundleFileBytes so the size check above can distinguish "exactly at the
// limit" from "over it". Any read error -- including the connection
// dropping mid-part -- is returned rather than swallowed, which is what lets
// ServeHTTP abort the whole request instead of publishing a bundle missing
// whatever this file was.
func readBundlePart(fh *multipart.FileHeader) ([]byte, error) {
	f, err := fh.Open()
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, maxBundleFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	return data, nil
}

// bodyTooLargeError reports whether err is how http.MaxBytesReader signals
// that the request body exceeded its limit, as opposed to any other
// multipart/read failure (a truncated connection, a malformed boundary).
//
// Extracted as its own function so the classification is unit-testable
// without constructing an actual maxBundleTotalBytes-sized request --
// *http.MaxBytesError is a concrete exported type (since Go 1.19) a test
// can construct directly.
func bodyTooLargeError(err error) bool {
	var tooBig *http.MaxBytesError
	return errors.As(err, &tooBig)
}

// bundleFileCountExceeded reports whether n files exceeds maxBundleFileCount.
// Extracted so the boundary (exactly maxBundleFileCount vs. one more) is
// testable as a plain integer comparison rather than requiring a test to
// construct that many real multipart parts.
func bundleFileCountExceeded(n int) bool {
	return n > maxBundleFileCount
}

// bundleFileTooLarge reports whether n bytes exceeds maxBundleFileBytes.
// Extracted for the same reason as bundleFileCountExceeded: the boundary is
// then testable as a plain integer comparison. Takes int64 rather than int
// because its callers are a multipart.FileHeader.Size (already int64) and a
// len(data) conversion -- both fold to the same comparison, but the header
// check exists specifically to run before that byte slice is ever read.
func bundleFileTooLarge(n int64) bool {
	return n > maxBundleFileBytes
}

// callerCredentialClass returns the "class" claim of the request's already-
// verified identity. ok is false ONLY when no claims are attached to the
// context at all -- no bearer was verified, the 401 case.
//
// When claims ARE present but carry no explicit "class", this returns
// verifier.ClassUser, mirroring the identical fallback
// VerifiedClaims.Class applies (component/identity/verifier/verifier.go):
// an ordinary user JWT commonly carries no explicit class claim at all, and
// without this fallback such a caller would read as "" here too -- making a
// real, logged-in user indistinguishable from an unauthenticated request
// and drawing 401 instead of the more accurate 403 (authenticated, wrong
// credential).
//
// An ambient-context read, not a fresh token re-verification -- on the bff
// (the only node this route is mounted on) the bearer middleware has
// already run ahead of this handler and attached the verified claims via
// verifier.AttachToContext -> auth.ContextWithClaims. ok mirrors
// callerIsOwnerOrAdmin's shape (server.go): absence is failure, the same
// nil-is-false posture HandlerAuthorizedPaths() membership requires, so
// this fails closed identically whether the ambient context is empty
// because no credential was presented or because (hypothetically) this
// handler ran on a binary with no verifier installed at all.
func callerCredentialClass(ctx context.Context) (class string, ok bool) {
	claims, present := auth.ClaimsFromContext(ctx)
	if !present || claims == nil {
		return "", false
	}
	if c, isStr := claims["class"].(string); isStr && c != "" {
		return c, true
	}
	return verifier.ClassUser, true
}

// parseSiteBundlePublishPath parses POST /sites/{id}/bundles. Returns
// ok=false for anything else, mirroring
// extractPartitionIdFromAttachmentPath's shape (attachment_handler.go): raw
// prefix/suffix matching on r.URL.Path, no base-URL stripping inside the
// handler -- the same convention every route in this file that parses a
// path segment out of itself already follows.
func parseSiteBundlePublishPath(path string) (siteID string, ok bool) {
	const prefix = "/sites/"
	const suffix = "/bundles"

	// The prefix/suffix match, the overlap guard and the single-segment rule
	// now live in segmentBetween (path_segment.go).
	//
	// That guard was written HERE first, in memql#3713, with a comment recording
	// that attachment_handler.go had the identical defect unfixed. memql#3773
	// then found a THIRD copy of the shape, in http_contract.go, that nobody had
	// looked at -- so two of the three were panicking at once while this one was
	// correct in isolation. A fourth careful edit was not the answer; one
	// function the callers share is.
	middle, ok := segmentBetween(path, prefix, suffix)
	if !ok {
		return "", false
	}
	// THE SAME BOUNDARY the bundle file names get, two dozen lines away in
	// ServeHTTP's per-file loop (fs.ValidPath): refuse rather than
	// sanitise. Without this, "/sites/../bundles" -- the decoded form of
	// "/sites/%2e%2e/bundles", which evades http.ServeMux's own path-clean
	// redirect because that only fires when RawPath matches a canonical
	// re-encoding of Path, and percent-encoding is precisely what makes it
	// not match -- parses to siteID="..", landing at the blob key
	// "sites/../vXXX/index.html". fs.ValidPath rejects ".." outright but
	// admits the lone "." as its own documented special case ("." is
	// valid even though otherwise not well-formed), which is wrong for a
	// site id, so "." needs its own rejection here.
	if middle == "." || !fs.ValidPath(middle) {
		return "", false
	}
	return middle, true
}
