package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/identity/verifier"
)

// http_access_verified_claims_test.go -- the memql#4843 regression family.
//
// THE CONTEXT UNDER TEST IS THE ONE PRODUCTION BUILDS, and that is the whole
// point of this file. The HTTP middleware chain attaches CLAIMS + TokenInfo and
// nothing else: verifier.HTTPMiddleware -> verifier.AttachToContext ->
// auth.ContextWithClaims + auth.ContextWithToken. No HTTP middleware anywhere
// calls auth.ContextWithAccess (component/server/server.go documents the
// invariant at callerIsOwnerOrAdmin). Every earlier test in this package
// stamped auth.ContextWithUserActor onto the request instead -- a THREE-surface
// context (claims + TokenInfo + AccessContext) that only server-side
// on-behalf-of code ever constructs -- which is exactly how the handlers
// shipped 401ing every real caller: the gates read auth.AccessFromContext,
// which over HTTP was always empty.
//
// So these tests mount the handlers behind verifier.AttachToContext with
// fabricated VerifiedClaims, the way a request arrives after the real
// middleware verified a bearer, and assert the routes WORK: the actor resolves
// from the verified `sub`, ownership stamps from it, per-row admission still
// refuses a second user, and a claimless request still gets the 401 it always
// got.

// verifiedSub is the caller every positive case authenticates as -- a
// canonical v1:identity:user id, which is what every identity-issued JWT
// carries in `sub`.
const verifiedSub = "v1:identity:user:test-owner"

// withVerifiedClaims attaches the context the REAL HTTP middleware produces:
// verifier.AttachToContext over fabricated VerifiedClaims. Claims + TokenInfo
// only -- deliberately NOT auth.ContextWithUserActor, which is the shape that
// hid memql#4843 from this suite.
func withVerifiedClaims(r *http.Request, sub, role string) *http.Request {
	vc := &verifier.VerifiedClaims{
		UserId: sub,
		Role:   role,
		Source: verifier.SourceJWT,
		ClaimsMap: map[string]any{
			"sub":   sub,
			"email": sub + "@example.test",
			"role":  role,
		},
	}
	return r.WithContext(verifier.AttachToContext(r.Context(), vc))
}

// ---------------------------------------------------------------------------
// POST /artifacts (one-shot)
// ---------------------------------------------------------------------------

// The one-shot upload lands for a caller carrying only verified claims, and
// the ownership facts -- the storage path and the analysis hand-off's owner --
// both resolve from the token's `sub`.
func TestArtifactUploadResolvesTheActorFromVerifiedClaims(t *testing.T) {
	store := newFakeLibraryStore()
	blob := newFakeBlob()
	analyzer := newFakeAnalyzer()
	h := NewArtifactHandler(ArtifactHandlerOptions{
		Logger: quietLogger(), Bucket: "lib", Uploader: blob, Downloader: blob,
		Store: store, Analyzer: analyzer, PromotionWait: 50 * time.Millisecond,
	})

	body, ct := uploadBody(t, "notes.md", "text/markdown", []byte("# hello\n"), nil)
	req := httptest.NewRequest(http.MethodPost, "/artifacts", body)
	req.Header.Set("Content-Type", ct)
	req = withVerifiedClaims(req, verifiedSub, "owner")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 for a caller the middleware verified. body: %s",
			rec.Code, rec.Body.String())
	}
	created := store.snapshotCreated()
	if len(created) != 1 {
		t.Fatalf("createLibraryFile called %d times, want 1", len(created))
	}
	if want := "library/" + verifiedSub + "/"; !strings.Contains(created[0].BlobUrl, want) {
		t.Errorf("blobUrl = %q, want the storage path keyed on the token's sub (%q)",
			created[0].BlobUrl, want)
	}
	select {
	case areq := <-analyzer.calls:
		if areq.OwnerUserId != verifiedSub {
			t.Errorf("analysis OwnerUserId = %q, want the verified sub %q -- ownership must stamp "+
				"from the claims-resolved actor", areq.OwnerUserId, verifiedSub)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the analyzer was never called for an analyzable type")
	}
}

// ---------------------------------------------------------------------------
// GET /artifacts/{id}/content
// ---------------------------------------------------------------------------

