package main

import (
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"strings"
	"testing"
)

type indexDelegate interface{ v1.ImageIndex }

type fixtureIndex struct {
	indexDelegate
	manifest *v1.IndexManifest
}

func (f fixtureIndex) IndexManifest() (*v1.IndexManifest, error) { return f.manifest, nil }

func TestNativePlatformsPreservesBothImagesAndTheirProvenance(t *testing.T) {
	manifest := &v1.IndexManifest{SchemaVersion: 2, MediaType: types.OCIImageIndex}
	for i, arch := range []string{"386", "amd64", "arm64"} {
		h := v1.Hash{Algorithm: "sha256", Hex: strings.Repeat(string(rune('a'+i)), 64)}
		manifest.Manifests = append(manifest.Manifests,
			v1.Descriptor{MediaType: types.OCIManifestSchema1, Digest: h, Size: 123, Platform: &v1.Platform{OS: "linux", Architecture: arch}},
			v1.Descriptor{MediaType: types.OCIManifestSchema1, Digest: v1.Hash{Algorithm: "sha256", Hex: strings.Repeat(string(rune('d'+i)), 64)}, Size: 100, Platform: &v1.Platform{OS: "unknown", Architecture: "unknown"}, Annotations: map[string]string{"vnd.docker.reference.type": "attestation-manifest", "vnd.docker.reference.digest": h.String()}})
	}
	idx, err := nativePlatforms(fixtureIndex{indexDelegate: empty.Index, manifest: manifest})
	if err != nil {
		t.Fatal(err)
	}
	got, err := idx.IndexManifest()
	if err != nil || len(got.Manifests) != 4 {
		t.Fatal("platform or provenance was lost", got, err)
	}
	for _, d := range got.Manifests {
		if d.Platform.Architecture == "386" || d.Annotations["vnd.docker.reference.digest"] == manifest.Manifests[0].Digest.String() {
			t.Fatal("unselected architecture leaked into derived index")
		}
	}
	missing := *manifest
	missing.Manifests = manifest.Manifests[:4]
	if _, err := nativePlatforms(fixtureIndex{indexDelegate: empty.Index, manifest: &missing}); err == nil {
		t.Fatal("missing ARM64 accepted")
	}
	ambiguous := *manifest
	ambiguous.Manifests = append(append([]v1.Descriptor(nil), manifest.Manifests...), manifest.Manifests[2])
	if _, err := nativePlatforms(fixtureIndex{indexDelegate: empty.Index, manifest: &ambiguous}); err == nil {
		t.Fatal("ambiguous platform accepted")
	}
}
