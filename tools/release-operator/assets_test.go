package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestCandidateAssetNames(t *testing.T) {
	names := cockpitAssetNames("0.21.0")
	if len(names) != 41 {
		t.Fatalf("asset count %d", len(names))
	}
	seen := map[string]bool{}
	for artifact, name := range names {
		if seen[name] || filepath.Base(name) != name || fixedTargets["cockpit-"+artifact].Artifact != artifact {
			t.Fatalf("invalid mapping %s=%s", artifact, name)
		}
		seen[name] = true
	}
}
func TestDownloadDigestAndSizeFailClosed(t *testing.T) {
	data := []byte("candidate fixture")
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(data))
	for _, tc := range []struct {
		name   string
		body   []byte
		size   int64
		digest string
		ok     bool
	}{
		{"exact", data, int64(len(data)), digest, true},
		{"different", []byte("wrong candidate!!"), int64(len(data)), digest, false},
		{"too-large", append(append([]byte{}, data...), 'x'), int64(len(data)), digest, false},
		{"truncated", data[:3], int64(len(data)), digest, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "asset")
			r := &http.Response{StatusCode: 200, ContentLength: -1, Body: io.NopCloser(bytes.NewReader(tc.body))}
			err := saveVerifiedAsset(r, path, tc.size, tc.digest)
			if (err == nil) != tc.ok {
				t.Fatalf("unexpected result %v", err)
			}
			if !tc.ok {
				if _, e := os.Stat(path); !os.IsNotExist(e) {
					t.Fatal("failed download retained as verified")
				}
				if _, e := os.Stat(path + ".partial"); !os.IsNotExist(e) {
					t.Fatal("failed partial retained")
				}
			}
		})
	}
}
