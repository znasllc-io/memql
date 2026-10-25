package release

import (
	"encoding/json"
	"errors"
	"strings"
)

// Absence is a first-class immutable-tree precondition, not the digest of an
// empty file. The Git writer independently checks it before creating a blob.
type overlayFilePrecondition struct {
	Path         string `json:"path"`
	State        string `json:"state"`
	BeforeSHA256 string `json:"beforeSHA256,omitempty"`
}

func overlayResolvedPath(file overlayFile, native *overlayNativeEvidence) (string, error) {
	if file.Codec != "nativeManifest" {
		return file.Path, nil
	}
	if native == nil || !overlayStableVersion.MatchString(native.Primary.Version) || strings.Count(file.Path, "{version}") != 1 {
		return "", errors.New("native manifest has no exact versioned path")
	}
	resolved := strings.Replace(file.Path, "{version}", native.Primary.Version, 1)
	if !overlayFilePath(resolved) {
		return "", errors.New("native manifest path escaped its configuration")
	}
	return resolved, nil
}

func (n overlayNativeEvidence) scalarFacts() overlayScalarFacts {
	if n.Companion.File == nil {
		return overlayScalarFacts{}
	}
	return overlayScalarFacts{Version: n.Primary.Version, Commit: n.Primary.Commit, CompanionDigest: n.Companion.File.Digest, CompanionSize: n.Companion.File.Size}
}

func validateOverlayNativeEvidence(c overlayConfiguration, r overlayRequest, n overlayNativeEvidence) error {
	if c.NativeEvidence == nil || c.FormatVersion != 3 || !overlayCompanionMatches(n.Primary, n.Companion) || n.Primary.Candidate.ID != r.CandidateID || n.Primary.Candidate.CatalogDigest != r.CatalogDigest || n.Primary.Component != c.NativeEvidence.Component || n.Companion.Component != c.NativeEvidence.CompanionComponent || n.Companion.File.Name != c.NativeEvidence.CompanionArtifact {
		return errors.New("stored native overlay selection differs from its configured source")
	}
	for _, evidence := range []overlayCandidateEvidence{n.Primary, n.Companion} {
		identity := evidence.Candidate
		if !stringsHasDigest(identity.ID) || !stringsHasDigest(identity.CatalogDigest) || !stringsHasDigest(identity.WorkflowDigest) || identity.ApprovalID == "" || len(identity.ApprovalID) > 512 || !overlayRepository.MatchString(evidence.Repository) || !overlayStableVersion.MatchString(evidence.Version) || !installationCommit.MatchString(evidence.Commit) || len(evidence.Runs) == 0 || len(evidence.Runs) > 64 {
			return errors.New("stored native overlay identity is incomplete")
		}
		seen := map[string]bool{}
		for _, run := range evidence.Runs {
			if run.WorkRunID == "" || run.PipelineID == "" || run.PipelineRunID == "" || run.Mode == "" || seen[run.WorkRunID] || run.StepsPassed < 1 || run.StepsSkipped < 0 || run.StepsPassed+run.StepsSkipped > 1024 || !stringsHasDigest(run.ReceiptDigest) {
				return errors.New("stored native overlay run evidence is incomplete")
			}
			seen[run.WorkRunID] = true
		}
	}
	file := n.Companion.File
	if file.Kind != "file" || file.Size <= 0 || !stringsHasDigest(file.Digest) || len(file.Locations) != 1 {
		return errors.New("stored overlay companion is not an exact published file")
	}
	location := file.Locations[0]
	if location.Kind != "github-release" || location.Repository != n.Primary.Repository || location.Tag != "v"+n.Primary.Version || location.ReleaseID <= 0 || location.AssetID <= 0 || location.AssetName == "" {
		return errors.New("stored overlay companion publication is incomplete")
	}
	if len(n.Primary.Images) != len(c.NativeEvidence.Images) || n.Primary.Platform == "" {
		return errors.New("stored native image set is incomplete")
	}
	for _, binding := range c.NativeEvidence.Images {
		image, ok := n.Primary.Images[binding.Name]
		p := image.Publication
		if !ok || !stringsHasDigest(image.Digest) || !stringsHasDigest(image.ArchiveDigest) || image.ArchiveSize <= 0 || image.InstanceRegistry != binding.Repository+"@"+image.Digest || p.State != "complete" || p.EffectID == "" || p.TargetID == "" || !stringsHasDigest(p.TargetDigest) || strings.TrimPrefix(p.Origin, "https://")+"/"+p.Repository != binding.Repository || !stringsHasDigest(image.ArtifactReceipt.ReceiptDigest) || !stringsHasDigest(image.ArtifactReceipt.DefinitionDigest) || image.ArtifactReceipt.IntentID == "" || image.ArtifactReceipt.StepKey == "" || image.ArtifactReceipt.Attempt < 1 {
			return errors.New("stored native image publication is incomplete")
		}
		found := false
		for _, run := range n.Primary.Runs {
			found = found || image.ArtifactReceipt.WorkRunID == run.WorkRunID
		}
		if !found {
			return errors.New("stored native image producer is outside its complete run evidence")
		}
	}
	return nil
}

func buildOverlayNativeManifest(n overlayNativeEvidence) ([]byte, error) {
	// This codec has one image-build header. A combined candidate may retain
	// other complete runs for files; select only the producer bound by every
	// image receipt, without combining its counts with unrelated evidence.
	if n.Companion.File == nil || len(n.Primary.Images) == 0 {
		return nil, errors.New("native manifest requires one complete image-producing run and its file companion")
	}
	producer := ""
	for _, image := range n.Primary.Images {
		if image.ArtifactReceipt.WorkRunID == "" || (producer != "" && image.ArtifactReceipt.WorkRunID != producer) {
			return nil, errors.New("native manifest image producers differ")
		}
		producer = image.ArtifactReceipt.WorkRunID
	}
	var imageRun overlayRunEvidence
	seen := map[string]bool{}
	for _, run := range n.Primary.Runs {
		if run.WorkRunID == "" || seen[run.WorkRunID] {
			return nil, errors.New("native manifest run evidence is ambiguous")
		}
		seen[run.WorkRunID] = true
		if run.WorkRunID == producer {
			imageRun = run
		}
	}
	if imageRun.WorkRunID == "" || imageRun.PipelineID == "" || imageRun.PipelineRunID == "" || imageRun.Mode == "" || imageRun.StepsPassed < 1 || imageRun.StepsSkipped < 0 || imageRun.StepsPassed+imageRun.StepsSkipped > 1024 || !stringsHasDigest(imageRun.ReceiptDigest) {
		return nil, errors.New("native manifest image producer has no complete run evidence")
	}
	type build struct {
		Provider string `json:"provider"`
		overlayRunEvidence
	}
	manifest := struct {
		Version        string                          `json:"version"`
		GitTag         string                          `json:"gitTag"`
		SourceRevision string                          `json:"sourceRevision"`
		Platform       string                          `json:"platform"`
		Build          build                           `json:"build"`
		Candidate      overlayCandidateIdentity        `json:"candidate"`
		Images         map[string]overlayImageEvidence `json:"images"`
		Companion      overlayCandidateEvidence        `json:"companion"`
	}{n.Primary.Version, "v" + n.Primary.Version, n.Primary.Commit, n.Primary.Platform, build{"MemQL Pipelines", imageRun}, n.Primary.Candidate, n.Primary.Images, n.Companion}
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil || len(body) >= 1<<20 {
		return nil, errors.New("native release manifest exceeds its bound")
	}
	return append(body, '\n'), nil
}
