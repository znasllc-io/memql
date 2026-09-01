package packages

import (
	"io/fs"
	"log/slog"
	"path"
	"sort"
	"strings"

	languageAst "github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/memql/dslimports"
)

// DslRoot is where DSL domains are discovered, mirroring MEMQL_DSL_PATH's own
// layout exactly (D2). Discovered, never declared: a dsl/<domain>/ directory
// already carries everything the engine needs to know about it, which is what
// makes the manifest's deployables list the only thing an author maintains.
const DslRoot = "dsl"

// GoPackDir is the one place a Go pack lives in a memql-project tree (D3).
const GoPackDir = "bff"

// Options tunes an analysis pass.
type Options struct {
	// SourceVersion is the commit SHA or content hash the snapshot is, echoed
	// into the report. Analysis never derives it: the fetch stage knows what
	// it fetched, and a hash computed here would be a second answer to a
	// question that already has one.
	SourceVersion string
	// Limits bounds the tree. Zero fields take their defaults.
	Limits Limits
	// Logger receives the DSL gate's own diagnostics. Optional.
	Logger *slog.Logger
}

// Analyze decides whether tree is a deployable package, and what deploying it
// would do.
//
// It ALWAYS returns a report, and returns an error only for the first FATAL
// problem. That pairing is deliberate: the pipeline lands the report on the
// deployment row so the OS can show every problem at once, and stamps the
// error's code as the row's typed refusal so a caller keying on one thing has
// one thing to key on. A caller that only wants the verdict reads err; a
// caller rendering the confirm gate reads the report.
//
// Nothing here writes, fetches, or reaches a cluster. That is D12: "this DSL
// would refuse boot" is an answer produced here, offline, before a pod is ever
// asked to run it.
func Analyze(tree fs.FS, opts Options) (*Report, error) {
	rep := &Report{
		SourceVersion: opts.SourceVersion,
		OK:            true,
		Deployables:   []DeployableReport{},
		DslDomains:    []DslDomainReport{},
		Problems:      []Problem{},
	}

	manifest, err := ReadManifest(tree)
	if err != nil {
		r := err.(*Refusal)
		rep.add(problemFrom(r, true))
		// No manifest means no package, and every rule below is a rule about
		// what the manifest declared. Reporting the Go pack or the DSL domains
		// of a tree that is not a package would describe something that cannot
		// deploy as though it nearly could.
		return rep, r
	}
	rep.Name = manifest.Name
	rep.FormatVersion = manifest.FormatVersion

	analyzeDeployables(tree, manifest, rep)
	analyzeGoPacks(tree, rep)
	analyzeDSL(tree, rep, opts.Logger)

	if p := rep.FirstFatal(); p != nil {
		return rep, &Refusal{Code: p.Code, Detail: p.Message, Scope: p.Scope}
	}
	return rep, nil
}

// analyzeDeployables validates every DECLARED deployable and works out its
// build plan.
func analyzeDeployables(tree fs.FS, manifest *Manifest, rep *Report) {
	for _, d := range manifest.Deployables {
		command, output := d.BuildPlanFor()
		dr := DeployableReport{
			Name:    d.Name,
			Kind:    d.Kind,
			Path:    d.Path,
			Command: command,
			Output:  output,
			Binding: d.Binding,
		}

		switch {
		case !ValidKind(d.Kind):
			dr.Problem = ptr(problemFrom(refuseScoped(CodeDeployableKindUnknown, d.Name,
				"deployable %q declares kind %q. This cluster serves %s, %s and %s.",
				d.Name, d.Kind, KindSPA, KindStatic, KindStorefront), true))

		case !isDirIn(tree, d.Path):
			dr.Problem = ptr(problemFrom(refuseScoped(CodeDeployablePathMissing, d.Name,
				"deployable %q points at %q, which is not a directory in this source.",
				d.Name, d.Path), true))

		case d.Kind == KindStorefront && !hasBinding(d.Binding):
			dr.Problem = ptr(problemFrom(refuseScoped(CodeDeployableBindingMissing, d.Name,
				"deployable %q is a %s but declares no binding. A storefront needs storeDomain and storefrontTokenRef -- the token itself stays in a cluster secret and is named, never written here.",
				d.Name, KindStorefront), true))
		}

		if dr.Problem == nil {
			// The D4 fast-path. Checked for index.html specifically rather
			// than for a non-empty directory: the publisher refuses an
			// index-less bundle anyway, so skipping the build on a `dist/`
			// holding leftovers would trade a build failure a person can read
			// for a publish failure three stages later that says less.
			if hasFileIn(tree, path.Join(d.Path, output, "index.html")) {
				dr.Prebuilt = true
				dr.BuildPlan = "prebuilt output found -- build skipped"
			} else {
				dr.BuildPlan = command + " (output: " + output + ")"
			}
		} else {
			dr.BuildPlan = "not analyzed -- see the problem on this deployable"
			rep.add(*dr.Problem)
		}

		rep.Deployables = append(rep.Deployables, dr)
	}
}

