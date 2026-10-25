package release

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/identity/githubapp"
	pl "github.com/znasllc-io/memql/component/pipelines"
)

func overlayNativeFixture() (overlayConfiguration, overlayRequest, overlayNativeEvidence) {
	c := overlayV3Configuration()
	digest := "sha256:" + strings.Repeat("b", 64)
	identity := overlayCandidateIdentity{ID: digest, ApprovalID: "approval", WorkflowDigest: digest, CatalogDigest: digest}
	run := overlayRunEvidence{PipelineID: "pipeline", PipelineRunID: "pipeline-run", WorkRunID: "work-run", Mode: "full", StepsPassed: 2, ReceiptDigest: digest}
	primary := overlayCandidateEvidence{Candidate: identity, Component: "engine", Version: "1.2.3", Repository: "acme/engine", Commit: strings.Repeat("a", 40), Platform: "linux/amd64", Runs: []overlayRunEvidence{run}, Images: map[string]overlayImageEvidence{"bff": {Digest: digest, InstanceRegistry: "registry.example/memql/engine@" + digest, ArchiveDigest: digest, ArchiveSize: 123, ArtifactReceipt: pl.ReleaseReceiptReference{WorkRunID: run.WorkRunID, StepKey: "build.image", Attempt: 1, IntentID: strings.Repeat("1", 64), ReceiptDigest: digest, DefinitionDigest: digest}, Publication: overlayPublicationEvidence{EffectID: "effect", State: "complete", TargetID: "registry", TargetDigest: digest, Origin: "https://registry.example", Repository: "memql/engine", Tag: "1.2.3"}}}}
	identity.ID = "sha256:" + strings.Repeat("c", 64)
	identity.CatalogDigest = "sha256:" + strings.Repeat("d", 64)
	companion := overlayCandidateEvidence{Candidate: identity, Component: "engine", Version: primary.Version, Repository: primary.Repository, Commit: primary.Commit, Runs: []overlayRunEvidence{run}, File: &pl.PublishedArtifact{Name: "docs", Kind: "file", Digest: digest, Size: 8123, Locations: []pl.PublishedLocation{{Kind: "github-release", Origin: "https://api.github.com", Repository: primary.Repository, Tag: "v1.2.3", ReleaseID: 42, AssetID: 43, AssetName: "docs-1.2.3.tgz"}}}}
	return c, overlayRequest{c.InstallationID, "request", primary.Candidate.ID, primary.Candidate.CatalogDigest, strings.Repeat("e", 40)}, overlayNativeEvidence{primary, companion}
}

func TestInstallationOverlayNativeManifestRecordsFactsWithoutInventedVerification(t *testing.T) {
	c, r, n := overlayNativeFixture()
	require.NoError(t, validateOverlayNativeEvidence(c, r, n))
	body, err := buildOverlayNativeManifest(n)
	require.NoError(t, err)
	require.Contains(t, string(body), `"stepsPassed": 2`)
	require.Contains(t, string(body), `"state": "complete"`)
	require.Contains(t, string(body), `"assetId": 43`)
	require.NotContains(t, string(body), "binaryExecuted")
	require.NotContains(t, string(body), "ghcrTagVerified")
	require.NotContains(t, string(body), "credential")
	path, err := overlayResolvedPath(c.Files[4], &n)
	require.NoError(t, err)
	require.Equal(t, "deploy/releases/component-1.2.3.json", path)
	again, err := buildOverlayNativeManifest(n)
	require.NoError(t, err)
	require.Equal(t, body, again)
	// Duplicate run identities are not silently summarized as one proof.
	n.Primary.Runs = append(n.Primary.Runs, n.Primary.Runs[0])
	_, err = buildOverlayNativeManifest(n)
	require.Error(t, err)
}

