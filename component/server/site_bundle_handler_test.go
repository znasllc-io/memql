package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/identity/verifier"
)

// fakeBundlePublisher fakes BundlePublisher, recording every call so a test
// can assert Publish was (or, more often here, was NOT) reached. Uses
// map[string][]byte / SiteBundlePublishResponse -- this package's own
// types, not component/edge's -- for the same module-boundary reason
// BundlePublisher's doc comment gives: component/server cannot import
// component/edge (see that comment for the full explanation).
type fakeBundlePublisher struct {
	calls     int
	gotSiteID string
	gotBundle map[string][]byte
	result    SiteBundlePublishResponse
	err       error
}

func (f *fakeBundlePublisher) Publish(_ context.Context, siteID string, files map[string][]byte) (SiteBundlePublishResponse, error) {
	f.calls++
	f.gotSiteID = siteID
	f.gotBundle = files
	if f.err != nil {
		return SiteBundlePublishResponse{}, f.err
	}
	return f.result, nil
}

// bundleFile names a file (its path within the bundle) and its content, for
// buildMultipartBundle.
type bundleFile struct {
	name string
	data []byte
}

// buildMultipartBundle encodes files as a multipart body, one part per
// file, each part's FORM NAME (not its filename parameter) set to f.name --
// mirroring the wire shape ServeHTTP reads (see SiteBundleHandler's "Wire
// shape" doc comment for why: mime/multipart runs filename through
// filepath.Base, which would silently collapse a nested path). The filename
// parameter is set to the same value for readability only; the handler
// never reads it.
func buildMultipartBundle(t *testing.T, files []bundleFile) (*bytes.Buffer, string) {
	t.Helper()
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	for _, f := range files {
		part, err := mw.CreateFormFile(f.name, f.name)
		if err != nil {
			t.Fatalf("CreateFormFile(%q): %v", f.name, err)
		}
		if _, err := part.Write(f.data); err != nil {
			t.Fatalf("write part %q: %v", f.name, err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return body, mw.FormDataContentType()
}

// serviceAccountRequest builds a POST request carrying a class=service_account
// credential on its context -- the shape verifier.AttachToContext leaves on a
// real request after the bff's bearer middleware runs. Mirrors
// attachment_handler_authz_test.go's authedRequest for the same reason.
func serviceAccountRequest(t *testing.T, path string, body io.Reader, contentType string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	claims := map[string]any{"sub": "svc:ci", "class": verifier.ClassServiceAccount, "label": "ci-runner"}
	ctx := auth.ContextWithClaims(req.Context(), claims)
	return req.WithContext(ctx)
}

func testSiteBundleHandler(pub BundlePublisher) *SiteBundleHandler {
	return NewSiteBundleHandler(SiteBundleHandlerOptions{
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Publisher: pub,
	})
}

// --- authorization ---------------------------------------------------------

func TestSiteBundleHandler_UnauthenticatedIs401(t *testing.T) {
	pub := &fakeBundlePublisher{}
	h := testSiteBundleHandler(pub)

	req := httptest.NewRequest(http.MethodPost, "/sites/s1/bundles", strings.NewReader(""))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body=%q)", rec.Code, rec.Body.String())
	}
	if pub.calls != 0 {
		t.Errorf("Publish was called %d time(s), want 0", pub.calls)
	}
}

func TestSiteBundleHandler_WrongCredentialClassIs403(t *testing.T) {
	pub := &fakeBundlePublisher{}
	h := testSiteBundleHandler(pub)

	req := httptest.NewRequest(http.MethodPost, "/sites/s1/bundles", strings.NewReader(""))
	ctx := auth.ContextWithClaims(req.Context(), map[string]any{"sub": "v1:identity:user:alice", "class": "user", "role": "owner"})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body=%q)", rec.Code, rec.Body.String())
	}
	if pub.calls != 0 {
		t.Errorf("Publish was called %d time(s), want 0", pub.calls)
	}
}

// Even an owner-role user JWT (a real, valid memQL identity, with no
// explicit "class" claim at all -- the common shape for an ordinary user
// token) must not pass: this endpoint is pinned to the service-account
// class specifically, not merely "any authenticated caller". This is the
// case callerCredentialClass's ClassUser fallback exists for: without it,
// this request would misread as unauthenticated (401) rather than
// authenticated-but-wrong-credential (403).
func TestSiteBundleHandler_OwnerRoleWithoutServiceAccountClassIs403(t *testing.T) {
	pub := &fakeBundlePublisher{}
	h := testSiteBundleHandler(pub)

	req := httptest.NewRequest(http.MethodPost, "/sites/s1/bundles", strings.NewReader(""))
	ctx := auth.ContextWithClaims(req.Context(), map[string]any{"sub": "v1:identity:user:alice", "role": "owner"})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 -- not 401 -- for an authenticated caller with no class claim (body=%q)", rec.Code, rec.Body.String())
	}
	if pub.calls != 0 {
		t.Errorf("Publish was called %d time(s), want 0", pub.calls)
	}
}

