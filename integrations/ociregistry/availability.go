package ociregistry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

const (
	dockerIndexMedia      = "application/vnd.docker.distribution.manifest.list.v2+json"
	dockerManifestMedia   = "application/vnd.docker.distribution.manifest.v2+json"
	dockerConfigMedia     = "application/vnd.docker.container.image.v1+json"
	dockerLayerMedia      = "application/vnd.docker.image.rootfs.diff.tar.gzip"
	maxAvailableBytes     = int64(8 << 30)
	maxAvailableMetadata  = int64(16 << 20)
	maxAvailableManifests = 64
	maxAvailableLayers    = 1024
)

// AvailabilityVerifier performs no writes and has no ambient credentials. The
// caller binds this operator-configured target to its own admitted operation.
// Reuse is safe: each check builds a fresh, independent transport and byte proof.
type AvailabilityVerifier struct{ registry *Publisher }

// AvailabilityLimits may only reduce the native 8 GiB unique-content ceiling.
type AvailabilityLimits struct{ Bytes int64 }

// AvailableImage can only be constructed by a successful check. It is evidence
// of a completed read, not an approval, retention promise or installation token.
type AvailableImage struct{ receipt AvailabilityReceipt }

// AvailabilityReceipt records the requested immutable root and its selected
// platform manifest, plus the unique content actually read. Persist with the
// caller's exact registry configuration binding and observation time.
type AvailabilityReceipt struct {
	Repository, ImageDigest, ManifestDigest, Platform string
	ContentBytes                                      int64
}

func (a *AvailableImage) Receipt() (AvailabilityReceipt, error) {
	if a == nil || a.receipt.Repository == "" || !shaPattern.MatchString(a.receipt.ImageDigest) || !shaPattern.MatchString(a.receipt.ManifestDigest) || a.receipt.ContentBytes <= 0 {
		return AvailabilityReceipt{}, errors.New("unverified image availability")
	}
	return a.receipt, nil
}
func (*AvailableImage) String() string   { return "<verified OCI availability>" }
func (*AvailableImage) GoString() string { return "<verified OCI availability>" }

func NewAvailabilityVerifier(t Target) (*AvailabilityVerifier, error) {
	p, err := NewPublisher(t)
	if err != nil {
		return nil, err
	}
	return &AvailabilityVerifier{registry: p}, nil
}

