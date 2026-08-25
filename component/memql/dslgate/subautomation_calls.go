// subautomation_calls.go -- the unresolved sub-automation gate (memql#4471).
//
// THE RULE. An automation step may invoke another automation with the
// kind-prefixed call form `automation <name>( ... )`. This gate resolves every
// such callee against the automations DECLARED anywhere in the corpus, and
// reports one that resolves to nothing as a LOAD problem, so strict boot
// refuses it (MEMQL_DSL_ALLOW_SKIPS stays the break-glass).
//
// WHY IT IS A LOAD PROBLEM AND NOT A RUNTIME ONE. The tree already holds the
// precedent and the argument: a tool's @handler target is resolved against the
// function + builtin registry at boot (tool_handler_resolution.go, memql#3625),
// "so a handler naming a function that does not exist is a load problem strict
// boot refuses, not a mid-turn failure the first time a model calls the tool."
//
// Every word of that applies here and lands harder. A tool that fails is one
// model turn. A sub-automation step may be the fifth step of an orchestration
// that has already provisioned cloud infrastructure -- bringUpInstance is
// exactly that -- so a typo in the callee means the earlier steps RAN, the
// world changed, and the run stops partway with a half-built instance. The
// alternative the gate buys is that the automation was never loadable at all.
//
// WHY IT DID NOT SURFACE UNTIL NOW. Nothing in the corpus used the form. Until
// memql#4470 it could not: automationLooseHeader misparsed a call site as a
// declaration and refused boot, so a composition never got far enough to have
// an unresolved callee.
//
// CORPUS-LEVEL, AND THAT IS NOT A DETAIL. Resolution must run after EVERY
// automation is known, never per file -- otherwise A calling B fails whenever A
// happens to load first, which is a function of directory order rather than of
// anything an author wrote. That is why this hangs off ScanFiles rather than
// ScanSource.
//
// THE FAIL DIRECTION IS INVERTED FROM THE IMPORT GATE, AND THIS MATTERS.
// scanCrossNamespaceImports treats "declared nowhere" as "not our business" and
// passes it over, so a PARTIAL corpus makes it weaker. Here "declared nowhere"
// IS the violation, so a partial corpus makes this gate WRONG in the other
// direction: it would refuse a boot over a callee that is declared in a domain
// the caller simply did not pass in. ScanFiles' contract -- the merged set,
// embedded domains plus every RegisterTree overlay, which is where
// MEMQL_DSL_PATH product domains arrive -- is therefore load-bearing for THIS
// gate specifically. A product bundle calling an engine automation, or the
// reverse, resolves correctly only because boot scans them together.
//
// WHAT IT DELIBERATELY DOES NOT CATCH. A callee that is declared but
// @disabled: the declaration is in the text, the loader drops the construct,
// and the call fails at runtime after all. Catching it would mean reading
// lifecycle annotations here and duplicating the loader's own enable/disable
// decision, which is the second implementation this package exists to avoid.
// It is also the far less likely typo.
package dslgate

import (
	"fmt"
	"regexp"
	"strings"
)

// GateUnresolvedSubAutomation is the rule id. Declared here rather than in
// dslgate.go, beside the rule it names -- the convention GateCrossNamespaceImport
// set.
const GateUnresolvedSubAutomation Gate = "unresolved-sub-automation"

