package ociregistry

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func sha(b []byte) string { h := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(h[:]) }
func js(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
func descriptor(b []byte, media string) map[string]any {
	return map[string]any{"mediaType": media, "digest": sha(b), "size": len(b)}
}

type fixtureOptions struct {
	gzip           bool
	mutateConfig   func(map[string]any)
	mutateManifest func(map[string]any)
	mutateIndex    func(map[string]any)
	mutateFiles    func(map[string][]byte)
}

func fixture(t *testing.T, o fixtureOptions) ([]byte, Expected) {
	t.Helper()
	var content bytes.Buffer
	tw := tar.NewWriter(&content)
	data := []byte(strings.Repeat("verified layer bytes\n", 100))
	if err := tw.WriteHeader(&tar.Header{Name: "file.txt", Mode: 0644, Size: int64(len(data)), Format: tar.FormatUSTAR}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	layer := content.Bytes()
	diff := sha(layer)
	media := layerMedia
	if o.gzip {
		var b bytes.Buffer
		gz := gzip.NewWriter(&b)
		_, _ = gz.Write(layer)
		_ = gz.Close()
		layer = b.Bytes()
		media += "+gzip"
	}
	conf := map[string]any{"architecture": "arm64", "os": "linux", "rootfs": map[string]any{"type": "layers", "diff_ids": []string{diff}}, "config": map[string]any{}}
	if o.mutateConfig != nil {
		o.mutateConfig(conf)
	}
	config := js(conf)
	m := map[string]any{"schemaVersion": 2, "mediaType": manifestMedia, "config": descriptor(config, configMedia), "layers": []any{descriptor(layer, media)}}
	if o.mutateManifest != nil {
		o.mutateManifest(m)
	}
	manifest := js(m)
	index := map[string]any{"schemaVersion": 2, "mediaType": indexMedia, "manifests": []any{descriptor(manifest, manifestMedia)}}
	if o.mutateIndex != nil {
		o.mutateIndex(index)
	}
	files := map[string][]byte{"oci-layout": js(map[string]string{"imageLayoutVersion": "1.0.0"}), "index.json": js(index), blobPath(sha(config)): config, blobPath(sha(manifest)): manifest, blobPath(sha(layer)): layer}
	if o.mutateFiles != nil {
		o.mutateFiles(files)
	}
	raw := archive(t, files)
	return raw, Expected{ArchiveSHA256: sha(raw), ArchiveSize: int64(len(raw)), ImageDigest: sha(manifest), Platform: "linux/arm64"}
}
func archive(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for path, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: path, Mode: 0600, Size: int64(len(body)), Format: tar.FormatUSTAR}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func verifiedFixture(t *testing.T) (*VerifiedImage, Expected) {
	t.Helper()
	raw, want := fixture(t, fixtureOptions{gzip: true})
	v, err := Verify(context.Background(), bytes.NewReader(raw), want, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := v.Close(); err != nil {
			t.Error(err)
		}
	})
	return v, want
}

func TestVerifyOCIProfile(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		raw, want := fixture(t, fixtureOptions{gzip: compressed})
		v, err := Verify(context.Background(), bytes.NewReader(raw), want, Limits{})
		if err != nil {
			t.Fatal(err)
		}
		dir := v.dir
		if err := v.Close(); err != nil {
			t.Fatal(err)
		}
		if err := v.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("snapshot not removed: %v", err)
		}
		// The existing builder's independent Python verifier must agree with
		// the publication profile, including gzip DiffIDs and expected digest.
		python, err := exec.LookPath("python3")
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "image.tar")
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(python, "../../scripts/ci/verify-oci.py", "--archive", path, "--platform", want.Platform, "--expected-digest", want.ImageDigest)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("Python profile disagrees: %v %s", err, out)
		}
	}
}

