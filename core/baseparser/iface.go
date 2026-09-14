package baseparser

import (
	"fmt"
	"sort"
	"strings"
)

// ValidateConstructAnnotations hard-rejects unknown annotations on a
// construct source. kindLabel is the construct keyword used in the
// error message; allowed is the construct's allow-list.
//
// Also hard-rejects the legacy @use* family of annotations
// (@useConcept / @useShape / @useQuery / @useMutation / @useLogic /
// @useBuiltin / @useTrait / @useSpec / @useTool / @usePrompt /
// @useProvider / @useAutomation) with a migration-pointing error
// message. These were retired in the import-model pivot in favour of
// file-top `use <module>.{ names }` imports plus signature-bound
// concept binding for seeds / queries / mutations / shapes.
//
// Returns nil when every top-level `@name` is in the allow-list and
// no @use* annotation is present.

// AttributeRewriteHint is the one sentence every 2026.09 retirement hint in
// this file ends with. D17's last sentence makes naming the migrator part of
// the retirement rather than a courtesy: an author who meets a refusal needs
// the command that fixes their tree, and a hint that only explains the reason
// leaves them to go looking for it. Pinned as a constant so the wording
// cannot drift across the fourteen entries below, and so the gate in
// retired_attributes_test.go can assert on one string.
const AttributeRewriteHint = "run `memqlmigrate --rewrite=attributes`"

// retiredConstructAnnotations maps construct-level annotations hard-retired
// to their migration hints, checked BEFORE the allow-list (the @use*
// precedent) so the author gets the pointed retirement message rather than a
// generic unknown-annotation error. Field-level annotations are a separate
// surface with its own ledger, retiredFieldAnnotations below.
//
// The 2026.08 batch is the @internal / @role / @permission burial. The
// 2026.09 batch is epic memql#5375 (D17): everything nothing read, and the
// losing half of every pair that spelled one value two ways.
//
// Three candidates from D17 are deliberately ABSENT -- @displayCard,
// @composable and @allowedRoles each turned out to have a live reader when
// the spec's own section 10 re-verification was run. Their readers are named
// in their registry doc strings (component/language/annotations/registry.go).
// Adding one of them here is a behaviour regression, and
// TestRetiredSetIsTheD17Set fails the build on it.
var retiredConstructAnnotations = map[string]string{
	"internal":   "retired under the 2026.08 epoch (#2620 ruling / #2708); it only hid the construct from external discovery surfaces (tool listing, MCP promotion, the help()/listFunctions internal flag) while leaving it callable -- delete the annotation",
	"role":       "buried (#2631 ruling / #2709); it was documented but never enforced (nothing ever checked the value at runtime; the load gate rejects it) -- access control lives at the actor layer (RBAC + the @public per-row-authz classification)",
	"permission": "buried (#2631 ruling close-out / #2713); the @role twin -- documented but never enforced (its one help-payload reader was dead; the load gate rejects it) -- access control lives at the actor layer (RBAC + the @public per-row-authz classification)",

	// Parsed into FunctionDef fields, copied into the runtime function,
	// rendered by help() and editor hover, and refused by every allow-list
	// (memql#5375). No allow-list could populate any of them, so the only
	// reachable value was the zero value -- and the render made a field that
	// could not be set look like one that was.
	"deprecated": "retired (memql#5375): it was rendered by help() and editor hover and read by nothing that changes behaviour -- delete it, or say so in the construct's @description; " + AttributeRewriteHint,
	"timeout":    "retired (memql#5375): FunctionDef.Timeout had no reader, so the value never bounded anything -- delete it; a real deadline belongs on the caller's context; " + AttributeRewriteHint,
	"retry":      "retired (memql#5375): FunctionDef.Retry had no reader, so nothing ever retried -- delete it; " + AttributeRewriteHint,
	"idempotent": "retired (memql#5375): declared metadata with no check behind it -- delete it; " + AttributeRewriteHint,
	"audit":      "retired (memql#5375): declared metadata with no writer behind it; auditing is v1:identity:auditEvent, written from Go -- delete it; " + AttributeRewriteHint,

	// Restatements of something the engine already derives, which is worse
	// than absence: a restatement can disagree with what it restates.
	"latestMode": "retired (memql#5375): the engine derives time-dependence from `asOf latest` in the body, so the annotation restated it and could contradict it -- delete it; " + AttributeRewriteHint,
	"enabled":    "retired (memql#5375): constructs are enabled by default, so @enabled was an explicit no-op that read like a switch -- delete it, and use @disabled to deactivate; " + AttributeRewriteHint,

	// The losing spelling of a pair (D17, one spelling each).
	"nocache":  "retired (memql#5375): write @cache(0) -- one annotation for the cache TTL, with 0 meaning never; " + AttributeRewriteHint,
	"schedule": "retired (memql#5375): write @trigger(schedule=\"0 0 * * * *\") -- one annotation declares how an automation is reached, which is what lets @template be refused beside it coherently; " + AttributeRewriteHint,

	// Stored on the tool and enforced nowhere, so each read as a ceiling or
	// a gate while being neither.
	"rateLimit": "retired (memql#5375): Tool.RateLimit was cloned and copied into a Function field nothing reads, so the declared ceiling did not exist -- delete it; the live ceilings are the provider chokepoint in ai_guard.go and the run budget in component/work; " + AttributeRewriteHint,
	"scopes":    "retired (memql#5375): Tool.Scopes was advertised on the gRPC tool descriptor and checked nowhere, so it read as an authorization gate while gating nothing -- delete it; use @requiresCapability for a real one; " + AttributeRewriteHint,

	// Id-bearing only by redundancy: the namespace comes from the domain
	// directory, or from that directory's one-line namespace.pin.
	"namespace": "retired (memql#5375): a concept's namespace is its domain directory, or that directory's one-line namespace.pin -- the annotation could only restate one of those or silently disagree with it; delete it, and pin a deliberate divergence with a namespace.pin file (#2614); " + AttributeRewriteHint,
}

