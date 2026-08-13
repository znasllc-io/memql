package dslconformance

// nested_block_writes_3641_test.go is the WRITE-side sibling of
// utterance_source_leaves_test.go (memql#2794), which pinned the READ side.
//
// A nested block accepts undeclared keys: concept_parser.go emits
// `additionalProperties: false` for the top-level object only, so a key nobody
// declared inserts and reads back fine (memql#3641). That openness is what let
// `transcriptOnly` and `idempotencyKey` be written into
// `v1:cognition:utterance.source` for their whole life without appearing in the
// schema, and what let `capabilities.domains` / `capabilities.tools` -- the
// pre-#158 surface skillIds replaced -- keep being seeded after their removal.
//
// Until the block can be closed (memql#3641 sequences that: declare, migrate,
// then flip), this is what keeps the schema honest about the row. It also
// catches the second half of the same drift, which closing the block would NOT
// catch: a write of a literal value that is outside a DECLARED leaf's @enum.
// `additionalProperties` governs keys, not values, so
// `source: { inputMethod: "si" }` -- outside enum("typed", "stt",
// "realtimeVoice") -- would survive the flip untouched.
//
// Deliberately scoped to two (concept, block) pairs rather than every nested
// path in the tree, for the reason its read-side sibling records: general leaf
// validation has to tell blocks that enumerate their sub-fields apart from
// free-form `object` fields (planner's feedbackRequest.timeoutAt is undeclared
// by design) and collapse `@variant` arms. Both blocks here enumerate their
// sub-fields, and both are the ones memql#3641 measured as drifted.
//
// The GO write path is out of reach from here: insertSystemActionUtterance
// merges a caller-supplied map[string]string into the source, so its keys
// (trigger / severity / agentName / topic / feedbackReason) are declared on the
// concept but not checked by this gate. Closing that side needs the flip.

import (
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/dslfs"
	"github.com/znasllc-io/memql/dsl"
)

// blockLeaf is one declared sub-field of a nested block.
type blockLeaf struct {
	Name string
	Enum []string // declared @enum / enum(...) values; nil when unconstrained
}

// checkedNestedBlocks names the (file, concept, block) triples this gate reads
// the declared shape from.
var checkedNestedBlocks = []struct {
	File    string
	Concept string
	Block   string
}{
	{"cognition/concepts.memql", "utterance", "source"},
	{"agents/concepts.memql", "agent", "capabilities"},
	{"identity/concepts.memql", "user", "preferences"},
}

var enumValuesRe = regexp.MustCompile(`enum\(([^)]*)\)`)

// declaredBlockLeaves reads the sub-field names of `<block> { ... }` inside
// `concept <concept> {` in the named file, with each leaf's enum values when it
// declares any.
//
// Scoped by brace DEPTH, not by the first bare `}`: a concept declares several
// nested blocks (utterance has timestamps and action besides source), so a
// naive close-brace match ends the concept early.
func declaredBlockLeaves(t *testing.T, path, concept, block string) map[string]blockLeaf {
	t.Helper()
	f, err := dsl.Tree().Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	raw, readErr := io.ReadAll(f)
	f.Close()
	if readErr != nil {
		t.Fatalf("read %s: %v", path, readErr)
	}

	leaves := map[string]blockLeaf{}
	depth := 0
	inConcept := false
	inBlock := false
	sawBlock := false
	for _, line := range strings.Split(blankComments(string(raw)), "\n") {
		trimmed := strings.TrimSpace(line)
		structure := structureOf(trimmed)
		opens := strings.Count(structure, "{")
		closes := strings.Count(structure, "}")

		switch {
		case !inConcept:
			if strings.HasPrefix(trimmed, "concept "+concept+" ") || trimmed == "concept "+concept+" {" {
				inConcept = true
				depth = opens - closes
			}
			continue
		case inBlock:
			if closes > 0 && opens == 0 {
				inBlock = false
			} else if fields := strings.Fields(trimmed); len(fields) > 0 {
				// `  name  type  @annotations(...)` -- first token is the field.
				leaf := blockLeaf{Name: fields[0]}
				if m := enumValuesRe.FindStringSubmatch(trimmed); m != nil {
					for _, v := range strings.Split(m[1], ",") {
						leaf.Enum = append(leaf.Enum, strings.Trim(strings.TrimSpace(v), `"`))
					}
				}
				leaves[leaf.Name] = leaf
			}
		case trimmed == block+" {":
			inBlock, sawBlock = true, true
		}

		depth += opens - closes
		if depth <= 0 {
			break // End of the concept.
		}
	}
	if !sawBlock {
		t.Fatalf("concept %s in %s declares no `%s {` block; the concept shape changed and this guard has silently stopped protecting anything",
			concept, path, block)
	}
	if len(leaves) == 0 {
		t.Fatalf("found no sub-fields in the `%s` block of %s; the block shape changed and this guard has silently stopped protecting anything",
			block, path)
	}
	return leaves
}

