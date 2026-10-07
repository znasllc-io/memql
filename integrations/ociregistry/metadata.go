package ociregistry

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

type object map[string]json.RawMessage
type metadataFiles struct {
	root  string
	sizes map[string]int64
}

func (m metadataFiles) json(name string) (object, error) {
	size, ok := m.sizes[name]
	if !ok || size > maxJSON {
		return nil, errors.New("required bounded JSON object is absent")
	}
	b, err := os.ReadFile(filepath.Join(m.root, name))
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(b) {
		return nil, errors.New("metadata must be UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err = uniqueJSON(d, 0); err != nil {
		return nil, err
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, errors.New("trailing JSON data")
	}
	var o object
	if err = json.Unmarshal(b, &o); err != nil || o == nil {
		return nil, errors.New("metadata must be a JSON object")
	}
	return o, nil
}

func (o object) str(key string) string { var s string; _ = json.Unmarshal(o[key], &s); return s }
func (o object) integer(key string) (int64, bool) {
	var n int64
	err := json.Unmarshal(o[key], &n)
	return n, err == nil && string(o[key]) != "null"
}
func (o object) has(key string) bool { _, ok := o[key]; return ok }
func (o object) child(key string) (object, error) {
	var v object
	if err := json.Unmarshal(o[key], &v); err != nil || v == nil {
		return nil, fmt.Errorf("%s must be an object", key)
	}
	return v, nil
}
func (o object) array(key string) ([]json.RawMessage, error) {
	var v []json.RawMessage
	if err := json.Unmarshal(o[key], &v); err != nil || v == nil {
		return nil, fmt.Errorf("%s must be an array", key)
	}
	return v, nil
}
func asObject(raw json.RawMessage) (object, error) {
	var o object
	if err := json.Unmarshal(raw, &o); err != nil || o == nil {
		return nil, errors.New("descriptor must be an object")
	}
	return o, nil
}

func (m metadataFiles) descriptor(o object, media string) (blob, error) {
	if o.str("mediaType") != media || o.has("data") {
		return blob{}, errors.New("unsupported descriptor media type or inline content")
	}
	if o.has("urls") && string(o["urls"]) != "[]" && string(o["urls"]) != "null" {
		return blob{}, errors.New("external descriptors refused")
	}
	d := o.str("digest")
	n, ok := o.integer("size")
	actual, exists := m.sizes[blobPath(d)]
	if !shaPattern.MatchString(d) || !ok || !exists || n != actual {
		return blob{}, errors.New("descriptor digest or size differs from its blob")
	}
	return blob{d, n}, nil
}
func blobPath(d string) string { return "blobs/sha256/" + strings.TrimPrefix(d, "sha256:") }

func validSchema(o object, media string) bool {
	n, ok := o.integer("schemaVersion")
	return ok && n == 2 && (!o.has("mediaType") || o.str("mediaType") == media)
}

func validPlatform(o object, platform string) bool {
	p := strings.Split(platform, "/")
	if o.str("os") != p[0] || o.str("architecture") != p[1] {
		return false
	}
	v := o.str("variant")
	if o.has("variant") && string(o["variant"]) != "null" && string(o["variant"]) != `""` && !(p[1] == "arm64" && v == "v8") {
		return false
	}
	if o.has("os.features") && string(o["os.features"]) != "[]" && string(o["os.features"]) != "null" {
		return false
	}
	return !o.has("os.version") || string(o["os.version"]) == `""` || string(o["os.version"]) == "null"
}

func (m metadataFiles) verify(ctx context.Context, want Expected, unpackedLimit int64) ([]blob, error) {
	l, err := m.json("oci-layout")
	if err != nil {
		return nil, err
	}
	if l.str("imageLayoutVersion") != "1.0.0" {
		return nil, errors.New("unsupported OCI layout version")
	}
	index, err := m.json("index.json")
	if err != nil {
		return nil, err
	}
	if !validSchema(index, indexMedia) {
		return nil, errors.New("invalid OCI index")
	}
	images, err := index.array("manifests")
	if err != nil || len(images) != 1 {
		return nil, errors.New("expected exactly one image manifest")
	}
	d, err := asObject(images[0])
	if err != nil {
		return nil, err
	}
	image, err := m.descriptor(d, manifestMedia)
	if err != nil {
		return nil, err
	}
	if image.Digest != want.ImageDigest {
		return nil, errors.New("image differs from reviewed digest")
	}
	if d.has("platform") {
		p, e := d.child("platform")
		if e != nil || (len(p) != 0 && !validPlatform(p, want.Platform)) {
			return nil, errors.New("index platform differs from expected platform")
		}
	}
	manifest, err := m.json(blobPath(image.Digest))
	if err != nil {
		return nil, err
	}
	if !validSchema(manifest, manifestMedia) || manifest.has("subject") || manifest.has("artifactType") {
		return nil, errors.New("unsupported image manifest")
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
	if err != nil {
		return nil, err
	}
	if !validPlatform(conf, want.Platform) {
		return nil, errors.New("image platform differs from expected platform")
	}
	rootfs, err := conf.child("rootfs")
	if err != nil {
		return nil, err
	}
	if rootfs.str("type") != "layers" {
		return nil, errors.New("image has no layer rootfs")
	}
	diffs, err := rootfs.array("diff_ids")
	if err != nil {
		return nil, err
	}
	layers, err := manifest.array("layers")
	if err != nil || len(layers) != len(diffs) {
		return nil, errors.New("rootfs and manifest layers disagree")
	}
	blobs := []blob{config}
	for i, raw := range layers {
		layer, e := asObject(raw)
		if e != nil {
			return nil, e
		}
		media := layer.str("mediaType")
		if media != layerMedia && media != layerMedia+"+gzip" {
			return nil, errors.New("unsupported layer compression")
		}
		b, e := m.descriptor(layer, media)
		if e != nil {
			return nil, e
		}
		var diff string
		if json.Unmarshal(diffs[i], &diff) != nil || !shaPattern.MatchString(diff) {
			return nil, errors.New("invalid rootfs DiffID")
		}
		h, n, e := verifyLayer(ctx, filepath.Join(m.root, blobPath(b.Digest)), media, unpackedLimit)
		if e != nil {
			return nil, e
		}
		if h != diff {
			return nil, errors.New("uncompressed layer differs from rootfs DiffID")
		}
		unpackedLimit -= n
		blobs = append(blobs, b)
	}
	return blobs, nil
}

func verifyLayer(ctx context.Context, path, media string, limit int64) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	var r io.Reader = f
	if media == layerMedia+"+gzip" {
		gz, err := gzip.NewReader(contextReader{ctx, f})
		if err != nil {
			return "", 0, err
		}
		defer gz.Close()
		r = gz
	}
	return hashCopy(ctx, nil, r, limit)
}