// retiredFieldAnnotations is the same ledger for FIELD annotations -- the
// ones written on a property inside a construct body, where the receiver is
// a field rather than a construct.
//
// Separate map because the two surfaces have separate allow-lists and
// separate refusal sites, and because one map would let a field-only
// retirement refuse a construct that legitimately carries the name: a
// concept's @version is the "v1" of every canonical id it declares, while a
// function's @version was read by nothing.
var retiredFieldAnnotations = map[string]string{
	"unique":    "retired (memql#5375): declared metadata with no uniqueness check behind it (memql#2960), so it read as a constraint while constraining nothing -- delete it; " + AttributeRewriteHint,
	"immutable": "retired (memql#5375): declared metadata with no write guard behind it -- delete it; a field that must not change is enforced by the mutation that writes it; " + AttributeRewriteHint,
}

// RetiredFieldAnnotation reports whether a FIELD-level annotation name is
// hard-retired, returning its migration hint. The concept parser and the
// prompt / builtin / tool field converters all consult it, so a retired
// field annotation refuses with the same hint from whichever body it
// appears in -- which is the gap memql#5375 closed: the same name was a
// load error in an args block, silently tolerated on a prompt field and
// silently dropped on a builtin field.
func RetiredFieldAnnotation(name string) (string, bool) {
	hint, ok := retiredFieldAnnotations[name]
	return hint, ok
}

// RetiredConstructAnnotation reports whether a construct-level annotation
// name is hard-retired, returning its migration hint. Exported so every
// annotation gate (this validator, the parser's declarative-kind validator,
// the sense editor diagnostics) emits the same pointed message instead of a
// generic unknown-annotation error.
func RetiredConstructAnnotation(name string) (string, bool) {
	hint, ok := retiredConstructAnnotations[name]
	return hint, ok
}

// misplacedConstructAnnotations maps an annotation that is LIVE on some other
// construct kind to the hint an author needs when they put it on a kind that
// does not accept it. Without this they get the generic unknown-annotation
// error, which lists the allow-list but never says where the annotation DOES
// belong or what to write instead (memql#2779).
//
// `@row` is the motivating case: an author who wants to filter on the row
// envelope reaches for `@row` on the query, because that is how a SHAPE opts
// into the row surface. A query needs no kind marker -- it binds its concept
// in the signature, and `row.` is available in its filter unconditionally.
var misplacedConstructAnnotations = map[string]string{
	"row": "`@row` is a SHAPE kind marker -- it declares that a shape body projects the row envelope (`shape <Concept> <name> { row.id ... }`). A query needs no kind marker: it binds its concept in the signature (`query <Concept> <name>`), and the `row.` namespace is always available in its filter. To filter on the row id, delete the annotation and write `filter row.id == args.<x>`",
}