// nestedBlockWrite is one key written into a nested block by authored DSL.
type nestedBlockWrite struct {
	File    string
	Line    int
	Key     string
	Literal string // the value when it is exactly a quoted string; "" otherwise
}

// blockWriteOpenerRe matches a line that opens a write into a nested block, in
// either authored form: `source: {` inside a mutation / automation, and
// `capabilities {` inside a seed.
func blockWriteOpenerRe(block string) *regexp.Regexp {
	return regexp.MustCompile(`^` + regexp.QuoteMeta(block) + `[ \t]*:?[ \t]*\{`)
}

// collectNestedBlockWrites walks the tree and returns every key written into a
// `block` write, skipping the concept's own DECLARATION of it (which is not a
// write).
func collectNestedBlockWrites(t *testing.T, block, declarationFile string) []nestedBlockWrite {
	t.Helper()
	tree := dsl.Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}
	opener := blockWriteOpenerRe(block)

	var out []nestedBlockWrite
	for _, p := range paths {
		if p == declarationFile {
			continue
		}
		f, openErr := tree.Open(p)
		if openErr != nil {
			t.Fatalf("open %s: %v", p, openErr)
		}
		raw, readErr := io.ReadAll(f)
		f.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}

		depth := 0
		for i, line := range strings.Split(blankComments(string(raw)), "\n") {
			trimmed := strings.TrimSpace(line)
			structure := structureOf(trimmed)

			if depth == 0 {
				m := opener.FindString(trimmed)
				if m == "" {
					continue
				}
				// Everything after the opening brace is body on this line too,
				// which is how the single-line form is reached.
				rest := trimmed[len(m):]
				depth = 1 + strings.Count(structureOf(rest), "{") - strings.Count(structureOf(rest), "}")
				out = append(out, parseBlockWriteKeys(p, i+1, rest)...)
				continue
			}
			out = append(out, parseBlockWriteKeys(p, i+1, trimmed)...)
			depth += strings.Count(structure, "{") - strings.Count(structure, "}")
			if depth <= 0 {
				depth = 0
			}
		}
	}
	return out
}

// parseBlockWriteKeys pulls the keys out of one line of a block-write body.
//
// Three authored spellings reach a key:
//
//	name: <expr>       the ordinary assignment
//	args.name          the splat shorthand -- writes `name` from the arg of the
//	                   same name, which is how `idempotencyKey` reaches the
//	                   utterance source
//	name: v, other: w  several on one line, as the single-line `{ ... }` form
//	                   writes them
func parseBlockWriteKeys(file string, line int, text string) []nestedBlockWrite {
	var out []nestedBlockWrite
	for _, seg := range splitTopLevelSingle(text, ',') {
		seg = strings.TrimSpace(strings.Trim(strings.TrimSpace(seg), "{}"))
		if seg == "" {
			continue
		}
		key, value, hasColon := strings.Cut(seg, ":")
		key = strings.TrimSpace(key)
		if !hasColon {
			// `args.name` splat shorthand; anything else is not a key.
			if rest, ok := strings.CutPrefix(key, "args."); ok && isBareIdentifier(rest) {
				out = append(out, nestedBlockWrite{File: file, Line: line, Key: rest})
			}
			continue
		}
		if !isBareIdentifier(key) {
			continue
		}
		w := nestedBlockWrite{File: file, Line: line, Key: key}
		// Only an exact quoted literal is recorded as a value. An expression
		// (`args.x ?? "realtimeVoice"`) writes whatever the caller sent, so its
		// value is not decidable here -- see the enum test's scope note.
		if v := strings.TrimSpace(value); len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' &&
			!strings.Contains(v[1:len(v)-1], `"`) {
			w.Literal = v[1 : len(v)-1]
		}
		out = append(out, w)
	}
	return out
}

func isBareIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (i > 0 && c >= '0' && c <= '9') {
			continue
		}
		return false
	}
	return true
}

// TestNestedBlockWritesAreDeclared: every key authored DSL writes into a
// checked nested block must be a declared sub-field of it.
//
// Two live drifts failed this when it was written (memql#3641):
// `transcriptOnly` + `idempotencyKey` on utterance.source, written by
// sendRealtimeTranscriptUtterance and read back by cognition_handler.go; and
// `domains` + `tools` on agent.capabilities, seeded by plannerAgent.memql and
// trainerAgent.memql long after #158 replaced that surface with skillIds.
//
// The two have opposite fixes, which is the point of reporting the key rather
// than prescribing one: the first pair is legitimately in use and wanted a
// DECLARATION, the second is a retired surface and wanted DELETING.
func TestNestedBlockWritesAreDeclared(t *testing.T) {
	for _, b := range checkedNestedBlocks {
		declared := declaredBlockLeaves(t, b.File, b.Concept, b.Block)
		writes := collectNestedBlockWrites(t, b.Block, b.File)
		if len(writes) == 0 {
			t.Errorf("found no authored writes into `%s` on %s; either the corpus stopped writing it or "+
				"the write form changed, and this guard now protects nothing", b.Block, b.Concept)
			continue
		}
		for _, w := range writes {
			if _, ok := declared[w.Key]; ok {
				continue
			}
			t.Errorf("%s:%d writes `%s.%s`, which concept %s does not declare. Nested blocks are OPEN at "+
				"runtime (memql#3641), so this stores and reads back fine -- which is the problem. Either "+
				"declare the sub-field (if the writer is legitimate) or delete the write (if it is a "+
				"retired surface).", w.File, w.Line, b.Block, w.Key, b.Concept)
		}
	}
}

// TestNestedBlockEnumWritesAreInRange: a literal written to a declared leaf
// that carries an @enum must be one of its values.
//
// This survives the memql#3641 flip rather than being replaced by it:
// `additionalProperties` governs KEYS. `source: { inputMethod: "si" }` --
// dsl/cognition/automations.memql's generateResponse, outside
// enum("typed", "stt", "realtimeVoice") -- has a perfectly declared key and a
// value the schema does not admit.
//
// Scope: exact quoted literals. A value computed from an expression
// (`args.source.inputMethod ?? "realtimeVoice"`) is whatever the caller sent,
// so it is not decidable from the source text.
func TestNestedBlockEnumWritesAreInRange(t *testing.T) {
	for _, b := range checkedNestedBlocks {
		declared := declaredBlockLeaves(t, b.File, b.Concept, b.Block)
		for _, w := range collectNestedBlockWrites(t, b.Block, b.File) {
			leaf, ok := declared[w.Key]
			if !ok || len(leaf.Enum) == 0 || w.Literal == "" {
				continue
			}
			admitted := false
			for _, v := range leaf.Enum {
				if v == w.Literal {
					admitted = true
					break
				}
			}
			if !admitted {
				t.Errorf("%s:%d writes `%s.%s: %q`, which is outside the declared enum(%s). The value is "+
					"stored anyway -- an @enum on a nested leaf is not enforced on write -- so the row "+
					"carries a value no reader of the schema expects.",
					w.File, w.Line, b.Block, w.Key, w.Literal, strings.Join(leaf.Enum, ", "))
			}
		}
	}
}
