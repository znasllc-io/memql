package ociregistry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/registry"
)

func quietRegistry() http.Handler { return registry.New(registry.Logger(log.New(io.Discard, "", 0))) }
func publisher(t *testing.T, origin string) *Publisher {
	t.Helper()
	p, err := NewPublisher(Target{Origin: origin, Repository: "receipts/image", AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPublishRegistryReadbackAndDuplicate(t *testing.T) {
	v, want := verifiedFixture(t)
	handler := quietRegistry()
	var writes atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" || r.Method == "PATCH" || r.Method == "POST" {
			writes.Add(1)
		}
		handler.ServeHTTP(w, r)
	}))
	defer s.Close()
	p := publisher(t, s.URL)
	got, err := p.Publish(context.Background(), v)
	if err != nil {
		t.Fatal(err)
	}
	if got.ImageDigest != want.ImageDigest || got.ArchiveSHA256 != want.ArchiveSHA256 || got.ArchiveSize != want.ArchiveSize || got.Platform != want.Platform || !strings.HasSuffix(got.Repository, "/receipts/image") {
		t.Fatalf("wrong receipt: %+v", got)
	}
	count := writes.Load()
	if count == 0 {
		t.Fatal("did not publish")
	}
	// A second host independently verifies the artifact and reconciles entirely
	// through registry bytes, without the first publisher's local state.
	raw, secondWant := fixture(t, fixtureOptions{gzip: true})
	second, err := Verify(context.Background(), strings.NewReader(string(raw)), secondWant, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if _, err := publisher(t, s.URL).Publish(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if writes.Load() != count {
		t.Fatal("duplicate publication wrote again")
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Publish(context.Background(), v); err == nil {
		t.Fatal("closed handle accepted")
	}
	if _, err := p.Publish(context.Background(), &VerifiedImage{}); err == nil {
		t.Fatal("forged handle accepted")
	}
}

func TestPublishQueueDeadlineIncludesHandleAdmission(t *testing.T) {
	v, _ := verifiedFixture(t)
	entered := make(chan struct{})
	var once sync.Once
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		<-r.Context().Done()
	}))
	defer s.Close()
	p := publisher(t, s.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	active := make(chan error, 1)
	go func() { _, err := p.Publish(ctx, v); active <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("active publication never acquired the handle")
	}
	queuedCtx, stopQueued := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer stopQueued()
	queued := make(chan error, 1)
	go func() { _, err := p.Publish(queuedCtx, v); queued <- err }()
	select {
	case err := <-queued:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("queued publication ignored deadline", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued publication waited for unrelated transfer after its deadline")
	}
	closed := make(chan error, 1)
	go func() { closed <- v.Close() }()
	select {
	case <-closed:
		t.Fatal("Close removed the active publication's snapshot")
	default:
	}
	cancel()
	if err := <-active; !errors.Is(err, context.Canceled) {
		t.Fatal("active publication lost cancellation", err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if _, err := p.Publish(context.Background(), v); err == nil {
		t.Fatal("closed handle published")
	}
}

func TestPublishLostManifestReplyReconciles(t *testing.T) {
	v, _ := verifiedFixture(t)
	handler := quietRegistry()
	var lost atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" && strings.Contains(r.URL.Path, "/manifests/") {
			buffer := httptest.NewRecorder()
			handler.ServeHTTP(buffer, r)
			if buffer.Code != http.StatusCreated {
				t.Errorf("manifest not accepted: %d %s", buffer.Code, buffer.Body.String())
			}
			lost.Add(1)
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer s.Close()
	if _, err := publisher(t, s.URL).Publish(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	if lost.Load() == 0 {
		t.Fatal("lost response not exercised")
	}
}

func TestPublishUncertainThenIndependentRecovery(t *testing.T) {
	v, _ := verifiedFixture(t)
	handler := quietRegistry()
	var refuse atomic.Bool
	refuse.Store(true)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if refuse.Load() && r.Method == "PUT" && strings.Contains(r.URL.Path, "/manifests/") {
			http.Error(w, "provider failure", http.StatusForbidden)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer s.Close()
	if _, err := publisher(t, s.URL).Publish(context.Background(), v); !errors.Is(err, ErrUncertain) {
		t.Fatalf("wanted uncertain after blobs, got %v", err)
	}
	refuse.Store(false)
	if _, err := publisher(t, s.URL).Publish(context.Background(), v); err != nil {
		t.Fatal(err)
	}
}

func TestPublishVerifiesActualRegistryBlobBytes(t *testing.T) {
	v, _ := verifiedFixture(t)
	handler := quietRegistry()
	var corrupt atomic.Bool
	var writes atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" || r.Method == "PATCH" || r.Method == "POST" {
			writes.Add(1)
		}
		if corrupt.Load() && r.Method == "GET" && strings.Contains(r.URL.Path, "/blobs/sha256:") {
			_, _ = io.WriteString(w, "corrupt")
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer s.Close()
	p := publisher(t, s.URL)
	if _, err := p.Publish(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	count := writes.Load()
	corrupt.Store(true)
	if _, err := p.Publish(context.Background(), v); err == nil {
		t.Fatal("trusted blob HEAD/digest header without bytes")
	}
	if count != writes.Load() {
		t.Fatal("corrupt readback initiated new writes")
	}
}

func TestPublishRefusesCredentialDiversion(t *testing.T) {
	for _, attack := range []string{"challenge", "redirect", "upload-location", "wrong-token-scope"} {
		t.Run(attack, func(t *testing.T) {
			v, _ := verifiedFixture(t)
			var leaks atomic.Int64
			sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaks.Add(1); w.WriteHeader(200) }))
			defer sink.Close()
			handler := quietRegistry()
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch attack {
				case "challenge":
					w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm=%q,service="evil"`, sink.URL+"/token"))
					w.WriteHeader(401)
				case "wrong-token-scope":
					w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm=%q,service="configured",scope="repository:other/repo:pull,push"`, sink.URL+"/token"))
					w.WriteHeader(401)
				case "redirect":
					http.Redirect(w, r, sink.URL+"/v2/", 307)
				case "upload-location":
					if r.Method == "POST" {
						w.Header().Set("Location", sink.URL+"/v2/receipts/image/blobs/uploads/anything")
						w.WriteHeader(202)
					} else {
						handler.ServeHTTP(w, r)
					}
				}
			}))
			defer s.Close()
			target := Target{Origin: s.URL, Repository: "receipts/image", AllowLoopbackHTTP: true, Username: "explicit", Password: "credential-must-stay-scoped"}
			// Same configured token URL but an injected extra scope is still refused.
			if attack == "wrong-token-scope" {
				target.TokenEndpoint = sink.URL + "/token"
				target.TokenService = "configured"
			}
			p, err := NewPublisher(target)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := p.Publish(context.Background(), v); err == nil {
				t.Fatal("unsafe registry interaction succeeded")
			}
			if leaks.Load() != 0 {
				t.Fatalf("out-of-scope endpoint received %d requests", leaks.Load())
			}
		})
	}
}

