package pipelines

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func releaseCandidateFixture() ReleaseCandidate {
	digest := "sha256:" + strings.Repeat("a", 64)
	c := ReleaseCandidate{FormatVersion: 1, OwnerUserID: "owner", WorkflowDigest: digest,
		Components:   []ReleaseComponent{{Name: "engine", Version: "0.25.0", Repository: "acme/engine", Commit: strings.Repeat("b", 40), Artifacts: []ReleaseArtifact{{Name: "linux-arm64", Kind: "oci", Platform: "linux/arm64", Digest: digest, Size: 10, ImageDigest: digest, Receipt: ReleaseReceiptReference{WorkRunID: "run", StepKey: "build.image", Attempt: 1, IntentID: strings.Repeat("c", 64), ReceiptDigest: digest, DefinitionDigest: digest}}}}},
		Evidence:     []ReleaseEvidence{{Name: "full-tests", Component: "engine", WorkRunID: "run", StepKey: "test.full", Attempt: 1, ReceiptID: "receipt", ReceiptDigest: digest, DefinitionDigest: digest, ArtifactIntentIDs: []string{strings.Repeat("d", 64)}}},
		Destinations: []ReleaseDestination{{TargetID: "registry", TargetDigest: digest, Component: "engine", Artifact: "linux-arm64", Operation: "publish"}},
	}
	return c
}

func TestReleaseCandidateBindsEveryReviewedInput(t *testing.T) {
	_, original, err := CanonicalReleaseCandidate(releaseCandidateFixture())
	if err != nil {
		t.Fatal(err)
	}
	other := "sha256:" + strings.Repeat("e", 64)
	for name, change := range map[string]func(*ReleaseCandidate){
		"owner":                func(c *ReleaseCandidate) { c.OwnerUserID = "another" },
		"workflow":             func(c *ReleaseCandidate) { c.WorkflowDigest = other },
		"version":              func(c *ReleaseCandidate) { c.Components[0].Version = "0.26.0" },
		"repository":           func(c *ReleaseCandidate) { c.Components[0].Repository = "acme/other" },
		"source":               func(c *ReleaseCandidate) { c.Components[0].Commit = strings.Repeat("f", 40) },
		"artifact bytes":       func(c *ReleaseCandidate) { c.Components[0].Artifacts[0].Digest = other },
		"artifact size":        func(c *ReleaseCandidate) { c.Components[0].Artifacts[0].Size++ },
		"image":                func(c *ReleaseCandidate) { c.Components[0].Artifacts[0].ImageDigest = other },
		"platform":             func(c *ReleaseCandidate) { c.Components[0].Artifacts[0].Platform = "linux/amd64" },
		"producer":             func(c *ReleaseCandidate) { c.Components[0].Artifacts[0].Receipt.WorkRunID = "other" },
		"attempt":              func(c *ReleaseCandidate) { c.Components[0].Artifacts[0].Receipt.Attempt++ },
		"storage receipt":      func(c *ReleaseCandidate) { c.Components[0].Artifacts[0].Receipt.IntentID = strings.Repeat("f", 64) },
		"build definition":     func(c *ReleaseCandidate) { c.Components[0].Artifacts[0].Receipt.DefinitionDigest = other },
		"test receipt digest":  func(c *ReleaseCandidate) { c.Evidence[0].ReceiptDigest = other },
		"build receipt digest": func(c *ReleaseCandidate) { c.Components[0].Artifacts[0].Receipt.ReceiptDigest = other },
		"test receipt":         func(c *ReleaseCandidate) { c.Evidence[0].ReceiptID = "other" },
		"test definition":      func(c *ReleaseCandidate) { c.Evidence[0].DefinitionDigest = other },
		"test artifacts":       func(c *ReleaseCandidate) { c.Evidence[0].ArtifactIntentIDs = []string{strings.Repeat("f", 64)} },
		"target":               func(c *ReleaseCandidate) { c.Destinations[0].TargetID = "other" },
		"target configuration": func(c *ReleaseCandidate) { c.Destinations[0].TargetDigest = other },
		"installation": func(c *ReleaseCandidate) {
			c.Destinations[0].Operation = "install"
			c.Destinations[0].DesiredStateDigest = other
			c.Destinations[0].RollbackCandidateID = other
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := releaseCandidateFixture()
			change(&c)
			_, got, err := CanonicalReleaseCandidate(c)
			if err != nil {
				t.Fatal(err)
			}
			if got == original {
				t.Fatal("changed review inputs retained approval identity")
			}
		})
	}
}

