package dsl

// TestCrossDomainReferencesAreImported enforces the half of authoring rule 25
// (#2617) that was documented but never checked: "`use` stays required (and
// lint-enforced) cross-domain."
//
// Same-domain constructs are ambient and must NOT be imported -- that is the
// sibling gate, TestNoSameDomainUse. The asymmetry is deliberate and it is
// about information, not consistency: by the flattened one-file-per-kind
// layout, a same-domain name can only come from one place, so the import
// carries nothing. Nothing about `isActiveRecord` tells you it lives in
// `common`, so there the import is the only thing that says so.
//
// Fix a failure by adding the named `use` line at the top of the file.
//
// SCOPE. This checks the references whose bare spelling is genuinely a
// construct reference rather than data:
//
//   - a trait or spec invoked in a `filter` clause
//   - a shape named in a `shape` projection clause
//
// It deliberately does NOT check bare identifiers that resolve to a CONCEPT.
// A bare name in a filter clause is a payload field (`filter surface!=""` in
// safety/queries.memql reads the payload field `surface`, not the
// v1:actions:surface concept), and 10 construct names double as payload field
// names across the tree. Requiring imports there would be wrong, not merely
// noisy.
//
// RUNTIME-DELIVERED CONSTRUCTS. A reference to something declared nowhere in
// this tree is skipped rather than flagged. A product bundle mounts extra
// domains at boot via MEMQL_DSL_PATH (dsl/cognition/logic.memql calls
// mutationCreateCanvasState, which lives in no file here), and you cannot
// import what does not exist at lint time. The rule is therefore "if the tree
// can see it in another domain, name it" -- never "every reference must
// resolve here".

import (
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/dslfs"
)

var (
	crossDomainDeclRe   = regexp.MustCompile(`(?m)^(trait|spec|shape)\s+([A-Za-z_][\w.-]*)(?:\s+([A-Za-z_][\w.-]*))?\s*\{`)
	crossDomainUseRe    = regexp.MustCompile(`(?m)^use\s+([\w.]+)\.\{([^}]*)\}`)
	crossDomainFilterRe = regexp.MustCompile(`(?m)^\s*filter\s+(.+)$`)
	crossDomainShapeRe  = regexp.MustCompile(`(?m)^\s*shape\s+([A-Za-z_]\w*)\s*$`)
	crossDomainIdentRe  = regexp.MustCompile(`\b([A-Za-z_]\w*)\b`)
	crossDomainStringRe = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
)

// crossDomainDecl is where a referenceable construct is declared.
type crossDomainDecl struct {
	domain string
	module string // the file's construct kind, e.g. "traits" -> use <domain>.traits.{ ... }
}

func TestCrossDomainReferencesAreImported(t *testing.T) {
	tree := Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}

	sources := make(map[string]string, len(paths))
	for _, p := range paths {
		file, openErr := tree.Open(p)
		if openErr != nil {
			t.Fatalf("open %s: %v", p, openErr)
		}
		raw, readErr := io.ReadAll(file)
		file.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}
		sources[p] = string(raw)
	}

	// Index every trait / spec / shape by the domain that declares it. A name
	// declared in several domains is kept as a set: if the referencing file's
	// own domain is among them the reference is ambient, not cross-domain.
	decls := map[string][]crossDomainDecl{}
	for p, raw := range sources {
		domain := crossDomainOf(p)
		if domain == "" {
			continue
		}
		module := strings.TrimSuffix(path.Base(p), ".memql")
		body := languageParser.BlankComments(raw)
		for _, m := range crossDomainDeclRe.FindAllStringSubmatch(body, -1) {
			name := m[3]
			if name == "" {
				name = m[2]
			}
			decls[name] = append(decls[name], crossDomainDecl{domain: domain, module: module})
		}
	}

	type finding struct{ file, name, want string }
	var findings []finding

	for _, p := range paths {
		domain := crossDomainOf(p)
		if domain == "" {
			continue
		}
		raw := sources[p]
		imported := crossDomainImports(raw)
		body := crossDomainStringRe.ReplaceAllString(languageParser.BlankComments(raw), `""`)

		refs := map[string]bool{}
		for _, m := range crossDomainFilterRe.FindAllStringSubmatch(body, -1) {
			for _, id := range crossDomainIdentRe.FindAllStringSubmatch(m[1], -1) {
				refs[id[1]] = true
			}
		}
		for _, m := range crossDomainShapeRe.FindAllStringSubmatch(body, -1) {
			refs[m[1]] = true
		}

		names := make([]string, 0, len(refs))
		for name := range refs {
			names = append(names, name)
		}
		sort.Strings(names)

		for _, name := range names {
			if imported[name] {
				continue
			}
			homes := decls[name]
			if len(homes) == 0 {
				continue // declared nowhere here -- runtime-delivered, unimportable
			}
			ambient := false
			for _, h := range homes {
				if h.domain == domain {
					ambient = true
					break
				}
			}
			if ambient {
				continue
			}
			h := homes[0]
			findings = append(findings, finding{
				file: p,
				name: name,
				want: fmt.Sprintf("use %s.%s.{ %s }", h.domain, h.module, name),
			})
		}
	}

	if len(findings) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "%d cross-domain reference(s) without a file-top `use` import (authoring rule 25: `use` stays required cross-domain):\n", len(findings))
		for _, f := range findings {
			fmt.Fprintf(&b, "  %s: %s -- add `%s`\n", f.file, f.name, f.want)
		}
		t.Error(b.String())
	}
}

// crossDomainOf returns the domain directory a tree path belongs to, or ""
// for a path that is not inside one.
func crossDomainOf(p string) string {
	dir := path.Dir(p)
	if dir == "." || dir == "/" {
		return ""
	}
	// Nested layouts (agents/roles/legal.memql) still belong to their
	// top-level domain.
	parts := strings.Split(dir, "/")
	if parts[0] == "" {
		return ""
	}
	return parts[0]
}

// crossDomainImports returns the set of names the file pulls into local scope.
func crossDomainImports(raw string) map[string]bool {
	out := map[string]bool{}
	for _, m := range crossDomainUseRe.FindAllStringSubmatch(raw, -1) {
		for _, n := range strings.Split(m[2], ",") {
			if n = strings.TrimSpace(n); n != "" {
				out[n] = true
			}
		}
	}
	return out
}
