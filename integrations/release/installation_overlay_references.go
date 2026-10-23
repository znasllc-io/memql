package release

import (
	"bytes"
	"errors"
	"io"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/integrations/argocd"
	"gopkg.in/yaml.v3"
)

func readOverlayKustomization(before []byte) (*yaml.Node, map[string]*yaml.Node, error) {
	if len(before) == 0 || len(before) > 1<<20 || !utf8.Valid(before) || bytes.ContainsRune(before, 0) {
		return nil, nil, errors.New("installation overlay YAML exceeds its bound or is invalid text")
	}
	var document yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(before))
	if decoder.Decode(&document) != nil || len(document.Content) != 1 {
		return nil, nil, errors.New("installation overlay YAML is invalid")
	}
	var extra yaml.Node
	if decoder.Decode(&extra) != io.EOF || overlayNodes(&document) != nil {
		return nil, nil, errors.New("installation overlay YAML is ambiguous")
	}
	root, err := overlayMap(document.Content[0])
	if err != nil || root["kind"] == nil || root["kind"].Kind != yaml.ScalarNode || root["kind"].Value != "Kustomization" {
		return nil, nil, errors.New("installation overlay requires a Kustomization")
	}
	return &document, root, nil
}
func writeOverlayKustomization(document *yaml.Node) ([]byte, error) {
	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	if err := encoder.Encode(document); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	if output.Len() > 1<<20 {
		return nil, errors.New("installation overlay replacement exceeds its bound")
	}
	return output.Bytes(), nil
}
func overlayPublishedComponent(publication pipelines.VerifiedPublishedRelease, name string) (pipelines.PublishedComponent, error) {
	release, err := publication.Release()
	if err != nil {
		return pipelines.PublishedComponent{}, err
	}
	for _, component := range release.Components {
		if component.Name == name {
			return component, nil
		}
	}
	return pipelines.PublishedComponent{}, errors.New("installation overlay component is absent from the signed publication")
}

// Only ref changes. Repository, entry directory, entry kind, and qualified URL
// options remain fixed; an unrelated explicitly pinned dependency stays intact.
func pinInstallationGitReferences(before []byte, bindings []overlayGitReference, publication pipelines.VerifiedPublishedRelease) ([]byte, error) {
	document, root, err := readOverlayKustomization(before)
	if err != nil {
		return nil, err
	}
	type target struct {
		commit string
		found  bool
	}
	wanted := map[string]*target{}
	for _, binding := range bindings {
		component, err := overlayPublishedComponent(publication, binding.Component)
		if err != nil {
			return nil, err
		}
		if binding.Repository != "https://github.com/"+component.Repository+".git" || !installationCommit.MatchString(component.Commit) {
			return nil, errors.New("installation Git-reference source disagrees with the signed component")
		}
		key := binding.Field + "\x00" + binding.Repository + "\x00" + binding.Path
		if wanted[key] != nil {
			return nil, errors.New("installation Git-reference binding is duplicated")
		}
		wanted[key] = &target{commit: component.Commit}
	}
	if len(wanted) == 0 {
		return nil, errors.New("installation Git-reference bindings are missing")
	}
	for _, field := range []string{"resources", "components"} {
		sequence := root[field]
		if sequence == nil {
			continue
		}
		if sequence.Kind != yaml.SequenceNode {
			return nil, errors.New("installation source references must be YAML sequences")
		}
		seen := map[string]bool{}
		for _, entry := range sequence.Content {
			if entry.Kind != yaml.ScalarNode || entry.Tag != "!!str" || entry.Value == "" {
				return nil, errors.New("installation source reference is not a literal string")
			}
			if seen[entry.Value] {
				return nil, errors.New("installation source reference is duplicated")
			}
			seen[entry.Value] = true
			u, parseErr := url.Parse(entry.Value)
			if parseErr != nil {
				return nil, errors.New("installation source reference is malformed")
			}
			if u.Scheme == "" && u.Host == "" && !strings.Contains(entry.Value, "?") {
				continue
			}
			// Syntax validation is not acquisition permission. Only exact operator
			// bindings below authorize replacing an entry, and the receiver separately
			// enforces the dependency repository/directory allowlist when rendering.
			dependency, err := argocd.ParseSourceDependencyReference(entry.Value, field == "components")
			if err != nil {
				return nil, err
			}
			key := field + "\x00" + dependency.Repository + "\x00" + dependency.Path
			target := wanted[key]
			if target == nil {
				continue
			}
			if target.found {
				return nil, errors.New("installation Git-reference slot is ambiguous")
			}
			target.found = true
			// Replace only the literal ref segment, retaining query order and every
			// other byte of the independently validated qualified reference.
			segments := strings.Split(u.RawQuery, "&")
			for index, segment := range segments {
				if strings.HasPrefix(segment, "ref=") {
					segments[index] = "ref=" + target.commit
				}
			}
			u.RawQuery = strings.Join(segments, "&")
			entry.Value = u.String()
		}
	}
	for _, target := range wanted {
		if !target.found {
			return nil, errors.New("installation Git-reference binding does not name an existing exact slot")
		}
	}
	return writeOverlayKustomization(document)
}

var overlayEnvironmentAssignment = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=([A-Za-z0-9_./:@+\-]*)$`)
var overlayLiteralRef = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)

func pinInstallationEnvValue(before []byte, binding overlayEnvValue, publication pipelines.VerifiedPublishedRelease) ([]byte, error) {
	if len(before) == 0 || len(before) > 1<<20 || !utf8.Valid(before) || bytes.ContainsRune(before, 0) || binding.Key != "ENGINE_REF" {
		return nil, errors.New("installation product environment is invalid")
	}
	component, err := overlayPublishedComponent(publication, binding.Component)
	if err != nil {
		return nil, err
	}
	value := ""
	switch binding.Value {
	case "componentCommit":
		value = component.Commit
		if !installationCommit.MatchString(value) {
			return nil, errors.New("installation environment needs an immutable signed component revision")
		}
	case "componentVersion":
		value = "v" + component.Version // Release() has already validated the signed semantic version.
	default:
		return nil, errors.New("installation environment value codec is unsupported")
	}
	lines := strings.Split(string(before), "\n")
	found := false
	seen := map[string]bool{}
	for index, line := range lines {
		body := strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(body) == "" || strings.HasPrefix(strings.TrimSpace(body), "#") {
			continue
		}
		// This is a literal dotenv codec, never a shell parser. Refuse alternate
		// assignments or statements that could override the reviewed value when
		// a downstream tool reads product.env.
		assignment := overlayEnvironmentAssignment.FindStringSubmatch(body)
		if assignment == nil || seen[assignment[1]] {
			return nil, errors.New("product.env requires unique literal assignments without shell expressions")
		}
		seen[assignment[1]] = true
		if assignment[1] != "ENGINE_REF" {
			continue
		}
		if found || !overlayLiteralRef.MatchString(assignment[2]) {
			return nil, errors.New("product.env requires one literal ENGINE_REF assignment")
		}
		found = true
		ending := ""
		if strings.HasSuffix(line, "\r") {
			ending = "\r"
		}
		lines[index] = "ENGINE_REF=" + value + ending
	}
	if !found {
		return nil, errors.New("product.env has no explicit ENGINE_REF assignment")
	}
	return []byte(strings.Join(lines, "\n")), nil
}
