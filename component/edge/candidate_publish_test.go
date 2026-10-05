package edge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// A version can be published as the CANDIDATE (memql#5601). Every publish
// route used to end in updateSiteBundle, so the only way to put version N+1
// beside a live N was to serve N+1 to the public first and roll back. These
// tests hold the one seam that replaced that: a publish names its target, a
// candidate publish writes candidateRef and leaves bundleRef -- and so every
// visitor -- exactly where they were.

// versionRow is the site row's two version cells. Modelled as cells rather
// than as a call log for rowSiteStore's reason: what matters is what the row
// SAYS afterwards, which is what the edge reads.
type versionRow struct {
	mu        sync.Mutex
	serving   string
	candidate string
	writes    int
}

func (r *versionRow) PointVersion(_ context.Context, _ string, target Target, ref string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writes++
	switch target {
	case TargetServing:
		r.serving = ref
	case TargetCandidate:
		r.candidate = ref
	}
	return nil
}

func (r *versionRow) refs() (serving, candidate string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.serving, r.candidate
}

func TestACandidatePublishLeavesTheServingVersionAndThePublicViewAlone(t *testing.T) {
	blobs := newMemBlobs()
	row := &versionRow{}
	pub := NewPublisher(blobs, row)
	ctx := context.Background()

	n, err := pub.Publish(ctx, "s1", Bundle{"index.html": []byte("<!doctype html><title>N</title><p>version N</p>")}, TargetServing)
	if err != nil {
		t.Fatalf("publish N: %v", err)
	}
	next, err := pub.Publish(ctx, "s1", Bundle{"index.html": []byte("<!doctype html><title>N+1</title><p>version N+1</p>")}, TargetCandidate)
	if err != nil {
		t.Fatalf("publish N+1 as the candidate: %v", err)
	}

	serving, candidate := row.refs()
	if serving != n.BundleRef {
		t.Fatalf("publishing the candidate moved the serving version to %q; it must still be N (%q)", serving, n.BundleRef)
	}
	if candidate != next.BundleRef || next.Target != TargetCandidate {
		t.Fatalf("the candidate is %q (target %q), want N+1 at %q", candidate, next.Target, next.BundleRef)
	}
	if row.writes != 2 {
		t.Errorf("the row was written %d times for two publishes, want 2", row.writes)
	}

	for _, status := range []string{siteStatusDraftValue, siteStatusLiveValue} {
		t.Run(status, func(t *testing.T) {
			site := &Site{ID: "s1", Hostname: "app.example.com", Kind: "spa", Status: status, BundleRef: serving, CandidateRef: candidate}
			exec := newStubPreviewExec()
			h := NewHandler(Options{Resolver: staticResolver{site: site}, Opener: NewBlobOpener(blobs), PreviewExec: exec})
			get := func(token string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodGet, "/", nil)
				req.Host = site.Hostname
				if token != "" {
					req.AddCookie(&http.Cookie{Name: memql.PreviewCookieName, Value: token})
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				return rec
			}

			public := get("")
			switch status {
			case siteStatusLiveValue:
				if public.Code != http.StatusOK || !strings.Contains(public.Body.String(), "version N<") {
					t.Fatalf("the public was served %d %q, want version N", public.Code, public.Body.String())
				}
			default:
				if public.Code != http.StatusNotFound {
					t.Fatalf("a draft answered the public %d, want 404", public.Code)
				}
			}

			token := exec.issue(t, PreviewGrant{ID: "g-" + status, SiteID: site.ID, CandidateRef: candidate})
			previewed := get(token)
			if previewed.Code != http.StatusOK || !strings.Contains(previewed.Body.String(), "version N+1") {
				t.Fatalf("a preview grant was served %d %q, want the candidate N+1", previewed.Code, previewed.Body.String())
			}
		})
	}
}

// AN UNKNOWN TARGET IS REFUSED BEFORE A BYTE IS UPLOADED. Reading it as the
// serving version would put in front of shoppers a version somebody asked to
// keep away from them, which is the one direction this must never fail in.
func TestPublishRefusesAnUnknownTargetBeforeUploading(t *testing.T) {
	for _, target := range []Target{"", "staging", "Candidate"} {
		blobs := newFakeBlobStore()
		row := &versionRow{}
		if _, err := NewPublisher(blobs, row).Publish(context.Background(), "s1", bundleWith(2), target); err == nil {
			t.Errorf("Publish accepted target %q", target)
		}
		if blobs.calls != 0 || row.writes != 0 {
			t.Errorf("target %q uploaded %d files and wrote the row %d times before refusing", target, blobs.calls, row.writes)
		}
	}
}

