package packages

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/edge"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// candidate_publish_db_test.go -- a version published as the CANDIDATE
// (memql#5601), over real rows, through two of the routes that publish, and
// read back by the real edge resolver.
//
// candidate_publish_test.go beside this drives the pipeline over fakes, and
// component/edge's own tests drive the publisher over fakes. Neither can say
// the thing the issue is about: that setSiteCandidate, reached from the
// package route under a person and from the CI route under the synthetic
// publish owner, leaves bundleRef on the ROW where it was -- and that the edge,
// reading that row, serves N to the public and N+1 to a preview grant, on a
// draft deployable and on a live one. That needs the real mutations, the real
// write guards and the real siteByHostname read.
//
// Postgres-gated like its neighbours; MEMQL_REQUIRE_DB=1 turns the skip into a
// failure.

// servingBlobs is one in-memory object store serving both halves: the
// publisher's Put and the edge's Get, so a version uploaded here is a version
// the edge can open by the very ref the row was pointed at.
type servingBlobs struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func (b *servingBlobs) Put(_ context.Context, key string, data []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.objects[key] = append([]byte(nil), data...)
	return nil
}

func (b *servingBlobs) Get(_ context.Context, key string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	data, ok := b.objects[key]
	if !ok {
		return nil, edge.ErrNotFound
	}
	return append([]byte(nil), data...), nil
}

// oneGrant answers the edge's preview-grant read with the one grant this test
// minted. The grant read is not what is under test -- sitePreviewOpen mints
// real ones -- and a stub keeps this case about the publish and the row.
type oneGrant struct {
	digest string
	grant  *edge.PreviewGrant
}

func (g oneGrant) PreviewGrantByToken(_ context.Context, digest string) (*edge.PreviewGrant, error) {
	if digest == g.digest {
		return g.grant, nil
	}
	return nil, nil
}

func (oneGrant) TouchPreviewGrant(context.Context, string, time.Time) error { return nil }

func siteVersions(t *testing.T, db *bun.DB, siteId string) (bundleRef, candidateRef string) {
	t.Helper()
	err := db.QueryRowContext(context.Background(),
		`SELECT COALESCE(payload->>'bundleRef', ''), COALESCE(payload->>'candidateRef', '')
		   FROM "MemoryNodes" WHERE id = ? ORDER BY "createdAt" DESC LIMIT 1`, siteId).Scan(&bundleRef, &candidateRef)
	if err != nil {
		t.Fatalf("reading the versions of %s: %v", siteId, err)
	}
	return bundleRef, candidateRef
}

