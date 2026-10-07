package pipelines

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// ReleaseCandidate is the immutable review boundary for a coordinated release.
// The installed DSL selects components, required evidence and destinations. A
// trusted preparer resolves every receipt and destination before sealing this
// value; JSON supplied by a client is never evidence that a check passed.
// Runtime identity, approvals and publication effects live outside this value.
type ReleaseCandidate struct {
	FormatVersion  int                    `json:"formatVersion"`
	OwnerUserID    string                 `json:"ownerUserId"`
	WorkflowDigest string                 `json:"workflowDigest"`
	Components     []ReleaseComponent     `json:"components"`
	Compatibility  []ReleaseCompatibility `json:"compatibility"`
	Evidence       []ReleaseEvidence      `json:"evidence"`
	Destinations   []ReleaseDestination   `json:"destinations"`
}

type ReleaseComponent struct {
	Name       string            `json:"name"`
	Version    string            `json:"version"`
	Repository string            `json:"repository"`
	Commit     string            `json:"commit"`
	Artifacts  []ReleaseArtifact `json:"artifacts"`
}

// ReleaseReceiptReference names one immutable journal receipt, never an
// editable Library file. Artifact scope and content must be revalidated by the
// trusted preparer and pinned before verification or publication starts.
type ReleaseReceiptReference struct {
	WorkRunID        string `json:"workRunId"`
	StepKey          string `json:"stepKey"`
	Attempt          int    `json:"attempt"`
	IntentID         string `json:"intentId"`
	ReceiptDigest    string `json:"receiptDigest"`
	DefinitionDigest string `json:"definitionDigest"`
}

type ReleaseArtifact struct {
	Name        string                  `json:"name"`
	Kind        string                  `json:"kind"` // oci or file
	Platform    string                  `json:"platform"`
	Digest      string                  `json:"digest"` // whole retained artifact bytes
	Size        int64                   `json:"size"`
	ImageDigest string                  `json:"imageDigest,omitempty"`
	Receipt     ReleaseReceiptReference `json:"receipt"`
}

type ReleaseCompatibility struct {
	Component    string `json:"component"`
	Requires     string `json:"requires"`
	MinVersion   string `json:"minVersion"`
	MaxExclusive string `json:"maxExclusive"`
}

// A receipt's success and exact source are read from the native work journal.
// There is deliberately no caller-settable passed/approved boolean.
type ReleaseEvidence struct {
	Name              string   `json:"name"`
	Component         string   `json:"component"`
	WorkRunID         string   `json:"workRunId"`
	StepKey           string   `json:"stepKey"`
	Attempt           int      `json:"attempt"`
	ReceiptID         string   `json:"receiptId"`
	ReceiptDigest     string   `json:"receiptDigest"`
	DefinitionDigest  string   `json:"definitionDigest"`
	ArtifactIntentIDs []string `json:"artifactIntentIds"`
}

// TargetID resolves through operator configuration. TargetDigest binds that
// resolved configuration (excluding credential material), so changing a
// registry, repository or installation invalidates prior approval.
type ReleaseDestination struct {
	TargetID            string `json:"targetId"`
	TargetDigest        string `json:"targetDigest"`
	Component           string `json:"component"`
	Artifact            string `json:"artifact"`
	Operation           string `json:"operation"` // publish or install
	DesiredStateDigest  string `json:"desiredStateDigest,omitempty"`
	RollbackCandidateID string `json:"rollbackCandidateId,omitempty"`
}

var releaseName = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,127}$`)
var releaseSource = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var releaseCommit = regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`)
var releaseVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func releaseDigest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != 71 {
		return false
	}
	b, err := hex.DecodeString(value[7:])
	return err == nil && hex.EncodeToString(b) == value[7:]
}

func releaseIdentity(value string) bool {
	// These are machine identifiers, not labels. Refuse invalid UTF-8 before
	// JSON's replacement-character normalization can alias a different value.
	return value != "" && len(value) <= 512 && strings.IndexFunc(value, func(r rune) bool { return r < 33 || r > 126 }) < 0
}

func validReleaseReceipt(r ReleaseReceiptReference) bool {
	return releaseIdentity(r.WorkRunID) && releaseIdentity(r.StepKey) && r.Attempt > 0 &&
		ValidArtifactIntentIDs([]string{r.IntentID}) && releaseDigest(r.DefinitionDigest) && releaseDigest(r.ReceiptDigest)
}

