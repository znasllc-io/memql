package dsl

import (
	"io"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/dslfs"
)

// default_stamped_test.go -- memql#3038.
//
// A concept-field `@default("value")` reaches the emitted schema as the JSON
// Schema `default` keyword, which is ANNOTATION, NOT BEHAVIOUR: JSON Schema
// validators do not fill it, and Concept.Create clones, validates and marshals
// the payload verbatim. So when a caller omits the field it is simply absent.
// `TestDefaultIsEmittedButNeverApplied` (component/database/memory-nodes) pins
// that, and CLAUDE.md states it.
//
// The failure this gate closes is not the missing value. It is that THE AUTHOR
// NEVER FINDS OUT: writing `@default("draft")` looks like it does something,
// and nothing anywhere tells you it does not. The working mechanism is the `??`
// null-coalescing operator in the mutation body.
//
// # Why a conformance test and not a load-time gate
//
// The ruling is explicit, and the reason is worth keeping. A boot gate that
// reds on a populated set is precisely the failure this issue family exists to
// avoid, and it could refuse boot on a legitimate bundle topology -- a product
// mounting its own DSL at runtime through MEMQL_DSL_PATH. Moving off the boot
// path answers that rather than deferring it: there is no boot-time check, so
// no boot-time false positive is possible.
//
// It is also this package's established pattern for tree-wide authoring rules:
// TestFilterIntrinsicsUseRowNamespace, TestSortKeysUseRowNamespace,
// TestNoRetiredOperatorForms, no_coalesce_longhand_test.go.
//
// # SCOPE -- what this gate does NOT cover
//
// TWO carve-outs, both deliberate, both stated here and in
// dsl/_reference/_concept.memql section 8 so a reader does not infer coverage
// that is not there.
//
// 1. THE IN-REPO TREE ONLY (dsl.Tree()). Domains mounted at runtime via
// MEMQL_DSL_PATH -- a product's own DSL bundle -- are NOT scanned, so a
// bundle's own `@default` mistakes are not caught here. That is the deliberate
// trade for not being able to refuse boot.
//
// 2. TOP-LEVEL CONCEPT FIELDS ONLY. A `@default` on a NESTED object leaf
// (`preferences { theme string @default("auto") }`) is not a finding, because
// the gate's advertised remedy DOES NOT EXIST for it: there is no write form
// that stamps a single leaf. A mutation writes the PARENT object wholesale --
// `createAgent` does `accept { capabilities, triggerBehavior }`,
// `updateUserPreferences` writes `preferences` -- so `theme: args.theme ?? "auto"`
// is unspellable. The only remaining remedy would be deleting the annotation,
// which collides with the ruling's own reason for keeping `@default` at all:
// the emitted `default` keyword is documentation that a preferences form
// generator, sense hover and the generated SDK consume.
//
// Reporting fields whose only available fix is to delete documentation that is
// actually read would make the gate an instruction to vandalise the schema. So
// the nested leaves are carved out by a SCOPE RULE, not by a list of fields
// permitted to lie -- the distinction the no-exemption-list ruling turns on.
//
// The cost, stated rather than hidden: a NEW nested `@default` whose author
// believes it is applied is not caught. It is mitigated only by the reference
// saying plainly that NO `@default` is ever applied at ANY depth. Whether
// nested defaults should be applied at all is a separate question, deliberately
// not folded in here.