// --- routing / method --------------------------------------------------

func TestSiteBundleHandler_WrongMethodIs405(t *testing.T) {
	h := testSiteBundleHandler(&fakeBundlePublisher{})
	req := httptest.NewRequest(http.MethodGet, "/sites/s1/bundles", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestParseSiteBundlePublishPath(t *testing.T) {
	cases := []struct {
		path   string
		wantID string
		wantOK bool
	}{
		{"/sites/s1/bundles", "s1", true},
		{"/sites/v1:platform:site:abc123/bundles", "v1:platform:site:abc123", true},
		{"/sites/bundles", "", false},          // no id segment
		{"/sites//bundles", "", false},         // empty id
		{"/sites/s1/bundles/extra", "", false}, // trailing segment
		{"/sites/s1", "", false},               // no /bundles suffix
		{"/other/s1/bundles", "", false},       // wrong prefix
	}
	for _, c := range cases {
		id, ok := parseSiteBundlePublishPath(c.path)
		if ok != c.wantOK || id != c.wantID {
			t.Errorf("parseSiteBundlePublishPath(%q) = (%q, %v), want (%q, %v)", c.path, id, ok, c.wantID, c.wantOK)
		}
	}
}

// --- multipart body handling --------------------------------------------

func TestSiteBundleHandler_SuccessfulPublish(t *testing.T) {
	pub := &fakeBundlePublisher{result: SiteBundlePublishResponse{Version: "vabc123", BundleRef: "blob://sites/s1/vabc123/"}}
	h := testSiteBundleHandler(pub)

	body, ct := buildMultipartBundle(t, []bundleFile{
		{"index.html", []byte("<html>hi</html>")},
		{"assets/app.js", []byte("console.log(1)")},
	})
	req := serviceAccountRequest(t, "/sites/s1/bundles", body, ct)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%q)", rec.Code, rec.Body.String())
	}
	if pub.calls != 1 {
		t.Fatalf("Publish called %d times, want 1", pub.calls)
	}
	if pub.gotSiteID != "s1" {
		t.Errorf("Publish siteID = %q, want s1", pub.gotSiteID)
	}
	if len(pub.gotBundle) != 2 ||
		string(pub.gotBundle["index.html"]) != "<html>hi</html>" ||
		string(pub.gotBundle["assets/app.js"]) != "console.log(1)" {
		t.Errorf("Publish bundle = %+v, missing or wrong content", pub.gotBundle)
	}

	var resp SiteBundlePublishResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body=%q)", err, rec.Body.String())
	}
	if resp.Version != "vabc123" || resp.BundleRef != "blob://sites/s1/vabc123/" {
		t.Errorf("response = %+v, want version/bundleRef from Publish's Result", resp)
	}
}

func TestSiteBundleHandler_NoFilesIs400(t *testing.T) {
	pub := &fakeBundlePublisher{}
	h := testSiteBundleHandler(pub)

	body, ct := buildMultipartBundle(t, nil)
	req := serviceAccountRequest(t, "/sites/s1/bundles", body, ct)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%q)", rec.Code, rec.Body.String())
	}
	if pub.calls != 0 {
		t.Errorf("Publish was called %d time(s), want 0", pub.calls)
	}
}

// The handler-level index.html check must fire BEFORE Publish is called --
// proving the fail-fast optimization actually short-circuits rather than
// merely duplicating Publish's own check after the fact.
func TestSiteBundleHandler_MissingIndexHTMLIs400AndNeverCallsPublish(t *testing.T) {
	pub := &fakeBundlePublisher{}
	h := testSiteBundleHandler(pub)

	body, ct := buildMultipartBundle(t, []bundleFile{{"assets/app.js", []byte("x")}})
	req := serviceAccountRequest(t, "/sites/s1/bundles", body, ct)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%q)", rec.Code, rec.Body.String())
	}
	if pub.calls != 0 {
		t.Fatalf("Publish was called %d time(s) despite the missing-index.html rejection; want 0", pub.calls)
	}
}

func TestSiteBundleHandler_PathTraversalIsRejected(t *testing.T) {
	cases := []string{"../evil.html", "/etc/passwd", "assets/../../../etc/passwd", "./index.html"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			pub := &fakeBundlePublisher{}
			h := testSiteBundleHandler(pub)

			body, ct := buildMultipartBundle(t, []bundleFile{{"index.html", []byte("ok")}, {name, []byte("bad")}})
			req := serviceAccountRequest(t, "/sites/s1/bundles", body, ct)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 for filename %q (body=%q)", rec.Code, name, rec.Body.String())
			}
			if pub.calls != 0 {
				t.Fatalf("Publish was called despite a traversal filename %q", name)
			}
		})
	}
}

