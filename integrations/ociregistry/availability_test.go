package ociregistry

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type availabilityFixture struct {
	root, manifest, config, layer string
	bodies                        map[string][]byte
	media                         map[string]string
}

func availabilityImage(t *testing.T, docker bool, change func(map[string]any, map[string]any)) availabilityFixture {
	t.Helper()
	layer := []byte("immutable layer content")
	c := map[string]any{"os": "linux", "architecture": "arm64", "rootfs": map[string]any{"type": "layers", "diff_ids": []string{sha(layer)}}}
	cm, mm, lm := configMedia, manifestMedia, layerMedia
	if docker {
		cm, mm, lm = dockerConfigMedia, dockerManifestMedia, dockerLayerMedia
	}
	m := map[string]any{"schemaVersion": 2, "mediaType": mm, "layers": []any{descriptor(layer, lm)}}
	if change != nil {
		change(c, m)
	}
	config := js(c)
	if _, ok := m["config"]; !ok {
		m["config"] = descriptor(config, cm)
	}
	manifest := js(m)
	f := availabilityFixture{root: sha(manifest), manifest: sha(manifest), config: sha(config), layer: sha(layer), bodies: map[string][]byte{}, media: map[string]string{}}
	f.bodies[f.manifest], f.bodies[f.config], f.bodies[f.layer] = manifest, config, layer
	f.media[f.manifest] = mm
	return f
}

