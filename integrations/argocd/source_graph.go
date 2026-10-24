package argocd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/core/id"
)

// A root may import only these exact entry directories. Local dependencies of
// each entry still resolve inside that entry's own verified commit tree.
// This is native installation configuration, never an authority from the repo.
type SourceRepository struct {
	Repository string   `json:"repository"`
	Paths      []string `json:"paths"`
}

const MaxSourceDependencies = 16

// SourceFragmentSpec binds one acquisition. Dependency captures cannot contain
// another remote edge. Component distinguishes the Kustomize entry-point kind.
type SourceFragmentSpec struct {
	Render              RenderSpec         `json:"render"`
	AllowedDependencies []SourceRepository `json:"allowedDependencies"`
	Dependency          bool               `json:"dependency"`
	Component           bool               `json:"component"`
}

type SourceDependency struct {
	Repository string `json:"repository"`
	Revision   string `json:"revision"`
	Path       string `json:"path"`
	Component  bool   `json:"component"`
}

type SourceEdge struct {
	FromPath  string           `json:"fromPath"`
	Field     string           `json:"field"`
	Reference string           `json:"reference"`
	Target    SourceDependency `json:"target"`
}

// SourceFragment is verified local input, not a complete source proof. All its
// fields are private; only graph assembly can turn fragments into ClosedSource.
type SourceFragment struct {
	spec   []byte
	inputs sourceInputs
	digest string
}

func (s SourceFragment) Digest() string      { return s.digest }
func (s SourceFragment) Files() []SourceFile { return append([]SourceFile(nil), s.inputs.files...) }
func (s SourceFragment) Spec() SourceFragmentSpec {
	var out SourceFragmentSpec
	_ = json.Unmarshal(s.spec, &out)
	return out
}
func (s SourceFragment) String() string {
	return fmt.Sprintf("ArgoCD source fragment files=%d digest=%s", len(s.inputs.files), s.digest)
}
func (s SourceFragment) GoString() string { return s.String() }
func (s SourceFragment) Dependencies() []SourceDependency {
	unique := map[SourceDependency]bool{}
	for _, edge := range s.inputs.edges {
		unique[edge.Target] = true
	}
	result := make([]SourceDependency, 0, len(unique))
	for dependency := range unique {
		result = append(result, dependency)
	}
	sort.Slice(result, func(i, j int) bool { return dependencyKey(result[i]) < dependencyKey(result[j]) })
	return result
}

var githubSourcePath = regexp.MustCompile(`^/[A-Za-z0-9][A-Za-z0-9_.-]{0,127}/[A-Za-z0-9][A-Za-z0-9_.-]{0,127}\.git$`)

func canonicalGitHubSource(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host == "github.com" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && u.RawPath == "" && githubSourcePath.MatchString(u.Path) && u.String() == raw
}
func canonicalSourceDirectory(value string) bool {
	p, err := sourcePath(".", value)
	return err == nil && p == value && path.Clean(value) == value
}

func ValidateSourceRepositories(repositories []SourceRepository) error {
	if len(repositories) > MaxSourceDependencies {
		return errors.New("source dependency repository allowlist exceeds its bound")
	}
	seen := map[string]bool{}
	total := 0
	for _, repository := range repositories {
		if !canonicalGitHubSource(repository.Repository) || seen[repository.Repository] || len(repository.Paths) == 0 {
			return errors.New("source dependencies require unique canonical GitHub repositories and exact directories")
		}
		seen[repository.Repository] = true
		paths := map[string]bool{}
		for _, p := range repository.Paths {
			total++
			if total > MaxSourceDependencies || !canonicalSourceDirectory(p) || paths[p] || len(p) > 1024 {
				return errors.New("source dependency directories are invalid, duplicate or excessive")
			}
			paths[p] = true
		}
	}
	return nil
}
func sourceAdmitted(repository, directory string, allowed []SourceRepository) bool {
	for _, entry := range allowed {
		if entry.Repository == repository {
			for _, p := range entry.Paths {
				if p == directory {
					return true
				}
			}
		}
	}
	return false
}