// THE THREE DECLARATION FORMS. All three are live in the tree, and a
// declaration index that knows only the first produces confident false
// positives on the other two -- a refused boot naming an automation that is
// right there in the file.
//
//  1. strict:  `automation NAME {`
//  2. loose:   `automation NAME` with the brace on the following line
//  3. terse:   `automation NAME @trigger(...) => logic X`
//
// These mirror automationStructHeader / automationLooseHeader
// (component/automations/unified_loader.go) and terseAutomationHeader
// (component/language/parser/rewriter.go). They are copied rather than imported
// because dslgate must not depend on the loader or the parser -- but they must
// stay in step, which is what TestDeclarationFormsCoverTheLoaders asserts.
var (
	automationDeclStrict = regexp.MustCompile(`(?m)^[ \t]*automation[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)
	automationDeclLoose  = regexp.MustCompile(`(?m)^[ \t]*automation[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*$`)
	automationDeclTerse  = regexp.MustCompile(`(?m)^[ \t]*automation[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]+@trigger\(`)
)

// THE CALL FORM. `automation NAME(` -- the trailer is the whole discriminator
// between a call and a declaration (memql#4463): a declaration is followed by
// `{` or end-of-line, a call is always followed by `(`. The name is permitted
// to sit at column zero here so that a malformed corpus cannot hide a call by
// unindenting it; the declaration patterns above claim `{`/EOL trailers first,
// and the two trailer sets are disjoint.
//
// Optional whitespace before `(` is accepted because the rewriter accepts it
// (splitLeadingIdent then TrimLeft), so `automation foo ( x: 1 )` is a real
// call the engine would dispatch.
var automationCall = regexp.MustCompile(`(?m)^[ \t]*automation[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\(`)

// enclosingAutomation finds the automation a byte offset sits inside, so the
// violation names the CALLER. Without it the report says only "some file has a
// bad call", and the operator has to find it.
func enclosingAutomation(code string, offset int) string {
	head := code[:offset]
	name, at := "", -1
	for _, re := range []*regexp.Regexp{automationDeclStrict, automationDeclLoose, automationDeclTerse} {
		for _, m := range re.FindAllStringSubmatchIndex(head, -1) {
			// The LAST declaration to open before the call site is the one the
			// call sits inside. Compared across all three forms, so a terse
			// declaration between a struct one and the call is not skipped.
			if m[0] > at {
				at, name = m[0], head[m[2]:m[3]]
			}
		}
	}
	return name
}

// scanSubAutomationCalls is the corpus-level gate. Pass 1 indexes every
// declaration; pass 2 walks every call site and reports the ones that resolve
// to nothing.
func scanSubAutomationCalls(files []SourceFile) []Violation {
	code := make(map[string]string, len(files))
	paths := make([]string, 0, len(files))
	for _, f := range files {
		if skipForAutomationScan(f.Path) {
			continue
		}
		paths = append(paths, f.Path)
		// Strings first, then comments -- the same order imports.go uses, and
		// for the same reason: blanking comments first lets a `//` inside a
		// string literal eat the rest of a real line.
		code[f.Path] = codeOnly(f.Content)
	}

	declared := map[string]struct{}{}
	for _, p := range paths {
		src := code[p]
		for _, re := range []*regexp.Regexp{automationDeclStrict, automationDeclLoose, automationDeclTerse} {
			for _, m := range re.FindAllStringSubmatch(src, -1) {
				declared[m[1]] = struct{}{}
			}
		}
	}

	var out []Violation
	for _, p := range paths {
		src := code[p]
		for _, m := range automationCall.FindAllStringSubmatchIndex(src, -1) {
			callee := src[m[2]:m[3]]
			if _, ok := declared[callee]; ok {
				continue
			}
			caller := enclosingAutomation(src, m[0])
			out = append(out, Violation{
				Gate:      GateUnresolvedSubAutomation,
				File:      p,
				Line:      strings.Count(src[:m[0]], "\n") + 1,
				Kind:      "automation",
				Construct: caller,
				Detail: fmt.Sprintf(
					"step calls `automation %s( ... )`, and no automation named %q is declared anywhere in the loaded corpus -- "+
						"check the spelling, or add the domain it lives in to the tree this node loads (MEMQL_DSL_PATH for a product bundle). "+
						"Left unresolved this loads clean and fails at RUN time, after every earlier step of %s has already taken effect (memql#4471)",
					callee, callee, describeCaller(caller)),
			})
		}
	}
	return out
}

func describeCaller(caller string) string {
	if caller == "" {
		return "the enclosing automation"
	}
	return caller
}

// skipForAutomationScan drops the paths the loaders themselves drop, so the
// gate's corpus and the engine's corpus agree.
//
// A `_`- or `.`-prefixed segment is the walker's soft-disable / hidden
// convention. dsl/_reference is the one that matters: its skeletons exist to
// SHOW retired and illustrative forms, including sub-automation calls to names
// that were never meant to resolve. ScanFiles at boot never sees them (the
// loader skips the directory), but ScanTree over a raw fs.FS can, and a gate
// that fires only under the conformance harness is a gate nobody can trust.
func skipForAutomationScan(path string) bool {
	for _, seg := range strings.Split(path, "/") {
		if strings.HasPrefix(seg, "_") || strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}
