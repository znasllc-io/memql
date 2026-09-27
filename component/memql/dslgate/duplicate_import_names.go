// Two `use` declarations in one file binding the SAME local name (memql#5196).
//
// A .memql file that imports the same bare name from two different domains gets
// a POSITIONAL PICK, with no error and no warning. Both resolution paths agree
// on which one wins, and both are the opposite of what a reader assumes.
//
// IT IS FIRST-WINS, NOT LAST-WINS. Every instinct from "a symbol table is a
// map" says the later import overwrites the earlier one. It does not:
//
//   - component/memql/concept_resolver.go, namespaceHintForName -- loops the
//     file's `use` declarations and RETURNS on the first one whose
//     SourceNameFor(name) matches. The second is never consulted, so the first
//     import's Parts[0] becomes the namespace hint unconditionally.
//   - component/memql/concept_resolver.go, resolveUseDeclarations -- not a map
//     overwrite but a deliberate `if _, dup := symbols[name]; dup { continue }`.
//     The second binding is dropped from the symbol table.
//
// Anyone reasoning from "maps overwrite" gets the direction backwards, which is
// what would make a wrong fix pass its own test and look correct. Any test
// written for this gate must assert on WHICH import survives, never merely that
// something was reported.
//
// WHY A GATE AND NOT A RESOLVER CHANGE. Picking a winner is the defect; there is
// no correct winner to pick. The loader already treats cross-namespace ambiguity
// as fatal in the case it can SEE -- an UNIMPORTED ambiguous bare name is a hard
// boot error (`ambiguous concept name %q matches %d concepts`, and
// dslimports/integrity.go's conceptAmbiguous for the offline lane). The gap is
// the case where an import is PRESENT but ambiguous, and there the loader went
// quiet in both paths. This closes that asymmetry rather than adding a
// preference.
//
// THE RULE ALREADY EXISTS TWICE, ONE SCOPE TOO NARROW EACH TIME:
//
//   - component/language/parser/parser.go refuses one `use` CLAUSE binding a
//     name twice ("binds %q twice in local scope ... alias one of them"). It
//     walks a single UseDeclaration's Names and cannot see a second `use` line.
//   - component/actions/loader.go's parseCapabilityImports refuses a capability
//     verb imported from two namespaces in one file, as a loud load error. That
//     is exactly this rule, for exactly one kind.
//
// So this gate is those two, generalised to every kind and every pair of `use`
// lines -- deliberately NOT a general duplicate-name check.
//
// SAME PATH TWICE IS REDUNDANT, NOT AMBIGUOUS, and is passed over. The parser
// makes the same call inside one clause (`{ order, order }` is tolerated) and
// parseCapabilityImports makes it explicitly (`dup && existing != full`). Two
// imports that resolve to the same construct cannot disagree about anything, so
// refusing them would be a style rule wearing a correctness rule's error
// message.
//
// WHY THE ALIAS ACTUALLY FIXES IT, which is not obvious and belongs here rather
// than only in the message: the symbol table is keyed by the LOCAL name
// (`name := u.LocalNameFor(source)`), so aliasing one of the two imports changes
// the KEY and BOTH entries survive. It is not merely that the body reads a
// different word. Keying that table by the SOURCE name instead would re-create
// import capture through the symbol table (memql#3802) -- so a later
// "simplification" of the keying would both reopen that capture and break this
// remedy.
//
// ZERO INSTANCES IN THE TREE when this landed: 305 .memql files, 205 `use { }`
// lines, three concept names declared in two domains each (`account` in
// identity + accounts, `invocation` in worker + observability, `run` in work +
// bench) and every one resolving to a single source corpus-wide. That is a
// latent gap, not a live defect -- and it means every observation on the real
// tree is a NULL RESULT, so the tests are fixture-driven with a paired control.

package dslgate

import (
	"fmt"
	"sort"
	"strings"
)

// GateDuplicateImportName -- one local name bound by two `use` declarations
// naming different modules, in a single file.
const GateDuplicateImportName Gate = "duplicate-import-name"

// importBinding is one local name bound by one `use` declaration.
type importBinding struct {
	local  string // the name the body writes
	source string // the name the module spells, differs only for an alias
	path   string // the dotted module path, e.g. "worker.concepts"
	line   int
}

// scanDuplicateImportNames reports a local name bound by two `use` declarations
// that name different modules.
func scanDuplicateImportNames(file, src string) []Violation {
	if skipForAutomationScan(file) {
		return nil
	}
	// The findings are the text's alone, derived once per process
	// (source_facts.go); the file they are reported against is this call's.
	return stampFile(file, factsOf(src).duplicateImportFindings())
}

// duplicateImportNamesIn is the gate over one file's `use` lines, reporting
// against file.
func duplicateImportNamesIn(file string, uses []useFact) []Violation {
	first := map[string]importBinding{}
	// Report at most one violation per local name: a name bound three times is
	// one decision to make, not two.
	reported := map[string]bool{}
	var out []Violation

	for _, u := range uses {
		for _, b := range parseImportBraceList(u.names, u.path, u.line) {
			prior, dup := first[b.local]
			if !dup {
				first[b.local] = b
				continue
			}
			if prior.path == b.path || reported[b.local] {
				// Same module twice is redundant, not ambiguous.
				continue
			}
			reported[b.local] = true
			out = append(out, Violation{
				Gate:      GateDuplicateImportName,
				File:      file,
				Line:      b.line,
				Kind:      "use",
				Construct: b.local,
				Detail: fmt.Sprintf(
					"%q is bound by two `use` declarations naming different modules -- %s (line %d) and %s (line %d) -- so every bare %q in this file resolves to ONE of them, silently. "+
						"It is the FIRST import that wins, not the last: namespaceHintForName returns on its first match and resolveUseDeclarations skips a duplicate key rather than overwriting it, "+
						"so a reader reasoning from \"a later import overwrites an earlier one\" gets the direction backwards. "+
						"ALIAS ONE OF THEM: `use %s.{ %s as <name> }`. The symbol table is keyed by the LOCAL name, so aliasing changes the key and BOTH imports survive -- "+
						"it is not merely that the body reads a different word (memql#5196, and memql#3802 for why that keying must stay local-name-keyed)",
					b.local,
					prior.path, prior.line,
					b.path, b.line,
					b.local,
					b.path, b.source),
			})
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Line < out[j].Line })
	return out
}

// parseImportBraceList splits a `use <path>.{ a, b as c }` brace body into its
// bindings. Separators are `,` and `;`, matching what the parser tolerates.
func parseImportBraceList(body, modulePath string, line int) []importBinding {
	var out []importBinding
	for _, raw := range strings.FieldsFunc(body, func(r rune) bool { return r == ',' || r == ';' }) {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		// `bar as baz` binds the LOCAL name baz; the module still spells it bar.
		source, local := entry, entry
		if fields := strings.Fields(entry); len(fields) == 3 && fields[1] == "as" {
			source, local = fields[0], fields[2]
		} else if len(fields) != 1 {
			// Anything else is a parse problem the parser reports better.
			continue
		}
		out = append(out, importBinding{local: local, source: source, path: modulePath, line: line})
	}
	return out
}
