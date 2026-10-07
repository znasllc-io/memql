package release

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
)

// An independently encoded minimal OCI image exercises the real verifier in
// preparation tests. No registry, image builder or external fixture is needed.
func candidateOCIBytes(t *testing.T) ([]byte, string, string) {
	t.Helper()
	hash := func(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
	encode := func(v any) []byte {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	descriptor := func(b []byte, media string) map[string]any {
		return map[string]any{"mediaType": "application/vnd.oci.image." + media + ".v1+json", "digest": "sha256:" + hash(b), "size": len(b)}
	}
	config := []byte(`{"architecture":"arm64","os":"linux","rootfs":{"type":"layers","diff_ids":[]},"config":{}}`)
	manifest := encode(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json", "config": descriptor(config, "config"), "layers": []any{}})
	index := encode(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json", "manifests": []any{descriptor(manifest, "manifest")}})
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	for _, file := range []struct {
		name string
		body []byte
	}{
		{"oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`)}, {"index.json", index},
		{"blobs/sha256/" + hash(config), config}, {"blobs/sha256/" + hash(manifest), manifest},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: file.name, Mode: 0600, Size: int64(len(file.body)), Format: tar.FormatUSTAR}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(file.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes(), "sha256:" + hash(archive.Bytes()), "sha256:" + hash(manifest)
}
