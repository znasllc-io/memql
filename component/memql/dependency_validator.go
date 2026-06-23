package memql

// dependency_validator.go -- the DSL dependency-tree validator (grammar
// redesign epic #2031, C3/#2043).
//
// It REPLACES the retired naming-prefix convention (C2/#2042) with a
// structural correctness check that runs at engine load time. The old
// rule said "a query must be named query*, a spec spec*"; that was a
// naming crutch for resolution. The new model resolves a reference by
// its SLOT keyword (the kind) and its enclosing concept (the scope):
//
//   - inside `query <Concept> name { ... shape <ref> }` the `shape`
//     slot resolves `<ref>` to a shape OF `<Concept>`.
//   - `include <ref>` inside a shape resolves to another shape of the
//     SAME concept.
//
// A cross-concept reference is allowed ONLY when written explicitly as
// `concept.name` (dotted). An UNqualified reference that resolves to a
// construct bound to a different concept is a HARD load-time error with
// an actionable message (it names the offending ref, its slot, the
// enclosing concept, the concept the ref actually belongs to, and the
// fix).
//
// The validator scans the on-disk DSL source (not the post-rewrite
// registries) so it can read each construct's signature concept, which
// the runtime Function/Shape registries flatten away. It is pure +
// fs.FS-driven so it unit-tests against synthetic trees.

import (
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/memql/dslfs"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// depShape captures a shape's concept binding + its `include` refs.
type depShape struct {
	name     string
	concept  string // bare signature concept; "" for an @actor-only shape (universal)
	file     string
	line     int
	includes []depRef
}

// depConsumer is a query/mutation that references a shape via its
// `shape` slot.
type depConsumer struct {
	kind     string // "query" | "mutation"
	name     string
	concept  string // bare signature concept
	shapeRef string // raw text in the shape slot (may be dotted `concept.name`)
	file     string
	line     int
}

// depRef pairs a reference token with its source position.
type depRef struct {
	ref  string
	line int
}

var (
	depShapeRowRe    = regexp.MustCompile(`^\s*shape\s+([A-Za-z_]\w*)\s+([A-Za-z_]\w*)\s*\{`)
	depShapeActorRe  = regexp.MustCompile(`^\s*shape\s+([A-Za-z_]\w*)\s*\{`)
	depConsumerRe    = regexp.MustCompile(`^\s*(query|mutate)\s+([A-Za-z_]\w*)\s+([A-Za-z_]\w*)\s*\{`)
	depShapeClauseRe = regexp.MustCompile(`^\s*shape\s+([A-Za-z_][\w.]*)\s*$`)
	depIncludeRe     = regexp.MustCompile(`^\s*include\s+([A-Za-z_][\w.]*)\s*$`)
)

// depSharedShapeExempt mirrors dsl/conformance_test.go's
// sharedShapeExempt: a shape deliberately REUSED across two sibling
// concepts with an identical field layout. The consumer's scan concept
// is correct; only the projection borrows a shape whose signature names
// the sibling. This is not a cross-concept-leak bug.
var depSharedShapeExempt = map[string]string{
	"globalVariable":  "variableFull",
	"globalVariables": "variableFull",
}

// ValidateDependencyTree runs the dependency-tree validator over the
// embedded DSL tree. It is the load-time entry point wired into engine
// bootstrap; a violation is a hard error that fails engine init.
func ValidateDependencyTree() error {
	return validateDependencyTree(memqldsl.Tree())
}

// validateDependencyTree is the fs.FS-driven core, split out so unit
// tests can drive it against a synthetic tree.
func validateDependencyTree(tree fs.FS) error {
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		return fmt.Errorf("dependency validator: walk DSL tree: %w", err)
	}

	shapes := map[string]*depShape{} // shape name -> info (last decl wins, mirrors registry upsert)
	var shapeList []*depShape
	var consumers []depConsumer

	for _, p := range paths {
		if strings.HasPrefix(p, "_reference/") {
			continue
		}
		raw, rErr := readTreeFile(tree, p)
		if rErr != nil {
			return fmt.Errorf("dependency validator: read %s: %w", p, rErr)
		}
		collectShapes(p, raw, shapes, &shapeList)
		collectConsumers(p, raw, &consumers)
	}

	var violations []string

	// Slot check 1: a query/mutation `shape <ref>` must resolve to a
	// shape of the enclosing concept (or be an @actor-only shape, or be
	// qualified `concept.name`).
	for _, c := range consumers {
		if msg := checkShapeSlot(c, shapes); msg != "" {
			violations = append(violations, msg)
		}
	}

	// Slot check 2: a shape `include <ref>` must resolve to a shape of
	// the SAME concept (or be qualified `concept.name`).
	for _, s := range shapeList {
		for _, inc := range s.includes {
			if msg := checkInclude(s, inc, shapes); msg != "" {
				violations = append(violations, msg)
			}
		}
	}

	if len(violations) == 0 {
		return nil
	}
	sort.Strings(violations)
	return fmt.Errorf("DSL dependency-tree validation failed (%d violation(s)):\n  %s",
		len(violations), strings.Join(violations, "\n  "))
}

