package pipelines

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func publishedReleaseFixture() PublishedRelease {
	d := "sha256:" + strings.Repeat("a", 64)
	return PublishedRelease{FormatVersion: 1, Publisher: "fixture", CandidateID: d, ApprovalID: "approval", WorkflowDigest: d, ProvenanceDigest: d, Compatibility: []ReleaseCompatibility{}, Components: []PublishedComponent{{Name: "engine", Version: "0.25.0", Repository: "acme/engine", Commit: strings.Repeat("b", 40), Artifacts: []PublishedArtifact{{Name: "image", Kind: "oci", Platform: "linux/arm64", Digest: d, Size: 100, ImageDigest: d, Locations: []PublishedLocation{{Kind: "oci", Origin: "https://registry.example", Repository: "acme/engine"}}}}}}}
}

func TestReleaseCatalogDSSETrustAndOwnedProjection(t *testing.T) {
	if got := string(releasePAE("http://example.com/HelloWorld", []byte("hello world"))); got != "DSSEv1 29 http://example.com/HelloWorld 11 hello world" {
		t.Fatal("DSSE reference vector differs", got)
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	c := publishedReleaseFixture()
	body, err := SignPublishedRelease(c, "key-one", key)
	if err != nil {
		t.Fatal(err)
	}
	trust := map[string]ed25519.PublicKey{"key-one": pub}
	v, err := VerifyPublishedRelease(body, "fixture", trust)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := v.Release()
	if err != nil {
		t.Fatal(err)
	}
	projection.Components[0].Artifacts[0].Locations[0].Origin = "https://evil.example"
	again, _ := v.Release()
	if again.Components[0].Artifacts[0].Locations[0].Origin != "https://registry.example" {
		t.Fatal("caller changed verified evidence")
	}
	var zero VerifiedPublishedRelease
	if _, err := zero.Release(); err == nil || zero.Digest() != "" {
		t.Fatal("zero evidence was valid")
	}
	if _, err := VerifyPublishedRelease(body, "another-publisher", trust); err == nil {
		t.Fatal("publisher scope ignored")
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := VerifyPublishedRelease(body, "fixture", map[string]ed25519.PublicKey{"key-one": other}); err == nil {
		t.Fatal("untrusted signature accepted")
	}
	var envelope releaseEnvelope
	json.Unmarshal(body, &envelope)
	payload, _ := releaseBase64(envelope.Payload)
	payload = bytes.Replace(payload, []byte("0.25.0"), []byte("0.26.0"), 1)
	envelope.Payload = base64.StdEncoding.EncodeToString(payload)
	tampered, _ := json.Marshal(envelope)
	if _, err := VerifyPublishedRelease(tampered, "fixture", trust); err == nil {
		t.Fatal("tampered release accepted")
	}
}

func TestReleaseCatalogRefusesAmbiguousSignedPayloads(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	canonical, _, err := CanonicalPublishedRelease(publishedReleaseFixture())
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{
		"duplicate": bytes.Replace(canonical, []byte(`"publisher":"fixture"`), []byte(`"publisher":"fixture","publisher":"fixture"`), 1),
		"alias":     bytes.Replace(canonical, []byte(`"publisher"`), []byte(`"Publisher"`), 1),
		"unknown":   bytes.Replace(canonical, []byte(`"formatVersion":1`), []byte(`"extra":true,"formatVersion":1`), 1),
		"version":   bytes.Replace(canonical, []byte(`"formatVersion":1`), []byte(`"formatVersion":2`), 1),
		"trailing":  append(bytes.Clone(canonical), []byte(` {}`)...),
	} {
		t.Run(name, func(t *testing.T) {
			// These signatures are valid. The payload contract must still refuse.
			sig := ed25519.Sign(key, releasePAE(ReleaseCatalogPayloadType, body))
			envelope, _ := json.Marshal(releaseEnvelope{PayloadType: ReleaseCatalogPayloadType, Payload: base64.StdEncoding.EncodeToString(body), Signatures: []releaseSignature{{KeyID: "key", Signature: base64.StdEncoding.EncodeToString(sig)}}})
			if _, err := VerifyPublishedRelease(envelope, "fixture", map[string]ed25519.PublicKey{"key": pub}); err == nil {
				t.Fatal("invalid signed payload accepted")
			}
		})
	}
	valid, _ := SignPublishedRelease(publishedReleaseFixture(), "key", key)
	duplicate := bytes.Replace(valid, []byte(`"payloadType":`), []byte(`"payloadType":"wrong","payloadType":`), 1)
	if _, err := VerifyPublishedRelease(duplicate, "fixture", map[string]ed25519.PublicKey{"key": pub}); err == nil {
		t.Fatal("duplicate envelope accepted")
	}
	if _, err := VerifyPublishedRelease(bytes.Repeat([]byte("x"), 2*maxReleaseCatalogBytes+1), "fixture", map[string]ed25519.PublicKey{"key": pub}); err == nil {
		t.Fatal("oversized envelope accepted")
	}
}

func TestReleaseCatalogCompatibilityAndCompleteLocations(t *testing.T) {
	c := publishedReleaseFixture()
	other := c.Components[0]
	other.Name = "client"
	other.Repository = "acme/client"
	c.Components = append(c.Components, other)
	c.Compatibility = []ReleaseCompatibility{{Component: "client", Requires: "engine", MinVersion: "0.25.0", MaxExclusive: "0.26.0"}}
	body, digest, err := CanonicalPublishedRelease(c)
	if err != nil {
		t.Fatal(err)
	}
	c.Components[0], c.Components[1] = c.Components[1], c.Components[0]
	reordered, digest2, err := CanonicalPublishedRelease(c)
	if err != nil || digest != digest2 || !bytes.Equal(body, reordered) {
		t.Fatal("collection order changed identity", err)
	}
	c.Components[1].Version = "0.26.0"
	if _, _, err := CanonicalPublishedRelease(c); err == nil {
		t.Fatal("incompatible set accepted")
	}
	c = publishedReleaseFixture()
	c.Components[0].Artifacts[0].Locations = nil
	if _, _, err := CanonicalPublishedRelease(c); err == nil {
		t.Fatal("unpublished artifact accepted")
	}
}