// Check hashes the complete registry content required for one platform: the
// immutable root/index path, selected manifest, config and every compressed
// layer. It never pulls tags, uploads, unpacks, runs images or creates local
// archives. The DSL/native host chooses which candidate and rollback images
// require this evidence and when fresh evidence must be collected.
func (v *AvailabilityVerifier) Check(ctx context.Context, digest, platform string, limits AvailabilityLimits) (*AvailableImage, error) {
	if v == nil || v.registry == nil || !shaPattern.MatchString(digest) || (platform != "linux/amd64" && platform != "linux/arm64") {
		return nil, errors.New("configured registry, immutable digest and supported platform required")
	}
	if limits.Bytes == 0 {
		limits.Bytes = maxAvailableBytes
	}
	if limits.Bytes < 1 || limits.Bytes > maxAvailableBytes {
		return nil, errors.New("availability limit exceeds native ceiling")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	p := v.registry
	httpTransport := p.httpTransport()
	defer httpTransport.CloseIdleConnections()
	guard := &scopedTransport{publisher: p, inner: httpTransport, readOnly: true, image: digest, manifests: map[string]int64{digest: maxJSON}, blobs: map[string]int64{}}
	s := availabilityRead{
		ctx: ctx, p: p, guard: guard, remaining: limits.Bytes,
		seen: map[string]bool{}, blobs: map[string]int64{},
		opts: []remote.Option{remote.WithContext(ctx), remote.WithTransport(guard), remote.WithJobs(1),
			remote.WithAuth(authn.FromConfig(authn.AuthConfig{Username: p.target.Username, Password: p.target.Password, RegistryToken: p.target.BearerToken})),
			remote.WithRetryBackoff(remote.Backoff{Duration: 100 * time.Millisecond, Factor: 2, Jitter: 0.1, Steps: 3})},
	}
	leaves, err := s.manifest(availableDescriptor{blob: blob{Digest: digest}}, platform, 0)
	if err == nil && len(leaves) != 1 {
		err = errors.New("one unambiguous platform manifest required")
	}
	if err == nil {
		err = s.image(leaves[0], platform)
	}
	if err != nil {
		// Library errors can contain registry response bodies and signed URLs.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("OCI image availability could not be verified")
	}
	return &AvailableImage{receipt: AvailabilityReceipt{Repository: p.repo.Name(), ImageDigest: digest, ManifestDigest: leaves[0].Digest, Platform: platform, ContentBytes: limits.Bytes - s.remaining}}, nil
}

type availableDescriptor struct {
	blob
	media string
}
type availableManifest struct {
	availableDescriptor
	object object
}
type availabilityRead struct {
	ctx                 context.Context
	p                   *Publisher
	guard               *scopedTransport
	opts                []remote.Option
	remaining, metadata int64
	seen                map[string]bool
	blobs               map[string]int64
}

func availabilityDescriptor(o object, limit int64) (availableDescriptor, error) {
	n, ok := o.integer("size")
	if !ok || n <= 0 || n > limit || !shaPattern.MatchString(o.str("digest")) || o.str("mediaType") == "" || o.has("data") || o.has("artifactType") || (o.has("urls") && string(o["urls"]) != "[]" && string(o["urls"]) != "null") {
		return availableDescriptor{}, errors.New("unsupported availability descriptor")
	}
	return availableDescriptor{blob: blob{Digest: o.str("digest"), Size: n}, media: o.str("mediaType")}, nil
}

func isAvailableIndex(media string) bool { return media == indexMedia || media == dockerIndexMedia }
func isAvailableManifest(media string) bool {
	return media == manifestMedia || media == dockerManifestMedia
}

func (s *availabilityRead) manifest(want availableDescriptor, platform string, depth int) ([]availableManifest, error) {
	if depth > 4 || len(s.seen) >= maxAvailableManifests || s.seen[want.Digest] {
		return nil, errors.New("index graph exceeds bounds or repeats a manifest")
	}
	s.seen[want.Digest] = true
	limit := min(maxJSON, s.remaining, maxAvailableMetadata-s.metadata)
	if limit <= 0 || want.Size > limit {
		return nil, errors.New("manifest exceeds remaining byte limit")
	}
	s.guard.manifests[want.Digest] = limit
	desc, err := remote.Get(s.p.repo.Digest(want.Digest), s.opts...)
	if err != nil {
		return nil, err
	}
	digest, n, err := hashCopy(s.ctx, nil, bytes.NewReader(desc.Manifest), limit)
	if err != nil || digest != want.Digest || (want.Size > 0 && n != want.Size) {
		return nil, errors.New("manifest bytes differ from descriptor")
	}
	s.remaining -= n
	s.metadata += n
	media := string(desc.MediaType)
	if want.media != "" && want.media != media {
		return nil, errors.New("manifest media differs from descriptor")
	}
	o, err := decodeMetadata(desc.Manifest)
	if err != nil || !validSchema(o, media) || o.has("subject") || o.has("artifactType") {
		return nil, errors.New("unsupported image metadata")
	}
	want.Size, want.media = n, media
	if isAvailableManifest(media) {
		return []availableManifest{{availableDescriptor: want, object: o}}, nil
	}
	if !isAvailableIndex(media) {
		return nil, errors.New("unsupported registry manifest type")
	}
	entries, err := o.array("manifests")
	if err != nil || len(entries) == 0 || len(entries) > 128 {
		return nil, errors.New("index descriptor count exceeds profile")
	}
	var matches []availableManifest
	for _, raw := range entries {
		entry, err := asObject(raw)
		if err != nil {
			return nil, err
		}
		if entry.has("platform") {
			p, e := entry.child("platform")
			if e != nil {
				return nil, e
			}
			if !validPlatform(p, platform) {
				continue
			}
		} else if !isAvailableIndex(entry.str("mediaType")) {
			return nil, errors.New("platformless image descriptor is ambiguous")
		}
		next, err := availabilityDescriptor(entry, maxJSON)
		if err != nil {
			return nil, err
		}
		if !isAvailableIndex(next.media) && !isAvailableManifest(next.media) {
			return nil, errors.New("unsupported matching descriptor")
		}
		found, err := s.manifest(next, platform, depth+1)
		if err != nil {
			return nil, err
		}
		matches = append(matches, found...)
		if len(matches) > 1 {
			return nil, errors.New("ambiguous platform manifests")
		}
	}
	return matches, nil
}

func (s *availabilityRead) readBlob(want availableDescriptor, dst io.Writer, metadata bool) error {
	if n, ok := s.blobs[want.Digest]; ok {
		if metadata || n != want.Size {
			return errors.New("inconsistent or reused config descriptor")
		}
		return nil
	}
	if want.Size > s.remaining || (metadata && want.Size > maxAvailableMetadata-s.metadata) {
		return errors.New("image exceeds remaining byte limit")
	}
	s.guard.blobs[want.Digest] = want.Size
	layer, err := remote.Layer(s.p.repo.Digest(want.Digest), s.opts...)
	if err != nil {
		return err
	}
	r, err := layer.Compressed()
	if err != nil {
		return err
	}
	d, n, readErr := hashCopy(s.ctx, dst, r, want.Size)
	closeErr := r.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	if d != want.Digest || n != want.Size {
		return errors.New("registry blob differs from descriptor")
	}
	s.remaining -= n
	if metadata {
		s.metadata += n
	}
	s.blobs[want.Digest] = n
	return nil
}

func (s *availabilityRead) image(m availableManifest, platform string) error {
	c, err := m.object.child("config")
	if err != nil {
		return err
	}
	config, err := availabilityDescriptor(c, maxJSON)
	if err != nil {
		return err
	}
	if config.media != configMedia && config.media != dockerConfigMedia {
		return errors.New("unsupported image config")
	}
	var buf bytes.Buffer
	if err = s.readBlob(config, &buf, true); err != nil {
		return err
	}
	o, err := decodeMetadata(buf.Bytes())
	if err != nil || !validPlatform(o, platform) {
		return errors.New("image config platform differs from requested platform")
	}
	root, err := o.child("rootfs")
	if err != nil || root.str("type") != "layers" {
		return errors.New("unsupported image rootfs")
	}
	diffs, err := root.array("diff_ids")
	if err != nil {
		return err
	}
	layers, err := m.object.array("layers")
	if err != nil || len(layers) != len(diffs) || len(layers) > maxAvailableLayers {
		return errors.New("image layers differ from rootfs or exceed limit")
	}
	for i, raw := range layers {
		var diff string
		if json.Unmarshal(diffs[i], &diff) != nil || !shaPattern.MatchString(diff) {
			return errors.New("invalid image rootfs DiffID")
		}
		o, err := asObject(raw)
		if err != nil {
			return err
		}
		b, err := availabilityDescriptor(o, maxAvailableBytes)
		if err != nil {
			return err
		}
		if b.media != layerMedia && b.media != layerMedia+"+gzip" && b.media != layerMedia+"+zstd" && b.media != dockerLayerMedia {
			return errors.New("unsupported image layer media")
		}
		if strings.HasSuffix(b.media, ".tar") && b.Digest != diff {
			return errors.New("plain layer differs from rootfs DiffID")
		}
		if err = s.readBlob(b, nil, false); err != nil {
			return err
		}
	}
	return nil
}
