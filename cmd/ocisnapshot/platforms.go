package main

import (
	"errors"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
)

// The declared native runner accepts exactly these container architectures.
// Keep their original child bytes and provenance; derive a distinct index,
// never label the selected subset with the upstream index's digest.
func nativePlatforms(source v1.ImageIndex) (v1.ImageIndex, error) {
	manifest, err := source.IndexManifest()
	if err != nil || len(manifest.Manifests) > 64 {
		return nil, errors.New("source index is unreadable or unbounded")
	}
	images, keep, platforms := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, d := range manifest.Manifests {
		p := d.Platform
		if p == nil || p.OS != "linux" || (p.Architecture != "amd64" && p.Architecture != "arm64") {
			continue
		}
		if platforms[p.Architecture] {
			return nil, errors.New("source has ambiguous native platform images")
		}
		platforms[p.Architecture], images[d.Digest.String()], keep[d.Digest.String()] = true, true, true
	}
	if len(platforms) != 2 {
		return nil, errors.New("source must contain both native Linux architectures")
	}
	for _, d := range manifest.Manifests {
		if d.Platform != nil && d.Platform.OS == "unknown" && d.Platform.Architecture == "unknown" &&
			d.Annotations["vnd.docker.reference.type"] == "attestation-manifest" && images[d.Annotations["vnd.docker.reference.digest"]] {
			keep[d.Digest.String()] = true
		}
	}
	return mutate.RemoveManifests(source, func(d v1.Descriptor) bool { return !keep[d.Digest.String()] }), nil
}
