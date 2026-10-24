package argocd

import (
	"errors"
	"strings"

	"gopkg.in/yaml.v3"
)

// Replacements either contain values/selectors or load one committed YAML
// document. Loaded replacement lists cannot recursively load another file.
func (v *sourceVerifier) replacements(dir string, node *yaml.Node) error {
	entries, err := sourceSequence(node)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		fields, err := sourceFields(entry, "path", "source", "targets")
		if err != nil {
			return err
		}
		if fields["path"] != nil {
			if fields["source"] != nil || fields["targets"] != nil {
				return errors.New("Kustomize replacement cannot combine a file and inline values")
			}
			body, err := v.relativeFile(dir, sourceString(fields["path"]))
			if err != nil {
				return err
			}
			doc, err := sourceYAML(body)
			if err != nil {
				return err
			}
			loaded := []*yaml.Node{doc}
			if doc.Kind == yaml.SequenceNode {
				loaded = doc.Content
			}
			if len(loaded) == 0 {
				return errors.New("Kustomize replacement file is empty")
			}
			for _, replacement := range loaded {
				if err := replacementValue(replacement); err != nil {
					return err
				}
			}
		} else if err := replacementValue(entry); err != nil {
			return err
		}
	}
	return nil
}
func replacementValue(node *yaml.Node) error {
	fields, err := sourceFields(node, "source", "targets")
	if err != nil {
		return err
	}
	source, err := sourceFields(fields["source"], "group", "version", "kind", "name", "namespace", "fieldPath", "options")
	if err != nil {
		return err
	}
	for key, node := range source {
		if key != "options" && (node.Kind != yaml.ScalarNode || node.Tag != "!!str") {
			return errors.New("Kustomize replacement source selectors must be strings")
		}
	}
	if err := replacementOptions(source["options"]); err != nil {
		return err
	}
	targets, err := sourceSequence(fields["targets"])
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return errors.New("Kustomize replacement requires targets")
	}
	for _, target := range targets {
		f, err := sourceFields(target, "select", "reject", "fieldPaths", "options")
		if err != nil {
			return err
		}
		if err := sourceSelector(f["select"]); err != nil {
			return err
		}
		rejected, err := sourceSequence(f["reject"])
		if err != nil {
			return err
		}
		for _, selector := range rejected {
			if err := sourceSelector(selector); err != nil {
				return err
			}
		}
		if _, err := sourceStrings(f["fieldPaths"]); err != nil {
			return err
		}
		if err := replacementOptions(f["options"]); err != nil {
			return err
		}
	}
	return nil
}
func sourceSelector(node *yaml.Node) error {
	f, err := sourceFields(node, "group", "version", "kind", "name", "namespace", "labelSelector", "annotationSelector")
	if err != nil {
		return err
	}
	for _, value := range f {
		if value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
			return errors.New("Kustomize selectors must be strings")
		}
	}
	return nil
}
func replacementOptions(node *yaml.Node) error {
	if node == nil {
		return nil
	}
	f, err := sourceFields(node, "delimiter", "index", "create")
	if err != nil {
		return err
	}
	for key, value := range f {
		expected := "!!str"
		if key == "index" {
			expected = "!!int"
		}
		if key == "create" {
			expected = "!!bool"
		}
		if value.Kind != yaml.ScalarNode || value.Tag != expected {
			return errors.New("Kustomize replacement options have invalid types")
		}
	}
	return nil
}
func (v *sourceVerifier) transformer(dir string, node *yaml.Node) error {
	f, err := sourceFields(node, "apiVersion", "kind", "metadata", "fieldSpecs", "unsetOnly", "setRoleBindingSubjects", "namespace", "target", "patch", "path", "options")
	if err != nil {
		return err
	}
	if sourceString(f["apiVersion"]) != "builtin" {
		return errors.New("Kustomize transformer is not a qualified built-in transformer")
	}
	switch sourceString(f["kind"]) {
	case "NamespaceTransformer":
		_, err := sourceFields(node, "apiVersion", "kind", "metadata", "fieldSpecs", "unsetOnly", "setRoleBindingSubjects", "namespace")
		return err
	case "PatchTransformer":
		f, err = sourceFields(node, "apiVersion", "kind", "metadata", "target", "patch", "path", "options")
		if err != nil {
			return err
		}
		if err := sourceSelector(f["target"]); err != nil {
			return err
		}
		inline, p := strings.TrimSpace(sourceString(f["patch"])), sourceString(f["path"])
		if (inline == "") == (p == "") {
			return errors.New("Kustomize patch transformer requires exactly one path or inline patch")
		}
		if p != "" {
			if _, err := v.relativeFile(dir, p); err != nil {
				return err
			}
		}
		if f["options"] != nil {
			options, err := sourceFields(f["options"], "allowNameChange", "allowKindChange")
			if err != nil {
				return err
			}
			for _, value := range options {
				if value.Kind != yaml.ScalarNode || value.Tag != "!!bool" {
					return errors.New("Kustomize patch transformer options must be boolean")
				}
			}
		}
		return nil
	default:
		return errors.New("Kustomize transformer is not a qualified built-in namespace or patch transformer")
	}
}
