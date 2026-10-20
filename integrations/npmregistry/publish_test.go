package npmregistry

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const packageName = "@acme/sdk"
const packageVersion = "1.2.3"

func archive(t *testing.T, manifest string, extra ...*tar.Header) []byte {
	t.Helper()
	var out bytes.Buffer
	z := gzip.NewWriter(&out)
	w := tar.NewWriter(z)
	for _, entry := range []struct{ name, body string }{{"package/package.json", manifest}, {"package/dist/index.js", "export const version = '1.2.3';\n"}} {
		if err := w.WriteHeader(&tar.Header{Name: entry.name, Mode: 0644, Size: int64(len(entry.body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, entry.body); err != nil {
			t.Fatal(err)
		}
	}
	for _, h := range extra {
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(make([]byte, h.Size)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func expected(body []byte) Expected {
	h := sha256.Sum256(body)
	return Expected{Name: packageName, Version: packageVersion, SHA256: "sha256:" + hex.EncodeToString(h[:]), Size: int64(len(body))}
}

func verified(t *testing.T) *VerifiedPackage {
	t.Helper()
	body := archive(t, `{"name":"@acme/sdk","version":"1.2.3","scripts":{"prepublishOnly":"exit 99"},"dist":{"tarball":"https://attacker.invalid/steal"},"tag":"unsafe"}`)
	v, err := Verify(context.Background(), bytes.NewReader(body), expected(body))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

type registry struct {
	server        *httptest.Server
	writes        atomic.Int64
	metadataCode  int
	writeCode     int
	storeWrite    bool
	stored        []byte
	tarball       string
	name, version string
	badMetadata   string
}

func newRegistry(t *testing.T) *registry {
	t.Helper()
	r := &registry{writeCode: 201, storeWrite: true, name: packageName, version: packageVersion}
	r.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.Header.Get("Authorization") != "Bearer test-credential" {
			t.Error("missing exact credential")
			w.WriteHeader(401)
			return
		}
		if q.URL.Path == "/archive" {
			w.Write(r.stored)
			return
		}
		if q.URL.Path != "/"+packageName {
			t.Errorf("unexpected request path: %s", q.URL.EscapedPath())
			w.WriteHeader(404)
			return
		}
		if q.Method == "GET" {
			if r.metadataCode != 0 {
				w.WriteHeader(r.metadataCode)
				return
			}
			if r.badMetadata != "" {
				io.WriteString(w, r.badMetadata)
				return
			}
			versions := map[string]any{}
			if r.stored != nil {
				u := r.tarball
				if u == "" {
					u = r.server.URL + "/archive"
				}
				versions[packageVersion] = map[string]any{"name": r.name, "version": r.version, "dist": map[string]string{"tarball": u}}
			}
			json.NewEncoder(w).Encode(map[string]any{"name": packageName, "versions": versions})
			return
		}
		if q.Method != "PUT" {
			t.Errorf("unexpected method: %s", q.Method)
			w.WriteHeader(405)
			return
		}
		r.writes.Add(1)
		var payload struct {
			Name        string                                `json:"name"`
			Tags        map[string]string                     `json:"dist-tags"`
			Versions    map[string]map[string]json.RawMessage `json:"versions"`
			Attachments map[string]struct {
				Data   string
				Length int64
			} `json:"_attachments"`
		}
		if err := json.NewDecoder(q.Body).Decode(&payload); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if payload.Name != packageName || len(payload.Tags) != 1 || payload.Tags["latest"] != packageVersion || len(payload.Versions) != 1 || len(payload.Attachments) != 1 {
			t.Error("publication payload changed approved identity")
		}
		if _, ok := payload.Versions[packageVersion]["tag"]; ok {
			t.Error("archive changed publishing tag")
		}
		var dist map[string]string
		if err := json.Unmarshal(payload.Versions[packageVersion]["dist"], &dist); err != nil {
			t.Error(err)
		}
		if !strings.HasPrefix(dist["tarball"], r.server.URL+"/") || !strings.HasPrefix(dist["integrity"], "sha512-") || len(dist["shasum"]) != 40 {
			t.Error("publication retained archive-provided dist metadata")
		}
		for _, a := range payload.Attachments {
			body, err := base64.StdEncoding.DecodeString(a.Data)
			if err != nil || int64(len(body)) != a.Length {
				t.Error("invalid attachment")
			}
			if r.storeWrite {
				r.stored = body
			}
		}
		w.WriteHeader(r.writeCode)
	}))
	t.Cleanup(r.server.Close)
	return r
}

func (r *registry) publisher(t *testing.T) *Publisher {
	t.Helper()
	roots := x509.NewCertPool()
	roots.AddCert(r.server.Certificate())
	p, err := NewPublisher(Target{Origin: r.server.URL, Name: packageName, Version: packageVersion, DistTag: "latest", Token: "test-credential", RootCAs: roots})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPublishExactArchiveAndRecoverWithoutWriting(t *testing.T) {
	r := newRegistry(t)
	v := verified(t)
	for i := 0; i < 2; i++ {
		p := r.publisher(t)
		got, err := p.Publish(context.Background(), v)
		if err != nil {
			t.Fatal(err)
		}
		if got.SHA256 != v.expected.SHA256 || got.Size != v.expected.Size || got.Name != packageName || got.Version != packageVersion {
			t.Fatalf("wrong receipt: %+v", got)
		}
	}
	if r.writes.Load() != 1 || !bytes.Equal(r.stored, v.archive) {
		t.Fatal("recovery rewrote a package or changed the archive")
	}
}

func TestPublicationRefusalsAndUncertainOutcomes(t *testing.T) {
	for _, name := range []string{"wrong bytes", "wrong name", "wrong version", "unauthorized", "server error", "duplicate metadata", "hidden uppercase identity", "upload rejected", "response lost but bytes present", "foreign tarball", "zero handle", "no credential", "wrong approved version"} {
		t.Run(name, func(t *testing.T) {
			r := newRegistry(t)
			v := verified(t)
			p := r.publisher(t)
			want := error(nil)
			wantWrites := int64(0)
			switch name {
			case "wrong bytes":
				r.stored = []byte("changed")
				want = ErrConflict
			case "wrong name":
				r.stored = v.archive
				r.name = "@acme/other"
				want = ErrConflict
			case "wrong version":
				r.stored = v.archive
				r.version = "9.9.9"
				want = ErrConflict
			case "unauthorized":
				r.metadataCode = 401
			case "server error":
				r.metadataCode = 500
			case "duplicate metadata":
				r.badMetadata = `{"name":"@acme/sdk","name":"other","versions":{}}`
			case "hidden uppercase identity":
				r.badMetadata = `{"NAME":"@acme/sdk","versions":{}}`
			case "upload rejected":
				r.writeCode = 401
				r.storeWrite = false
				want = ErrUncertain
				wantWrites = 1
			case "response lost but bytes present":
				r.writeCode = 500
				wantWrites = 1
			case "foreign tarball":
				r.stored = v.archive
				r.tarball = "https://unapproved.invalid/archive"
			case "zero handle":
				v = &VerifiedPackage{}
			case "no credential":
				p.target.Token = ""
			case "wrong approved version":
				p.target.Version = "2.0.0"
			}
			_, err := p.Publish(context.Background(), v)
			if name == "response lost but bytes present" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || (want != nil && !errors.Is(err, want)) {
				t.Fatalf("wanted %v, got %v", want, err)
			}
			if r.writes.Load() != wantWrites {
				t.Fatalf("writes=%d want=%d", r.writes.Load(), wantWrites)
			}
		})
	}
}

func TestArchiveRedirectUsesOnlyApprovedOriginsWithoutCredential(t *testing.T) {
	v := verified(t)
	r := newRegistry(t)
	r.stored = v.archive
	var downloads atomic.Int64
	cdn := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		downloads.Add(1)
		if q.Header.Get("Authorization") != "" {
			t.Error("credential escaped registry")
		}
		w.Write(v.archive)
	}))
	defer cdn.Close()
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.Header.Get("Authorization") != "" {
			t.Error("credential escaped registry")
		}
		http.Redirect(w, q, cdn.URL+"/bytes", 302)
	}))
	defer redirect.Close()
	r.tarball = redirect.URL + "/redirect"
	p := r.publisher(t)
	if _, err := p.Publish(context.Background(), v); err == nil {
		t.Fatal("unapproved origin succeeded")
	}
	if downloads.Load() != 0 {
		t.Fatal("unapproved download contacted")
	}
	for _, s := range []*httptest.Server{cdn, redirect} {
		p.target.RootCAs.AddCert(s.Certificate())
		p.downloads[s.URL] = true
	}
	if _, err := p.Publish(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	if downloads.Load() != 1 || r.writes.Load() != 0 {
		t.Fatal("wrong recovery effects")
	}
}

