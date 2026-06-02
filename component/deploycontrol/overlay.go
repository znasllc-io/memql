package deploycontrol

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Overlay is the typed projection of a kustomize overlay
// kustomization.yaml -- specifically the images: block that is the
// SINGLE image authority for an environment (deployment-v2 Phase 1,
// znasllc-io/memql#699).
//
// For the prod overlay the file's first line is a provenance comment
//
//	# Promoted from releases/<version>.yaml by scripts/release/promote.sh ...
//
// from which PromotedVersion is parsed. The staging overlay carries
// no such comment, so PromotedVersion is empty there.
type Overlay struct {
	// PromotedVersion is the release version parsed from the prod
	// overlay's "Promoted from releases/<version>.yaml" comment.
	// Empty for staging.
	PromotedVersion string
	// Namespace is the kustomization's namespace field.
	Namespace string
	// Images is the ordered images: list (8 components).
	Images []OverlayImage
}

// OverlayImage is one entry of the overlay's images: list.
type OverlayImage struct {
	// Name is the fully-qualified image name (e.g.
	// "acrmemql.azurecr.io/memql-identity").
	Name string
	// Digest is the pinned "sha256:<64hex>" image authority.
	Digest string
}

// overlayDoc mirrors the subset of kustomization.yaml we parse.
type overlayDoc struct {
	Namespace string `yaml:"namespace"`
	Images    []struct {
		Name   string `yaml:"name"`
		Digest string `yaml:"digest"`
	} `yaml:"images"`
}

// promotedVersionRe extracts the version from the prod overlay's
// provenance comment. Tolerates the ".yaml" suffix being present.
var promotedVersionRe = regexp.MustCompile(`Promoted from releases/([^\s]+?)\.yaml`)

// ShortName returns the image name with the ACR registry prefix
// stripped, matching the lockfile's component map keys (e.g.
// "acrmemql.azurecr.io/memql-identity" -> "memql-identity").
func (i OverlayImage) ShortName() string {
	if idx := strings.LastIndex(i.Name, "/"); idx >= 0 {
		return i.Name[idx+1:]
	}
	return i.Name
}

// ParseOverlay parses a kustomize overlay kustomization.yaml's raw
// bytes into an Overlay. The promoted-version provenance comment (if
// present) is parsed from the leading comment lines; yaml.Unmarshal
// ignores comments, so we scan the raw bytes for it separately.
func ParseOverlay(raw []byte) (Overlay, error) {
	var doc overlayDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return Overlay{}, fmt.Errorf("deploycontrol: parse overlay yaml: %w", err)
	}

	ov := Overlay{
		Namespace:       doc.Namespace,
		PromotedVersion: parsePromotedVersion(raw),
		Images:          make([]OverlayImage, 0, len(doc.Images)),
	}
	for _, img := range doc.Images {
		ov.Images = append(ov.Images, OverlayImage{
			Name:   strings.TrimSpace(img.Name),
			Digest: strings.TrimSpace(img.Digest),
		})
	}
	return ov, nil
}

// parsePromotedVersion scans the raw overlay bytes for the prod
// provenance comment and returns the parsed version, or "" when the
// comment is absent (staging).
func parsePromotedVersion(raw []byte) string {
	m := promotedVersionRe.FindSubmatch(raw)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(string(m[1]))
}
