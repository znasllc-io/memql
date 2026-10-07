package githubrelease

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fixtureAsset struct {
	asset
	body []byte
}

type releaseFixture struct {
	mu      sync.Mutex
	server  *httptest.Server
	target  Target
	assets  []fixtureAsset
	posts   int
	draft   bool
	commit  string
	tag     gitObject
	objects map[string]gitObject
	lost    bool
	corrupt bool
	reject  bool
	// hook runs outside mu and may implement one response or update fixture
	// state under mu. It is installed before starting any publication.
	hook func(http.ResponseWriter, *http.Request) bool
}

func fixture(t *testing.T) *releaseFixture {
	t.Helper()
	f := &releaseFixture{draft: true, commit: strings.Repeat("a", 40), objects: map[string]gitObject{}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.hook != nil && f.hook(w, r) {
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer fixture-token" || r.Header.Get("X-GitHub-Api-Version") != "2026-03-10" {
			t.Error("missing explicit API/upload credential or API version")
			w.WriteHeader(401)
			return
		}
		base := "/repos/owner/project/releases/73"
		switch {
		case r.Method == "GET" && r.URL.Path == base:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": int64(73), "draft": f.draft, "tag_name": f.target.Tag, "upload_url": f.target.UploadOrigin + base + "/assets{?name,label}"})
		case r.Method == "GET" && r.URL.Path == "/repos/owner/project/git/ref/tags/"+f.target.Tag:
			object := f.tag
			if object.Type == "" {
				object = gitObject{SHA: f.commit, Type: "commit"}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ref": "refs/tags/" + f.target.Tag, "object": object})
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/repos/owner/project/git/tags/"):
			sha := strings.TrimPrefix(r.URL.Path, "/repos/owner/project/git/tags/")
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": sha, "object": f.objects[sha]})
		case r.Method == "GET" && r.URL.Path == base+"/assets":
			page, err := strconv.Atoi(r.URL.Query().Get("page"))
			if err != nil || page < 1 || r.URL.Query().Get("per_page") != "100" {
				t.Error("unbounded inventory query")
				w.WriteHeader(400)
				return
			}
			entries := []asset{}
			for index := (page - 1) * 100; index < len(f.assets) && index < page*100; index++ {
				entries = append(entries, f.assets[index].asset)
			}
			_ = json.NewEncoder(w).Encode(entries)
		case r.Method == "POST" && r.URL.Path == base+"/assets":
			f.posts++
			name := r.URL.Query().Get("name")
			if name != f.target.AssetName || len(r.URL.Query()) != 1 || r.Header.Get("Content-Type") != "application/octet-stream" || r.ContentLength < 0 {
				t.Error("wrong upload contract")
				w.WriteHeader(400)
				return
			}
			body, err := io.ReadAll(r.Body)
			if err != nil || int64(len(body)) != r.ContentLength {
				t.Error("upload bytes incomplete", err)
				w.WriteHeader(400)
				return
			}
			if f.reject {
				w.WriteHeader(502)
				return
			}
			for _, existing := range f.assets {
				if existing.Name == name {
					w.WriteHeader(422)
					return
				}
			}
			f.addAsset(name, body)
			if f.lost {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = conn.Close()
				return
			}
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(f.assets[len(f.assets)-1].asset)
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/repos/owner/project/releases/assets/"):
			id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/repos/owner/project/releases/assets/"), 10, 64)
			if r.Header.Get("Accept") != "application/octet-stream" {
				t.Error("asset metadata was accepted as byte proof")
				w.WriteHeader(400)
				return
			}
			for _, a := range f.assets {
				if a.ID == id {
					if f.corrupt {
						_, _ = io.WriteString(w, "corrupt")
					} else {
						_, _ = w.Write(a.body)
					}
					return
				}
			}
			w.WriteHeader(404)
		default:
			t.Error("unexpected protocol operation", r.Method, r.URL.Path)
			w.WriteHeader(400)
		}
	}))
	f.target = Target{APIOrigin: f.server.URL, UploadOrigin: f.server.URL, Repository: "owner/project", Tag: "v1.2.3", SourceCommit: f.commit, ReleaseID: 73, AssetName: "cockpit-linux.tar.gz", Token: "fixture-token", AllowLoopbackHTTP: true}
	t.Cleanup(f.server.Close)
	return f
}