func TestACandidatePublishLeavesTheServingVersionOnTheRowAndTheEdgeServesBoth(t *testing.T) {
	t.Setenv("MEMQL_DOMAIN", "example.test")
	eng, db := dbEngine(t)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	owner := "candidate-owner-" + suffix
	accountId := "candidate-org-" + suffix
	packageId := "candidate-package-" + suffix
	seedTiePrincipal(t, eng, owner, auth.RoleOwner)
	ownerCtx := tieActorCtx(owner, auth.RoleOwner)
	mustExecute(t, eng, ownerCtx, fmt.Sprintf(`mutation createClientAccount(accountId: %s, name: "Candidate organization")`, langparser.QuoteString(accountId)))
	createTiedPackage(t, eng, ownerCtx, packageId, "Candidate source "+suffix, accountId)

	blobs := &servingBlobs{objects: map[string][]byte{}}
	pub := &enginePublisher{engine: eng, store: &store{engine: eng}, blobs: blobs, logger: discardLogger()}
	ci := edge.NewPublisher(blobs, edge.NewEngineSiteStore(engineAdapter{engine: eng}))

	ensure := func(name, kind string) (siteId, hostname string) {
		t.Helper()
		siteId, hostname, _, err := pub.EnsureSite(ownerCtx, EnsureSiteRequest{
			PackageId: packageId, DeployableName: name, Kind: kind,
			Hostname: "candidate-" + name + "-" + suffix + ".example.test", AccountId: accountId,
		})
		if err != nil {
			t.Fatalf("creating %s: %v", name, err)
		}
		return siteId, hostname
	}
	draftId, draftHost := ensure("draft", KindSPA)
	liveId, liveHost := ensure("live", KindSPA)
	shopId, _ := ensure("shop", KindStorefront)
	t.Cleanup(func() {
		for _, id := range []string{draftId, liveId, shopId, "v1:platform:package:" + packageId, "v1:accounts:account:" + accountId, "v1:identity:user:" + owner} {
			_, _ = db.NewDelete().TableExpr(`"MemoryNodes"`).Where("id = ?", id).Exec(context.Background())
		}
	})

	page := func(text string) edge.Bundle {
		return edge.Bundle{"index.html": []byte("<!doctype html><title>" + text + "</title><p>" + text + "</p>")}
	}

	// N serves on both deployables, published by the package route as ever.
	nRef := map[string]string{}
	for _, id := range []string{draftId, liveId} {
		res, err := pub.PublishBundle(ownerCtx, id, page("version N of "+id), edge.TargetServing)
		if err != nil {
			t.Fatalf("publishing N to %s: %v", id, err)
		}
		nRef[id] = res.BundleRef
	}
	mustExecute(t, eng, ownerCtx, fmt.Sprintf(`mutation updateSiteStatus(siteId: %s, status: "live")`, langparser.QuoteString(liveId)))

	// N+1 as the CANDIDATE: the package route under the person on the draft,
	// the CI route under its synthetic owner on the live one.
	draftNext, err := pub.PublishBundle(ownerCtx, draftId, page("version N+1 of the draft"), edge.TargetCandidate)
	if err != nil {
		t.Fatalf("the package route refused a candidate on a draft: %v", err)
	}
	liveNext, err := ci.Publish(context.Background(), liveId, page("version N+1 of the live site"), edge.TargetCandidate)
	if err != nil {
		t.Fatalf("the CI route refused a candidate on a live site: %v", err)
	}
	for id, next := range map[string]string{draftId: draftNext.BundleRef, liveId: liveNext.BundleRef} {
		bundleRef, candidateRef := siteVersions(t, db, id)
		if bundleRef != nRef[id] {
			t.Fatalf("%s: a candidate publish moved bundleRef to %q; it must still be N (%q)", id, bundleRef, nRef[id])
		}
		if candidateRef != next {
			t.Fatalf("%s: candidateRef is %q, want the version just published (%q)", id, candidateRef, next)
		}
	}

	// THE EDGE, READING THE REAL ROWS. Every read happens after the last write,
	// so neither cache layer can hand back a row from before it.
	resolver := edge.NewResolver(edge.NewEngineExecutor(engineAdapter{engine: eng}), time.Minute)
	for _, c := range []struct {
		name, siteId, host, publicWant, previewWant string
		publicCode                                  int
	}{
		{"draft", draftId, draftHost, "", "version N+1 of the draft", http.StatusNotFound},
		{"live", liveId, liveHost, "version N of " + liveId, "version N+1 of the live site", http.StatusOK},
	} {
		t.Run(c.name, func(t *testing.T) {
			token, digest, err := memqlengine.MintPreviewToken()
			if err != nil {
				t.Fatal(err)
			}
			_, candidate := siteVersions(t, db, c.siteId)
			h := edge.NewHandler(edge.Options{
				Resolver: resolver,
				Opener:   edge.NewBlobOpener(blobs),
				PreviewExec: oneGrant{digest: digest, grant: &edge.PreviewGrant{
					ID: "g-" + c.name, SiteID: memqlengine.BareShortId(c.siteId), CandidateRef: candidate,
					ExpiresAt: time.Now().UTC().Add(time.Hour),
				}},
			})
			get := func(withGrant bool) *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodGet, "/", nil)
				req.Host = c.host
				if withGrant {
					req.AddCookie(&http.Cookie{Name: memqlengine.PreviewCookieName, Value: token})
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				return rec
			}
			public := get(false)
			if public.Code != c.publicCode || (c.publicWant != "" && !strings.Contains(public.Body.String(), c.publicWant)) {
				t.Fatalf("the public got %d %q, want %d carrying %q", public.Code, public.Body.String(), c.publicCode, c.publicWant)
			}
			previewed := get(true)
			if previewed.Code != http.StatusOK || !strings.Contains(previewed.Body.String(), c.previewWant) {
				t.Fatalf("the preview grant got %d %q, want the candidate %q", previewed.Code, previewed.Body.String(), c.previewWant)
			}
		})
	}

	// A STOREFRONT IS REFUSED A CANDIDATE BY THE ENGINE ITSELF, from the CI
	// route's system actor as from a person -- and the row is untouched.
	if _, err := ci.Publish(context.Background(), shopId, page("a storefront candidate"), edge.TargetCandidate); err == nil ||
		!strings.Contains(err.Error(), memqlengine.PreviewRefusalStorefrontHasNoCandidate) {
		t.Fatalf("the CI route published a storefront candidate: %v", err)
	}
	if _, err := pub.PublishBundle(ownerCtx, shopId, page("a storefront candidate"), edge.TargetCandidate); err == nil ||
		!strings.Contains(err.Error(), memqlengine.PreviewRefusalStorefrontHasNoCandidate) {
		t.Fatalf("the package route published a storefront candidate: %v", err)
	}
	if _, candidate := siteVersions(t, db, shopId); candidate != "" {
		t.Fatalf("a refused storefront candidate reached the row: %q", candidate)
	}
}