// CanonicalReleaseCandidate checks structure and compatibility and returns a
// deep-copy canonical JSON plus its domain-separated SHA-256 identity. It does
// not certify receipts or grant authority. The preparer must resolve and verify
// all inputs first; the approval store must reread and verify this digest.
func CanonicalReleaseCandidate(input ReleaseCandidate) ([]byte, string, error) {
	encoded, err := json.Marshal(input)
	if err != nil || len(encoded) > 1<<20 {
		return nil, "", errors.New("release candidate exceeds its encoding bound")
	}
	var c ReleaseCandidate
	if err := json.Unmarshal(encoded, &c); err != nil {
		return nil, "", err
	}
	if c.FormatVersion != 1 || !releaseIdentity(c.OwnerUserID) || !releaseDigest(c.WorkflowDigest) ||
		len(c.Components) == 0 || len(c.Components) > 64 || len(c.Evidence) == 0 || len(c.Evidence) > 1024 ||
		len(c.Destinations) == 0 || len(c.Destinations) > 1024 || len(c.Compatibility) > 256 {
		return nil, "", errors.New("release candidate requires bounded components, evidence, destinations and workflow identity")
	}
	components := map[string]ReleaseComponent{}
	artifacts := map[string]bool{}
	for i := range c.Components {
		component := &c.Components[i]
		if !releaseName.MatchString(component.Name) || components[component.Name].Name != "" ||
			!releaseVersion.MatchString(component.Version) || !releaseSource.MatchString(component.Repository) ||
			len(component.Repository) > 257 || !releaseCommit.MatchString(component.Commit) || len(component.Artifacts) == 0 || len(component.Artifacts) > 128 {
			return nil, "", errors.New("release component has invalid or repeated identity")
		}
		components[component.Name] = *component
		for _, artifact := range component.Artifacts {
			key := component.Name + "/" + artifact.Name
			if !releaseName.MatchString(artifact.Name) || artifacts[key] || !releaseDigest(artifact.Digest) ||
				artifact.Size < 0 || artifact.Size > 2<<30 || !validReleaseReceipt(artifact.Receipt) {
				return nil, "", errors.New("release artifact requires a unique name and immutable bounded receipt")
			}
			switch artifact.Kind {
			case "oci":
				if !releaseDigest(artifact.ImageDigest) || (artifact.Platform != "linux/amd64" && artifact.Platform != "linux/arm64") {
					return nil, "", errors.New("OCI candidate requires verified image digest and platform")
				}
			case "file":
				if artifact.ImageDigest != "" || !releaseIdentity(artifact.Platform) {
					return nil, "", errors.New("file artifact has inconsistent image or platform identity")
				}
			default:
				return nil, "", errors.New("unsupported release artifact kind")
			}
			artifacts[key] = true
		}
		slices.SortFunc(component.Artifacts, func(a, b ReleaseArtifact) int { return strings.Compare(a.Name, b.Name) })
	}
	slices.SortFunc(c.Components, func(a, b ReleaseComponent) int { return strings.Compare(a.Name, b.Name) })
	compatibility := map[string]bool{}
	for _, rule := range c.Compatibility {
		required, ok := components[rule.Requires]
		key := rule.Component + "/" + rule.Requires
		if !ok || components[rule.Component].Name == "" || rule.Component == rule.Requires || compatibility[key] ||
			!releaseVersion.MatchString(rule.MinVersion) || !releaseVersion.MatchString(rule.MaxExclusive) ||
			compareReleaseVersion(rule.MinVersion, rule.MaxExclusive) >= 0 || compareReleaseVersion(required.Version, rule.MinVersion) < 0 || compareReleaseVersion(required.Version, rule.MaxExclusive) >= 0 {
			return nil, "", fmt.Errorf("incompatible or invalid component requirement %q", key)
		}
		compatibility[key] = true
	}
	slices.SortFunc(c.Compatibility, func(a, b ReleaseCompatibility) int {
		return strings.Compare(a.Component+"/"+a.Requires, b.Component+"/"+b.Requires)
	})
	evidence := map[string]bool{}
	componentEvidence := map[string]bool{}
	for i := range c.Evidence {
		e := &c.Evidence[i]
		if !releaseName.MatchString(e.Name) || evidence[e.Name] || components[e.Component].Name == "" ||
			!releaseIdentity(e.WorkRunID) || !releaseIdentity(e.StepKey) || e.Attempt < 1 || !releaseIdentity(e.ReceiptID) ||
			!releaseDigest(e.DefinitionDigest) || !releaseDigest(e.ReceiptDigest) || !ValidArtifactIntentIDs(e.ArtifactIntentIDs) {
			return nil, "", errors.New("release evidence requires a unique name and exact work receipt")
		}
		evidence[e.Name], componentEvidence[e.Component] = true, true
		slices.Sort(e.ArtifactIntentIDs)
	}
	for name := range components {
		if !componentEvidence[name] {
			return nil, "", errors.New("each component requires verification evidence")
		}
	}
	slices.SortFunc(c.Evidence, func(a, b ReleaseEvidence) int { return strings.Compare(a.Name, b.Name) })
	destinations := map[string]bool{}
	for _, d := range c.Destinations {
		key := d.TargetID + "/" + d.Component + "/" + d.Artifact + "/" + d.Operation
		if !releaseName.MatchString(d.TargetID) || !releaseDigest(d.TargetDigest) || !artifacts[d.Component+"/"+d.Artifact] || destinations[key] {
			return nil, "", errors.New("release destination must bind an existing artifact and operator target")
		}
		switch d.Operation {
		case "publish":
			if d.DesiredStateDigest != "" || d.RollbackCandidateID != "" {
				return nil, "", errors.New("publication cannot carry installation state")
			}
		case "install":
			if !releaseDigest(d.DesiredStateDigest) || !releaseDigest(d.RollbackCandidateID) {
				return nil, "", errors.New("installation requires pinned desired state and rollback candidate")
			}
		default:
			return nil, "", errors.New("unsupported release destination operation")
		}
		destinations[key] = true
	}
	slices.SortFunc(c.Destinations, func(a, b ReleaseDestination) int {
		return strings.Compare(a.TargetID+"/"+a.Component+"/"+a.Artifact+"/"+a.Operation, b.TargetID+"/"+b.Component+"/"+b.Artifact+"/"+b.Operation)
	})
	body, err := json.Marshal(c)
	if err != nil {
		return nil, "", err
	}
	hash := sha256.Sum256(append([]byte("memql.release-candidate.v1\x00"), body...))
	return body, "sha256:" + hex.EncodeToString(hash[:]), nil
}

// Compare canonical numeric components without narrowing untrusted integers.
func compareReleaseVersion(a, b string) int {
	aa, bb := strings.Split(a, "."), strings.Split(b, ".")
	for i := range aa {
		if len(aa[i]) != len(bb[i]) {
			if len(aa[i]) < len(bb[i]) {
				return -1
			}
			return 1
		}
		if c := strings.Compare(aa[i], bb[i]); c != 0 {
			return c
		}
	}
	return 0
}