// collectShapes scans one file for shape declarations + their `include`
// refs, upserting into the shapes map (last decl wins) + appending to
// the ordered list.
func collectShapes(path, src string, shapes map[string]*depShape, list *[]*depShape) {
	var cur *depShape
	depth := 0
	for i, raw := range strings.Split(src, "\n") {
		line := stripDepLineComment(raw)
		lineno := i + 1

		if cur != nil {
			// Accumulate include refs + track brace depth to know when the
			// shape body closes.
			if m := depIncludeRe.FindStringSubmatch(line); m != nil {
				cur.includes = append(cur.includes, depRef{ref: m[1], line: lineno})
			}
			depth += strings.Count(line, "{") - strings.Count(line, "}")
			if depth <= 0 {
				cur = nil
				depth = 0
			}
			continue
		}

		if m := depShapeRowRe.FindStringSubmatch(line); m != nil {
			s := &depShape{name: m[2], concept: m[1], file: path, line: lineno}
			shapes[s.name] = s
			*list = append(*list, s)
			cur = s
			depth = strings.Count(line, "{") - strings.Count(line, "}")
			if depth <= 0 {
				cur = nil
				depth = 0
			}
			continue
		}
		if m := depShapeActorRe.FindStringSubmatch(line); m != nil {
			// @actor-only shape: single identifier, no signature concept.
			s := &depShape{name: m[1], concept: "", file: path, line: lineno}
			shapes[s.name] = s
			*list = append(*list, s)
			cur = s
			depth = strings.Count(line, "{") - strings.Count(line, "}")
			if depth <= 0 {
				cur = nil
				depth = 0
			}
			continue
		}
	}
}

// collectConsumers scans one file for query/mutation declarations and
// records the shape ref in each one's `shape` slot.
func collectConsumers(path, src string, out *[]depConsumer) {
	var cur *depConsumer
	depth := 0
	for i, raw := range strings.Split(src, "\n") {
		line := stripDepLineComment(raw)
		lineno := i + 1

		if cur != nil {
			if m := depShapeClauseRe.FindStringSubmatch(line); m != nil {
				cur.shapeRef = m[1]
			}
			depth += strings.Count(line, "{") - strings.Count(line, "}")
			if depth <= 0 {
				if cur.shapeRef != "" {
					*out = append(*out, *cur)
				}
				cur = nil
				depth = 0
			}
			continue
		}

		if m := depConsumerRe.FindStringSubmatch(line); m != nil {
			c := &depConsumer{kind: m[1], concept: m[2], name: m[3], file: path, line: lineno}
			cur = c
			depth = strings.Count(line, "{") - strings.Count(line, "}")
			if depth <= 0 {
				cur = nil
				depth = 0
			}
		}
	}
}

