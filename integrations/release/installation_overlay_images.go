package release

import (
	"bytes"
	"errors"
	"io"
	"strings"

	"github.com/znasllc-io/memql/component/pipelines"
	"gopkg.in/yaml.v3"
)

func overlayMap(n *yaml.Node) (map[string]*yaml.Node, error) {
	if n.Kind != yaml.MappingNode {
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
	if len(before) == 0 || len(before) > 1<<20 {
		return nil, errors.New("installation overlay file is empty or too large")
	}
	var doc yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(before))
	if decoder.Decode(&doc) != nil || decoder.Decode(new(yaml.Node)) != io.EOF || len(doc.Content) != 1 {
		return nil, errors.New("installation overlay requires one valid YAML document")
	}
	if err := overlayNodes(&doc); err != nil {
		return nil, err
	}
	root, err := overlayMap(doc.Content[0])
	if err != nil {
		return nil, err
	}
	if root["kind"] == nil || root["kind"].Value != "Kustomization" || root["images"] == nil || root["images"].Kind != yaml.SequenceNode {
		return nil, errors.New("installation overlay requires an explicit Kustomization image list")
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
		if name == nil || name.Kind != yaml.ScalarNode || seen[name.Value] {
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
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(&doc); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	if out.Len() > 1<<20 {
		return nil, errors.New("installation overlay exceeds its file bound")
	}
	return out.Bytes(), nil
}