func (f *releaseFixture) addAsset(name string, body []byte) {
	want := expected(body)
	f.assets = append(f.assets, fixtureAsset{asset: asset{ID: int64(len(f.assets)) + 100, Name: name, State: "uploaded", Size: &want.Size, Digest: want.SHA256}, body: append([]byte(nil), body...)})
}

func publisher(t *testing.T, target Target) *Publisher {
	t.Helper()
	p, err := NewPublisher(target)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPublishExactBytesAndReconcileFromAnotherHost(t *testing.T) {
	for _, mode := range []string{"ordinary", "lost reply", "empty", "annotated tag", "tag slash", "no metadata digest"} {
		t.Run(mode, func(t *testing.T) {
			f := fixture(t)
			data := []byte("exact release file\x00\xff")
			switch mode {
			case "lost reply":
				f.lost = true
			case "empty":
				data = nil
			case "annotated tag":
				f.tag = gitObject{SHA: strings.Repeat("b", 40), Type: "tag"}
				f.objects[f.tag.SHA] = gitObject{SHA: f.commit, Type: "commit"}
			case "tag slash":
				f.target.Tag = "cockpit/v1.2.3"
			case "no metadata digest":
				f.addAsset(f.target.AssetName, data)
				f.assets[0].Digest = ""
			}
			v := verified(t, data)
			proof, err := publisher(t, f.target).Publish(context.Background(), v)
			if err != nil || proof.SHA256 != expected(data).SHA256 || proof.Size != int64(len(data)) || proof.ReleaseID != 73 || proof.AssetID != 100 || proof.SourceCommit != f.commit || proof.AssetName != f.target.AssetName {
				t.Fatal("publication did not verify exact target", proof, err)
			}
			posts := f.posts
			// New publisher and verified handle; no in-memory publication state.
			second, err := publisher(t, f.target).Publish(context.Background(), verified(t, data))
			if err != nil || second != proof || f.posts != posts {
				t.Fatal("second host repeated effect", second, err)
			}
		})
	}
}

func TestPublishRefusesConflictsWithoutMutation(t *testing.T) {
	for _, mode := range []string{"checksum", "metadata", "size", "starter", "duplicate", "missing size", "published", "moved tag", "tag loop", "tag depth", "wrong upload URL", "wrong draft ID"} {
		t.Run(mode, func(t *testing.T) {
			f := fixture(t)
			data := []byte("approved asset")
			f.addAsset(f.target.AssetName, data)
			switch mode {
			case "checksum":
				f.corrupt = true
			case "metadata":
				f.assets[0].Digest = "sha256:" + strings.Repeat("f", 64)
			case "size":
				*f.assets[0].Size++
			case "starter":
				f.assets[0].State = "starter"
			case "duplicate":
				f.addAsset(f.target.AssetName, data)
			case "missing size":
				f.assets[0].Size = nil
			case "published":
				f.draft = false
			case "moved tag":
				f.commit = strings.Repeat("b", 40)
			case "tag loop":
				f.tag = gitObject{SHA: strings.Repeat("b", 40), Type: "tag"}
				f.objects[f.tag.SHA] = f.tag
			case "tag depth":
				for n := 0; n < 10; n++ {
					sha := fmt.Sprintf("%040x", n+1)
					next := gitObject{SHA: fmt.Sprintf("%040x", n+2), Type: "tag"}
					f.objects[sha] = next
				}
				f.tag = gitObject{SHA: fmt.Sprintf("%040x", 1), Type: "tag"}
			case "wrong upload URL", "wrong draft ID":
				f.hook = func(w http.ResponseWriter, r *http.Request) bool {
					if r.URL.Path != "/repos/owner/project/releases/73" {
						return false
					}
					id, upload := int64(73), f.target.UploadOrigin+"/repos/owner/project/releases/73/assets{?name,label}"
					if mode == "wrong draft ID" {
						id++
					} else {
						upload = "https://unapproved.invalid/assets{?name,label}"
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "draft": true, "tag_name": f.target.Tag, "upload_url": upload})
					return true
				}
			}
			if _, err := publisher(t, f.target).Publish(context.Background(), verified(t, data)); err == nil || errors.Is(err, ErrUncertain) {
				t.Fatal("preexisting mismatch did not refuse before effect", err)
			}
			if f.posts != 0 {
				t.Fatal("conflict caused upload")
			}
		})
	}
}

