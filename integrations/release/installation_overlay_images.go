package release

import (
	"errors"
	"strings"

	"github.com/znasllc-io/memql/component/pipelines"
	"gopkg.in/yaml.v3"
)

func overlayMap(n *yaml.Node) (map[string]*yaml.Node, error) {
	if n.Kind != yaml.MappingNode || len(n.Content)%2 != 0 {
		return nil, errors.New("installation overlay requires YAML mappings")
	}
	out := map[string]*yaml.Node{}
	for j := 0; j < len(n.Content); j += 2 {
		key := n.Content[j]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || out[key.Value] != nil {
			return nil, errors.New("installation overlay YAML keys are duplicated or malformed")
		}
		out[key.Value] = n.Content[j+1]
	}
	return out, nil
}
func overlayNodes(n *yaml.Node) error {
	if n.Kind == yaml.AliasNode || n.Anchor != "" {
		return errors.New("installation overlay aliases and anchors are unsupported")
	}
	if n.Tag != "" && !strings.HasPrefix(n.Tag, "!!") {
		return errors.New("installation overlay custom YAML tags are unsupported")
	}
	if n.Kind == yaml.MappingNode {
		if _, err := overlayMap(n); err != nil {
			return err
		}
	}
	for _, child := range n.Content {
		if err := overlayNodes(child); err != nil {
			return err
		}
	}
	return nil
}
func setOverlayScalar(n *yaml.Node, key, value string) {
	for j := 0; j < len(n.Content); j += 2 {
		if n.Content[j].Value == key {
			n.Content[j+1] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
			return
		}
	}
	n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value})
}
func removeOverlayScalar(n *yaml.Node, key string) {
	for j := 0; j < len(n.Content); j += 2 {
		if n.Content[j].Value == key {
			n.Content = append(n.Content[:j], n.Content[j+2:]...)
			return
		}
	}
}

// Only configured image slots change. Every replacement resolves from a signed
// published artifact, and the later installation independently verifies the
// entire rendered diff and actual bytes, including unchanged infrastructure.
func pinInstallationImages(before []byte, bindings []overlayImage, verified pipelines.VerifiedPublishedRelease) ([]byte, error) {
	document, root, err := readOverlayKustomization(before)
	if err != nil {
		return nil, err
	}
	if root["images"] == nil || root["images"].Kind != yaml.SequenceNode {
		return nil, errors.New("installation overlay must be a Kustomization with explicit image slots")
	}
	release, err := verified.Release()
	if err != nil {
		return nil, err
	}
	type replacement struct{ repository, digest string }
	replacements := map[string]replacement{}
	for _, binding := range bindings {
		matches := 0
		var result replacement
		for _, component := range release.Components {
			if component.Name != binding.Component {
				continue
			}
			for _, artifact := range component.Artifacts {
				if artifact.Name != binding.Artifact || artifact.Kind != "oci" {
					continue
				}
				for _, location := range artifact.Locations {
					if location.Kind == "oci" && strings.TrimPrefix(location.Origin, "https://")+"/"+location.Repository == binding.Repository && strings.HasPrefix(location.Origin, "https://") {
						matches++
						result = replacement{binding.Repository, artifact.ImageDigest}
					}
				}
			}
		}
		if matches != 1 || !stringsHasDigest(result.digest) || replacements[binding.Name].digest != "" {
			return nil, errors.New("installation image binding has no unique signed published OCI destination")
		}
		replacements[binding.Name] = result
	}
	seen := map[string]bool{}
	for _, item := range root["images"].Content {
		fields, err := overlayMap(item)
		if err != nil {
			return nil, err
		}
		name := fields["name"]
		if name == nil || name.Kind != yaml.ScalarNode || name.Tag != "!!str" || name.Value == "" || seen[name.Value] {
			return nil, errors.New("installation image slot is missing or duplicated")
		}
		seen[name.Value] = true
		replacement, ok := replacements[name.Value]
		if !ok {
			continue
		}
		for field := range fields {
			if field != "name" && field != "newName" && field != "newTag" && field != "digest" {
				return nil, errors.New("installation image slot has unsupported fields")
			}
			// Kustomize accepts an unquoted numeric tag (for example 16). It
			// is removed, never interpreted as code or carried into the output.
			literalTag := field == "newTag" && fields[field].Tag == "!!int"
			if fields[field].Kind != yaml.ScalarNode || (fields[field].Tag != "!!str" && !literalTag) {
				return nil, errors.New("installation image slot values must be literal strings")
			}
		}
		setOverlayScalar(item, "newName", replacement.repository)
		setOverlayScalar(item, "digest", replacement.digest)
		removeOverlayScalar(item, "newTag")
	}
	for name := range replacements {
		if !seen[name] {
			return nil, errors.New("installation overlay is missing a configured image slot")
		}
	}
	return writeOverlayKustomization(document)
}
