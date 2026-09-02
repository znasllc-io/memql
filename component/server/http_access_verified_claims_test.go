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
	return withVerifiedClaimsClass(r, sub, role, "")
}

// withVerifiedClaimsClass is the machine-credential variant: a verified JWT
// whose `class` claim names a surface-pinned credential.
func withVerifiedClaimsClass(r *http.Request, sub, role, class string) *http.Request {
	claims := map[string]any{
		"sub":   sub,
		"email": sub + "@example.test",
		"role":  role,
	}
	if class != "" {
		claims["class"] = class
	}
	vc := &verifier.VerifiedClaims{
		UserId:    sub,
		Role:      role,
		Source:    verifier.SourceJWT,
		Class:     class,
		ClaimsMap: claims,
	}
	return r.WithContext(verifier.AttachToContext(r.Context(), vc))
}

// A MACHINE-SUBJECT credential must NOT gain the Library's write surface just
// because its JWT verifies. Their pins (service_account is read/query-pinned,
// voice_agent is pinned to its gRPC message set) are enforced by the gRPC
// interceptors, which HTTP never runs -- and the deeper reason is the SUBJECT:
// a service-account `sub` names a binary, so resolving an actor for it would
// invent a person to own the bytes. The HTTP access resolution refuses to mint
// an actor for either, and the handler's own gate answers 401.
//
// app_session is deliberately absent from this list since memql#4857 -- see
// TestAppSessionCredentialReachesItsOwnersLibrary directly below, which is the
// other half of one rule and must be read with it.
func TestMachineSubjectCredentialClassesStayOffTheLibraryWriteSurface(t *testing.T) {
	store := newFakeLibraryStore()
	blob := newFakeBlob()
	h := NewArtifactHandler(ArtifactHandlerOptions{
		Logger: quietLogger(), Bucket: "lib", Uploader: blob, Downloader: blob,
		Store: store, Analyzer: newFakeAnalyzer(), PromotionWait: 50 * time.Millisecond,
	})
	for _, class := range []string{"service_account", "voice_agent"} {
		body, ct := uploadBody(t, "notes.md", "text/markdown", []byte("# hello\n"), nil)
		req := httptest.NewRequest(http.MethodPost, "/artifacts", body)
		req.Header.Set("Content-Type", ct)
		req = withVerifiedClaimsClass(req, "v1:identity:user:machine-owner", "owner", class)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("class %q: status = %d, want 401 -- a surface-pinned machine credential must not store bytes over HTTP", class, rec.Code)
		}
	}
	if got := len(store.snapshotCreated()); got != 0 {
		t.Fatalf("createLibraryFile called %d times for machine credentials, want 0", got)
	}
}

// The delegated app run's back-channel DOES reach the Library, as its owner
// (memql#4857).
//
// WHY THIS IS NOT A HOLE IN THE TEST ABOVE. Every other machine class names a
// machine; this one's `sub` is a real user's id, because that is what makes
// row authz apply to a delegated app exactly as it applies to that person's
// browser. So the bytes have an owner without anything being invented, and
// the row lands under the token's subject -- which is what this asserts,
// rather than merely asserting a 201.
//
// It was minted AS service_account until memql#4857, which is why the cockpit
// app-session runner's Library pull and push 401'd against the user's own
// rows: one class name for two kinds of subject cannot express a rule that
// admits one and refuses the other.
func TestAppSessionCredentialReachesItsOwnersLibrary(t *testing.T) {
	store := newFakeLibraryStore()
	blob := newFakeBlob()
	h := NewArtifactHandler(ArtifactHandlerOptions{
		Logger: quietLogger(), Bucket: "lib", Uploader: blob, Downloader: blob,
		Store: store, Analyzer: newFakeAnalyzer(), PromotionWait: 50 * time.Millisecond,
	})

	body, ct := uploadBody(t, "notes.md", "text/markdown", []byte("# hello\n"), nil)
	req := httptest.NewRequest(http.MethodPost, "/artifacts", body)
	req.Header.Set("Content-Type", ct)
	// No role claim, exactly as the mint leaves it: the actor resolves to
	// `reader` plus the real user id. The byte routes gate on the actor
	// resolving to a USER and never on a role, which is why that is enough
	// here and still short of every admin gate.
	req = withVerifiedClaimsClass(req, verifiedSub, "", verifier.ClassAppSession)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 -- an app-session credential acts as the user it names. body: %s",
			rec.Code, rec.Body.String())
	}
	created := store.snapshotCreated()
	if len(created) != 1 {
		t.Fatalf("createLibraryFile called %d times, want 1", len(created))
	}
	if want := "library/" + verifiedSub + "/"; !strings.Contains(created[0].BlobUrl, want) {
		t.Errorf("blobUrl = %q, want the storage path keyed on the OWNING USER (%q) -- "+
			"the credential must not own the bytes, the person must",
			created[0].BlobUrl, want)
	}
}

// The class is admitted on the Library's byte routes and NOWHERE ELSE it was
// not already. The site-bundle publish route names service_account exactly, so
// widening the Library must not have widened that -- a CI publish credential
// and a delegated app run are different things and this is the assertion that
// keeps them so.
func TestAppSessionCredentialCannotPublishASiteBundle(t *testing.T) {
	if verifier.ClassAppSession == verifier.ClassServiceAccount {
		t.Fatal("the two machine classes must not collapse back into one name")
	}
	if httpResolvableClass(verifier.ClassServiceAccount) {
		t.Error("service_account must not resolve an HTTP actor: its subject names a machine")
	}
	if httpResolvableClass(verifier.ClassVoiceAgent) {
		t.Error("voice_agent must not resolve an HTTP actor: its subject names a process")
	}
	if !httpResolvableClass(verifier.ClassAppSession) {
		t.Error("app_session must resolve an HTTP actor: its subject is a real user")
	}
	if !httpResolvableClass("") || !httpResolvableClass("user") || !httpResolvableClass("badge") {
		t.Error("the human classes must keep resolving")
	}
	if httpResolvableClass("something_nobody_adjudicated") {
		t.Error("an unknown class must resolve nothing -- this gate fails closed")
	}
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