// checkShapeSlot validates a single consumer's `shape <ref>` slot.
func checkShapeSlot(c depConsumer, shapes map[string]*depShape) string {
	ref := c.shapeRef
	enclosing := fmt.Sprintf("%s %q (concept %q)", c.kind, c.name, c.concept)

	// Explicit cross-concept reference: `concept.name`.
	if qConcept, bare, ok := splitDottedRef(ref); ok {
		s, known := shapes[bare]
		if !known {
			return fmt.Sprintf("%s:%d: %s references shape %q in its `shape` slot, but no shape named %q is defined anywhere in the tree",
				c.file, c.line, enclosing, ref, bare)
		}
		if s.concept != "" && s.concept != qConcept {
			return fmt.Sprintf("%s:%d: %s qualifies shape %q as %q, but shape %q belongs to concept %q -- write it as %q",
				c.file, c.line, enclosing, bare, ref, bare, s.concept, s.concept+"."+bare)
		}
		return ""
	}

	s, known := shapes[ref]
	if !known {
		// Unknown shape: not a cross-concept violation. Could be defined in
		// a tree slice this scan does not model; resolution failures surface
		// elsewhere. Stay lenient (mirrors the conformance gate).
		return ""
	}
	if s.concept == "" {
		// @actor-only shape: universal, no concept binding to honor.
		return ""
	}
	if s.concept == c.concept {
		return ""
	}
	if depSharedShapeExempt[c.name] == ref {
		return ""
	}
	return fmt.Sprintf("%s:%d: %s references shape %q in its `shape` slot, but shape %q belongs to concept %q -- qualify it as %q for an explicit cross-concept reference, or project a shape of concept %q instead",
		c.file, c.line, enclosing, ref, ref, s.concept, s.concept+"."+ref, c.concept)
}

// checkInclude validates a single shape `include <ref>` reference.
func checkInclude(s *depShape, inc depRef, shapes map[string]*depShape) string {
	enclosing := fmt.Sprintf("shape %q (concept %q)", s.name, s.concept)

	if qConcept, bare, ok := splitDottedRef(inc.ref); ok {
		target, known := shapes[bare]
		if !known {
			return fmt.Sprintf("%s:%d: %s includes shape %q, but no shape named %q is defined anywhere in the tree",
				s.file, inc.line, enclosing, inc.ref, bare)
		}
		if target.concept != "" && target.concept != qConcept {
			return fmt.Sprintf("%s:%d: %s qualifies include %q as %q, but shape %q belongs to concept %q -- write it as %q",
				s.file, inc.line, enclosing, bare, inc.ref, bare, target.concept, target.concept+"."+bare)
		}
		return ""
	}

	target, known := shapes[inc.ref]
	if !known {
		return ""
	}
	if target.concept == "" || s.concept == "" || target.concept == s.concept {
		return ""
	}
	return fmt.Sprintf("%s:%d: %s includes shape %q, which belongs to concept %q -- a shape may only include shapes of its own concept; qualify it as %q for an explicit cross-concept reference",
		s.file, inc.line, enclosing, inc.ref, target.concept, target.concept+"."+inc.ref)
}

// splitDottedRef splits an explicit `concept.name` reference into its
// concept + bare-name parts. Returns ok=false for a bare (undotted)
// reference. Only the final dot is treated as the qualifier boundary.
func splitDottedRef(ref string) (concept, name string, ok bool) {
	i := strings.LastIndexByte(ref, '.')
	if i <= 0 || i == len(ref)-1 {
		return "", "", false
	}
	return ref[:i], ref[i+1:], true
}

// readTreeFile reads a whole file out of the DSL fs.FS.
func readTreeFile(tree fs.FS, p string) (string, error) {
	f, err := tree.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	raw, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// stripDepLineComment trims a trailing `// ...` comment, preserving any
// `//` that appears inside a double-quoted string literal.
func stripDepLineComment(line string) string {
	inStr := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == '"' && (i == 0 || line[i-1] != '\\') {
			inStr = !inStr
			continue
		}
		if !inStr && c == '/' && i+1 < len(line) && line[i+1] == '/' {
			return line[:i]
		}
	}
	return line
}