// MisplacedConstructAnnotation reports whether an annotation is live on a
// different construct kind, returning the hint that names where it belongs
// and what to write instead. Consulted only after the allow-list rejects the
// name, so a kind that legitimately accepts the annotation is unaffected.
func MisplacedConstructAnnotation(name string) (string, bool) {
	hint, ok := misplacedConstructAnnotations[name]
	return hint, ok
}

func ValidateConstructAnnotations(source, kindLabel string, allowed map[string]bool) error {
	keyword := kindLabel
	if kindLabel == "mutation" {
		// Mutation slices spell the header `mutate NAME {`; without the
		// keyword mapping the header scan would run past the body open
		// and inspect body lines too.
		keyword = "mutate"
	}
	// Scan a COMMENT-BLANKED view, not the raw source (memql#2872).
	//
	// Two distinct silent failures, both closed by this:
	//
	//   - findConstructBodyOpen cut the header scan at the first
	//     `<keyword> ... {`. A COMMENTED-OUT copy of the construct above the
	//     live one -- exactly what an author writes when parking a version --
	//     supplied that header, so the LIVE construct's annotations were never
	//     inspected and this gate silently did nothing. An invalid @public
	//     loaded clean.
	//   - an `@`-annotation named inside a comment was read as a real one, so
	//     an ordinary note like `/* @useConcept(node) */` refused the boot.
	//
	// BlankComments preserves byte offsets and newlines, so scanning it is
	// positionally identical to scanning source; only comment CONTENT differs.
	// Same treatment the header detectors got in #1074 / #2868 / #2896.
	scan := BlankComments(source)
	bodyStart := findConstructBodyOpen(scan, keyword)
	if bodyStart < 0 {
		bodyStart = len(scan)
	}
	header := scan[:bodyStart]

	for _, raw := range strings.Split(header, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, "@") {
			continue
		}
		name := extractAnnotationIdent(line)
		if name == "" {
			continue
		}
		if strings.HasPrefix(name, "use") && len(name) > 3 && name[3] >= 'A' && name[3] <= 'Z' {
			return fmt.Errorf("`@%s(...)` is retired -- declare the dependency via a file-top `use <module>.{ ... }` import instead, and (for seeds/queries/mutations/shapes) put the bound concept in the signature (`%s <Concept> <name> { ... }`)", name, kindLabel)
		}
		if hint, retired := retiredConstructAnnotations[name]; retired {
			return fmt.Errorf("@%s on a %s is retired -- %s", name, kindLabel, hint)
		}
		if !allowed[name] {
			if hint, misplaced := MisplacedConstructAnnotation(name); misplaced {
				return fmt.Errorf("@%s is not valid on a %s -- %s", name, kindLabel, hint)
			}
			return fmt.Errorf("unknown %s annotation @%s -- supported: %s", kindLabel, name, FormatAnnotationAllowList(allowed))
		}
	}
	return nil
}

// FormatAnnotationAllowList renders the allow-list as a sorted
// comma-separated string for use in error messages.
func FormatAnnotationAllowList(allowed map[string]bool) string {
	names := make([]string, 0, len(allowed))
	for n := range allowed {
		names = append(names, "@"+n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// findConstructBodyOpen returns the byte index of the `{` that opens
// the construct's body. Looks for the keyword line and returns the
// position of its `{`. Returns -1 when no body-open is found.
func findConstructBodyOpen(source, keyword string) int {
	for offset := 0; offset < len(source); {
		nl := strings.IndexByte(source[offset:], '\n')
		var line string
		var lineStart int
		if nl < 0 {
			line = source[offset:]
			lineStart = offset
			offset = len(source)
		} else {
			line = source[offset : offset+nl]
			lineStart = offset
			offset += nl + 1
		}
		trimmed := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimmed, keyword+" ") || strings.HasPrefix(trimmed, keyword+"\t") {
			braceIdx := strings.IndexByte(line, '{')
			if braceIdx < 0 {
				return -1
			}
			return lineStart + braceIdx
		}
	}
	return -1
}

// extractAnnotationIdent extracts the `name` from `@name(...)` or
// `@name`. Stops at the first non-identifier character.
func extractAnnotationIdent(line string) string {
	if !strings.HasPrefix(line, "@") {
		return ""
	}
	rest := line[1:]
	for i, r := range rest {
		if (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '_' {
			continue
		}
		return rest[:i]
	}
	return rest
}
