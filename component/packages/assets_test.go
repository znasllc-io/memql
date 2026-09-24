package packages

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/znasllc-io/memql/component/edge"
)

func assetFixture(raw []byte) ManifestAsset {
	digest := sha256.Sum256(raw)
	return ManifestAsset{Path: "media/demo.mp4", Source: "https://github.com/acme/widget/releases/download/media-v1/demo.mp4", SHA256: hex.EncodeToString(digest[:]), Size: int64(len(raw))}
}

type testAssetCache struct{ objects map[string][]byte }

func (c *testAssetCache) Read(_ context.Context, key string, _ int64) ([]byte, bool, error) {
	raw, ok := c.objects[key]
	return raw, ok, nil
}
func (c *testAssetCache) Write(_ context.Context, key string, raw []byte) error {
	c.objects[key] = append([]byte(nil), raw...)
	return nil
}

func TestExternalAssetImportChecksBytesAndReusesSharedBlobAcrossReplicas(t *testing.T) {
	raw := []byte("complete video bytes")
	a := assetFixture(raw)
	requests, credentials := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("package credential missing")
		}
		switch r.URL.Path {
		case "/repos/acme/widget/releases/tags/media-v1":
			fmt.Fprintf(w, `{"assets":[{"id":42,"name":"demo.mp4","state":"uploaded","size":%d}]}`, len(raw))
		case "/repos/acme/widget/releases/assets/42":
			if r.Header.Get("Accept") != "application/octet-stream" {
				t.Error("not requesting asset bytes")
			}
			_, _ = w.Write(raw)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	target, _ := url.Parse(server.URL)
	client := server.Client()
	client.Transport = sourceTestTransport{base: client.Transport, target: target}
	cache := &testAssetCache{objects: map[string][]byte{}}
	resolver := func(_ context.Context, id, owner string) (ResolvedCredential, error) {
		credentials++
		if id != "credential" || owner != "owner" {
			t.Fatal("lost package owner")
		}
		return ResolvedCredential{Bearer: "secret"}, nil
	}
	src := RepoSource{RepoUrl: "https://github.com/acme/widget", CredentialId: "credential", OwnerUserId: "owner"}
	for i := 0; i < 2; i++ {
		// Separate process-local objects, one shared durable cache.
		importer := &releaseAssetImporter{http: client, credentials: resolver, cache: cache}
		got, err := importer.Import(context.Background(), "package-a", src, a, Limits{})
		if err != nil || string(got) != string(raw) {
			t.Fatalf("replica%d: %q %v", i, got, err)
		}
	}
	if requests != 2 || credentials != 2 || len(cache.objects) != 1 {
		t.Fatalf("network=%d credentials=%d cache=%d", requests, credentials, len(cache.objects))
	}
	importer := &releaseAssetImporter{http: client, credentials: resolver, cache: cache}
	if _, err := importer.Import(context.Background(), "package-b", src, a, Limits{}); err != nil {
		t.Fatal(err)
	}
	if requests != 4 || len(cache.objects) != 2 {
		t.Fatal("package cache scopes crossed")
	}
	importer.credentials = func(context.Context, string, string) (ResolvedCredential, error) {
		return ResolvedCredential{}, errors.New("revoked")
	}
	if _, err := importer.Import(context.Background(), "package-a", src, a, Limits{}); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("revoked credential read cache: %v", err)
	}
	for key := range cache.objects {
		cache.objects[key] = []byte("tampered cache")
	}
	importer.credentials = resolver
	if _, err := importer.Import(context.Background(), "package-a", src, a, Limits{}); err == nil {
		t.Fatal("corrupt cache accepted")
	}
}

func TestExternalAssetRejectsBadBytesAndForeignRepositories(t *testing.T) {
	want := []byte("good")
	for _, body := range []string{"evil", "short", "far too many bytes"} {
		t.Run(body, func(t *testing.T) {
			a := assetFixture(want)
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if strings.Contains(r.URL.Path, "/tags/") {
					fmt.Fprint(w, `{"assets":[{"id":42,"name":"demo.mp4","state":"uploaded","size":4}]}`)
					return
				}
				fmt.Fprint(w, body)
			}))
			defer server.Close()
			u, _ := url.Parse(server.URL)
			client := server.Client()
			client.Transport = sourceTestTransport{base: client.Transport, target: u}
			cache := &testAssetCache{objects: map[string][]byte{}}
			f := &releaseAssetImporter{http: client, cache: cache}
			if _, err := f.Import(context.Background(), "pkg", RepoSource{RepoUrl: "https://github.com/acme/widget"}, a, Limits{}); err == nil {
				t.Fatal("invalid bytes accepted")
			}
			if len(cache.objects) != 0 {
				t.Fatal("unverified bytes cached")
			}
			a.Source = strings.Replace(a.Source, "acme/widget", "someone/private", 1)
			if _, err := f.Import(context.Background(), "pkg", RepoSource{RepoUrl: "https://github.com/acme/widget"}, a, Limits{}); err == nil {
				t.Fatal("foreign repo accepted")
			}
			if requests != 2 {
				t.Fatal("foreign repository was contacted")
			}
		})
	}
}

type recordingAssetImporter struct {
	calls int
	raw   []byte
	err   error
}

func (f *recordingAssetImporter) Import(context.Context, string, RepoSource, ManifestAsset, Limits) ([]byte, error) {
	f.calls++
	return f.raw, f.err
}

