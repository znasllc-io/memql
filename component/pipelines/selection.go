package pipelines

// ChangedPathFacts is the mechanical view of one path in a repository diff.
// The pipeline DSL decides whether these facts require a full run; Go only
// reports what the import graph and manifest matcher can prove.
type ChangedPathFacts struct {
	Path           string `json:"path"`
	BaseName       string `json:"baseName"`
	RepositoryPath bool   `json:"repositoryPath"`
	Vendored       bool   `json:"vendored"`
	ConfiguredFull bool   `json:"configuredFull"`
}

// SelectionFacts are observations from the source tree, not a selection.
// In particular, they do not contain the policy answer to run every package
// or only the affected packages.
type SelectionFacts struct {
	Known         bool               `json:"known"`
	ChangedCount  int                `json:"changedCount"`
	GraphComplete bool               `json:"graphComplete"`
	Paths         []ChangedPathFacts `json:"paths"`
	Incomplete    []string           `json:"incomplete"`
	AllPackages   []string           `json:"allPackages"`
	Seeds         []string           `json:"seeds"`
	Missing       []string           `json:"missing"`
}

// SelectionDecision is the answer from the pinned pipeline DSL. Full means
// select every package; otherwise ResolveSelection follows the import graph
// from the facts' seeds.
type SelectionDecision struct {
	Full   bool   `json:"full"`
	Reason string `json:"reason"`
}

// PackagePolicy is the per-step package coverage and filter decision returned
// by the pinned pipeline DSL. Go applies it to the import graph mechanically.
type PackagePolicy struct {
	Coverage string `json:"coverage"`
	Filter   string `json:"filter"`
}

const (
	PackageCoverageAll      = "all"
	PackageCoverageAffected = "affected"

	PackageFilterAll        = "all"
	PackageFilterDBGated    = "db-gated"
	PackageFilterNotDBGated = "not-db-gated"
)

// BucketSelection is the set of manifest buckets whose guarded steps should
// run, selected by the pinned pipeline DSL from path-match facts.
type BucketSelection struct {
	Included []string
}

// Selection is what a change selects among a repository's Go packages (D1,
// D8). Full selects every package: the change could not be read, touched
// something everything depends on, or the graph is incomplete.
type Selection struct {
	Full bool
	// Reason says why Full, or how many packages the change seeded.
	Reason string
	// Seeds are the packages a changed path belongs to directly, and the
	// importers of a package the change deleted or moved away: a changed
	// path in a directory that holds no package, which they still import.
	Seeds []string
	// Packages are the selected import paths, sorted; every package when Full.
	Packages []string
}

// Selector supplies package candidates and repository directories from the
// source graph. The pinned DSL's PackagePolicy decides which candidates are
// used; GraphSelector implements this mechanical view and tests may fake it.
type Selector interface {
	// All is every package, as sorted import paths.
	All() []string
	// Affected is this run's selection; Full means All().
	Affected() Selection
	// DirOf is a package's repository-relative directory, "" when unknown.
	DirOf(importPath string) string
}