func TestVerifyRefusesInvalidImageBeforeNetwork(t *testing.T) {
	cases := map[string]fixtureOptions{
		"platform": {mutateConfig: func(m map[string]any) { m["architecture"] = "amd64" }},
		"variant":  {mutateConfig: func(m map[string]any) { m["variant"] = "v9" }},
		"diffid": {mutateConfig: func(m map[string]any) {
			m["rootfs"] = map[string]any{"type": "layers", "diff_ids": []string{sha([]byte("wrong"))}}
		}},
		"external": {mutateManifest: func(m map[string]any) {
			m["config"].(map[string]any)["urls"] = []string{"https://example.invalid/secret"}
		}},
		"inline":    {mutateManifest: func(m map[string]any) { m["config"].(map[string]any)["data"] = "" }},
		"subject":   {mutateManifest: func(m map[string]any) { m["subject"] = nil }},
		"alias":     {mutateManifest: func(m map[string]any) { m["Subject"] = nil }},
		"size":      {mutateManifest: func(m map[string]any) { m["config"].(map[string]any)["size"] = 1 }},
		"multi":     {mutateIndex: func(m map[string]any) { m["manifests"] = append(m["manifests"].([]any), m["manifests"].([]any)[0]) }},
		"traversal": {mutateFiles: func(m map[string][]byte) { m["../escape"] = []byte("no") }},
		"wrongblob": {mutateFiles: func(m map[string][]byte) {
			for k := range m {
				if strings.HasPrefix(k, "blobs/") {
					m[k] = []byte("no")
					return
				}
			}
		}},
		"duplicatejson": {mutateFiles: func(m map[string][]byte) {
			m["oci-layout"] = []byte(`{"imageLayoutVersion":"1.0.0","imageLayoutVersion":"1.0.0"}`)
		}},
		"trailingjson": {mutateFiles: func(m map[string][]byte) { m["oci-layout"] = append(m["oci-layout"], []byte(` {}`)...) }},
		"jsonlimit":    {mutateFiles: func(m map[string][]byte) { m["oci-layout"] = bytes.Repeat([]byte(" "), int(maxJSON)+1) }},
	}
	for label, o := range cases {
		t.Run(label, func(t *testing.T) {
			raw, want := fixture(t, o)
			v, err := Verify(context.Background(), bytes.NewReader(raw), want, Limits{})
			if v != nil {
				v.Close()
			}
			if err == nil {
				t.Fatal("invalid image accepted")
			}
		})
	}
}

func TestVerifyReceiptBoundsAndCancellation(t *testing.T) {
	raw, want := fixture(t, fixtureOptions{gzip: true})
	for _, mutate := range []func(*Expected){func(w *Expected) { w.ArchiveSize-- }, func(w *Expected) { w.ArchiveSize++ }, func(w *Expected) { w.ArchiveSHA256 = sha(nil) }, func(w *Expected) { w.ImageDigest = sha(nil) }, func(w *Expected) { w.Platform = "" }} {
		w := want
		mutate(&w)
		if v, err := Verify(context.Background(), bytes.NewReader(raw), w, Limits{}); err == nil {
			v.Close()
			t.Fatal("wrong receipt accepted")
		}
	}
	for _, limits := range []Limits{{ArchiveBytes: want.ArchiveSize - 1}, {UnpackedBytes: 1}, {ArchiveBytes: maxArchive + 1}, {UnpackedBytes: maxUnpacked + 1}, {ArchiveBytes: -1}} {
		if v, err := Verify(context.Background(), bytes.NewReader(raw), want, limits); err == nil {
			v.Close()
			t.Fatal("limit ignored")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if v, err := Verify(ctx, bytes.NewReader(raw), want, Limits{}); err == nil {
		v.Close()
		t.Fatal("cancellation ignored")
	}
}

func TestVerifyRefusesTarExtensionAndTrailingData(t *testing.T) {
	raw, want := fixture(t, fixtureOptions{})
	for label, mutate := range map[string]func([]byte) []byte{
		"extension":  func(b []byte) []byte { b[156] = 'x'; return b },
		"base256":    func(b []byte) []byte { b[124] = 0x80; return b },
		"trailing":   func(b []byte) []byte { return append(b, 'x') },
		"missingend": func(b []byte) []byte { return b[:len(b)-1024] },
	} {
		t.Run(label, func(t *testing.T) {
			b := mutate(bytes.Clone(raw))
			w := want
			w.ArchiveSHA256 = sha(b)
			w.ArchiveSize = int64(len(b))
			v, err := Verify(context.Background(), bytes.NewReader(b), w, Limits{})
			if v != nil {
				v.Close()
			}
			if err == nil {
				t.Fatal("invalid tar accepted")
			}
		})
	}
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for range 2 {
		_ = tw.WriteHeader(&tar.Header{Name: "index.json", Mode: 0600, Size: 2})
		_, _ = io.WriteString(tw, "{}")
	}
	_ = tw.Close()
	want.ArchiveSHA256 = sha(b.Bytes())
	want.ArchiveSize = int64(b.Len())
	if v, err := Verify(context.Background(), &b, want, Limits{}); err == nil {
		v.Close()
		t.Fatal("duplicate path accepted")
	}
}
