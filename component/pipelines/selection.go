package pipelines

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

// Selector is the Go selection a compile consults. GraphSelector implements
// it over a graph read from source; a test may fake it.
type Selector interface {
	// All is every package, as sorted import paths.
	All() []string
	// Affected is this run's selection; Full means All().
	Affected() Selection
	// DirOf is a package's repository-relative directory, "" when unknown.
	DirOf(importPath string) string
}
