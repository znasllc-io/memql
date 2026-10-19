package ociregistry

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func indexFixture(t *testing.T, mutate func(map[string][]byte, map[string]any)) ([]byte, Expected) {
	t.Helper()
	files := map[string][]byte{}
	var children []any
	for _, arch := range []string{"amd64", "arm64"} {
		raw, want := fixture(t, fixtureOptions{gzip: true, mutateConfig: func(c map[string]any) { c["architecture"] = arch }})
		tr := tar.NewReader(bytes.NewReader(raw))
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			files[h.Name], err = io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
		}
		d := descriptor(files[blobPath(want.ImageDigest)], manifestMedia)
		d["platform"] = map[string]any{"os": "linux", "architecture": arch}
		children = append(children, d)
	}
	target := children[0].(map[string]any)["digest"].(string)
	statement := js(map[string]any{"_type": "https://in-toto.io/Statement/v0.1", "subject": []any{map[string]any{"name": "fixture", "digest": map[string]string{"sha256": strings.TrimPrefix(target, "sha256:")}}}})
	config := js(map[string]any{"os": "unknown", "architecture": "unknown", "rootfs": map[string]any{"type": "layers", "diff_ids": []string{sha(statement)}}})
	attestation := js(map[string]any{"schemaVersion": 2, "mediaType": manifestMedia, "config": descriptor(config, configMedia), "layers": []any{descriptor(statement, "application/vnd.in-toto+json")}})
	for _, b := range [][]byte{statement, config, attestation} {
		files[blobPath(sha(b))] = b
	}
	a := descriptor(attestation, manifestMedia)
	a["platform"] = map[string]any{"os": "unknown", "architecture": "unknown"}
	a["annotations"] = map[string]any{"vnd.docker.reference.type": "attestation-manifest", "vnd.docker.reference.digest": target}
	children = append(children, a)
	index := map[string]any{"schemaVersion": 2, "mediaType": indexMedia, "manifests": children}
	if mutate != nil {
		mutate(files, index)
	}
	root := js(index)
	files[blobPath(sha(root))] = root
	files["index.json"] = js(map[string]any{"schemaVersion": 2, "mediaType": indexMedia, "manifests": []any{descriptor(root, indexMedia)}})
	raw := archive(t, files)
	return raw, Expected{ArchiveSHA256: sha(raw), ArchiveSize: int64(len(raw)), ImageDigest: sha(root), Platform: "oci-index"}
}

func TestIndexVerifiesEveryPlatformAndPreservesAttestations(t *testing.T) {
	raw, want := indexFixture(t, nil)
	v, err := Verify(context.Background(), bytes.NewReader(raw), want, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if v.index == nil || v.image != nil || len(v.manifests) != 3 {
		t.Fatal("index was flattened or lost its attestation")
	}
	actual, err := v.index.Digest()
	if err != nil || actual.String() != want.ImageDigest {
		t.Fatal("index bytes changed", err)
	}
	if _, err := Verify(context.Background(), bytes.NewReader(raw), want, Limits{UnpackedBytes: 1}); err == nil {
		t.Fatal("unpacked budget ignored")
	}
	want.Platform = "linux/amd64"
	if _, err := Verify(context.Background(), bytes.NewReader(raw), want, Limits{}); err == nil {
		t.Fatal("index accepted as a single platform")
	}
}

func TestIndexRefusesIncompleteOrAmbiguousGraph(t *testing.T) {
	cases := map[string]func(map[string][]byte, map[string]any){
		"missing child": func(f map[string][]byte, i map[string]any) {
			delete(f, blobPath(i["manifests"].([]any)[1].(map[string]any)["digest"].(string)))
		},
		"duplicate child": func(_ map[string][]byte, i map[string]any) {
			i["manifests"] = append(i["manifests"].([]any), i["manifests"].([]any)[0])
		},
		"duplicate platform": func(_ map[string][]byte, i map[string]any) {
			i["manifests"].([]any)[1].(map[string]any)["platform"] = map[string]any{"os": "linux", "architecture": "amd64"}
		},
		"unbound attestation": func(_ map[string][]byte, i map[string]any) {
			i["manifests"].([]any)[2].(map[string]any)["annotations"].(map[string]any)["vnd.docker.reference.digest"] = sha([]byte("elsewhere"))
		},
		"nested index": func(_ map[string][]byte, i map[string]any) {
			i["manifests"].([]any)[0].(map[string]any)["mediaType"] = indexMedia
		},
		"external child": func(_ map[string][]byte, i map[string]any) {
			i["manifests"].([]any)[0].(map[string]any)["urls"] = []string{"https://outside.invalid/image"}
		},
		"corrupt child": func(f map[string][]byte, i map[string]any) {
			f[blobPath(i["manifests"].([]any)[1].(map[string]any)["digest"].(string))] = []byte("changed")
		},
		"wrong platform": func(_ map[string][]byte, i map[string]any) {
			i["manifests"].([]any)[0].(map[string]any)["platform"] = map[string]any{"os": "linux", "architecture": "arm", "variant": "v6"}
		},
		"attestation only": func(_ map[string][]byte, i map[string]any) { i["manifests"] = i["manifests"].([]any)[2:] },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			raw, want := indexFixture(t, mutate)
			v, err := Verify(context.Background(), bytes.NewReader(raw), want, Limits{})
			if err == nil {
				v.Close()
				t.Fatal("invalid index accepted")
			}
		})
	}
}