func TestParseTargetReadsTheWireValues(t *testing.T) {
	for in, want := range map[string]Target{"": TargetServing, "serving": TargetServing, "candidate": TargetCandidate, " candidate ": TargetCandidate} {
		got, err := ParseTarget(in)
		if err != nil || got != want {
			t.Errorf("ParseTarget(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"staging", "production", "CANDIDATE", "preview"} {
		if got, err := ParseTarget(in); err == nil {
			t.Errorf("ParseTarget(%q) = %q, want a refusal", in, got)
		}
	}
}

// THE CI ROUTE'S CANDIDATE IS setSiteCandidate, UNDER THE SAME SYNTHETIC
// OWNER. The one place a target becomes a field is PointVersionStatement, and
// the CI store must reach it: a candidate written through updateSiteBundle
// would be served to every visitor.
func TestEngineSiteStorePointsACandidateThroughSetSiteCandidate(t *testing.T) {
	fe := &fakeEngine{}
	if err := NewEngineSiteStore(fe).PointVersion(context.Background(), "s1", TargetCandidate, "blob://sites/s1/v2/"); err != nil {
		t.Fatalf("PointVersion: %v", err)
	}
	if !strings.HasPrefix(fe.gotQuery, "mutation setSiteCandidate(") {
		t.Fatalf("query = %q, want setSiteCandidate", fe.gotQuery)
	}
	if !strings.Contains(fe.gotQuery, `candidateRef: "blob://sites/s1/v2/"`) || strings.Contains(fe.gotQuery, "bundleRef") {
		t.Errorf("query %q must name the candidate and never the serving version", fe.gotQuery)
	}
	if ac, ok := auth.AccessFromContext(fe.gotCtx); !ok || ac == nil || !ac.IsClusterOwner() || ac.UserId != systemEdgePublishActor {
		t.Errorf("the candidate write ran as %+v, want the synthetic publish owner", ac)
	}
}

func TestPointVersionStatementNamesOneFieldPerTarget(t *testing.T) {
	serving, err := PointVersionStatement("s1", TargetServing, "blob://a/", "")
	if err != nil || serving != `mutation updateSiteBundle(siteId: "s1", bundleRef: "blob://a/")` {
		t.Errorf("serving = %q, %v", serving, err)
	}
	withArtifact, err := PointVersionStatement("s1", TargetServing, "blob://a/", "art-1")
	if err != nil || withArtifact != `mutation updateSiteBundle(siteId: "s1", bundleRef: "blob://a/", artifactId: "art-1")` {
		t.Errorf("serving with provenance = %q, %v", withArtifact, err)
	}
	candidate, err := PointVersionStatement("s1", TargetCandidate, "blob://b/", "")
	if err != nil || candidate != `mutation setSiteCandidate(siteId: "s1", candidateRef: "blob://b/")` {
		t.Errorf("candidate = %q, %v", candidate, err)
	}
	if _, err := PointVersionStatement("s1", Target("staging"), "blob://c/", ""); err == nil {
		t.Error("an unknown target rendered a statement")
	}
}

// A CANDIDATE CARRIES NO PROVENANCE (memql#5601). setSiteCandidate accepts
// artifactId, and the site row has one artifactId: the provenance of the
// bundle it SERVES. A candidate written with one would overwrite that, so the
// statement refuses it rather than dropping it silently -- a caller passing
// provenance for a candidate has a bug worth hearing about.
func TestACandidateStatementRefusesProvenance(t *testing.T) {
	if q, err := PointVersionStatement("s1", TargetCandidate, "blob://b/", "art-1"); err == nil {
		t.Fatalf("a candidate statement carried provenance: %q", q)
	}
	if _, err := PointVersionStatement("s1", TargetServing, "blob://a/", "art-1"); err != nil {
		t.Fatalf("the serving version lost its provenance: %v", err)
	}
}