// analyzeGoPacks records a Go pack and defers it (D3).
//
// Reported rather than ignored, and non-fatal rather than refusing: an author
// whose bff/ silently did not deploy would conclude the platform ran it. The
// per-half refusal says exactly which half was left out and where Go delivery
// happens today, and the rest of the package deploys around it.
func analyzeGoPacks(tree fs.FS, rep *Report) {
	if !hasFileIn(tree, path.Join(GoPackDir, "go.mod")) {
		return
	}
	note := "reported, not deployable through this path -- Go packs ship as engine images built by CI, and full Go delivery through packages is a later epic. Every other half of this package deploys."
	rep.GoPacks = append(rep.GoPacks, GoPackReport{
		Path:   GoPackDir,
		Module: goModulePath(tree, path.Join(GoPackDir, "go.mod")),
		Note:   note,
	})
	rep.add(problemFrom(refuseScoped(CodeGoPackNotDeployable, GoPackDir, "%s", note), false))
}

// analyzeDSL discovers the package's DSL domains, counts what they hold, and
// runs the Init-grade gates strict boot runs (D12).
func analyzeDSL(tree fs.FS, rep *Report, logger *slog.Logger) {
	dslTree, err := fs.Sub(tree, DslRoot)
	if err != nil {
		return // no dsl/ -- an SPAs-only package, which is ordinary (D6)
	}
	if _, err := fs.ReadDir(dslTree, "."); err != nil {
		return
	}

	counts := countConstructs(dslTree)

	result, gateErr := memql.AnalyzePackageDSL(logger, dslTree)
	if gateErr != nil {
		rep.add(problemFrom(refuse(CodeDslRefusesBoot,
			"this cluster could not validate the package's DSL: %v", gateErr), true))
		return
	}

	reserved := make(map[string]struct{}, len(result.SkippedCore))
	for _, d := range result.SkippedCore {
		reserved[d] = struct{}{}
		rep.add(problemFrom(refuseScoped(CodeDslDomainReserved, d,
			"this package ships a DSL domain named %q, which is a name the engine already owns. The engine's own domain wins at boot and the package's would never load, so it is refused here instead of disappearing silently. Rename the domain.",
			d), true))
	}

	domains := make([]string, 0, len(result.Mounted)+len(result.SkippedCore))
	domains = append(domains, result.Mounted...)
	domains = append(domains, result.SkippedCore...)
	sort.Strings(domains)
	for _, d := range domains {
		_, isReserved := reserved[d]
		dr := DslDomainReport{Domain: d, Reserved: isReserved}
		if !isReserved {
			if c, ok := counts[d]; ok {
				dr.Constructs = c.byKind
				dr.Files = c.files
			}
		}
		if dr.Constructs == nil {
			dr.Constructs = map[string]int{}
		}
		rep.DslDomains = append(rep.DslDomains, dr)
	}

	if len(result.Diagnostics) > 0 {
		var b strings.Builder
		b.WriteString("this package's DSL would refuse boot. ")
		b.WriteString(plural(len(result.Diagnostics), "problem", "problems"))
		b.WriteString(":")
		for _, d := range result.Diagnostics {
			b.WriteString("\n  - ")
			if d.File != "" {
				b.WriteString(d.File)
				b.WriteString(": ")
			}
			b.WriteString(d.Message)
		}
		rep.add(problemFrom(refuse(CodeDslRefusesBoot, "%s", b.String()), true))
	}
}

