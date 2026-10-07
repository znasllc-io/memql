package release

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path"
	"regexp"
	"slices"
	"strings"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

// Version sources are declared operator configuration. A submitted candidate
// selects a component identity, never a credential name, URL or file to read.
// The complete rule belongs in the preparer's workflow identity.
type candidateVersionSource struct {
	Component        string `json:"component"`
	Repository       string `json:"repository"`
	Path             string `json:"path"`
	JSONField        string `json:"jsonField,omitempty"` // empty means a plain version file
	CredentialSecret string `json:"credentialSecret,omitempty"`
}

var candidateVersionPath = regexp.MustCompile(`^[A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)*$`)
var candidateSecretName = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
var candidateJSONField = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)

func (s candidateVersionSource) validate() error {
	if s.Component == "" || len(s.Component) > 128 || len(s.Repository) > 257 {
		return errors.New("version source requires bounded component and repository identities")
	}
	if _, ok := parseRepo(s.Repository); !ok {
		return errors.New("invalid version source repository")
	}
	if !candidateVersionPath.MatchString(s.Path) || len(s.Path) > 512 || path.Clean(s.Path) != s.Path || s.Path == "." || s.Path == ".." || strings.HasPrefix(s.Path, "../") {
		return errors.New("version source must name a canonical repository file")
	}
	if s.JSONField != "" && !candidateJSONField.MatchString(s.JSONField) {
		return errors.New("version JSON field must be an explicit top-level name")
	}
	if s.CredentialSecret != "" && !candidateSecretName.MatchString(s.CredentialSecret) {
		return errors.New("invalid version source credential reference")
	}
	return nil
}

type candidateVersionReader struct {
	client   *Client
	resolver resolver
	sources  map[string]candidateVersionSource
}

func (r candidateVersionReader) freezeFor(components []pl.ReleaseComponent) (candidateVersionReader, []candidateVersionSource, error) {
	if len(components) == 0 || len(components) > 64 {
		return candidateVersionReader{}, nil, errors.New("version sources require bounded components")
	}
	owned := r
	owned.sources = map[string]candidateVersionSource{}
	rules := []candidateVersionSource{}
	for _, component := range components {
		source, ok := r.sources[component.Name]
		if !ok || source.Component != component.Name || source.Repository != component.Repository || owned.sources[component.Name].Component != "" {
			return candidateVersionReader{}, nil, errors.New("component has no unique matching configured version source")
		}
		if err := source.validate(); err != nil {
			return candidateVersionReader{}, nil, err
		}
		owned.sources[component.Name] = source
		rules = append(rules, source)
	}
	slices.SortFunc(rules, func(a, b candidateVersionSource) int { return strings.Compare(a.Component, b.Component) })
	return owned, rules, nil
}

func (r candidateVersionReader) check(ctx context.Context, component pl.ReleaseComponent) error {
	if _, err := candidateOwner(ctx); err != nil {
		return err
	}
	source, ok := r.sources[component.Name]
	if !ok || source.Component != component.Name || source.Repository != component.Repository {
		return errors.New("component has no matching configured version source")
	}
	if err := source.validate(); err != nil {
		return err
	}
	if r.client == nil {
		return errors.New("source version reader is unavailable")
	}
	// The candidate structure also checks this, but the bounded native reader
	// never accepts a moving branch or tag even when invoked independently.
	if !candidateSourceCommit(component.Commit) {
		return errors.New("version read requires an exact source commit")
	}
	repo, _ := parseRepo(source.Repository)
	token := ""
	if source.CredentialSecret != "" {
		if r.resolver.systemSecret == nil {
			return errors.New("version source credential resolver is unavailable")
		}
		var err error
		token, err = r.resolver.systemSecret(ctx, source.CredentialSecret)
		if err != nil || strings.TrimSpace(token) == "" {
			return errors.New("version source credential is unavailable")
		}
	} else if r.resolver.appToken != nil {
		var err error
		token, err = r.resolver.appToken(ctx, repo)
		if err != nil {
			return errors.New("version source installation credential is unavailable")
		}
	}
	// A public repository can be read anonymously. There is deliberately no
	// plaintext-variable or environment fallback for a configured secret.
	raw, _, err := r.client.GetFile(ctx, token, repo, source.Path, component.Commit)
	if err != nil {
		return err
	}
	version, err := candidateFileVersion(raw, source.JSONField)
	if err != nil {
		return err
	}
	if version != component.Version {
		return errors.New("source version does not match the release candidate")
	}
	return nil
}

func candidateSourceCommit(value string) bool {
	return (len(value) == 40 || len(value) == 64) && strings.IndexFunc(value, func(ch rune) bool { return !(ch >= '0' && ch <= '9') && !(ch >= 'a' && ch <= 'f') }) < 0
}

func candidateFileVersion(raw, field string) (string, error) {
	if len(raw) == 0 || len(raw) > 256<<10 {
		return "", errors.New("version file is empty or exceeds its bound")
	}
	if field == "" {
		return strings.TrimSpace(raw), nil
	}
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return "", errors.New("version JSON must be one object")
	}
	seen := map[string]bool{}
	version := ""
	for decoder.More() {
		key, err := decoder.Token()
		name, ok := key.(string)
		if err != nil || !ok || seen[name] {
			return "", errors.New("version JSON contains an invalid or duplicate property")
		}
		seen[name] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return "", errors.New("version JSON could not be read")
		}
		if name == field && json.Unmarshal(value, &version) != nil {
			return "", errors.New("version property must be a string")
		}
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') {
		return "", errors.New("version JSON is incomplete")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return "", errors.New("version JSON contains trailing data")
	}
	if version == "" {
		return "", errors.New("version JSON has no configured version")
	}
	return version, nil
}