// Only the existing qualified URL form is understood. In particular query
// decoding must not erase duplicate keys or disguise a mutable/alternate ref.
// ParseSourceDependency validates syntax and the native repository/path allowlist.
func ParseSourceDependency(reference string, component bool, allowed []SourceRepository) (SourceDependency, error) {
	if err := ValidateSourceRepositories(allowed); err != nil {
		return SourceDependency{}, err
	}
	dependency, err := ParseSourceDependencyReference(reference, component)
	if err != nil {
		return SourceDependency{}, err
	}
	if !sourceAdmitted(dependency.Repository, dependency.Path, allowed) {
		return SourceDependency{}, errors.New("remote Kustomize source is outside the native allowlist")
	}
	return dependency, nil
}
func parseSourceDependency(reference string, component bool, allowed []SourceRepository) (SourceDependency, error) {
	return ParseSourceDependency(reference, component, allowed)
}

// ParseSourceDependencyReference is a syntax codec, not acquisition authority.
// Acquisition must additionally pass ParseSourceDependency with native bindings.
func ParseSourceDependencyReference(reference string, component bool) (SourceDependency, error) {
	fail := func() (SourceDependency, error) {
		return SourceDependency{}, errors.New("remote Kustomize source requires an allowed GitHub directory and exact commit")
	}
	u, err := url.Parse(reference)
	// Kustomize's Azure marker is a substring, not a path segment, and takes
	// precedence over // even when it appears inside the intended directory.
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.Fragment != "" || u.RawPath != "" || strings.Contains(reference, "%") || strings.Contains(u.Path, "_git/") {
		return fail()
	}
	repo, directory, ok := strings.Cut(strings.TrimPrefix(u.Path, "/"), "//")
	if !ok || !canonicalSourceDirectory(directory) || len(directory) > 1024 {
		return fail()
	}
	repository := "https://github.com/" + strings.TrimSuffix(repo, ".git") + ".git"
	if !canonicalGitHubSource(repository) {
		return fail()
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(query) != 3 {
		return fail()
	}
	for _, key := range []string{"ref", "timeout", "submodules"} {
		if len(query[key]) != 1 {
			return fail()
		}
	}
	if !commitSHA.MatchString(query.Get("ref")) || query.Get("timeout") != "120s" || query.Get("submodules") != "false" {
		return fail()
	}
	return SourceDependency{Repository: repository, Revision: query.Get("ref"), Path: directory, Component: component}, nil
}

func DecodeSourceFragmentSpec(body []byte) (SourceFragmentSpec, error) {
	if len(body) > 512<<10 {
		return SourceFragmentSpec{}, errors.New("source fragment specification exceeds its bound")
	}
	fields, err := decodeObject(body)
	if err != nil {
		return SourceFragmentSpec{}, err
	}
	for key := range fields {
		if key != "render" && key != "allowedDependencies" && key != "dependency" && key != "component" {
			return SourceFragmentSpec{}, errors.New("source fragment specification has unsupported fields")
		}
	}
	var spec SourceFragmentSpec
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&spec) != nil {
		return SourceFragmentSpec{}, errors.New("source fragment specification is invalid")
	}
	// The recursive strict object codec catches duplicates, while a canonical
	// key comparison rejects encoding/json's case-insensitive field matching.
	canonical, _ := json.Marshal(spec)
	var canonicalObject map[string]any
	_ = json.Unmarshal(canonical, &canonicalObject)
	if !sameSourceKeys(fields, canonicalObject) {
		return SourceFragmentSpec{}, errors.New("source fragment specification has noncanonical fields")
	}
	renderBody, _ := json.Marshal(fields["render"])
	spec.Render, err = DecodeRenderSpec(renderBody)
	if err != nil {
		return SourceFragmentSpec{}, err
	}
	if err = ValidateSourceRepositories(spec.AllowedDependencies); err != nil {
		return SourceFragmentSpec{}, err
	}
	var source map[string]any
	_ = json.Unmarshal(spec.Render.Source, &source)
	repository, directory := stringAt(source, "repoURL"), stringAt(source, "path")
	if !canonicalGitHubSource(repository) || (spec.Dependency && !sourceAdmitted(repository, directory, spec.AllowedDependencies)) || (!spec.Dependency && spec.Component) {
		return SourceFragmentSpec{}, errors.New("source fragment has an unapproved entry point")
	}
	return spec, nil
}
func sameSourceKeys(value, canonical any) bool {
	switch v := value.(type) {
	case map[string]any:
		c, ok := canonical.(map[string]any)
		if !ok {
			return false
		}
		for key, child := range v {
			expected, ok := c[key]
			if !ok || !sameSourceKeys(child, expected) {
				return false
			}
		}
	case []any:
		c, ok := canonical.([]any)
		if !ok || len(v) != len(c) {
			return false
		}
		for n := range v {
			if !sameSourceKeys(v[n], c[n]) {
				return false
			}
		}
	}
	return true
}

