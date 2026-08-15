package dslgate

import (
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"strings"

	"github.com/znasllc-io/memql/core/dslfs"
)

// imports.go -- the cross-namespace import gate (memql#3803).
//
// # What `use` meant, and why that was two things
//
// `use` had two jobs and only one of them was enforced:
//
//	kind      registry                     does `use` affect resolution?
//	concept   namespaced, v1:<ns>:<name>   YES -- it is the disambiguator
//	the 12    flat, keyed by bare name     NO  -- resolves without it
//
// For twelve kinds the import was documentation that nothing checked. A
// cross-domain reference to `statusIsActive` (which lives in common.traits)
// validated from a bundle with NO import at all and no domain.
//
// That conflation is not a tidiness problem. memql#2617 banned same-domain
// imports on the premise that they are "pure ceremony" -- TRUE of the twelve,
// FALSE of the one -- and the rule was written across all thirteen. The two
// bugs that followed both trace to it: #3800 (the authoring path could not do
// ambient resolution) and #3802 (a foreign import silently captured every bare
// use of a name).
//
// # The decision
//
// ENFORCE it. A cross-namespace reference must be imported, for every kind, with
// same-namespace ambient as the uniform exception. The import block becomes the
// file's dependency list rather than a comment about it.
//
// The measured cost was ONE import line. Counting only at RESOLUTION SITES
// (a call, a `shape` clause, a bare filter conjunct), with comments AND string
// literals stripped, the tree was already 113/114 compliant by habit -- authors
// have been writing the imports the engine never asked for. The single holdout
// is `builtin cognitionTrackPresence(...)` in dsl/cognition/automations.memql,
// a file that carries no `use` block at all.
//
// # What this gate does NOT do
//
// It does not resolve. It reports a reference that names a construct another
// namespace declares with no import naming it, which is the contract; binding
// stays where binding lives. And it is deliberately CONSERVATIVE about what
// counts as a reference -- see referenceSites -- because a false positive here
// refuses a boot.

// GateCrossNamespaceImport -- a construct references another namespace's
// construct with no `use` naming it.
const GateCrossNamespaceImport Gate = "cross-namespace-import"

// flatKinds are the twelve kinds whose registries are keyed by bare name. They
// are the kinds this gate is FOR: concepts already enforce their own imports
// through signature resolution, and reporting them here would double-report a
// refusal the resolver states better.
var flatKinds = map[string]bool{
	"query": true, "mutation": true, "logic": true, "spec": true,
	"trait": true, "shape": true, "tool": true, "prompt": true,
	"provider": true, "builtin": true, "policy": true, "seed": true,
}

// declLineRe matches a TOP-LEVEL construct declaration. Unindented on purpose:
// `^\s*` also matches a concept FIELD, and `provider enum("heygen", ...)` --
// a field named `provider` whose type is an enum -- then registers a bogus
// `provider enum`, after which every `@enum(...)` annotation in the tree reads
// as a cross-namespace provider call. That mistake cost 45 phantom findings.
var declLineRe = regexp.MustCompile(`(?m)^(query|mutate|mutation|logic|spec|trait|shape|tool|prompt|provider|builtin|policy|seed|concept|automation|action|capability)\s+([A-Za-z_][A-Za-z0-9_]*)(?:\s+([A-Za-z_][A-Za-z0-9_]*))?\s*[{(]`)

var useLineRe = regexp.MustCompile(`(?m)^\s*use\s+([a-zA-Z0-9_.]+)\.\{([^}]*)\}`)

// callRe matches a call, excluding an ANNOTATION (`@enum(...)`) and a member
// call (`x.y(...)`) -- neither names a construct.
var callRe = regexp.MustCompile(`(^|[^@\w.])([a-z][A-Za-z0-9_]*)\s*\(`)

var shapeClauseRe = regexp.MustCompile(`(?m)^\s*shape\s+([a-z][A-Za-z0-9_]*)\s*$`)
var filterLineRe = regexp.MustCompile(`(?m)^\s*filter\s+(.+)$`)
var identRe = regexp.MustCompile(`\b([a-z][A-Za-z0-9_]*)\b`)
var lineCommentRe = regexp.MustCompile(`(?m)//.*$`)
var stringLitRe = regexp.MustCompile(`"(\\.|[^"\\])*"`)

// codeOnly strips string literals and then comments, so neither prose nor a
// quoted example can be read as a reference.
//
// ORDER IS LOad-BEARING, and getting it backwards is a real defect rather than
// a style point: stripping comments FIRST removes a `//` that lives inside a
// string -- a URL in a @description -- which eats that string's closing quote
// and desynchronises every string match after it in the file. Measured: it made
// a sentence in dsl/knowledge/concepts.memql read as a call to the builtin it
// was describing.
func codeOnly(src string) string {
	return lineCommentRe.ReplaceAllString(stringLitRe.ReplaceAllString(src, `""`), "")
}

