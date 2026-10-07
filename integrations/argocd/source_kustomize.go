package argocd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

var kustomizationNames = []string{"kustomization.yaml", "kustomization.yml", "Kustomization"}

// Every admitted field either has no external file reads or is handled below.
// New Kustomize features fail closed until their complete input codec is added.
var closedKustomizationFields = strings.Fields(`apiVersion kind metadata namePrefix nameSuffix namespace commonLabels labels commonAnnotations resources bases components patches patchesStrategicMerge patchesJson6902 images replicas configMapGenerator secretGenerator generatorOptions configurations transformers vars buildMetadata sortOptions`)

func (v *sourceVerifier) kustomization(dir string, component bool) error {
	if v.visiting[dir] {
		return errors.New("Kustomize source has a recursive directory dependency")
	}
	visitKey := fmt.Sprintf("%t:%s", component, dir)
	if v.visited[visitKey] {
		return nil
	}
	if v.settings >= 256 {
		return errors.New("Kustomize source exceeds its directory bound")
	}
	v.settings++
	v.visiting[dir] = true
	defer delete(v.visiting, dir)
	var setting string
	for _, name := range kustomizationNames {
		p := path.Join(dir, name)
		_, found, err := v.lookup(p)
		if err != nil {
			return err
		}
		if found {
			if setting != "" {
				return errors.New("Kustomize directory has ambiguous settings files")
			}
			setting = p
		}
	}
	if setting == "" {
		return fmt.Errorf("Kustomize directory %q has no committed settings", dir)
	}
	body, err := v.file(setting)
	if err != nil {
		return err
	}
	doc, err := sourceYAML(body)
	if err != nil {
		return err
	}
	fields, err := sourceFields(doc, closedKustomizationFields...)
	if err != nil {
		return err
	}
	wantKind, wantVersion := "Kustomization", "kustomize.config.k8s.io/v1beta1"
	if component {
		wantKind, wantVersion = "Component", "kustomize.config.k8s.io/v1alpha1"
	}
	if sourceString(fields["kind"]) != wantKind || sourceString(fields["apiVersion"]) != wantVersion {
		return errors.New("Kustomize source settings have an unsupported kind or version")
	}
	for _, key := range []string{"resources", "bases", "components"} {
		refs, err := sourceStrings(fields[key])
		if err != nil {
			return err
		}
		for _, ref := range refs {
			p, err := sourcePath(dir, ref)
			if err != nil {
				return err
			}
			entry, found, err := v.lookup(p)
			if err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("Kustomize input %q is absent from the commit", p)
			}
			if entry.mode == "40000" {
				if err := v.kustomization(p, key == "components"); err != nil {
					return err
				}
			} else {
				if key == "components" {
					return errors.New("Kustomize component must be a committed directory")
				}
				if _, err := v.file(p); err != nil {
					return err
				}
			}
		}
	}
	for _, key := range []string{"configurations"} {
		refs, err := sourceStrings(fields[key])
		if err != nil {
			return err
		}
		for _, ref := range refs {
			if _, err := v.relativeFile(dir, ref); err != nil {
				return err
			}
		}
	}
	for _, key := range []string{"patches", "patchesJson6902"} {
		patches, err := sourceSequence(fields[key])
		if err != nil {
			return err
		}
		for _, patch := range patches {
			f, err := sourceFields(patch, "path", "patch", "target", "options")
			if err != nil {
				return err
			}
			p, inline := sourceString(f["path"]), sourceString(f["patch"])
			if (p == "") == (inline == "") {
				return errors.New("Kustomize patch must have exactly one path or inline body")
			}
			if p != "" {
				if _, err := v.relativeFile(dir, p); err != nil {
					return err
				}
			}
		}
	}
	strategic, err := sourceStrings(fields["patchesStrategicMerge"])
	if err != nil {
		return err
	}
	for _, patch := range strategic {
		if strings.Contains(patch, "\n") || strings.HasPrefix(strings.TrimSpace(patch), "{") {
			// Kustomize tries inline decoding, then falls back to loading the
			// string as a path. Use the explicit inline field whose decoder
			// cannot fall back to an unverified read.
			return errors.New("Kustomize inline strategic patches must use patches.patch")
		}
		if _, err := v.relativeFile(dir, patch); err != nil {
			return err
		}
	}
	for _, key := range []string{"configMapGenerator", "secretGenerator"} {
		generators, err := sourceSequence(fields[key])
		if err != nil {
			return err
		}
		for _, generator := range generators {
			if err := v.generator(dir, generator); err != nil {
				return err
			}
		}
	}
	transformers, err := sourceStrings(fields["transformers"])
	if err != nil {
		return err
	}
	for _, reference := range transformers {
		body, err := v.relativeFile(dir, reference)
		if err != nil {
			return err
		}
		doc, err := sourceYAML(body)
		if err != nil {
			return err
		}
		// This installed topology uses only the built-in namespace transformer.
		// Other plugin kinds need their own input/side-effect qualification.
		f, err := sourceFields(doc, "apiVersion", "kind", "metadata", "fieldSpecs", "unsetOnly", "setRoleBindingSubjects", "namespace")
		if err != nil {
			return err
		}
		if sourceString(f["apiVersion"]) != "builtin" || sourceString(f["kind"]) != "NamespaceTransformer" {
			return errors.New("Kustomize transformer is not a qualified built-in namespace transformer")
		}
	}
	v.visited[visitKey] = true
	return nil
}