// The content route serves the owner behind verified claims -- and STILL
// refuses a second user's verified claims with the same opaque 404, because
// resolving the actor must widen nothing beyond what per-row admission grants.
func TestArtifactContentServesTheOwnerBehindVerifiedClaims(t *testing.T) {
	store := newFakeLibraryStore()
	blob := newFakeBlob()
	data := []byte("stored bytes")
	seedFileArtifact(store, blob, verifiedSub, "artifact-1", "file-1", data, "text/plain", "notes.txt")

	h := NewArtifactHandler(ArtifactHandlerOptions{
		Logger: quietLogger(), Bucket: "lib", Uploader: blob, Downloader: blob, Store: store,
	})

	get := func(sub string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/artifacts/artifact-1/content", nil)
		req = withVerifiedClaims(req, sub, "owner")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	rec := get(verifiedSub)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner behind verified claims got %d, want 200. body: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != string(data) {
		t.Errorf("body = %q, want the stored bytes", rec.Body.String())
	}

	if rec := get("v1:identity:user:somebody-else"); rec.Code != http.StatusNotFound {
		t.Fatalf("a second user behind verified claims got %d, want 404 -- resolving the actor "+
			"must not widen per-row admission", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// the session family: init / chunk / inventory / complete
// ---------------------------------------------------------------------------

// One chunked session runs end to end behind verified claims: init opens it
// with the owner stamped from `sub`, the chunk stages, the inventory lists it,
// and complete commits and answers the ids.
func TestUploadSessionRoutesResolveTheActorFromVerifiedClaims(t *testing.T) {
	store := newFakeLibraryStore()
	sessions := newFakeSessionStore()
	blocks := newFakeBlocks()
	h := sessionsHandler(store, sessions, blocks, 4, nil)

	do := func(method, path string, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = withVerifiedClaims(req, verifiedSub, "owner")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// init
	rec := do(http.MethodPost, "/artifacts/uploads", `{"name":"clip.mp4","size":4,"mimeType":"video/mp4"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("init status = %d, want 201. body: %s", rec.Code, rec.Body.String())
	}
	var out initResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode init response %q: %v", rec.Body.String(), err)
	}
	row, err := sessions.ByID(auth.ContextWithUserActor(context.Background(), verifiedSub), out.UploadId)
	if err != nil || row == nil {
		t.Fatalf("session not readable by the verified sub: row=%v err=%v -- the owner must stamp "+
			"from the claims-resolved actor", row, err)
	}

	// chunk
	if rec := do(http.MethodPut, "/artifacts/uploads/"+out.UploadId+"/chunks/1", "abcd"); rec.Code != http.StatusNoContent {
		t.Fatalf("chunk status = %d, want 204. body: %s", rec.Code, rec.Body.String())
	}

	// inventory
	rec = do(http.MethodGet, "/artifacts/uploads/"+out.UploadId, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("inventory status = %d, want 200. body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"n":1`) {
		t.Errorf("inventory %q does not list the staged chunk", rec.Body.String())
	}

	// complete
	rec = do(http.MethodPost, "/artifacts/uploads/"+out.UploadId+"/complete", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("complete status = %d, want 201. body: %s", rec.Code, rec.Body.String())
	}
	if created := store.snapshotCreated(); len(created) != 1 {
		t.Fatalf("complete created %d file rows, want 1", len(created))
	}
}

// ---------------------------------------------------------------------------
// the attachment route's ownership gate
// ---------------------------------------------------------------------------

// actorGatedAttachmentStore models what EngineAttachmentStore actually gets
// from the engine: queryOwnedSpaceById pins ownerUserId == actor.userId, and
// actor.userId binds from the AccessContext -- absent, the envelope DENIES and
// the owner's own space reads as not-there. A stub that ignores the context
// (like stubAttachmentStore) is exactly how this suite missed memql#4843.
type actorGatedAttachmentStore struct {
	owner        string
	createdCount int
	attachment   *AttachmentRow
}

func (s *actorGatedAttachmentStore) CallerOwnsSpace(ctx context.Context, _ string) (bool, error) {
	ac, ok := auth.AccessFromContext(ctx)
	return ok && ac != nil && strings.TrimSpace(ac.UserId) == s.owner, nil
}

func (s *actorGatedAttachmentStore) CreateAttachment(_ context.Context, _ AttachmentCreateParams) (json.RawMessage, error) {
	s.createdCount++
	return json.RawMessage(`{"id":"v1:common:attachment:new"}`), nil
}

func (s *actorGatedAttachmentStore) GetAttachment(ctx context.Context, _, _ string) (*AttachmentRow, error) {
	if ac, ok := auth.AccessFromContext(ctx); !ok || ac == nil || strings.TrimSpace(ac.UserId) != s.owner {
		return nil, nil
	}
	return s.attachment, nil
}

// The attachment upload's CallerOwnsSpace gate resolves the caller from
// verified claims: the space owner's POST lands 201 instead of the opaque 404
// an unresolvable actor produces.
func TestAttachmentUploadResolvesTheActorForTheOwnershipCheck(t *testing.T) {
	store := &actorGatedAttachmentStore{owner: verifiedSub}
	h := NewAttachmentHandler(AttachmentHandlerOptions{Logger: quietLogger(), Store: store})

	body, ct := uploadBody(t, "notes.md", "text/markdown", []byte("# hi\n"), nil)
	req := httptest.NewRequest(http.MethodPost, "/spaces/v1:cognition:space:s1/attachments", body)
	req.Header.Set("Content-Type", ct)
	req = withVerifiedClaims(req, verifiedSub, "owner")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 -- the ownership check must see the claims-resolved "+
			"actor, or the owner's own space reads as not-there. body: %s", rec.Code, rec.Body.String())
	}
	if store.createdCount != 1 {
		t.Fatalf("CreateAttachment called %d times, want 1", store.createdCount)
	}
}

// The download half of the same route resolves the caller the same way.
func TestAttachmentDownloadResolvesTheActorForTheOwnershipCheck(t *testing.T) {
	store := &actorGatedAttachmentStore{
		owner: verifiedSub,
		attachment: &AttachmentRow{
			ID: "att1", FileName: "birds.md", MimeType: "text/markdown",
			BlobUrl:     "https://acct.blob.core.windows.net/c/spaces/s1/attachments/x/birds.md",
			PartitionId: "v1:cognition:space:s1",
		},
	}
	h := NewAttachmentHandler(AttachmentHandlerOptions{
		Logger: quietLogger(), Store: store,
		Downloader: &stubDownloader{data: []byte("# Ten birds")},
	})

	req := httptest.NewRequest(http.MethodGet,
		"/spaces/v1:cognition:space:s1/attachments/v1:cognition:space:s1:att1", nil)
	req = withVerifiedClaims(req, verifiedSub, "owner")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "# Ten birds" {
		t.Fatalf("body = %q, want the file bytes", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// claimless requests keep their 401
// ---------------------------------------------------------------------------

// Resolving the actor from claims must not invent one where there are none: a
// request with NO claims -- which in production the middleware refuses before
// any handler runs, and which reaches a handler only on a node with no
// verifier installed -- keeps answering 401 exactly as before.
func TestLibraryRoutesStillRefuseAClaimlessRequest(t *testing.T) {
	store := newFakeLibraryStore()
	sessions := newFakeSessionStore()
	h := sessionsHandler(store, sessions, newFakeBlocks(), 4, nil)

	for name, req := range map[string]*http.Request{
		"one-shot upload":   httptest.NewRequest(http.MethodPost, "/artifacts", strings.NewReader("")),
		"content":           httptest.NewRequest(http.MethodGet, "/artifacts/artifact-1/content", nil),
		"session init":      httptest.NewRequest(http.MethodPost, "/artifacts/uploads", strings.NewReader(`{"name":"x","size":1}`)),
		"session chunk":     httptest.NewRequest(http.MethodPut, "/artifacts/uploads/u1/chunks/1", strings.NewReader("a")),
		"session inventory": httptest.NewRequest(http.MethodGet, "/artifacts/uploads/u1", nil),
		"session complete":  httptest.NewRequest(http.MethodPost, "/artifacts/uploads/u1/complete", nil),
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s with no claims answered %d, want 401", name, rec.Code)
		}
	}
}