func verifySourceFragment(ctx context.Context, objects GitObjects, spec SourceFragmentSpec, verified func(string, string, []byte) error) (SourceFragment, error) {
	body, err := json.Marshal(spec)
	if err != nil {
		return SourceFragment{}, errors.New("source fragment specification could not be encoded")
	}
	spec, err = DecodeSourceFragmentSpec(body)
	if err != nil {
		return SourceFragment{}, err
	}
	inputs, err := verifySourceInputs(ctx, objects, spec.Render, verified, &spec)
	if err != nil {
		return SourceFragment{}, err
	}
	evidence, err := json.Marshal(struct {
		Spec  json.RawMessage `json:"spec"`
		Files []SourceFile    `json:"files"`
		Edges []SourceEdge    `json:"edges"`
	}{body, inputs.files, inputs.edges})
	if err != nil {
		return SourceFragment{}, err
	}
	return SourceFragment{spec: body, inputs: inputs, digest: "memql-id:" + string(id.NewUntracked().FromString("memql-argocd-source-fragment-v1\n"+string(evidence)))}, nil
}

// DependencySpec can only select an edge actually verified in this root.
func (s SourceFragment) DependencySpec(dependency SourceDependency) (SourceFragmentSpec, error) {
	if s.digest == "" || s.Spec().Dependency {
		return SourceFragmentSpec{}, errors.New("dependency capture requires a verified root fragment")
	}
	found := false
	for _, candidate := range s.Dependencies() {
		found = found || candidate == dependency
	}
	if !found {
		return SourceFragmentSpec{}, errors.New("dependency is not part of the verified root")
	}
	spec := s.Spec()
	var source map[string]any
	_ = json.Unmarshal(spec.Render.Source, &source)
	source["repoURL"], source["targetRevision"], source["path"] = dependency.Repository, dependency.Revision, dependency.Path
	spec.Render.Source, _ = json.Marshal(source)
	spec.Dependency, spec.Component = true, dependency.Component
	return spec, nil
}
func dependencyKey(d SourceDependency) string { body, _ := json.Marshal(d); return string(body) }
func fragmentIdentity(s SourceFragment) SourceDependency {
	spec := s.Spec()
	var source map[string]any
	_ = json.Unmarshal(spec.Render.Source, &source)
	return SourceDependency{Repository: stringAt(source, "repoURL"), Revision: stringAt(source, "targetRevision"), Path: stringAt(source, "path"), Component: spec.Component}
}

