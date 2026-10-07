package pipelines

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
)

func signedOverlapFixture(t *testing.T, release PublishedRelease) VerifiedPublishedRelease {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	body, err := SignPublishedRelease(release, "key", key)
	if err != nil {
		t.Fatal(err)
	}
	v, err := VerifyPublishedRelease(body, release.Publisher, map[string]ed25519.PublicKey{"key": pub})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func overlapFixture() PublishedRelease {
	r := publishedReleaseFixture()
	client := publishedReleaseFixture().Components[0]
	client.Name, client.Repository = "client", "acme/client"
	r.Components = append(r.Components, client)
	r.Compatibility = []ReleaseCompatibility{{Component: "client", Requires: "engine", MinVersion: "0.24.0", MaxExclusive: "0.28.0"}}
	return r
}

func TestPublishedOverlapRequiresBothDirectionsAndCompleteDeclarations(t *testing.T) {
	for _, fault := range []string{"none", "new client requires new engine", "old client refuses new engine", "removed declaration", "both omitted", "component renamed", "repository changed", "version republished", "artifact republished", "publisher changed", "duplicate required pair", "unknown required pair"} {
		t.Run(fault, func(t *testing.T) {
			before, after := overlapFixture(), overlapFixture()
			after.Components[0].Version, after.Components[0].Commit = "0.26.0", strings.Repeat("c", 40)
			required := []ReleaseDependency{{"client", "engine"}}
			switch fault {
			case "new client requires new engine":
				after.Compatibility[0].MinVersion = "0.26.0"
			case "old client refuses new engine":
				before.Compatibility[0].MaxExclusive = "0.26.0"
			case "removed declaration":
				after.Compatibility = nil
				required = nil // union of signed declarations must still be complete
			case "both omitted":
				before.Compatibility, after.Compatibility = nil, nil
			case "component renamed":
				after.Components[1].Name, after.Compatibility[0].Component = "other", "other"
			case "repository changed":
				after.Components[0].Repository = "other/engine"
			case "version republished":
				after.Components[0].Version = "0.25.0"
			case "artifact republished":
				after.Components[1].Artifacts[0].ImageDigest = "sha256:" + strings.Repeat("c", 64)
			case "publisher changed":
				after.Publisher = "other"
			case "duplicate required pair":
				required = append(required, required[0])
			case "unknown required pair":
				required[0].Requires = "absent"
			}
			err := CheckPublishedReleaseOverlap(signedOverlapFixture(t, before), signedOverlapFixture(t, after), required)
			if fault == "none" && err != nil {
				t.Fatal(err)
			}
			if fault != "none" && err == nil {
				t.Fatal("expected overlap refusal")
			}
		})
	}
}

func TestPublishedOverlapHandlesLargeVersionsWithoutNarrowing(t *testing.T) {
	before, after := overlapFixture(), overlapFixture()
	for _, r := range []*PublishedRelease{&before, &after} {
		r.Components[0].Version = "999999999999999999999999999999.0.0"
		r.Compatibility[0].MaxExclusive = "1000000000000000000000000000000.0.0"
	}
	// Adding a mirror does not change the bytes belonging to a version.
	after.Components[0].Artifacts[0].Locations = append(after.Components[0].Artifacts[0].Locations, PublishedLocation{Kind: "oci", Origin: "https://mirror.example", Repository: "acme/engine"})
	a, b := signedOverlapFixture(t, before), signedOverlapFixture(t, after)
	if err := CheckPublishedReleaseOverlap(a, b, nil); err != nil {
		t.Fatal(err)
	}
	again, err := b.Release()
	if err != nil {
		t.Fatal(err)
	}
	if got := len(again.Components[1].Artifacts[0].Locations); got != 2 {
		t.Fatalf("verifier mutated verified publication content: got %d locations, want 2", got)
	}
	if err := CheckPublishedReleaseOverlap(VerifiedPublishedRelease{}, b, nil); err == nil {
		t.Fatal("accepted an unverified publication")
	}
}