func TestExternalAssetsAreNotFetchedUntilConfirmedBuild(t *testing.T) {
	raw := []byte("movie")
	a := assetFixture(raw)
	for _, prebuilt := range []bool{false, true} {
		t.Run(fmt.Sprint(prebuilt), func(t *testing.T) {
			manifest := Manifest{FormatVersion: 1, Name: "acme", Deployables: []ManifestDeployable{{Name: "web", Path: "web", Kind: KindStatic, Assets: []ManifestAsset{a}}}}
			// JSON is also valid YAML, keeping the fixture identical to the public contract.
			encoded, _ := json.Marshal(manifest)
			tree := fstest.MapFS{ManifestName: &fstest.MapFile{Data: encoded}, "web/package.json": file(`{}`)}
			if prebuilt {
				tree["web/dist/index.html"] = file("<video src='/media/demo.mp4'></video>")
			}
			h := newHarness(t, tree, ownerPackage())
			importer := &recordingAssetImporter{raw: raw}
			h.deps.Assets = importer
			out, err := Deploy(context.Background(), h.deps, DeployRequest{PackageId: "pkg", Actor: plainUser()})
			if err != nil || !out.AwaitingConfirm || importer.calls != 0 {
				t.Fatalf("analysis fetched media: %+v %v calls%d", out, err, importer.calls)
			}
			if len(out.Report.Deployables[0].Assets) != 1 || !strings.Contains(out.Report.Deployables[0].BuildPlan, "external asset") {
				t.Fatal("review omitted assets")
			}
			bundles, err := h.deps.build(context.Background(), DeployRequest{PackageId: "pkg"}, ownerPackage(), &SourceSnapshot{Tree: tree}, out.Report, &DeployOutcome{})
			if err != nil || string(bundles["web"][a.Path]) != string(raw) || importer.calls != 1 {
				t.Fatalf("post-build media missing: %v %v", bundles, err)
			}
		})
	}
}

func TestExternalAssetLimitsAndCollisionsRefuseBeforeImport(t *testing.T) {
	a := assetFixture([]byte("movie"))
	for _, name := range []string{"path", "parent", "size", "total", "count", "skip"} {
		t.Run(name, func(t *testing.T) {
			imp := &recordingAssetImporter{raw: []byte("movie")}
			d := &Deps{Assets: imp}
			dep := DeployableReport{Name: "web", Assets: []ManifestAsset{a}}
			bundle := edge.Bundle{"index.html": []byte("hello")}
			switch name {
			case "path":
				bundle[a.Path] = []byte("existing")
			case "parent":
				bundle["media"] = []byte("file")
			case "size":
				d.Limits.MaxFileBytes = 4
			case "total":
				d.Limits.MaxSourceBytes = 9
			case "count":
				d.Limits.MaxFileCount = 1
			case "skip":
				_, err := d.build(context.Background(), DeployRequest{Placements: map[string]Placement{"web": {Skip: true}}}, ownerPackage(), nil, &Report{Deployables: []DeployableReport{dep}}, &DeployOutcome{})
				if err != nil || imp.calls != 0 {
					t.Fatalf("skipped assets imported: %v", err)
				}
				return
			}
			if err := d.importAssets(context.Background(), DeployRequest{PackageId: "pkg"}, ownerPackage(), dep, bundle); err == nil || imp.calls != 0 {
				t.Fatalf("guard bypass: %v calls%d", err, imp.calls)
			}
		})
	}
}

func TestExternalAssetManifestPathsAndPlanIdentity(t *testing.T) {
	a := assetFixture([]byte("movie"))
	for _, p := range []string{"../secret", "/absolute", "a/../secret", "a\\b", ".", "a/", " a"} {
		bad := a
		bad.Path = p
		if validateAssets([]ManifestAsset{bad}) == nil {
			t.Errorf("accepted path %q", p)
		}
	}
	for _, source := range []string{"http://github.com/acme/widget/releases/download/v/file", "https://localhost/private", "https://github.com@evil.invalid/acme/widget/releases/download/v/file", "https://github.com/acme/widget/releases/download/v/file?token=secret"} {
		bad := a
		bad.Source = source
		if validateAssets([]ManifestAsset{bad}) == nil {
			t.Errorf("accepted source %q", source)
		}
	}
	parent := a
	parent.Path = "media"
	if validateAssets([]ManifestAsset{a, parent}) == nil {
		t.Fatal("file/directory collision accepted")
	}
	if validateAssets([]ManifestAsset{a, a}) == nil {
		t.Fatal("duplicate paths accepted")
	}
	b := a
	b.Path = "second.mp4"
	if assetPlanFingerprint([]ManifestAsset{a, b}) != assetPlanFingerprint([]ManifestAsset{b, a}) {
		t.Fatal("order changed plan")
	}
	rep := &Report{Deployables: []DeployableReport{{Name: "web", Assets: []ManifestAsset{a}}}}
	before := PlanFingerprint(rep)
	rep.Deployables[0].Assets[0].SHA256 = strings.Repeat("a", 64)
	if PlanFingerprint(rep) == before {
		t.Fatal("changed asset bytes kept approved plan")
	}
	if err := validateAssetRepositories(rep, "https://github.com/someone/else"); err == nil || rep.OK {
		t.Fatal("foreign assets were not refused during analysis")
	}
}

func TestExternalAssetsCancelWithoutPublishingOrCachingPartialBytes(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/tags/") {
			fmt.Fprint(w, `{"assets":[{"id":42,"name":"demo.mp4","state":"uploaded","size":5}]}`)
			return
		}
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	client := server.Client()
	client.Transport = sourceTestTransport{base: client.Transport, target: u}
	cache := &testAssetCache{objects: map[string][]byte{}}
	f := &releaseAssetImporter{http: client, cache: cache}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := f.Import(ctx, "pkg", RepoSource{RepoUrl: "https://github.com/acme/widget"}, assetFixture([]byte("movie")), Limits{})
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; err == nil || len(cache.objects) != 0 {
		t.Fatal("cancelled transfer cached partial asset")
	}
}