func TestPossibleEffectsRemainUncertainAndReconcile(t *testing.T) {
	for _, mode := range []string{"corrupt readback", "lost rejected upload", "published during upload", "moved tag during upload"} {
		t.Run(mode, func(t *testing.T) {
			f := fixture(t)
			f.corrupt = mode == "corrupt readback"
			f.reject = mode == "lost rejected upload"
			f.hook = func(w http.ResponseWriter, r *http.Request) bool {
				if r.Method == http.MethodPost {
					f.mu.Lock()
					if mode == "published during upload" {
						f.draft = false
					}
					if mode == "moved tag during upload" {
						f.commit = strings.Repeat("b", 40)
					}
					f.mu.Unlock()
				}
				return false
			}
			v := verified(t, []byte("approved bytes"))
			if _, err := publisher(t, f.target).Publish(context.Background(), v); !errors.Is(err, ErrUncertain) {
				t.Fatal("possible write falsely reported known outcome", err)
			}
			if f.posts != 1 {
				t.Fatal("POST retried", f.posts)
			}
			if _, err := os.Stat(v.dir); err != nil {
				t.Fatal("uncertain publication discarded verified bytes", err)
			}
			if mode == "corrupt readback" {
				f.corrupt = false
				if _, err := publisher(t, f.target).Publish(context.Background(), v); err != nil || f.posts != 1 {
					t.Fatal("retry did not reconcile existing bytes", err)
				}
			}
		})
	}
}

func TestDownloadRedirectNeverCarriesCredentials(t *testing.T) {
	f := fixture(t)
	data := []byte("private draft asset")
	f.addAsset(f.target.AssetName, data)
	var downloads atomic.Int64
	cdn := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads.Add(1)
		for _, key := range []string{"Authorization", "Cookie", "Referer", "X-GitHub-Api-Version"} {
			if r.Header.Get(key) != "" {
				t.Error("credential material escaped to download origin", key)
			}
		}
		_, _ = w.Write(data)
	}))
	defer cdn.Close()
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.Contains(r.URL.Path, "/releases/assets/") {
			http.Redirect(w, r, cdn.URL+"/opaque?signature=private", 302)
			return true
		}
		return false
	}
	f.target.RootCAs = x509.NewCertPool()
	f.target.RootCAs.AddCert(cdn.Certificate())
	p := publisher(t, f.target)
	v := verified(t, data)
	if _, err := p.Publish(context.Background(), v); err == nil || downloads.Load() != 0 || strings.Contains(err.Error(), "signature") {
		t.Fatal("unconfigured CDN contacted or signed URL exposed", err)
	}
	f.target.DownloadOrigins = []string{cdn.URL}
	if _, err := publisher(t, f.target).Publish(context.Background(), v); err != nil || downloads.Load() != 1 {
		t.Fatal("configured CDN did not verify", err)
	}
}