func TestVerifyRejectsMalformedOrUnboundArchives(t *testing.T) {
	for _, name := range []string{"digest", "size", "name", "version", "private", "duplicate key", "duplicate file", "traversal", "symlink", "gzip checksum", "cancelled", "version leading zero", "prerelease leading zero"} {
		t.Run(name, func(t *testing.T) {
			manifest := `{"name":"@acme/sdk","version":"1.2.3"}`
			var extra []*tar.Header
			switch name {
			case "private":
				manifest = `{"name":"@acme/sdk","version":"1.2.3","private":true}`
			case "duplicate key":
				manifest = `{"name":"@acme/sdk","name":"@acme/sdk","version":"1.2.3"}`
			case "duplicate file":
				extra = []*tar.Header{{Name: "package/package.json", Mode: 0644}}
			case "traversal":
				extra = []*tar.Header{{Name: "package/../../escape", Mode: 0644}}
			case "symlink":
				extra = []*tar.Header{{Name: "package/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}}
			}
			body := archive(t, manifest, extra...)
			if name == "gzip checksum" {
				body[len(body)-8] ^= 1
			}
			want := expected(body)
			switch name {
			case "digest":
				want.SHA256 = "sha256:" + strings.Repeat("0", 64)
			case "size":
				want.Size++
			case "name":
				want.Name = "@acme/other"
			case "version":
				want.Version = "1.2.4"
			case "version leading zero":
				want.Version = "01.2.3"
			case "prerelease leading zero":
				want.Version = "1.2.3-01"
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if name == "cancelled" {
				cancel()
			}
			if _, err := Verify(ctx, bytes.NewReader(body), want); err == nil {
				t.Fatal("invalid archive accepted")
			}
		})
	}
}

func TestTargetsRefuseAmbientOrAmbiguousAuthority(t *testing.T) {
	base := Target{Origin: "https://registry.example", Name: packageName, Version: packageVersion, DistTag: "latest"}
	for _, endpoint := range []string{"http://registry.example", "https://user:secret@registry.example", "https://registry.example/path", "https://registry.example?token=secret", "https://registry.example#fragment", "https://*.example"} {
		v := base
		v.Origin = endpoint
		if _, err := NewPublisher(v); err == nil {
			t.Errorf("accepted %s", endpoint)
		}
	}
	v := base
	v.Origin = "http://127.0.0.1:1234"
	v.AllowLoopbackHTTP = true
	if _, err := NewPublisher(v); err != nil {
		t.Fatal(err)
	}
	v = base
	v.Token = "secret\nheader"
	if _, err := NewPublisher(v); err == nil {
		t.Fatal("header injection accepted")
	}
	v = base
	v.DistTag = "1.2.3"
	if _, err := NewPublisher(v); err == nil {
		t.Fatal("version-like dist-tag accepted")
	}
}