func (v *sourceVerifier) relativeFile(dir, reference string) ([]byte, error) {
	p, err := sourcePath(dir, reference)
	if err != nil {
		return nil, err
	}
	return v.file(p)
}

func (v *sourceVerifier) generator(dir string, node *yaml.Node) error {
	f, err := sourceFields(node, "name", "namespace", "behavior", "options", "files", "envs", "env", "literals", "type")
	if err != nil {
		return err
	}
	files, err := sourceStrings(f["files"])
	if err != nil {
		return err
	}
	for _, file := range files {
		_, reference, keyed := strings.Cut(file, "=")
		if !keyed {
			reference = file
		}
		p, err := sourcePath(dir, reference)
		if err != nil {
			return err
		}
		// The pinned Kustomize KV loader loads one file per entry; directory
		// expansion is a kubectl generator behavior, not this codec's contract.
		if _, err := v.file(p); err != nil {
			return err
		}
	}
	envs, err := sourceStrings(f["envs"])
	if err != nil {
		return err
	}
	if f["env"] != nil {
		env := sourceString(f["env"])
		if env == "" {
			return errors.New("Kustomize environment input is invalid")
		}
		envs = append(envs, env)
	}
	for _, env := range envs {
		body, err := v.relativeFile(dir, env)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if !strings.Contains(line, "=") {
				return errors.New("Kustomize environment files must supply values without host environment fallback")
			}
		}
	}
	return nil
}

func sourceYAML(body []byte) (*yaml.Node, error) {
	if len(body) > 512<<10 {
		return nil, errors.New("Kustomize settings exceed the YAML byte bound")
	}
	d := yaml.NewDecoder(bytes.NewReader(body))
	var doc yaml.Node
	if err := d.Decode(&doc); err != nil || len(doc.Content) != 1 {
		return nil, errors.New("Kustomize source contains invalid YAML")
	}
	var extra yaml.Node
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("Kustomize settings require one YAML document")
	}
	count := 0
	if err := validateSourceYAML(&doc, 0, &count); err != nil {
		return nil, err
	}
	return doc.Content[0], nil
}

func validateSourceYAML(node *yaml.Node, depth int, count *int) error {
	*count++
	if depth > 64 || *count > 100000 || node.Kind == yaml.AliasNode {
		return errors.New("Kustomize source has aliases or exceeds its YAML bound")
	}
	if node.Kind == yaml.MappingNode {
		seen := map[string]bool{}
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || seen[key.Value] {
				return errors.New("Kustomize source has ambiguous YAML keys")
			}
			seen[key.Value] = true
		}
	}
	for _, child := range node.Content {
		if err := validateSourceYAML(child, depth+1, count); err != nil {
			return err
		}
	}
	return nil
}

func sourceFields(node *yaml.Node, names ...string) (map[string]*yaml.Node, error) {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil, errors.New("Kustomize source requires a mapping")
	}
	allowed := map[string]bool{}
	for _, name := range names {
		allowed[name] = true
	}
	fields := map[string]*yaml.Node{}
	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i].Value
		if !allowed[key] {
			return nil, fmt.Errorf("Kustomize source field %q has no qualified closed-input codec", key)
		}
		fields[key] = node.Content[i+1]
	}
	return fields, nil
}

func sourceString(node *yaml.Node) string {
	if node == nil || node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return ""
	}
	return node.Value
}

func sourceSequence(node *yaml.Node) ([]*yaml.Node, error) {
	if node == nil {
		return nil, nil
	}
	if node.Kind != yaml.SequenceNode {
		return nil, errors.New("Kustomize source requires a sequence")
	}
	return node.Content, nil
}

func sourceStrings(node *yaml.Node) ([]string, error) {
	items, err := sourceSequence(node)
	if err != nil {
		return nil, err
	}
	strings := make([]string, 0, len(items))
	for _, item := range items {
		value := sourceString(item)
		if value == "" {
			return nil, errors.New("Kustomize source requires nonempty string inputs")
		}
		strings = append(strings, value)
	}
	return strings, nil
}