func TestPublishExplicitTokenEndpoint(t *testing.T) {
	v, _ := verifiedFixture(t)
	handler := quietRegistry()
	var exchanges atomic.Int64
	var origin string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			user, pass, ok := r.BasicAuth()
			if !ok || user != "operator" || pass != "test-credential" {
				t.Error("explicit credential missing")
			}
			q := r.URL.Query()
			if q.Get("service") != "fixture" || !strings.HasPrefix(q.Get("scope"), "repository:receipts/image:") {
				t.Error("wrong scope")
			}
			exchanges.Add(1)
			_, _ = io.WriteString(w, `{"token":"verified-token"}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer verified-token" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm=%q,service="fixture"`, origin+"/token"))
			w.WriteHeader(401)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer s.Close()
	origin = s.URL
	p, err := NewPublisher(Target{Origin: origin, Repository: "receipts/image", AllowLoopbackHTTP: true, Username: "operator", Password: "test-credential", TokenEndpoint: origin + "/token", TokenService: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Publish(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	if exchanges.Load() == 0 {
		t.Fatal("no token exchange exercised")
	}
}

func TestRegistryCannotBroadenTokenScopeAfterAuthentication(t *testing.T) {
	v, _ := verifiedFixture(t)
	var exchanges atomic.Int64
	var origin string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			exchanges.Add(1)
			if scopes := r.URL.Query()["scope"]; len(scopes) != 1 || scopes[0] != "repository:receipts/image:pull" {
				t.Errorf("broadened token exchange escaped guard: %v", scopes)
			}
			_, _ = io.WriteString(w, `{"token":"initial-token"}`)
			return
		}
		if r.URL.Path == "/v2/" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm=%q,service="fixture"`, origin+"/token"))
		} else {
			if r.Header.Get("Authorization") != "Bearer initial-token" {
				t.Error("initial authentication was not exercised")
			}
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm=%q,service="fixture",scope="repository:other/image:pull,push"`, origin+"/token"))
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer s.Close()
	origin = s.URL
	p, err := NewPublisher(Target{Origin: origin, Repository: "receipts/image", AllowLoopbackHTTP: true, Username: "operator", Password: "test-credential", TokenEndpoint: origin + "/token", TokenService: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Publish(context.Background(), v); err == nil {
		t.Fatal("scope expansion succeeded")
	}
	if exchanges.Load() != 1 {
		t.Fatalf("token exchange count=%d; expected only the initial authorized exchange", exchanges.Load())
	}
}

func TestRegistryCannotMoveUploadIntoAnotherRepository(t *testing.T) {
	v, _ := verifiedFixture(t)
	handler := quietRegistry()
	var wrongRepository atomic.Int64
	var redirected atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v2/other/") {
			wrongRepository.Add(1)
			w.WriteHeader(201)
			return
		}
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/blobs/uploads/") {
			redirected.Add(1)
			w.Header().Set("Location", "/v2/other/image/blobs/uploads/id")
			w.WriteHeader(202)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer s.Close()
	if _, err := publisher(t, s.URL).Publish(context.Background(), v); !errors.Is(err, ErrUncertain) {
		t.Fatalf("expected uncertain after session creation, got %v", err)
	}
	if redirected.Load() == 0 || wrongRepository.Load() != 0 {
		t.Fatalf("upload session attempts=%d, wrong repository requests=%d", redirected.Load(), wrongRepository.Load())
	}
}

