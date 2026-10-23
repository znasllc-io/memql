package installation

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strings"
	"time"

	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/argocd"
)

// Requirements are addresses of native, persisted captures. They contain no
// proof and cannot add a repository, commit or acquisition option to a scope.
type sourceRequirement struct {
	Key        string `json:"key"`
	Repository string `json:"repository"`
	Revision   string `json:"revision"`
	Path       string `json:"path"`
	Component  bool   `json:"component"`
}

func dependencyCaptureKey(parent string, dependency argocd.SourceDependency) string {
	body, _ := json.Marshal(dependency)
	return "dependency-" + string(id.NewUntracked().FromString("installation-source-dependency-v1:"+parent+":"+string(body)))
}
func fragmentDependency(spec argocd.SourceFragmentSpec) argocd.SourceDependency {
	var source struct{ RepoURL, TargetRevision, Path string }
	_ = json.Unmarshal(spec.Render.Source, &source)
	return argocd.SourceDependency{Repository: source.RepoURL, Revision: source.TargetRevision, Path: source.Path, Component: spec.Component}
}
func dependencyCaptureSpec(root sourceCaptureSpec, key string, fragment argocd.SourceFragmentSpec) (sourceCaptureSpec, error) {
	if root.Fragment == nil || root.Fragment.Dependency || !fragment.Dependency {
		return sourceCaptureSpec{}, errors.New("dependency capture requires its native root")
	}
	// Everything except the exact verified source coordinates is inherited from
	// the root. A dependency collector never receives the root's GitHub token.
	// Argo rendering separately uses the root's repository-scoped read token;
	// its qualified credential helper is inherited by Kustomize child fetches.
	expected := *root.Fragment
	expected.Render = root.Render
	var source map[string]any
	if json.Unmarshal(root.Render.Source, &source) != nil {
		return sourceCaptureSpec{}, errors.New("root source is malformed")
	}
	dependency := fragmentDependency(fragment)
	source["repoURL"], source["targetRevision"], source["path"] = dependency.Repository, dependency.Revision, dependency.Path
	expected.Render.Source, _ = json.Marshal(source)
	expected.Dependency, expected.Component = true, dependency.Component
	if !sameJSON(expected, fragment) {
		return sourceCaptureSpec{}, errors.New("dependency changes native render configuration")
	}
	spec := root
	u, err := url.Parse(dependency.Repository)
	if err != nil {
		return sourceCaptureSpec{}, err
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) != 2 {
		return sourceCaptureSpec{}, errors.New("dependency repository is malformed")
	}
	spec.Repository = pl.Repository{Owner: parts[0], Name: strings.TrimSuffix(parts[1], ".git"), CloneURL: dependency.Repository}
	spec.CloneInstallationID, spec.StepKey = 0, "source-"+key
	spec.Render, spec.Fragment = fragment.Render, &fragment
	if _, err = newSourceCapture(spec); err != nil {
		return sourceCaptureSpec{}, err
	}
	return spec, nil
}

// preparationCaptureSpec is also used by retirement: every dynamically admitted
// Job and private artifact belongs to the same native lifecycle as its root.
func preparationCaptureSpec(r preparationRecord, key string) (sourceCaptureSpec, error) {
	entry, exists := r.Captures[key]
	if !exists {
		return sourceCaptureSpec{}, errors.New("source capture is outside this preparation")
	}
	if spec, root := r.Scope.Captures[key]; root {
		if entry.Parent != "" || entry.Spec != nil {
			return sourceCaptureSpec{}, errors.New("root capture has dependency metadata")
		}
		return spec, nil
	}
	root, exists := r.Scope.Captures[entry.Parent]
	if !exists || entry.Spec == nil || entry.Spec.Fragment == nil || entry.Parent == key {
		return sourceCaptureSpec{}, errors.New("dependency capture has no native parent")
	}
	parent := r.Captures[entry.Parent]
	if !internalDigest.MatchString(parent.FragmentDigest) || parent.Receipt == nil {
		return sourceCaptureSpec{}, errors.New("dependency capture has no verified root")
	}
	dependency := fragmentDependency(*entry.Spec.Fragment)
	if key != dependencyCaptureKey(entry.Parent, dependency) {
		return sourceCaptureSpec{}, errors.New("dependency capture identity changed")
	}
	expected, err := dependencyCaptureSpec(root, key, *entry.Spec.Fragment)
	if err != nil || !sameJSON(expected, *entry.Spec) {
		return sourceCaptureSpec{}, errors.New("dependency acquisition differs from its native parent")
	}
	return expected, nil
}