func TestAssetInventoryPaginationAndLimits(t *testing.T) {
	for _, count := range []int{101, 1000, 1001} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			f := fixture(t)
			data := []byte("last asset")
			for n := 0; n < count-1; n++ {
				f.addAsset(fmt.Sprintf("file-%d", n), nil)
			}
			f.addAsset(f.target.AssetName, data)
			_, err := publisher(t, f.target).Publish(context.Background(), verified(t, data))
			if (count <= 1000 && err != nil) || (count > 1000 && err == nil) || f.posts != 0 {
				t.Fatal("inventory bound/pagination failure", err, f.posts)
			}
		})
	}
}

func TestConcurrentPublishersConvergeOnOneAsset(t *testing.T) {
	f := fixture(t)
	data := []byte("same approved input")
	v1, v2 := verified(t, data), verified(t, data)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, v := range []*VerifiedFile{v1, v2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := publisher(t, f.target).Publish(context.Background(), v)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(f.assets) != 1 {
		t.Fatal("publishers created more than one asset")
	}
}

func TestTargetRefusalsAndNoAmbientCredentials(t *testing.T) {
	f := fixture(t)
	for _, change := range []func(*Target){
		func(t *Target) { t.APIOrigin = "http://remote.invalid" },
		func(t *Target) { t.APIOrigin += "/unscoped" },
		func(t *Target) { t.APIOrigin += "?" },
		func(t *Target) { t.UploadOrigin = "https://user:password@example.invalid" },
		func(t *Target) { t.Repository = "owner/../other" },
		func(t *Target) { t.Tag = "v1..2" },
		func(t *Target) { t.Tag = "v1/.hidden" },
		func(t *Target) { t.Tag = "v1.lock" },
		func(t *Target) { t.AssetName = ".unstable." },
		func(t *Target) { t.AssetName = "with space.zip" },
		func(t *Target) { t.SourceCommit = "main" },
		func(t *Target) { t.ReleaseID = 0 },
		func(t *Target) { t.Token = "secret\nheader" },
		func(t *Target) { t.DownloadOrigins = []string{t.APIOrigin} },
		func(t *Target) { t.DownloadOrigins = []string{"http://127.0.0.1:1234"} },
	} {
		target := f.target
		change(&target)
		if _, err := NewPublisher(target); err == nil {
			t.Fatal("invalid target accepted")
		}
	}
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("GITHUB_TOKEN", "ambient-is-not-authority")
	target := f.target
	target.Token = ""
	if _, err := publisher(t, target).Publish(context.Background(), verified(t, nil)); err == nil || f.posts != 0 {
		t.Fatal("ambient token used")
	}
	v := verified(t, []byte("explicit credentials"))
	if _, err := publisher(t, f.target).Publish(context.Background(), v); err != nil {
		t.Fatal("ambient proxy changed protocol", err)
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := publisher(t, f.target).Publish(context.Background(), v); err == nil {
		t.Fatal("closed handle published")
	}
}

func TestReleaseDriftImmediatelyBeforeUploadRefuses(t *testing.T) {
	f := fixture(t)
	var reads atomic.Int64
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/repos/owner/project/releases/73" && reads.Add(1) == 2 {
			f.mu.Lock()
			f.draft = false
			f.mu.Unlock()
		}
		return false
	}
	if _, err := publisher(t, f.target).Publish(context.Background(), verified(t, []byte("receipt"))); err == nil || errors.Is(err, ErrUncertain) || f.posts != 0 {
		t.Fatal("known draft drift reached upload", err)
	}
}