func TestTransportContractRejectsOtherRepositoriesTagsAndScopes(t *testing.T) {
	p := publisher(t, "http://127.0.0.1:1234")
	s := &scopedTransport{publisher: p, image: sha([]byte("image")), blobs: map[string]int64{sha([]byte("blob")): 4}}
	for _, path := range []string{"/v2/other/image/manifests/" + s.image, "/v2/receipts/image/manifests/latest", "/v2/receipts/image/blobs/uploads/../../other", "/v2/receipts/image/blobs/uploads/id?digest=" + sha(nil), "/v2/receipts/image/blobs/uploads/?mount=" + sha(nil) + "&from=other/image", "/v2/receipts/image/blobs/uploads/id?digest=" + sha([]byte("blob")) + "&other=x"} {
		r, _ := http.NewRequest("PUT", p.target.Origin+path, nil)
		if _, err := s.permitted(r); err == nil {
			t.Fatalf("accepted %s", path)
		}
	}
	for _, raw := range []string{"http://example.com", "https://user:password@example.com", "https://example.com/path", "https://example.com?x=y"} {
		if _, err := NewPublisher(Target{Origin: raw, Repository: "receipts/image", AllowLoopbackHTTP: true}); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestConcurrentPublishAndClose(t *testing.T) {
	v, _ := verifiedFixture(t)
	s := httptest.NewServer(quietRegistry())
	defer s.Close()
	p := publisher(t, s.URL)
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			_, err := p.Publish(context.Background(), v)
			if err != nil && !strings.Contains(err.Error(), "closed") {
				t.Error(err)
			}
		})
	}
	wg.Go(func() {
		if err := v.Close(); err != nil {
			t.Error(err)
		}
	})
	wg.Wait()
}

// Run against a separately started disposable Distribution registry. No cloud
// credentials are read, and an explicit loopback URL is required.
func TestDistributionRegistry(t *testing.T) {
	origin := os.Getenv("MEMQL_OCI_TEST_REGISTRY")
	if origin == "" {
		t.Skip("set MEMQL_OCI_TEST_REGISTRY to a disposable loopback registry")
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" {
		t.Fatal("expected explicit loopback HTTP fixture")
	}
	v, _ := verifiedFixture(t)
	p := publisher(t, origin)
	if _, err := p.Publish(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	if _, err := publisher(t, origin).Publish(context.Background(), v); err != nil {
		t.Fatal(err)
	}
}
