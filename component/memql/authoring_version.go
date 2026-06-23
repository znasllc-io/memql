package memql

// authoring_version.go -- edit / version semantics + impact-analysis
// re-validation for authored bundles (epic memql#954, issue #961, increment 4).
//
// A user edit to a Responsibility (or to a shared dependency) does NOT mutate an
// active bundle in place. It produces a NEW version that supersedes the prior
// one via supersedesBundleId; on activation the superseded bundle retires (the
// activation orchestrator in authoring_activation.go already handles that
// retire-on-supersede transition). This file adds the GUARDRAIL the design doc
// calls for: when the edit touches a SHARED / cataloged construct, every bundle
// that depends on it must be RE-VALIDATED through Gate 1 (isolated compile+bind)
// BEFORE the new version is allowed to go live -- so an edit that would break a
// dependent is caught up front instead of bricking it at runtime.
//
// Impact analysis uses dependentsOfConstruct (#957) to find the dependent
// edges, resolves each to its bundle, and re-runs SandboxCompileBundle (#956)
// over the dependent's constructs with the EDITED construct's new source
// overlaid. The pure decision (does every active dependent still compile?) is
// table-testable; the engine-backed driver loads the rows through a narrow
// store seam the tests fake.

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// VersionSupersession is the validated edit/version transition: a new bundle
// version replacing a prior one. Pure data -- computing it touches no engine.
type VersionSupersession struct {
	PriorBundleId string `json:"priorBundleId"`
	NewBundleId   string `json:"newBundleId"`
	PriorVersion  int    `json:"priorVersion"`
	NewVersion    int    `json:"newVersion"`
}

// PlanVersionSupersession validates that `next` is a well-formed new version of
// `prior`: it must point back at the prior bundle via supersedesBundleId, carry
// a strictly greater version (monotonic, mirroring the runtime registry's
// supersede rule), and share the prior's owner (no cross-owner supersession).
// Returns the transition or an error describing the violation.
func PlanVersionSupersession(prior, next AuthoringBundleRow) (VersionSupersession, error) {
	if next.SupersedesBundleId != prior.Id {
		return VersionSupersession{}, fmt.Errorf("authoring: new bundle %q must supersede prior %q (supersedesBundleId=%q)", next.Id, prior.Id, next.SupersedesBundleId)
	}
	if next.OwnerUserId != prior.OwnerUserId {
		return VersionSupersession{}, fmt.Errorf("authoring: version supersession cannot cross owners (%q -> %q)", prior.OwnerUserId, next.OwnerUserId)
	}
	if next.Version <= prior.Version {
		return VersionSupersession{}, fmt.Errorf("authoring: new version %d does not supersede prior version %d", next.Version, prior.Version)
	}
	return VersionSupersession{
		PriorBundleId: prior.Id,
		NewBundleId:   next.Id,
		PriorVersion:  prior.Version,
		NewVersion:    next.Version,
	}, nil
}

// EditedConstruct identifies the construct an edit changed plus its NEW source.
// Impact analysis re-validates every dependent against this new source.
type EditedConstruct struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	NewSource string `json:"newSource"`
}

// DependentRevalidation is the Gate-1 re-validation outcome for one bundle that
// depends on the edited construct: the bundle id + whether its closure still
// compiles with the edited construct's new source overlaid, plus any
// diagnostics so the caller can surface what broke.
type DependentRevalidation struct {
	BundleId    string              `json:"bundleId"`
	OK          bool                `json:"ok"`
	Diagnostics []SandboxDiagnostic `json:"diagnostics,omitempty"`
}

// ImpactAnalysisResult is the full impact-analysis verdict for an edit: every
// active dependent's re-validation outcome and the rollup AllPassed. The new
// version may go live ONLY when AllPassed is true.
type ImpactAnalysisResult struct {
	EditedConstruct EditedConstruct         `json:"editedConstruct"`
	Dependents      []DependentRevalidation `json:"dependents"`
	// AllPassed is true when every active dependent still compiles with the
	// edited construct overlaid (or there are no dependents). The gate.
	AllPassed bool `json:"allPassed"`
}