func TestSiteBundleHandler_DuplicatePathIsRejected(t *testing.T) {
	pub := &fakeBundlePublisher{}
	h := testSiteBundleHandler(pub)

	body, ct := buildMultipartBundle(t, []bundleFile{
		{"index.html", []byte("first")},
		{"index.html", []byte("second")},
	})
	req := serviceAccountRequest(t, "/sites/s1/bundles", body, ct)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%q)", rec.Code, rec.Body.String())
	}
	if pub.calls != 0 {
		t.Errorf("Publish was called despite a duplicate path")
	}
}

// A single file over maxBundleFileBytes is refused end to end, with the
// real production constant. The whole-body maxBundleTotalBytes cap is
// proven in isolation by TestBodyTooLargeError* below rather than by an
// actual 500MB request: constructing one would make this suite slow for no
// additional coverage, since the classification logic (bodyTooLargeError)
// is identical either way, and the wiring that applies it
// (http.MaxBytesReader wrapping r.Body before ParseMultipartForm) is a
// single unconditional line read directly in review.
func TestSiteBundleHandler_OversizedFileIs413(t *testing.T) {
	pub := &fakeBundlePublisher{}
	h := testSiteBundleHandler(pub)

	big := make([]byte, maxBundleFileBytes+1)
	body, ct := buildMultipartBundle(t, []bundleFile{{"index.html", []byte("ok")}, {"assets/big.bin", big}})
	req := serviceAccountRequest(t, "/sites/s1/bundles", body, ct)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (body=%q)", rec.Code, rec.Body.String())
	}
	if pub.calls != 0 {
		t.Errorf("Publish was called despite an oversized file")
	}
}

// A publisher error surfaces as 500, not a silent success.
func TestSiteBundleHandler_PublisherErrorIs500(t *testing.T) {
	pub := &fakeBundlePublisher{err: errors.New("blob storage unavailable")}
	h := testSiteBundleHandler(pub)

	body, ct := buildMultipartBundle(t, []bundleFile{{"index.html", []byte("ok")}})
	req := serviceAccountRequest(t, "/sites/s1/bundles", body, ct)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body=%q)", rec.Code, rec.Body.String())
	}
}

// A malformed multipart body (declares a boundary but isn't actually
// multipart-encoded) is refused as an invalid/aborted upload and never
// reaches Publish -- the shape a genuinely truncated connection produces.
// Publish's atomicity story assumes it receives a complete Bundle; this is
// the test that proves the handler never hands it a partial one.
func TestSiteBundleHandler_MalformedMultipartBodyIs400AndNeverCallsPublish(t *testing.T) {
	pub := &fakeBundlePublisher{}
	h := testSiteBundleHandler(pub)

	req := serviceAccountRequest(t, "/sites/s1/bundles", strings.NewReader("not a multipart body"), "multipart/form-data; boundary=xyz")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%q)", rec.Code, rec.Body.String())
	}
	if pub.calls != 0 {
		t.Fatalf("Publish was called %d time(s) despite a malformed body; want 0", pub.calls)
	}
}

// --- isolated boundary tests for the size-limit helpers -----------------

func TestBodyTooLargeErrorDetectsMaxBytesError(t *testing.T) {
	if !bodyTooLargeError(&http.MaxBytesError{Limit: maxBundleTotalBytes}) {
		t.Error("did not detect *http.MaxBytesError")
	}
}

func TestBodyTooLargeErrorRejectsOtherErrors(t *testing.T) {
	if bodyTooLargeError(errors.New("boom")) {
		t.Error("false positive on an unrelated error")
	}
	if bodyTooLargeError(io.ErrUnexpectedEOF) {
		t.Error("false positive on io.ErrUnexpectedEOF")
	}
}

func TestBundleFileCountExceeded(t *testing.T) {
	if bundleFileCountExceeded(maxBundleFileCount) {
		t.Errorf("bundleFileCountExceeded(%d) = true, want false (exactly at the limit)", maxBundleFileCount)
	}
	if !bundleFileCountExceeded(maxBundleFileCount + 1) {
		t.Errorf("bundleFileCountExceeded(%d) = false, want true", maxBundleFileCount+1)
	}
}

func TestBundleFileTooLarge(t *testing.T) {
	if bundleFileTooLarge(maxBundleFileBytes) {
		t.Errorf("bundleFileTooLarge(%d) = true, want false (exactly at the limit)", maxBundleFileBytes)
	}
	if !bundleFileTooLarge(maxBundleFileBytes + 1) {
		t.Errorf("bundleFileTooLarge(%d) = false, want true", maxBundleFileBytes+1)
	}
}