func (f *availabilityFixture) index(media string, entries ...map[string]any) {
	b := js(map[string]any{"schemaVersion": 2, "mediaType": media, "manifests": entries})
	f.root = sha(b)
	f.bodies[f.root], f.media[f.root] = b, media
}
func (f availabilityFixture) entry(digest string, platform bool) map[string]any {
	d := descriptor(f.bodies[digest], f.media[digest])
	if platform {
		d["platform"] = map[string]string{"os": "linux", "architecture": "arm64", "variant": "v8"}
	}
	return d
}
func (f availabilityFixture) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			t.Errorf("availability wrote: %s", r.Method)
			w.WriteHeader(405)
			return
		}
		if r.URL.Path == "/v2/" {
			w.WriteHeader(200)
			return
		}
		var digest string
		for _, kind := range []string{"manifests/", "blobs/"} {
			prefix := "/v2/receipts/image/" + kind
			if strings.HasPrefix(r.URL.Path, prefix) {
				digest = strings.TrimPrefix(r.URL.Path, prefix)
			}
		}
		body, ok := f.bodies[digest]
		if !ok {
			w.WriteHeader(404)
			return
		}
		if media, ok := f.media[digest]; ok {
			w.Header().Set("Content-Type", media)
		}
		w.Header().Set("Docker-Content-Digest", digest)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		if r.Method == http.MethodGet {
			_, _ = w.Write(body)
		}
	})
}
func availabilityClient(t *testing.T, origin string) *AvailabilityVerifier {
	t.Helper()
	v, err := NewAvailabilityVerifier(Target{Origin: origin, Repository: "receipts/image", AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestAvailabilityReadsSelectedPlatformClosure(t *testing.T) {
	for _, profile := range []string{"oci", "docker", "oci-index", "docker-index", "nested"} {
		t.Run(profile, func(t *testing.T) {
			f := availabilityImage(t, strings.HasPrefix(profile, "docker"), nil)
			if strings.Contains(profile, "index") || profile == "nested" {
				other := f.entry(f.manifest, true)
				other["platform"] = map[string]string{"os": "linux", "architecture": "amd64"}
				other["digest"] = "sha256:" + strings.Repeat("a", 64)
				media := indexMedia
				if profile == "docker-index" {
					media = dockerIndexMedia
				}
				f.index(media, other, f.entry(f.manifest, true))
				if profile == "nested" {
					f.index(indexMedia, f.entry(f.root, false))
				}
			}
			s := httptest.NewServer(f.handler(t))
			defer s.Close()
			for i := 0; i < 2; i++ {
				proof, err := availabilityClient(t, s.URL).Check(context.Background(), f.root, "linux/arm64", AvailabilityLimits{})
				if err != nil {
					t.Fatal(err)
				}
				r, err := proof.Receipt()
				if err != nil || r.ImageDigest != f.root || r.ManifestDigest != f.manifest || r.Platform != "linux/arm64" || !strings.HasSuffix(r.Repository, "/receipts/image") {
					t.Fatalf("invalid proof: %#v %v", r, err)
				}
				var total int64
				for _, b := range f.bodies {
					total += int64(len(b))
				}
				if r.ContentBytes != total {
					t.Fatalf("read %d bytes, want %d", r.ContentBytes, total)
				}
				if strings.Contains(fmt.Sprintf("%+v %#v", proof, proof), f.root) {
					t.Fatal("opaque evidence formatting exposed content")
				}
			}
		})
	}
}

func TestAvailabilityReadsPublishedImageWithoutArchive(t *testing.T) {
	v, want := verifiedFixture(t)
	s := httptest.NewServer(quietRegistry())
	defer s.Close()
	if _, err := publisher(t, s.URL).Publish(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := availabilityClient(t, s.URL).Check(context.Background(), want.ImageDigest, want.Platform, AvailabilityLimits{}); err != nil {
		t.Fatal(err)
	}
}

func TestAvailabilityRefusesMissingCorruptOrUnboundedBytes(t *testing.T) {
	for _, fault := range []string{"manifest-corrupt", "config-corrupt", "layer-corrupt", "manifest-missing", "config-missing", "layer-missing", "truncated", "extra-bytes", "byte-limit", "wrong-media", "duplicate-json", "oversized"} {
		t.Run(fault, func(t *testing.T) {
			f := availabilityImage(t, false, nil)
			limits := AvailabilityLimits{}
			switch fault {
			case "manifest-corrupt":
				f.bodies[f.root] = []byte(`{}`)
			case "config-corrupt":
				f.bodies[f.config] = []byte(`{}`)
			case "layer-corrupt":
				f.bodies[f.layer] = []byte("different bytes")
			case "manifest-missing":
				delete(f.bodies, f.root)
			case "config-missing":
				delete(f.bodies, f.config)
			case "layer-missing":
				delete(f.bodies, f.layer)
			case "truncated":
				f.bodies[f.layer] = f.bodies[f.layer][:3]
			case "extra-bytes":
				f.bodies[f.layer] = append(f.bodies[f.layer], 'x')
			case "byte-limit":
				limits.Bytes = 1
			case "wrong-media":
				f.media[f.root] = dockerIndexMedia
			case "duplicate-json":
				b := []byte(strings.Replace(string(f.bodies[f.root]), `"schemaVersion":2`, `"schemaVersion":1,"schemaVersion":2`, 1))
				f.root = sha(b)
				f.bodies[f.root] = b
				f.media[f.root] = manifestMedia
			case "oversized":
				f.bodies[f.root] = []byte(strings.Repeat("x", int(maxJSON)+1))
			}
			s := httptest.NewServer(f.handler(t))
			defer s.Close()
			proof, err := availabilityClient(t, s.URL).Check(context.Background(), f.root, "linux/arm64", limits)
			if err == nil || proof != nil {
				t.Fatal("invalid registry content admitted")
			}
		})
	}
}

func TestAvailabilityRefusesInvalidImageAndIndexProfiles(t *testing.T) {
	for _, fault := range []string{"platform", "features", "diff-count", "diff-digest", "plain-diff", "external-layer", "inline-layer", "layer-size", "layer-media", "config-media", "artifact", "ambiguous-index", "index-size", "index-unknown", "index-platformless", "index-depth", "index-count", "absent-platform"} {
		t.Run(fault, func(t *testing.T) {
			f := availabilityImage(t, false, func(c, m map[string]any) {
				l := m["layers"].([]any)[0].(map[string]any)
				switch fault {
				case "platform":
					c["architecture"] = "amd64"
				case "features":
					c["features"] = []string{"unsupported"}
				case "diff-count":
					c["rootfs"] = map[string]any{"type": "layers", "diff_ids": []string{}}
				case "diff-digest":
					c["rootfs"] = map[string]any{"type": "layers", "diff_ids": []string{"wrong"}}
				case "plain-diff":
					c["rootfs"] = map[string]any{"type": "layers", "diff_ids": []string{"sha256:" + strings.Repeat("b", 64)}}
				case "external-layer":
					l["urls"] = []string{"https://unconfigured.invalid/private"}
				case "inline-layer":
					l["data"] = "eA=="
				case "layer-size":
					l["size"] = maxAvailableBytes + 1
				case "layer-media":
					l["mediaType"] = "unsupported"
				case "config-media":
					m["config"] = descriptor(js(c), "unsupported")
				case "artifact":
					m["artifactType"] = "unsupported"
				}
			})
			if strings.HasPrefix(fault, "index-") || fault == "ambiguous-index" || fault == "absent-platform" {
				entry := f.entry(f.manifest, true)
				switch fault {
				case "index-size":
					entry["size"] = 1
				case "index-unknown":
					entry["mediaType"] = "unsupported"
				case "index-platformless":
					delete(entry, "platform")
				case "absent-platform":
					entry["platform"] = map[string]string{"os": "windows", "architecture": "amd64"}
				}
				entries := []map[string]any{entry}
				if fault == "ambiguous-index" {
					entries = append(entries, entry)
				}
				if fault == "index-count" {
					for len(entries) < 129 {
						entries = append(entries, entry)
					}
				}
				f.index(indexMedia, entries...)
				if fault == "index-depth" {
					for i := 0; i < 5; i++ {
						f.index(indexMedia, f.entry(f.root, false))
					}
				}
			}
			s := httptest.NewServer(f.handler(t))
			defer s.Close()
			if _, err := availabilityClient(t, s.URL).Check(context.Background(), f.root, "linux/arm64", AvailabilityLimits{}); err == nil {
				t.Fatal("unsupported profile accepted")
			}
		})
	}
}

func TestAvailabilityCancellationAndFreshReads(t *testing.T) {
	f := availabilityImage(t, false, nil)
	h := f.handler(t)
	var stall, missing atomic.Bool
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, f.layer) {
			if missing.Load() {
				w.WriteHeader(404)
				return
			}
			if stall.Load() {
				w.WriteHeader(200)
				_, _ = w.Write([]byte("i"))
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				return
			}
		}
		h.ServeHTTP(w, r)
	}))
	defer s.Close()
	v := availabilityClient(t, s.URL)
	if _, err := v.Check(context.Background(), f.root, "linux/arm64", AvailabilityLimits{}); err != nil {
		t.Fatal(err)
	}
	missing.Store(true)
	if _, err := v.Check(context.Background(), f.root, "linux/arm64", AvailabilityLimits{}); err == nil {
		t.Fatal("cached success after blob disappeared")
	}
	missing.Store(false)
	stall.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := v.Check(ctx, f.root, "linux/arm64", AvailabilityLimits{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lost deadline: %v", err)
	}
}

func TestAvailabilityGuardNeverWritesOrExpandsCredentials(t *testing.T) {
	p, err := NewPublisher(Target{Origin: "https://registry.invalid", Repository: "receipts/image", TokenEndpoint: "https://identity.invalid/token", TokenService: "registry"})
	if err != nil {
		t.Fatal(err)
	}
	d := "sha256:" + strings.Repeat("a", 64)
	g := &scopedTransport{publisher: p, readOnly: true, image: d, manifests: map[string]int64{d: 100}, blobs: map[string]int64{d: 200}}
	for _, tc := range []struct {
		method, path string
		ok           bool
	}{
		{"GET", "https://registry.invalid/v2/receipts/image/manifests/" + d, true},
		{"GET", "https://registry.invalid/v2/receipts/image/blobs/" + d, true},
		{"PUT", "https://registry.invalid/v2/receipts/image/manifests/" + d, false},
		{"POST", "https://registry.invalid/v2/receipts/image/blobs/uploads/", false},
		{"DELETE", "https://registry.invalid/v2/receipts/image/manifests/" + d, false},
		{"GET", "https://registry.invalid/v2/receipts/image/manifests/latest", false},
		{"GET", "https://registry.invalid/v2/other/image/blobs/" + d, false},
		{"GET", "https://outside.invalid/v2/receipts/image/blobs/" + d, false},
		{"GET", "https://identity.invalid/token?service=registry&scope=repository:receipts/image:pull", true},
		{"GET", "https://identity.invalid/token?service=registry&scope=repository:receipts/image:pull,push", false},
		{"GET", "https://identity.invalid/token?service=registry&scope=repository:other/image:pull", false},
	} {
		r, _ := http.NewRequest(tc.method, tc.path, nil)
		_, err := g.permitted(r)
		if (err == nil) != tc.ok {
			t.Errorf("%s %s: %v", tc.method, tc.path, err)
		}
	}
	var zero AvailableImage
	if _, err := zero.Receipt(); err == nil {
		t.Fatal("forged proof accepted")
	}
	for _, limits := range []AvailabilityLimits{{Bytes: -1}, {Bytes: maxAvailableBytes + 1}} {
		if _, err := (&AvailabilityVerifier{registry: p}).Check(context.Background(), d, "linux/arm64", limits); err == nil {
			t.Fatal("invalid ceiling accepted")
		}
	}
}

func TestAvailabilityRedactsRegistryFailure(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = io.WriteString(w, "private-token-sentinel")
	}))
	defer s.Close()
	_, err := availabilityClient(t, s.URL).Check(context.Background(), "sha256:"+strings.Repeat("a", 64), "linux/arm64", AvailabilityLimits{})
	if err == nil || strings.Contains(err.Error(), "private-token-sentinel") {
		t.Fatal("registry response escaped redaction")
	}
}