// revalidateDependent re-runs Gate 1 (SandboxCompileBundle) over a dependent
// bundle's constructs with the edited construct's NEW source substituted in
// place of the dependent's copy (matched by kind+name). This is the structural
// re-validation: does the dependent's closure still compile+bind against the
// changed shared construct? Pure -- it just compiles.
func revalidateDependent(bundleId string, dependentConstructs []SandboxConstruct, edited EditedConstruct) DependentRevalidation {
	overlaid := make([]SandboxConstruct, 0, len(dependentConstructs)+1)
	replaced := false
	for _, c := range dependentConstructs {
		if c.Kind == edited.Kind && c.Name == edited.Name {
			overlaid = append(overlaid, SandboxConstruct{Kind: edited.Kind, Name: edited.Name, Source: edited.NewSource})
			replaced = true
			continue
		}
		overlaid = append(overlaid, c)
	}
	// If the dependent composes the construct from the catalog (not a local
	// member), it isn't in the dependent's own construct list -- add the edited
	// construct so the closure binds against the NEW source.
	if !replaced {
		overlaid = append(overlaid, SandboxConstruct{Kind: edited.Kind, Name: edited.Name, Source: edited.NewSource})
	}
	rep := SandboxCompileBundle(overlaid)
	return DependentRevalidation{BundleId: bundleId, OK: rep.OK, Diagnostics: rep.Diagnostics}
}

// AnalyzeImpact is the pure impact-analysis core: given the edited construct and
// the per-dependent-bundle construct lists (keyed by bundleId), re-validate each
// dependent through Gate 1 with the edit overlaid and roll up AllPassed. The
// engine-backed AnalyzeConstructEdit loads the dependent bundles + constructs
// through the store and calls this. Deterministic -> table-testable.
func AnalyzeImpact(edited EditedConstruct, dependentConstructs map[string][]SandboxConstruct) ImpactAnalysisResult {
	res := ImpactAnalysisResult{EditedConstruct: edited, AllPassed: true}
	bundleIds := make([]string, 0, len(dependentConstructs))
	for id := range dependentConstructs {
		bundleIds = append(bundleIds, id)
	}
	sort.Strings(bundleIds) // deterministic output
	for _, id := range bundleIds {
		rv := revalidateDependent(id, dependentConstructs[id], edited)
		if !rv.OK {
			res.AllPassed = false
		}
		res.Dependents = append(res.Dependents, rv)
	}
	return res
}

// ImpactStore is the narrow graph surface impact analysis needs: find the active
// dependents of a construct (dependentsOfConstruct -> filtered to active
// bundles) and load each dependent bundle's constructs. *MemQLEngine satisfies
// it via engineImpactStore; tests fake it.
type ImpactStore interface {
	// ActiveDependentBundleIds returns the ids of ACTIVE bundles whose constructs
	// declare a dependency on (toKind, toName) -- the impact set to re-validate.
	ActiveDependentBundleIds(ctx context.Context, owner, toKind, toName string) ([]string, error)
	// ConstructsAsSandbox loads a bundle's member constructs as SandboxConstructs
	// for Gate-1 re-compilation.
	ConstructsAsSandbox(ctx context.Context, bundleId string) ([]SandboxConstruct, error)
}

// AnalyzeConstructEdit is the engine-backed impact analysis: find the edited
// construct's active dependents, load each one's closure, and re-validate them
// all through Gate 1 with the new source overlaid. The new version goes live
// only if the returned AllPassed is true -- the caller (the activation path)
// gates on it.
func (e *MemQLEngine) AnalyzeConstructEdit(ctx context.Context, owner string, edited EditedConstruct) (ImpactAnalysisResult, error) {
	return AnalyzeConstructEditWithStore(ctx, &engineImpactStore{engine: e}, owner, edited)
}

// AnalyzeConstructEditWithStore is the store-driven core: usable with a custom
// ImpactStore and fakeable in tests (no live DB).
func AnalyzeConstructEditWithStore(ctx context.Context, store ImpactStore, owner string, edited EditedConstruct) (ImpactAnalysisResult, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return ImpactAnalysisResult{}, fmt.Errorf("authoring: impact analysis requires an owner")
	}
	if edited.Name == "" || edited.Kind == "" {
		return ImpactAnalysisResult{}, fmt.Errorf("authoring: impact analysis requires the edited construct's kind + name")
	}

	bundleIds, err := store.ActiveDependentBundleIds(ctx, owner, edited.Kind, edited.Name)
	if err != nil {
		return ImpactAnalysisResult{}, fmt.Errorf("authoring: load dependents of %s/%s: %w", edited.Kind, edited.Name, err)
	}

	dependentConstructs := make(map[string][]SandboxConstruct, len(bundleIds))
	for _, id := range bundleIds {
		cs, err := store.ConstructsAsSandbox(ctx, id)
		if err != nil {
			return ImpactAnalysisResult{}, fmt.Errorf("authoring: load dependent bundle %q constructs: %w", id, err)
		}
		dependentConstructs[id] = cs
	}
	return AnalyzeImpact(edited, dependentConstructs), nil
}
