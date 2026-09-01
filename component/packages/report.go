package packages

// The analysis report (design section E).
//
// It is FIRST-CLASS DATA, not a log: it lands on packageDeployment.report, it
// is what the OS renders at the always-present confirm gate (D12), and it is
// the record of what a past deploy decided. So every field is JSON-tagged for
// the row, and the shape is written for a reader who is about to answer "yes,
// deploy this" rather than for a debugger.

// Report is what an analysis pass learned about one package source.
type Report struct {
	// Name is the manifest's name. Empty when the manifest could not be read
	// at all -- which is a report worth keeping, because it carries the
	// problem that explains why.
	Name string `json:"name,omitempty"`
	// FormatVersion echoes the manifest's declared version.
	FormatVersion int `json:"formatVersion,omitempty"`
	// SourceVersion is the commit SHA or content hash this snapshot is.
	SourceVersion string `json:"sourceVersion,omitempty"`

	// Deployables is every declared web surface and what deploying it would
	// do. Present even for a deployable that carries a problem, because
	// "storefront: path missing" is more useful beside the others than
	// instead of them.
	Deployables []DeployableReport `json:"deployables"`

	// DslDomains is every DISCOVERED dsl/<domain>/, with construct counts.
	DslDomains []DslDomainReport `json:"dslDomains"`

	// GoPacks is every bff/ with a go.mod (D3): reported so nobody wonders
	// where their Go went, and deferred so the rest of the package deploys.
	GoPacks []GoPackReport `json:"goPacks,omitempty"`

	// Problems is every problem found, fatal or not, in the order the
	// analysis reached them.
	Problems []Problem `json:"problems"`

	// OK reports whether this package can deploy: no FATAL problem. It is
	// stored rather than derived at read time so a report rendered from an
	// old row cannot disagree with the decision that row recorded.
	OK bool `json:"ok"`
}

// DeployableReport is one declared surface's analysis.
type DeployableReport struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	Path string `json:"path"`

	// BuildPlan is the sentence the confirm gate shows. Either the command
	// that will run, or the D4 fast-path's own answer -- "prebuilt output
	// found -- build skipped".
	BuildPlan string `json:"buildPlan"`
	// Command and Output are the machine-readable half of the same fact; the
	// build stage reads these, the person reads BuildPlan.
	Command string `json:"command,omitempty"`
	Output  string `json:"output"`
	// Prebuilt is the D4 fast-path: the output directory already carries a
	// built tree in the snapshot, so the build stage skips this deployable.
	Prebuilt bool `json:"prebuilt"`

	// Binding is the storefront's connection, carried through so the confirm
	// gate can show which store this deploys against. It names a secret; it
	// never carries one.
	Binding *ManifestBinding `json:"binding,omitempty"`

	// Problem, when set, is this deployable's own refusal. The package as a
	// whole is refused too -- a declared deployable that cannot deploy is not
	// a partial success -- but the problem is recorded here as well so the OS
	// can render it against the row it is about.
	Problem *Problem `json:"problem,omitempty"`
}

// DslDomainReport is one discovered DSL domain and what it holds.
type DslDomainReport struct {
	Domain string `json:"domain"`
	// Constructs counts by construct kind (concept, query, mutation, ...),
	// which is the answer to "what does deploying this actually add" that a
	// file count is not.
	Constructs map[string]int `json:"constructs"`
	// Files is how many .memql files carry them.
	Files int `json:"files"`
	// Reserved marks a domain that collides with a core engine namespace.
	// Its constructs are NOT counted -- nothing read them -- and its problem
	// is dsl_domain_reserved.
	Reserved bool `json:"reserved,omitempty"`
}

// GoPackReport is a detected, deferred Go pack (D3).
type GoPackReport struct {
	Path   string `json:"path"`
	Module string `json:"module,omitempty"`
	// Note is the sentence the report shows: what this is and where Go
	// delivery actually happens today.
	Note string `json:"note"`
}

// Problem is one thing the analysis found.
type Problem struct {
	// Code is a catalogued refusal code (refusal.go). Stable and
	// machine-readable; the OS keys its own sentence on it.
	Code string `json:"code"`
	// Message is the server's sentence, which the OS renders VERBATIM when it
	// has no copy of its own for the code.
	Message string `json:"message"`
	// Scope names the half this is about -- a deployable name, a DSL domain,
	// a path -- and is empty for a package-wide problem.
	Scope string `json:"scope,omitempty"`
	// Fatal reports whether this problem blocks the deploy. A Go pack is the
	// designed non-fatal case (D3): it is reported, the rest deploys.
	Fatal bool `json:"fatal"`
}

// problemFrom lifts a Refusal into a report Problem.
func problemFrom(r *Refusal, fatal bool) Problem {
	return Problem{Code: r.Code, Message: r.Detail, Scope: r.Scope, Fatal: fatal}
}

// add records a problem and keeps OK in step with it. Every problem goes
// through here so "OK" can never drift from the list it summarizes.
func (rep *Report) add(p Problem) {
	rep.Problems = append(rep.Problems, p)
	if p.Fatal {
		rep.OK = false
	}
}

// FirstFatal returns the first fatal problem, or nil. It is what the pipeline
// stamps as the deployment row's typed error.
func (rep *Report) FirstFatal() *Problem {
	for i := range rep.Problems {
		if rep.Problems[i].Fatal {
			return &rep.Problems[i]
		}
	}
	return nil
}