func TestMetadataBoundsAndAPIOriginConfinement(t *testing.T) {
	for _, mode := range []string{"oversized", "null inventory", "repeated inventory", "API redirect", "error secret", "upload redirect"} {
		t.Run(mode, func(t *testing.T) {
			f := fixture(t)
			f.hook = func(w http.ResponseWriter, r *http.Request) bool {
				if mode == "upload redirect" && r.Method == "POST" {
					http.Redirect(w, r, "https://unapproved.invalid/?private=token", 307)
					return true
				}
				if mode == "upload redirect" {
					return false
				}
				if (mode == "null inventory" || mode == "repeated inventory") && strings.HasSuffix(r.URL.Path, "/assets") {
					if mode == "null inventory" {
						_, _ = io.WriteString(w, "null")
					} else {
						size := int64(0)
						_ = json.NewEncoder(w).Encode([]asset{{ID: 1, Name: "same", Size: &size}, {ID: 1, Name: "same", Size: &size}})
					}
					return true
				}
				if r.URL.Path != "/repos/owner/project/releases/73" {
					return false
				}
				switch mode {
				case "oversized":
					_, _ = io.WriteString(w, strings.Repeat(" ", (2<<20)+1)+"{}")
				case "API redirect":
					http.Redirect(w, r, "https://unapproved.invalid/?private=token", 302)
				case "error secret":
					w.WriteHeader(503)
					_, _ = io.WriteString(w, "private=token")
				default:
					return false
				}
				return true
			}
			_, err := publisher(t, f.target).Publish(context.Background(), verified(t, []byte("asset")))
			if err == nil || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "unapproved") {
				t.Fatal("untrusted protocol reply escaped", err)
			}
			if (mode == "upload redirect") != errors.Is(err, ErrUncertain) {
				t.Fatal("possible-effect classification changed", err)
			}
		})
	}
}

func TestRemoteAssetBytesAndIdentityAreRequired(t *testing.T) {
	for _, mode := range []string{"truncated", "oversized", "replaced", "deleted"} {
		t.Run(mode, func(t *testing.T) {
			f := fixture(t)
			data := []byte("receipt bytes")
			f.addAsset(f.target.AssetName, data)
			f.hook = func(w http.ResponseWriter, r *http.Request) bool {
				if !strings.Contains(r.URL.Path, "/releases/assets/") {
					return false
				}
				f.mu.Lock()
				defer f.mu.Unlock()
				switch mode {
				case "truncated":
					_, _ = w.Write(data[:len(data)-1])
				case "oversized":
					_, _ = w.Write(append(append([]byte(nil), data...), 'x'))
				case "replaced":
					f.assets[0].ID++
					_, _ = w.Write(data)
				case "deleted":
					f.assets = nil
					_, _ = w.Write(data)
				}
				return true
			}
			if _, err := publisher(t, f.target).Publish(context.Background(), verified(t, data)); err == nil || f.posts != 0 {
				t.Fatal("unstable remote asset accepted", err)
			}
		})
	}
}

func TestRedirectLoopIsBoundedAndCredentialFree(t *testing.T) {
	f := fixture(t)
	data := []byte("asset")
	f.addAsset(f.target.AssetName, data)
	var count atomic.Int64
	var cdn *httptest.Server
	cdn = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Error("CDN received credential")
		}
		http.Redirect(w, r, cdn.URL+"/again", 302)
	}))
	defer cdn.Close()
	f.target.DownloadOrigins = []string{cdn.URL}
	f.target.RootCAs = x509.NewCertPool()
	f.target.RootCAs.AddCert(cdn.Certificate())
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.Contains(r.URL.Path, "/releases/assets/") {
			http.Redirect(w, r, cdn.URL+"/start", 302)
			return true
		}
		return false
	}
	if _, err := publisher(t, f.target).Publish(context.Background(), verified(t, data)); err == nil || count.Load() != 3 {
		t.Fatal("download redirect budget failed", count.Load(), err)
	}
}

func TestCloseWaitsForPublicationAndCancellation(t *testing.T) {
	f := fixture(t)
	data := []byte("owned snapshot")
	f.addAsset(f.target.AssetName, data)
	entered := make(chan struct{})
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.Contains(r.URL.Path, "/releases/assets/") {
			close(entered)
			<-r.Context().Done()
			return true
		}
		return false
	}
	v := verified(t, data)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	p := publisher(t, f.target)
	go func() { _, err := p.Publish(ctx, v); done <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("download did not start")
	}
	closed := make(chan error, 1)
	go func() { closed <- v.Close() }()
	select {
	case <-closed:
		t.Fatal("close removed in-use snapshot")
	default:
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost", err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}