func TestAvailabilityConfiguredAuthenticationAndDownloads(t *testing.T) {
	for _, attack := range []string{"none", "challenge", "scope", "manifest-redirect", "blob-origin", "blob-challenge"} {
		t.Run(attack, func(t *testing.T) {
			f := availabilityImage(t, false, nil)
			var forbidden, exchanges, downloads atomic.Int64
			sink := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forbidden.Add(1); w.WriteHeader(200) }))
			defer sink.Close()
			storage := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				downloads.Add(1)
				if r.Method != "GET" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Referer") != "" {
					t.Error("registry credentials crossed into blob storage")
				}
				if attack == "blob-challenge" {
					w.Header().Set("WWW-Authenticate", `Bearer realm="`+sink.URL+`/token"`)
					w.WriteHeader(401)
					return
				}
				b, ok := f.bodies[strings.TrimPrefix(r.URL.Path, "/objects/")]
				if !ok {
					w.WriteHeader(404)
					return
				}
				_, _ = w.Write(b)
			}))
			defer storage.Close()
			var origin string
			h := f.handler(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					exchanges.Add(1)
					user, password, ok := r.BasicAuth()
					if !ok || user != "operator" || password != "local-fixture" || r.URL.Query().Get("scope") != "repository:receipts/image:pull" {
						t.Error("wrong token authority")
					}
					_, _ = io.WriteString(w, `{"token":"availability-fixture"}`)
					return
				}
				if r.Header.Get("Authorization") != "Bearer availability-fixture" {
					realm := origin + "/token"
					if attack == "challenge" {
						realm = sink.URL + "/token"
					}
					challenge := fmt.Sprintf(`Bearer realm=%q,service="fixture"`, realm)
					w.Header().Set("WWW-Authenticate", challenge)
					w.WriteHeader(401)
					return
				}
				if attack == "scope" && strings.Contains(r.URL.Path, "/manifests/") {
					w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm=%q,service="fixture",scope="repository:other/image:pull"`, origin+"/token"))
					w.WriteHeader(401)
					return
				}
				if attack == "manifest-redirect" && strings.Contains(r.URL.Path, "/manifests/") {
					http.Redirect(w, r, storage.URL+"/forbidden", 307)
					return
				}
				if r.Method == "GET" && strings.Contains(r.URL.Path, "/blobs/") {
					d := strings.TrimPrefix(r.URL.Path, "/v2/receipts/image/blobs/")
					destination := storage.URL + "/objects/" + d + "?sig=private-query-fixture"
					if attack == "blob-origin" {
						destination = sink.URL + "/objects/" + d
					}
					w.Header().Set("Set-Cookie", "registry-only=fixture")
					http.Redirect(w, r, destination, 307)
					return
				}
				h.ServeHTTP(w, r)
			}))
			defer server.Close()
			origin = server.URL
			roots := x509.NewCertPool()
			roots.AddCert(storage.Certificate())
			roots.AddCert(sink.Certificate())
			v, err := NewAvailabilityVerifier(Target{Origin: origin, Repository: "receipts/image", AllowLoopbackHTTP: true, Username: "operator", Password: "local-fixture", TokenEndpoint: origin + "/token", TokenService: "fixture", RootCAs: roots, BlobDownloadOrigins: []string{storage.URL}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = v.Check(context.Background(), f.root, "linux/arm64", AvailabilityLimits{})
			if (err == nil) != (attack == "none") {
				t.Fatalf("unexpected outcome: %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "private-query-fixture") {
				t.Fatal("signed URL leaked")
			}
			if forbidden.Load() != 0 {
				t.Fatal("unconfigured endpoint received a request")
			}
			if attack == "none" && (downloads.Load() != 2 || exchanges.Load() == 0) {
				t.Fatal("credential exchange and complete downloads were not exercised")
			}
			if attack == "challenge" && exchanges.Load() != 0 {
				t.Fatal("unexpected credential exchange")
			}
			if attack == "scope" && exchanges.Load() != 1 {
				t.Fatal("broadened exchange passed read-only guard")
			}
			if attack == "manifest-redirect" && downloads.Load() != 0 {
				t.Fatal("manifest redirect reached blob origin")
			}
		})
	}
}

func TestAvailabilityDistributionRegistry(t *testing.T) {
	origin := os.Getenv("MEMQL_OCI_TEST_REGISTRY")
	if origin == "" {
		t.Skip("MEMQL_OCI_TEST_REGISTRY is not configured")
	}
	v, want := verifiedFixture(t)
	p := publisher(t, origin)
	if _, err := p.Publish(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		proof, err := availabilityClient(t, origin).Check(context.Background(), want.ImageDigest, want.Platform, AvailabilityLimits{})
		if err != nil {
			t.Fatal(err)
		}
		r, err := proof.Receipt()
		if err != nil || r.ImageDigest != want.ImageDigest {
			t.Fatalf("bad external registry receipt: %v", err)
		}
	}
}