func (j *preparationJournal) sourceRequirements(ctx context.Context, installation, key, workflow, root string, files sourceCaptureFiles) ([]sourceRequirement, error) {
	proof, err := j.verifyCapture(ctx, installation, key, workflow, root, files)
	if err != nil {
		return nil, err
	}
	if proof.fragment.Digest() == "" {
		if proof.source.Digest() == "" {
			return nil, errors.New("source requirements need a verified root")
		}
		return []sourceRequirement{}, nil
	}
	if proof.fragment.Spec().Dependency {
		return nil, errors.New("dependency fragments cannot declare another hop")
	}
	requirements := []sourceRequirement{}
	_, err = j.withRecord(ctx, installation, key, workflow, func(r *preparationRecord) error {
		rootSpec, exists := r.Scope.Captures[root]
		entry := r.Captures[root]
		if !exists || entry.FragmentDigest != proof.fragment.Digest() || entry.ReceiptDigest != preparationReceiptDigest(proof.receipt) {
			return errChanged
		}
		expected := map[string]bool{}
		for _, dependency := range proof.fragment.Dependencies() {
			captureKey := dependencyCaptureKey(root, dependency)
			fragment, e := proof.fragment.DependencySpec(dependency)
			if e != nil {
				return e
			}
			spec, e := dependencyCaptureSpec(rootSpec, captureKey, fragment)
			if e != nil {
				return e
			}
			if existing, found := r.Captures[captureKey]; found {
				if existing.Parent != root || existing.Spec == nil || !sameJSON(*existing.Spec, spec) {
					return errors.New("source dependency was already reserved with different inputs")
				}
			} else {
				r.Captures[captureKey] = preparationCapture{Parent: root, Spec: &spec}
			}
			expected[captureKey] = true
			requirements = append(requirements, sourceRequirement{Key: captureKey, Repository: dependency.Repository, Revision: dependency.Revision, Path: dependency.Path, Component: dependency.Component})
		}
		for captureKey, capture := range r.Captures {
			if capture.Parent == root && !expected[captureKey] {
				return errors.New("source graph contains an extra retained dependency")
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(requirements, func(i, j int) bool { return requirements[i].Key < requirements[j].Key })
	return requirements, nil
}

// Reopen every archive on this replica; journal digests never create a proof.
// Fragment verification can finish independently, but only this operation can
// bind a composed root SourceDigest for preview, promotion or acknowledgement.
func (j *preparationJournal) verifySourceGraph(ctx context.Context, installation, key, workflow, root string, files sourceCaptureFiles) (verifiedSourceCapture, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	requirements, err := j.sourceRequirements(ctx, installation, key, workflow, root, files)
	if err != nil {
		return verifiedSourceCapture{}, err
	}
	proof, err := j.verifyCapture(ctx, installation, key, workflow, root, files)
	if err != nil || proof.fragment.Digest() == "" {
		return proof, err
	}
	dependencies := make([]argocd.SourceFragment, 0, len(requirements))
	for _, requirement := range requirements {
		dependency, err := j.verifyCapture(ctx, installation, key, workflow, requirement.Key, files)
		if err != nil {
			return verifiedSourceCapture{}, err
		}
		dependencies = append(dependencies, dependency.fragment)
	}
	closed, err := argocd.AssembleSourceClosure(ctx, proof.fragment, dependencies)
	if err != nil {
		return verifiedSourceCapture{}, err
	}
	_, err = j.withRecord(ctx, installation, key, workflow, func(r *preparationRecord) error {
		entry := r.Captures[root]
		if entry.FragmentDigest != proof.fragment.Digest() || entry.ReceiptDigest != preparationReceiptDigest(proof.receipt) {
			return errChanged
		}
		for i, requirement := range requirements {
			if r.Captures[requirement.Key].FragmentDigest != dependencies[i].Digest() {
				return errChanged
			}
		}
		if entry.SourceDigest != "" && entry.SourceDigest != closed.Digest() {
			return errors.New("assembled source evidence changed")
		}
		entry.SourceDigest = closed.Digest()
		r.Captures[root] = entry
		return nil
	})
	if err != nil {
		return verifiedSourceCapture{}, err
	}
	proof.source, proof.fragment = closed, argocd.SourceFragment{}
	return proof, nil
}