func TestReleaseCandidateCompatibilityAndCanonicalOrder(t *testing.T) {
	c := releaseCandidateFixture()
	sdk := c.Components[0]
	sdk.Name, sdk.Version, sdk.Repository = "sdk", "2.1.3", "acme/sdk"
	c.Components = append(c.Components, sdk)
	e := c.Evidence[0]
	e.Name, e.Component = "sdk-tests", "sdk"
	c.Evidence = append(c.Evidence, e)
	c.Compatibility = []ReleaseCompatibility{{Component: "sdk", Requires: "engine", MinVersion: "0.25.0", MaxExclusive: "0.26.0"}}
	copyJSON, _ := json.Marshal(c)
	var original ReleaseCandidate
	json.Unmarshal(copyJSON, &original)
	a, hash, err := CanonicalReleaseCandidate(c)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c, original) {
		t.Fatal("canonicalization mutated caller values")
	}
	c.Components[0], c.Components[1] = c.Components[1], c.Components[0]
	c.Evidence[0], c.Evidence[1] = c.Evidence[1], c.Evidence[0]
	b, reordered, err := CanonicalReleaseCandidate(c)
	if err != nil || hash != reordered || string(a) != string(b) {
		t.Fatal("collection order changed identity", err)
	}
	c.Components[1].Version = "0.26.0"
	if _, _, err := CanonicalReleaseCandidate(c); err == nil {
		t.Fatal("incompatible component set accepted")
	}
}

func TestReleaseCandidateRefusesIncompleteOrAmbiguousReview(t *testing.T) {
	for name, change := range map[string]func(*ReleaseCandidate){
		"invalid UTF8":        func(c *ReleaseCandidate) { c.OwnerUserID = "owner\xff" },
		"no evidence":         func(c *ReleaseCandidate) { c.Evidence = nil },
		"duplicate component": func(c *ReleaseCandidate) { c.Components = append(c.Components, c.Components[0]) },
		"duplicate artifact": func(c *ReleaseCandidate) {
			c.Components[0].Artifacts = append(c.Components[0].Artifacts, c.Components[0].Artifacts[0])
		},
		"duplicate evidence":      func(c *ReleaseCandidate) { c.Evidence = append(c.Evidence, c.Evidence[0]) },
		"mutable image":           func(c *ReleaseCandidate) { c.Components[0].Artifacts[0].ImageDigest = "image:latest" },
		"floating source":         func(c *ReleaseCandidate) { c.Components[0].Commit = "main" },
		"missing receipt":         func(c *ReleaseCandidate) { c.Components[0].Artifacts[0].Receipt.IntentID = "" },
		"unknown target artifact": func(c *ReleaseCandidate) { c.Destinations[0].Artifact = "absent" },
		"unknown compatibility": func(c *ReleaseCandidate) {
			c.Compatibility = []ReleaseCompatibility{{Component: "sdk", Requires: "engine", MinVersion: "0.25.0", MaxExclusive: "0.26.0"}}
		},
		"rollback absent": func(c *ReleaseCandidate) { c.Destinations[0].Operation = "install" },
		"version alias":   func(c *ReleaseCandidate) { c.Components[0].Version = "00.25.0" },
	} {
		t.Run(name, func(t *testing.T) {
			c := releaseCandidateFixture()
			change(&c)
			if _, _, err := CanonicalReleaseCandidate(c); err == nil {
				t.Fatal("invalid candidate accepted")
			}
		})
	}
}