func TestIndexPublicationReadbackAndFreshHostReplay(t *testing.T) {
	raw, want := indexFixture(t, nil)
	v, err := Verify(context.Background(), bytes.NewReader(raw), want, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	registry := quietRegistry()
	var writes atomic.Int64
	var corruptChild atomic.Bool
	child := v.manifests[1].Digest
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" || r.Method == "POST" || r.Method == "PATCH" {
			writes.Add(1)
		}
		if corruptChild.Load() && r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/manifests/"+child) {
			_, _ = w.Write([]byte(`{"schemaVersion":2}`))
			return
		}
		registry.ServeHTTP(w, r)
	}))
	defer s.Close()
	newPublisher := func() *Publisher {
		p, err := NewPublisher(Target{Origin: s.URL, Repository: "receipts/image", Tag: "16.0.1", AllowLoopbackHTTP: true})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	got, err := newPublisher().Publish(context.Background(), v)
	if err != nil || got.ImageDigest != want.ImageDigest || got.Platform != "oci-index" {
		t.Fatal("full index publication failed", got, err)
	}
	count := writes.Load()
	second, err := Verify(context.Background(), bytes.NewReader(raw), want, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if _, err := newPublisher().Publish(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if writes.Load() != count {
		t.Fatal("fresh host repeated completed publication")
	}
	corruptChild.Store(true)
	if _, err := newPublisher().Publish(context.Background(), second); err == nil {
		t.Fatal("root-only readback accepted corrupted child")
	}
	if writes.Load() != count {
		t.Fatal("corrupt readback caused a repair overwrite")
	}
	corruptChild.Store(false)
	otherRaw, otherWant := indexFixture(t, func(_ map[string][]byte, i map[string]any) { i["annotations"] = map[string]string{"changed": "true"} })
	other, err := Verify(context.Background(), bytes.NewReader(otherRaw), otherWant, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := newPublisher().Publish(context.Background(), other); !errors.Is(err, ErrTagConflict) {
		t.Fatal("index tag collision accepted", err)
	}
	if writes.Load() != count {
		t.Fatal("index tag collision wrote content")
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := newPublisher().Publish(context.Background(), v); err == nil {
		t.Fatal("closed index handle accepted")
	}
}

func TestIndexChildManifestScope(t *testing.T) {
	p := publisher(t, "http://127.0.0.1:1234")
	child := sha([]byte("child"))
	for _, readOnly := range []bool{false, true} {
		s := &scopedTransport{publisher: p, image: sha([]byte("root")), manifests: map[string]int64{child: 42}, readOnly: readOnly}
		r, _ := http.NewRequest("PUT", p.target.Origin+"/v2/receipts/image/manifests/"+child, nil)
		_, err := s.permitted(r)
		if (err != nil) != readOnly {
			t.Fatal("child write did not preserve publication/read-only scope", err)
		}
		r.URL.Path = "/v2/receipts/image/manifests/" + sha([]byte("unreviewed"))
		if _, err := s.permitted(r); err == nil {
			t.Fatal("unreviewed child write accepted")
		}
	}
}

// This assertion makes sure malformed JSON in a retained provenance layer is
// rejected even when the archive and descriptor hashes are recomputed to match.
func TestIndexAttestationSubjectMustMatch(t *testing.T) {
	raw, want := indexFixture(t, func(f map[string][]byte, i map[string]any) {
		a := i["manifests"].([]any)[2].(map[string]any)
		var manifest map[string]any
		_ = json.Unmarshal(f[blobPath(a["digest"].(string))], &manifest)
		statement := js(map[string]any{"subject": []any{map[string]any{"digest": map[string]string{"sha256": strings.Repeat("0", 64)}}}})
		config := js(map[string]any{"os": "unknown", "architecture": "unknown", "rootfs": map[string]any{"type": "layers", "diff_ids": []string{sha(statement)}}})
		manifest["config"], manifest["layers"] = descriptor(config, configMedia), []any{descriptor(statement, "application/vnd.in-toto+json")}
		body := js(manifest)
		for _, b := range [][]byte{statement, config, body} {
			f[blobPath(sha(b))] = b
		}
		a["digest"], a["size"] = sha(body), len(body)
	})
	if v, err := Verify(context.Background(), bytes.NewReader(raw), want, Limits{}); err == nil {
		v.Close()
		t.Fatal("unrelated attestation accepted")
	}
}