func TestInstallationOverlayNativeManifestSelectsExactImageProducer(t *testing.T) {
	c, r, n := overlayNativeFixture()
	imageRun := n.Primary.Runs[0]
	docsRun := imageRun
	docsRun.WorkRunID, docsRun.PipelineRunID, docsRun.StepsPassed = "docs-work", "docs-pipeline-run", 200
	n.Primary.Runs = []overlayRunEvidence{docsRun, imageRun}
	n.Companion.Runs = []overlayRunEvidence{docsRun}
	require.NoError(t, validateOverlayNativeEvidence(c, r, n))
	body, err := buildOverlayNativeManifest(n)
	require.NoError(t, err)
	var manifest struct {
		Build     overlayRunEvidence       `json:"build"`
		Companion overlayCandidateEvidence `json:"companion"`
	}
	require.NoError(t, json.Unmarshal(body, &manifest))
	require.Equal(t, imageRun, manifest.Build)
	require.Equal(t, n.Companion, manifest.Companion)
	require.Equal(t, []overlayRunEvidence{docsRun, imageRun}, n.Primary.Runs)
	encoded, err := json.Marshal(n)
	require.NoError(t, err)
	for name, modify := range map[string]func(*overlayNativeEvidence){
		"multiple image producers": func(n *overlayNativeEvidence) {
			image := n.Primary.Images["bff"]
			image.ArtifactReceipt.WorkRunID = docsRun.WorkRunID
			n.Primary.Images["agent"] = image
		},
		"missing image producer":   func(n *overlayNativeEvidence) { n.Primary.Runs = []overlayRunEvidence{docsRun} },
		"duplicate image producer": func(n *overlayNativeEvidence) { n.Primary.Runs = append(n.Primary.Runs, imageRun) },
		"unbound image receipt": func(n *overlayNativeEvidence) {
			image := n.Primary.Images["bff"]
			image.ArtifactReceipt.WorkRunID = ""
			n.Primary.Images["bff"] = image
		},
		"incomplete producer": func(n *overlayNativeEvidence) { n.Primary.Runs[1].PipelineRunID = "" },
	} {
		t.Run(name, func(t *testing.T) {
			var changed overlayNativeEvidence
			require.NoError(t, json.Unmarshal(encoded, &changed))
			modify(&changed)
			_, err := buildOverlayNativeManifest(changed)
			require.Error(t, err)
		})
	}
}

func TestInstallationOverlayNativeRecordPreservesExpectedAbsence(t *testing.T) {
	c, r, n := overlayNativeFixture()
	c.Files = []overlayFile{c.Files[4]}
	body, err := buildOverlayNativeManifest(n)
	require.NoError(t, err)
	path, err := overlayResolvedPath(c.Files[0], &n)
	require.NoError(t, err)
	record := overlayRecord{Owner: c.OwnerUserID, Request: r, Configuration: c, NativeEvidence: &n, Replacements: []githubapp.BranchReplacement{{Path: path, Content: string(body)}}, FilePreconditions: []overlayFilePrecondition{{Path: path, State: "absent"}}}
	require.NoError(t, validateOverlayRecordFiles(record))
	encoded, err := json.Marshal(record)
	require.NoError(t, err)
	var peer overlayRecord
	require.NoError(t, decodeCandidateObject(encoded, &peer))
	require.NoError(t, validateOverlayRecordFiles(peer))
	require.Equal(t, overlayHash(record), overlayHash(peer))
	for name, modify := range map[string]func(*overlayRecord){
		"synthesized empty-file digest": func(r *overlayRecord) { r.Replacements[0].BeforeSHA256 = strings.Repeat("f", 64) },
		"missing absence state":         func(r *overlayRecord) { r.FilePreconditions = nil },
		"present instead of absent":     func(r *overlayRecord) { r.FilePreconditions[0].State = "present" },
		"different filename":            func(r *overlayRecord) { r.Replacements[0].Path = "deploy/releases/component-1.2.4.json" },
		"changed evidence bytes": func(r *overlayRecord) {
			r.Replacements[0].Content = strings.Replace(r.Replacements[0].Content, `"stepsPassed": 2`, `"stepsPassed": 200`, 1)
		},
		"changed companion source": func(r *overlayRecord) { r.NativeEvidence.Companion.Commit = strings.Repeat("f", 40) },
		"changed primary catalog": func(r *overlayRecord) {
			r.NativeEvidence.Primary.Candidate.CatalogDigest = "sha256:" + strings.Repeat("f", 64)
		},
		"legacy record": func(r *overlayRecord) { r.Configuration.FormatVersion = 2 },
	} {
		t.Run(name, func(t *testing.T) {
			var changed overlayRecord
			require.NoError(t, decodeCandidateObject(encoded, &changed))
			modify(&changed)
			require.Error(t, validateOverlayRecordFiles(changed))
		})
	}
}
