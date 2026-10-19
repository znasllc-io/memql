package ociregistry

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

// verifyIndex preserves an OCI index, including its attached provenance bytes.
// It certifies content identity, never the truth/signature of an attestation.
// The supported profile is a flat, bounded set of Linux images and Docker
// attestation manifests. Nested indexes, external content and ambiguous platform
// matches refuse before publication; this path never selects a host platform.
func (m metadataFiles) verifyIndex(ctx context.Context, want Expected, unpackedLimit int64) ([]blob, []blob, error) {
	l, err := m.json("oci-layout")
	if err != nil || l.str("imageLayoutVersion") != "1.0.0" {
		return nil, nil, errors.New("unsupported OCI layout version")
	}
	outer, err := m.json("index.json")
	if err != nil || !validSchema(outer, indexMedia) {
		return nil, nil, errors.New("invalid OCI layout index")
	}
	roots, err := outer.array("manifests")
	if err != nil || len(roots) != 1 {
		return nil, nil, errors.New("expected one reviewed OCI index")
	}
	d, err := asObject(roots[0])
	if err != nil {
		return nil, nil, err
	}
	root, err := m.descriptor(d, indexMedia)
	if err != nil || root.Digest != want.ImageDigest || d.has("platform") {
		return nil, nil, errors.New("OCI index differs from reviewed digest or selects a platform")
	}
	index, err := m.json(blobPath(root.Digest))
	if err != nil || !validSchema(index, indexMedia) || index.has("subject") || index.has("artifactType") {
		return nil, nil, errors.New("unsupported OCI index")
	}
	entries, err := index.array("manifests")
	if err != nil || len(entries) < 1 || len(entries) > 64 {
		return nil, nil, errors.New("OCI index requires 1 to 64 manifests")
	}
	var blobs, manifests []blob
	images, platforms, seen := map[string]bool{}, map[string]bool{}, map[string]bool{}
	type attachment struct {
		image  blob
		target string
	}
	var attachments []attachment
	for _, raw := range entries {
		d, err := asObject(raw)
		if err != nil {
			return nil, nil, err
		}
		image, err := m.descriptor(d, manifestMedia)
		if err != nil || seen[image.Digest] {
			return nil, nil, errors.New("index child must be a unique local image manifest")
		}
		seen[image.Digest] = true
		manifests = append(manifests, image)
		p, err := d.child("platform")
		if err != nil {
			return nil, nil, err
		}
		if validPlatform(p, "unknown/unknown") {
			a, err := d.child("annotations")
			if err != nil || a.str("vnd.docker.reference.type") != "attestation-manifest" || !shaPattern.MatchString(a.str("vnd.docker.reference.digest")) {
				return nil, nil, errors.New("unknown platform requires a bound attestation manifest")
			}
			attachments = append(attachments, attachment{image, a.str("vnd.docker.reference.digest")})
			continue
		}
		platform := p.str("os") + "/" + p.str("architecture")
		if p.str("architecture") == "arm" {
			platform += "/" + p.str("variant")
		}
		switch platform {
		case "linux/386", "linux/amd64", "linux/arm/v6", "linux/arm/v7", "linux/arm64":
		default:
			return nil, nil, errors.New("unsupported index image platform")
		}
		if !validPlatform(p, platform) || platforms[platform] {
			return nil, nil, errors.New("ambiguous or inconsistent index platform")
		}
		platforms[platform], images[image.Digest] = true, true
		content, err := m.verifyImage(ctx, image, platform, &unpackedLimit)
		if err != nil {
			return nil, nil, err
		}
		blobs = append(blobs, content...)
	}
	if len(images) == 0 {
		return nil, nil, errors.New("index contains no runnable images")
	}
	for _, a := range attachments {
		if !images[a.target] {
			return nil, nil, errors.New("attestation does not name an image in this index")
		}
		content, err := m.verifyAttestation(ctx, a.image, a.target, &unpackedLimit)
		if err != nil {
			return nil, nil, err
		}
		blobs = append(blobs, content...)
	}
	return blobs, manifests, nil
}

func (m metadataFiles) verifyAttestation(ctx context.Context, image blob, target string, remaining *int64) ([]blob, error) {
	manifest, err := m.json(blobPath(image.Digest))
	if err != nil || !validSchema(manifest, manifestMedia) || manifest.has("subject") || manifest.has("artifactType") {
		return nil, errors.New("unsupported attestation manifest")
	}
	c, err := manifest.child("config")
	if err != nil {
		return nil, err
	}
	config, err := m.descriptor(c, configMedia)
	if err != nil {
		return nil, err
	}
	conf, err := m.json(blobPath(config.Digest))
	if err != nil || !validPlatform(conf, "unknown/unknown") {
		return nil, errors.New("attestation config has a runnable platform")
	}
	rootfs, err := conf.child("rootfs")
	if err != nil || rootfs.str("type") != "layers" {
		return nil, errors.New("attestation config has no layer rootfs")
	}
	diffs, err := rootfs.array("diff_ids")
	if err != nil {
		return nil, err
	}
	layers, err := manifest.array("layers")
	if err != nil || len(layers) == 0 || len(layers) > 16 || len(diffs) != len(layers) {
		return nil, errors.New("attestation layers are absent or inconsistent")
	}
	blobs := []blob{config}
	for i, raw := range layers {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		d, err := asObject(raw)
		if err != nil {
			return nil, err
		}
		b, err := m.descriptor(d, "application/vnd.in-toto+json")
		var diff string
		if err != nil || json.Unmarshal(diffs[i], &diff) != nil || diff != b.Digest || b.Size > *remaining {
			return nil, errors.New("attestation layer differs from rootfs or exceeds byte limit")
		}
		statement, err := m.json(blobPath(b.Digest))
		if err != nil {
			return nil, err
		}
		subjects, err := statement.array("subject")
		if err != nil || len(subjects) == 0 || len(subjects) > 64 {
			return nil, errors.New("attestation subject is absent or unbounded")
		}
		bound := false
		for _, rawSubject := range subjects {
			s, err := asObject(rawSubject)
			if err != nil {
				return nil, err
			}
			digest, err := s.child("digest")
			if err != nil {
				return nil, err
			}
			bound = bound || digest.str("sha256") == strings.TrimPrefix(target, "sha256:")
		}
		if !bound {
			return nil, errors.New("attestation statement does not name its index image")
		}
		*remaining -= b.Size
		blobs = append(blobs, b)
	}
	return blobs, nil
}