// AssembleSourceClosure consumes opaque, independently reverified fragments.
// Missing, repeated and extra captures refuse. Aggregate bounds include all
// captures (even repeated shared objects) and never reset at repository edges.
func AssembleSourceClosure(ctx context.Context, root SourceFragment, dependencies []SourceFragment) (ClosedSource, error) {
	if err := ctx.Err(); err != nil {
		return ClosedSource{}, err
	}
	if root.digest == "" || root.Spec().Dependency || len(dependencies) > MaxSourceDependencies {
		return ClosedSource{}, errors.New("source graph requires a verified root and bounded dependencies")
	}
	expected := root.Dependencies()
	if len(expected) != len(dependencies) {
		return ClosedSource{}, errors.New("source graph has missing or extra dependency captures")
	}
	byKey := map[SourceDependency]SourceFragment{}
	for _, fragment := range dependencies {
		if fragment.digest == "" || !fragment.Spec().Dependency || len(fragment.inputs.edges) != 0 {
			return ClosedSource{}, errors.New("source graph contains an unverified or recursive dependency")
		}
		key := fragmentIdentity(fragment)
		if _, duplicate := byKey[key]; duplicate {
			return ClosedSource{}, errors.New("source graph contains a duplicate dependency")
		}
		byKey[key] = fragment
	}
	ordered := []SourceFragment{root}
	for _, dependency := range expected {
		fragment, ok := byKey[dependency]
		if !ok {
			return ClosedSource{}, errors.New("source graph dependency identity changed")
		}
		spec, err := root.DependencySpec(dependency)
		if err != nil {
			return ClosedSource{}, err
		}
		encoded, _ := json.Marshal(spec)
		if !bytes.Equal(encoded, fragment.spec) {
			return ClosedSource{}, errors.New("source graph dependency configuration changed")
		}
		ordered = append(ordered, fragment)
	}
	files := []SourceFile{}
	totalBytes, totalObjects, totalSettings := 0, 0, 0
	fragments := []string{}
	for _, fragment := range ordered {
		if err := ctx.Err(); err != nil {
			return ClosedSource{}, err
		}
		totalBytes += fragment.inputs.bytes
		totalObjects += fragment.inputs.objects
		totalSettings += fragment.inputs.settings
		if totalBytes > maxSourceBytes || totalObjects > maxSourceCount || totalSettings > 256 || len(files)+len(fragment.inputs.files) > maxSourceCount {
			return ClosedSource{}, errors.New("source graph exceeds its aggregate input bound")
		}
		identity := fragmentIdentity(fragment)
		for _, file := range fragment.inputs.files {
			file.Repository, file.Revision = identity.Repository, identity.Revision
			files = append(files, file)
		}
		fragments = append(fragments, fragment.digest)
	}
	sort.Slice(files, func(i, j int) bool {
		a, b := files[i], files[j]
		if a.Repository != b.Repository {
			return a.Repository < b.Repository
		}
		if a.Revision != b.Revision {
			return a.Revision < b.Revision
		}
		return a.Path < b.Path
	})
	// Multiple admitted roots can consume the same file at the same commit.
	dedup := files[:0]
	for _, file := range files {
		if len(dedup) > 0 && dedup[len(dedup)-1] == file {
			continue
		}
		dedup = append(dedup, file)
	}
	files = dedup
	body, _ := json.Marshal(struct {
		Spec      json.RawMessage `json:"spec"`
		Files     []SourceFile    `json:"files"`
		Fragments []string        `json:"fragments"`
		Edges     []SourceEdge    `json:"edges"`
	}{root.inputs.spec, files, fragments, root.inputs.edges})
	return ClosedSource{spec: append([]byte(nil), root.inputs.spec...), files: files, digest: "memql-id:" + string(id.NewUntracked().FromString("memql-argocd-closed-source-v2\n"+string(body)))}, nil
}

func CaptureSourceFragmentArchive(ctx context.Context, objects GitObjects, spec SourceFragmentSpec, output io.Writer) (SourceFragment, error) {
	var fragment SourceFragment
	err := writeSourceArchive(ctx, output, func(verified func(string, string, []byte) error) error {
		var err error
		fragment, err = verifySourceFragment(ctx, objects, spec, verified)
		return err
	})
	if err != nil {
		return SourceFragment{}, err
	}
	return fragment, nil
}
func VerifySourceFragmentArchive(ctx context.Context, archive []byte, spec SourceFragmentSpec) (SourceFragment, error) {
	objects, err := readSourceArchive(ctx, archive)
	if err != nil {
		return SourceFragment{}, err
	}
	fragment, err := verifySourceFragment(ctx, objects, spec, nil)
	if err != nil {
		return SourceFragment{}, err
	}
	if len(objects.used) != len(objects.objects) {
		return SourceFragment{}, errors.New("Git source archive contains objects outside the verified input closure")
	}
	return fragment, nil
}
