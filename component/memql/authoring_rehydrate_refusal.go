package memql

import (
	"fmt"
	"strings"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/annotations"
	"github.com/znasllc-io/memql/component/language/compiler"
	"github.com/znasllc-io/memql/component/language/deprecation"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/baseloader"
	"github.com/znasllc-io/memql/component/memql/dslgate"
)

// authoring_rehydrate_refusal.go -- what an operator is told about a stored
// authored construct that no longer compiles (memql#5426).
//
// A durably promoted v1:authoring:construct row is recompiled from its stored
// source at every boot and on every cross-node propagation. When the language
// narrows -- memql#5359 refused, at parse, annotations and body lines the
// parser used to accept and silently drop -- a row whose source carried such a
// line stops compiling. The row is quarantined, which is right (a rotted stored
// row must not brick a fleet), but what it said was the stamp guard's one
// sentence for every failure: "run memqlmigrate --rewrite for the intervening
// grammar epics". For a stray line no rewrite exists, so that sent the operator
// to a tool that changes nothing, and the report never named the row itself.
//
// explainRehydrationFailure splits the two cases by the rule the refusal
// carries:
//
//   - a rule that NARROWED the language (narrowsTheLanguage) -- an annotation
//     refusal, a retired or deprecated form, one of the spelling refusals the
//     DSL v1 follow-ups added -- refuses a line an earlier grammar accepted,
//     and its message already names the line and what to write (and the
//     rewrite, when one exists). It becomes a RehydrationRefusal: the row id,
//     bundle and owner, the rule, and the remedy -- including that a line the
//     stored grammar accepted and never read can be deleted with no change in
//     behaviour.
//   - any other refusal -- a syntax error, a missing dependency such as a
//     bound concept that no longer resolves (signature_concept_unresolved), a
//     concept that does not parse (concept_unparsed) -- keeps the stamp
//     guard's diagnosis (S6, memql#2361), unchanged. Those carry rule ids too,
//     and telling the operator "the language refuses this line, delete it"
//     for a concept the tree stopped declaring sent them to edit a construct
//     that was never wrong (memql#5426 review).

// RehydrationRefusal is a stored authored construct that did not re-hydrate
// because a rule that narrowed the language refuses its source.
type RehydrationRefusal struct {
	// Id is the v1:authoring:construct row -- the handle an operator reads
	// and fixes the row by.
	Id       string
	Kind     string
	Name     string
	BundleId string
	Owner    string
	// StoredGrammar is the grammar stamp the row was stored under, "" on a
	// row that predates the stamp.
	StoredGrammar string
	// Code is the refusal's rule id.
	Code  string
	Cause error
}

// Error names the row, what refused it, and what to do.
func (r *RehydrationRefusal) Error() string {
	id := r.Id
	if id == "" {
		id = "id unknown"
	}
	stored := "a grammar that predates the stamp"
	if r.StoredGrammar != "" {
		stored = fmt.Sprintf("grammar %q", r.StoredGrammar)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "authored %s %q (row %s, bundle %s, owner %s) was stored under %s and does not compile under this engine's %q, so it is not re-registered: %v. ",
		r.Kind, r.Name, id, r.BundleId, r.Owner, stored, languageParser.GrammarVersion, r.Cause)
	fmt.Fprintf(&b, "The refusal is a rule of the language (%s) and names what to write: ", r.Code)
	b.WriteString("a line the stored grammar accepted and never read -- an annotation or a clause no loader consumed -- is deleted with no change in what the construct does, and a retired spelling is rewritten as the refusal says. ")
	b.WriteString("Correct the construct's source in its bundle and promote the bundle again, or demote the construct so it is no longer re-hydrated. ")
	b.WriteString("Every boot logs each such row as \"durable authored construct quarantined at re-hydration\" with its id")
	return b.String()
}

// Unwrap exposes the compile refusal.
func (r *RehydrationRefusal) Unwrap() error { return r.Cause }

// RuleCode is the compile refusal's rule id (baseloader.CodedRefusal).
func (r *RehydrationRefusal) RuleCode() string { return r.Code }

// explainRehydrationFailure turns a stored row's recompile failure into what
// the quarantine reports (see the file comment).
func explainRehydrationFailure(row AuthoringConstructRow, err error) error {
	if err == nil {
		return nil
	}
	if code := baseloader.RuleCode(err); narrowsTheLanguage(code) {
		return &RehydrationRefusal{
			Id: row.Id, Kind: row.Kind, Name: row.Name, BundleId: row.BundleId, Owner: row.OwnerUserId,
			StoredGrammar: row.GrammarVersion, Code: code, Cause: err,
		}
	}
	return explainStaleGrammarStamp(row, err)
}

// narrowsTheLanguage reports whether code is the rule id of a refusal that
// NARROWED the language -- one that refuses a line an earlier grammar
// accepted, so a row stored under that grammar can carry the line without
// ever having been wrong about anything else.
func narrowsTheLanguage(code string) bool {
	return code != "" && narrowingRules[code]
}

// narrowingRules is narrowsTheLanguage's set, read from the registries that
// hold each family where one exists, so a form retired or deprecated later
// joins it without an edit here.
var narrowingRules = func() map[string]bool {
	out := map[string]bool{}
	// The annotation refusals (memql#5359): the parser accepted, and dropped,
	// an annotation it had no receiver for, a retired or misplaced one, and an
	// argument form or key it did not read.
	for _, code := range []string{
		annotations.CodeUnknown, annotations.CodeRetired, annotations.CodeMisplaced,
		annotations.CodeForm, annotations.CodeKey, annotations.CodeRepeated,
	} {
		out[code] = true
	}
	// The retired spellings of edition 2026: the expression forms, and the
	// statement parser's retired body and trigger forms.
	for _, form := range languageParser.V1RetiredForms() {
		out[form.Rule] = true
	}
	for _, code := range languageParser.BodyRefusalCodes() {
		if strings.HasSuffix(code, "_retired") {
			out[code] = true
		}
	}
	// A form in a deprecation window refuses once the window closes.
	for _, form := range deprecation.Forms() {
		out[form.Rule] = true
	}
	// The spelling refusals of the DSL v1 follow-ups (memql#5426 and its
	// siblings): each spelling loaded before, and was ignored, read absent, or
	// failed at run time instead.
	for _, code := range []string{
		languageParser.CodeUseNotFileTop,
		languageParser.RuleQueryClauseDuplicate,
		dslgate.CodeConstructMisplaced,
		compiler.CodeArgsUndeclared,
		compiler.CodeBodyCallUnknown,
		compiler.CodeBodyConfigUnknown,
		memoryNodes.CodePatternInvalid,
		CodePromptLevelMissing,
		PromptDefaultCode,
		ToolDefaultCode,
		SortCodeUnknownKey,
		SortCodeUnknownDirection,
		CodeWriteAnnotationUndeclaredField,
		CodeWriteAnnotationFieldType,
		CodeWriteAnnotationUnwrittenField,
		ruleToolHandlerArgUndeclared,
	} {
		out[code] = true
	}
	return out
}()