var (
	dsConceptHdr  = regexp.MustCompile(`^concept[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)
	dsMutateHdr   = regexp.MustCompile(`^mutate[ \t]+(?:([A-Za-z_][A-Za-z0-9_]*)[ \t]+)?([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)
	dsUseConcepts = regexp.MustCompile(`^[ \t]*use[ \t]+([A-Za-z_][A-Za-z0-9_.]*)\.concepts\.\{([^}]*)\}`)
	dsFieldDecl   = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)[ \t]+(\S+)`)
	dsWriteOpen   = regexp.MustCompile(`^[ \t]*(insert|update)[ \t]*\{`)
	dsKeyed       = regexp.MustCompile(`^[ \t]*([A-Za-z_][A-Za-z0-9_]*)[ \t]*:[ \t]*(.*)$`)
	dsArgsShort   = regexp.MustCompile(`^[ \t]*args\.([A-Za-z_][A-Za-z0-9_]*)[ \t]*,?[ \t]*$`)
	dsIdent       = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	dsBareArgsRHS = regexp.MustCompile(`^args\.[A-Za-z_][A-Za-z0-9_]*[ \t]*,?$`)
)

// defaultField is one `@default`-carrying concept field.
type defaultField struct {
	domain, concept, field string
	required               bool
	// nested is true for a field declared inside an object block rather than
	// directly in the concept body. Carved out of the findings (see the scope
	// note above) but still collected, so the positive control can report the
	// size of the carve-out instead of letting it vanish.
	nested bool
	path   string
	line   int
}

// conceptWrites is what a concept's bound mutations do to each of its fields.
//
// The distinction the whole gate turns on: only a STAMPED value realises a
// default. `accept { f }`, a bare `args.f` shorthand and `f: args.f` all bind
// the field to a caller argument -- omit it and nothing is written, so the
// default is still not applied. `f: args.f ?? "v"`, a literal, or any computed
// expression DOES produce the value.
type conceptWrites struct {
	stamped  map[string]bool
	supplied map[string]bool
	splat    bool
}

func dsStripLineComment(l string) string {
	if i := strings.Index(l, "//"); i >= 0 {
		l = l[:i]
	}
	return l
}

func dsDomainOf(p string) string {
	if i := strings.Index(p, "/"); i >= 0 {
		return p[:i]
	}
	return ""
}

func dsReadTree(t *testing.T) (paths []string, read func(string) string) {
	t.Helper()
	tree := Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}
	return paths, func(p string) string {
		f, openErr := tree.Open(p)
		if openErr != nil {
			t.Fatalf("open %s: %v", p, openErr)
		}
		raw, readErr := io.ReadAll(f)
		f.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}
		// Block comments are blanked through the shared sense scanner so a
		// commented-out example never reads as live corpus (#2615 / #2658).
		return blankBlockComments(string(raw))
	}
}

// collectDefaultFields returns every `@default`-carrying concept field.
func collectDefaultFields(t *testing.T, paths []string, read func(string) string) []defaultField {
	t.Helper()
	var out []defaultField
	for _, p := range paths {
		if !strings.HasSuffix(p, "concepts.memql") {
			continue
		}
		concept := ""
		// depth is the brace depth INSIDE the current concept body: 1 for a
		// field declared directly in it, >1 for a leaf inside a nested object
		// block or a @variant branch. It is what separates a field the gate
		// can demand a stamp for from one where no write form can produce it.
		depth := 0
		for i, raw := range strings.Split(read(p), "\n") {
			line := dsStripLineComment(raw)
			if m := dsConceptHdr.FindStringSubmatch(line); m != nil {
				concept, depth = m[1], 1
				continue
			}
			if concept == "" {
				continue
			}

			// The field's own depth is the depth BEFORE this line's braces are
			// counted -- `settings {` is itself a field of the concept body,
			// and it is the fields after it that are nested.
			fieldDepth := depth
			depth += strings.Count(line, "{") - strings.Count(line, "}")
			if depth <= 0 {
				concept, depth = "", 0
				continue
			}

			if !strings.Contains(line, "@default(") {
				continue
			}
			trim := strings.TrimSpace(line)
			m := dsFieldDecl.FindStringSubmatch(trim)
			if m == nil {
				continue
			}
			out = append(out, defaultField{
				domain:  dsDomainOf(p),
				concept: concept,
				field:   m[1],
				// A required field has no omitted case, so its @default is
				// documenting the conventional value rather than silently
				// doing nothing. Carved out by the ruling.
				required: strings.Contains(trim, "@required") || strings.HasSuffix(m[2], "!"),
				nested:   fieldDepth > 1,
				path:     p,
				line:     i + 1,
			})
		}
	}
	return out
}

// collectConceptWrites maps "<domain>.<concept>" to what its bound mutations
// write. A mutation's concept comes from its signature; the domain is resolved
// through the file's `use <ns>.concepts.{ ... }` imports, defaulting to the
// mutation file's own domain (same-domain imports are stripped, #2617).
func collectConceptWrites(paths []string, read func(string) string) map[string]*conceptWrites {
	out := map[string]*conceptWrites{}
	get := func(k string) *conceptWrites {
		if out[k] == nil {
			out[k] = &conceptWrites{stamped: map[string]bool{}, supplied: map[string]bool{}}
		}
		return out[k]
	}

	for _, p := range paths {
		if !strings.HasSuffix(p, "mutations.memql") {
			continue
		}
		dom := dsDomainOf(p)
		lines := strings.Split(read(p), "\n")

		imports := map[string]string{}
		for _, raw := range lines {
			if m := dsUseConcepts.FindStringSubmatch(dsStripLineComment(raw)); m != nil {
				ns := m[1]
				if i := strings.LastIndex(ns, "."); i >= 0 {
					ns = ns[i+1:]
				}
				for _, n := range strings.Split(m[2], ",") {
					if n = strings.TrimSpace(n); n != "" {
						imports[n] = ns
					}
				}
			}
		}

		cur := ""
		inWrite, depth, inAccept := false, 0, false
		for _, raw := range lines {
			line := dsStripLineComment(raw)
			if m := dsMutateHdr.FindStringSubmatch(line); m != nil {
				d := dom
				if ns, ok := imports[m[1]]; ok {
					d = ns
				}
				cur = d + "." + m[1]
				inWrite, depth, inAccept = false, 0, false
				continue
			}
			if cur == "" {
				continue
			}
			if !inWrite {
				if dsWriteOpen.MatchString(line) {
					inWrite, depth = true, 1
				}
				continue
			}
			depth += strings.Count(line, "{") - strings.Count(line, "}")
			if depth <= 0 {
				inWrite, inAccept = false, false
				continue
			}
			c := get(cur)
			trim := strings.TrimSpace(line)

			// `accept { a, b }` -- each name binds to its same-named arg.
			if strings.HasPrefix(trim, "accept") && strings.Contains(trim, "{") {
				body := strings.TrimSuffix(strings.TrimSpace(trim[strings.Index(trim, "{")+1:]), "}")
				for _, n := range strings.Split(body, ",") {
					if n = strings.TrimSpace(n); dsIdent.MatchString(n) {
						c.supplied[n] = true
					}
				}
				inAccept = !strings.Contains(trim, "}")
				continue
			}
			if inAccept {
				if strings.Contains(trim, "}") {
					inAccept = false
				}
				for _, n := range strings.Split(strings.Trim(trim, "},"), ",") {
					if n = strings.TrimSpace(n); dsIdent.MatchString(n) {
						c.supplied[n] = true
					}
				}
				continue
			}
			// `stamp {` groups stamped fields; it names no field itself.
			if strings.HasPrefix(trim, "stamp") && strings.Contains(trim, "{") {
				continue
			}
			if m := dsKeyed.FindStringSubmatch(line); m != nil {
				rhs := strings.TrimSpace(m[2])
				switch {
				case dsBareArgsRHS.MatchString(rhs):
					c.supplied[m[1]] = true
				case rhs != "":
					c.stamped[m[1]] = true
				}
				continue
			}
			if m := dsArgsShort.FindStringSubmatch(line); m != nil {
				if m[1] == "payload" {
					c.splat = true
				} else {
					c.supplied[m[1]] = true
				}
			}
		}
	}
	return out
}

// TestDefaultIsCoalescedOrStamped is the memql#3038 gate.
//
// An OPTIONAL, TOP-LEVEL concept field carrying `@default` must be STAMPED by
// some mutation bound to that concept -- `f: args.f ?? "v"`, a literal, or a
// computed value. Otherwise the annotation is a silent no-op: the engine never
// applies it, and no caller is obliged to.
//
// Two remedies, and which one is right is a per-field judgement:
//
//   - the default IS the intended create-time value -> stamp it, so the
//     behaviour matches what the annotation has been claiming;
//   - it is documentation of a convention some consumer applies -> delete the
//     annotation and say it in @description instead.
//
// Removing a `@default` changes NO behaviour, since it is already a no-op.
// Adding a `??` stamp DOES change what a create writes. So the second remedy is
// the default and the first is reserved for a field whose create path clearly
// ought to produce that value and whose absence is a live defect.
//
// Do NOT add an exemption list. A list of fields permitted to lie is the
// outcome this issue exists to remove. The nested carve-out above is a scope
// rule -- it names a class the remedy cannot reach, not fields excused from it.
func TestDefaultIsCoalescedOrStamped(t *testing.T) {
	paths, read := dsReadTree(t)
	defaults := collectDefaultFields(t, paths, read)
	if len(defaults) == 0 {
		t.Fatal("no @default fields found in the tree, so this gate measures nothing")
	}
	writes := collectConceptWrites(paths, read)

	var findings []defaultField
	for _, d := range defaults {
		if d.required || d.nested {
			continue
		}
		if w := writes[d.domain+"."+d.concept]; w != nil && w.stamped[d.field] {
			continue
		}
		findings = append(findings, d)
	}

	sort.Slice(findings, func(i, j int) bool {
		if findings[i].path != findings[j].path {
			return findings[i].path < findings[j].path
		}
		return findings[i].line < findings[j].line
	})

	for _, f := range findings {
		extra := ""
		if w := writes[f.domain+"."+f.concept]; w != nil && w.splat {
			extra = "\n  (a mutation writes this concept with an object splat, so the field CAN be " +
				"populated by a caller -- but the declared default is still never applied.)"
		}
		t.Errorf("%s:%d  %s.%s carries @default but no mutation bound to %q ever stamps it -- "+
			"the annotation is a silent no-op.\n"+
			"  Either stamp it in the mutation (`%s: args.%s ?? <default>`) if that IS the "+
			"intended create-time value, or delete @default and put the convention in "+
			"@description.%s",
			f.path, f.line, f.concept, f.field, f.concept, f.field, f.field, extra)
	}
	if len(findings) > 0 {
		t.Errorf("%d optional @default field(s) are never stamped (memql#3038). "+
			"@default is emitted into the schema as documentation and is NEVER applied on "+
			"insert -- `??` in the mutation body is the only mechanism that fills a value.",
			len(findings))
	}
}

// TestDefaultGateSeesStampedFields is the positive control, and it exists for
// the reason memql#3043 taught: a scanner that matches nothing produces no
// findings, which is indistinguishable from a clean tree. If the write-block
// scan silently stops recognising `stamp { }` / `??` / `accept { }`, the gate
// above goes GREEN while enforcing nothing.
//
// So this asserts the scan still SEES coverage: a healthy tree has many
// stamped @default fields, and the counts are reported.
func TestDefaultGateSeesStampedFields(t *testing.T) {
	paths, read := dsReadTree(t)
	defaults := collectDefaultFields(t, paths, read)
	writes := collectConceptWrites(paths, read)

	stamped, required, nested := 0, 0, 0
	for _, d := range defaults {
		switch {
		case d.required:
			required++
		case d.nested:
			nested++
		case writes[d.domain+"."+d.concept] != nil && writes[d.domain+"."+d.concept].stamped[d.field]:
			stamped++
		}
	}
	if stamped == 0 {
		t.Fatal("the write-block scan found ZERO stamped @default fields. That is not a clean " +
			"tree -- it means the scanner no longer recognises `stamp { }` / `f: args.f ?? v`, " +
			"so TestDefaultIsCoalescedOrStamped is enforcing nothing (memql#3043's lesson).")
	}
	// The nested count is REPORTED, not merely skipped. A carve-out that
	// silently absorbs a growing population is how an exemption list forms
	// without anyone deciding to write one; printing its size each run keeps
	// the trade visible.
	if nested == 0 {
		t.Error("the depth scan found ZERO nested @default fields. The tree has them " +
			"(user.preferences, agent.capabilities, ...), so the depth tracking has broken and " +
			"the top-level scope rule is no longer doing anything.")
	}
	t.Logf("@default fields: %d total, %d required (carved out), %d nested (carved out, "+
		"no write form stamps a leaf), %d top-level and stamped by a mutation",
		len(defaults), required, nested, stamped)
}