// ---------------------------------------------------------------------------
// Construct counting
// ---------------------------------------------------------------------------

type domainCounts struct {
	byKind map[string]int
	files  int
}

// countConstructs counts declarations per domain, from the PARSED tree rather
// than from a source scan.
//
// The count is a summary for a person deciding whether to deploy -- "this adds
// 14 concepts and 30 queries" is the answer a file count is not -- so it is
// best-effort by design: a file the parser cannot read contributes nothing
// here and is separately a hard diagnostic from the gate above, which is where
// a parse failure belongs.
func countConstructs(dslTree fs.FS) map[string]domainCounts {
	out := map[string]domainCounts{}
	tree, _ := dslimports.Load(dslTree)
	if tree == nil {
		return out
	}
	for p, file := range tree.Files {
		if file == nil {
			continue
		}
		domain, _, ok := strings.Cut(path.Clean(p), "/")
		if !ok || domain == "" {
			continue
		}
		dc, seen := out[domain]
		if !seen {
			dc = domainCounts{byKind: map[string]int{}}
		}
		dc.files++
		for _, def := range file.Definitions {
			if kind := constructKind(def); kind != "" {
				dc.byKind[kind]++
			}
		}
		out[domain] = dc
	}
	return out
}

// constructKind names the DSL construct a parsed definition is, or "" for a
// node that is not a declaration (an import, a stray expression).
func constructKind(def languageAst.Node) string {
	switch d := def.(type) {
	case *languageAst.ConceptDecl:
		return "concept"
	case *languageAst.ShapeDecl:
		return "shape"
	case *languageAst.SpecDecl:
		return "spec"
	case *languageAst.BuiltinDecl:
		return "builtin"
	case *languageAst.PromptDecl:
		return "prompt"
	case *languageAst.ProviderDecl:
		return "provider"
	case *languageAst.ToolDecl:
		return "tool"
	case *languageAst.PolicyDecl:
		return "policy"
	case *languageAst.SeedDecl:
		return "seed"
	case *languageAst.ActionDecl:
		return "action"
	case *languageAst.CapabilityDecl:
		return "capability"
	case *languageAst.AutomationDef:
		return "automation"
	case *languageAst.FunctionDef:
		// One AST node covers query / mutation / logic / spec / tool /
		// automation, so the kind is the field, not the type.
		if d.Type != "" {
			return string(d.Type)
		}
		return "function"
	}
	return ""
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

func isDirIn(tree fs.FS, p string) bool {
	clean := strings.TrimSpace(p)
	if clean == "" || !fs.ValidPath(path.Clean(clean)) {
		return false
	}
	info, err := fs.Stat(tree, path.Clean(clean))
	return err == nil && info.IsDir()
}

func hasFileIn(tree fs.FS, p string) bool {
	clean := path.Clean(strings.TrimSpace(p))
	if clean == "" || !fs.ValidPath(clean) {
		return false
	}
	info, err := fs.Stat(tree, clean)
	return err == nil && !info.IsDir()
}

func hasBinding(b *ManifestBinding) bool {
	return b != nil &&
		strings.TrimSpace(b.StoreDomain) != "" &&
		strings.TrimSpace(b.StorefrontTokenRef) != ""
}

// goModulePath reads the module line out of a go.mod, for the report only.
func goModulePath(tree fs.FS, p string) string {
	raw, err := fs.ReadFile(tree, p)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "module "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return itoa(n) + " " + many
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func ptr[T any](v T) *T { return &v }