// namespaceOf is a file's namespace: its containing directory within the tree.
//
// EACH DIRECTORY IS A NAMESPACE, including a nested one -- dsl/agents/roles/ is
// `agents/roles`, not `agents`. That is the authoring model the corpus already
// follows: every seed file under agents/roles/ carries
// `use agents.concepts.{ agentRole }` for a concept in its PARENT directory,
// which is an import nothing required and which would be redundant if the
// subdirectory were part of the same namespace.
func namespaceOf(file string) string {
	dir := path.Dir(file)
	if dir == "." || dir == "/" {
		return ""
	}
	return dir
}

type declaredAt struct {
	kind      string
	namespace string
	file      string
}

// scanCrossNamespaceImports is a TREE-level gate: whether a reference crosses a
// namespace boundary cannot be answered from one file, because it depends on
// where the referenced name is declared.
func scanCrossNamespaceImports(tree fs.FS) ([]Violation, error) {
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		return nil, fmt.Errorf("walking DSL tree: %w", err)
	}

	code := make(map[string]string, len(paths))
	for _, p := range paths {
		f, openErr := tree.Open(p)
		if openErr != nil {
			return nil, fmt.Errorf("open %s: %w", p, openErr)
		}
		raw, readErr := io.ReadAll(f)
		f.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read %s: %w", p, readErr)
		}
		code[p] = codeOnly(string(raw))
	}

	// Pass 1: where each construct name is declared. The S5 uniqueness gate
	// (#2360) means a flat name cannot legitimately be declared twice, so first
	// wins and a duplicate is that gate's problem rather than this one's.
	declared := map[string]declaredAt{}
	for _, p := range paths {
		for _, m := range declLineRe.FindAllStringSubmatch(code[p], -1) {
			kind := m[1]
			if kind == "mutate" {
				kind = "mutation"
			}
			name := m[2]
			if m[3] != "" {
				name = m[3] // two-identifier signature: `query <Concept> <name>`
			}
			if _, seen := declared[name]; !seen {
				declared[name] = declaredAt{kind: kind, namespace: namespaceOf(p), file: p}
			}
		}
	}

	var out []Violation
	for _, p := range paths {
		src := code[p]
		ns := namespaceOf(p)

		imported := map[string]bool{}
		for _, m := range useLineRe.FindAllStringSubmatch(src, -1) {
			for _, n := range strings.Split(m[2], ",") {
				n = strings.TrimSpace(n)
				// `<source> as <local>`: the SOURCE name is what authorizes the
				// reference (memql#3802). Both spellings are accepted here --
				// the gate asks whether the dependency is declared, not which
				// name it was bound to.
				if i := strings.Index(n, " as "); i >= 0 {
					local := strings.TrimSpace(n[i+4:])
					n = strings.TrimSpace(n[:i])
					if local != "" {
						imported[local] = true
					}
				}
				if n != "" {
					imported[n] = true
				}
			}
		}
		declaredHere := map[string]bool{}
		for _, m := range declLineRe.FindAllStringSubmatch(src, -1) {
			name := m[2]
			if m[3] != "" {
				name = m[3]
			}
			declaredHere[name] = true
		}

		reported := map[string]bool{}
		report := func(name string) {
			d, known := declared[name]
			switch {
			case !known, !flatKinds[d.kind], d.namespace == ns, declaredHere[name], imported[name], reported[name]:
				return
			}
			reported[name] = true
			out = append(out, Violation{
				Gate: GateCrossNamespaceImport,
				File: p,
				Kind: d.kind,
				Detail: fmt.Sprintf("references %s %q, which namespace %q declares, with no import naming it -- add `use %s.%ss.{ %s }` (memql#3803)",
					d.kind, name, d.namespace, strings.ReplaceAll(d.namespace, "/", "."), d.kind, name),
			})
		}

		for _, m := range callRe.FindAllStringSubmatch(src, -1) {
			report(m[2])
		}
		for _, m := range shapeClauseRe.FindAllStringSubmatch(src, -1) {
			report(m[1])
		}
		for _, m := range filterLineRe.FindAllStringSubmatch(src, -1) {
			for _, id := range identRe.FindAllStringSubmatch(m[1], -1) {
				// A bare conjunct is a spec or a trait; any other bare
				// identifier on a filter line is a payload property.
				if d, ok := declared[id[1]]; ok && (d.kind == "spec" || d.kind == "trait") {
					report(id[1])
				}
			}
		}
	}
	return out, nil
}
