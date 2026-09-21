package memql

// executor_language_status.go -- the languageStatus builtin (memql#5390): the
// MemQL line this node speaks, the forms the language deprecates, and where the
// DSL its last load read still spells one. The read behind MemQL OS's
// Settings -> Language.
//
// NOTHING HERE IS STORED, AND NOTHING HERE READS A ROW. The line, the edition,
// its status, the grammar version and the editor release are constants in
// component/language/parser; the forms and their windows are the deprecation
// table (component/language/deprecation); the uses are what recordDeprecatedUses
// found in the tree the last Init read (deprecated_uses.go, which also says what
// that tree is and is not). So there is no @rowAuthz tier to meet it, and the
// capability the builtin declares -- read on app:settings/language -- is the
// whole gate, enforced by the builtin path before this runs. That is also why
// the builtin carries @requiresCapability rather than @requiresRank: a builtin's
// annotation set has no rank floor, and the resource is seeded on exactly the
// three roles the OS section is seeded for.
//
// PER NODE, and the same answer from every node that loaded the same tree,
// which in a cluster is every mesh node: they mount one bundle. Two nodes that
// loaded different trees -- a rolling deploy half done -- answer differently,
// and that difference is the truth about them.
//
// # The state is DERIVED, never stored
//
// deprecation.Form carries no "is it refused yet" flag, deliberately (forms.go):
// the decision is Form.RefusesAt(release), a pure function of the form and the
// release asking. So this reads the release ONCE, at the top, and asks each form
// -- rather than reporting a field somebody would have to remember to flip.
//
// In a build that was not cut from a release the release is empty, which
// window.go cannot parse and therefore fails OPEN on: every form reads
// `deprecated`, which is exactly what such a node does with one. The state is a
// fact about THIS node's loader, not a prediction about the fleet.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/deprecation"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// LanguageStatusConcept is the id and concept of the one virtual row
// languageStatus answers, in the engine's own `memql:` namespace beside
// memql:docs, memql:grammar and memql:version: it describes the engine, not the
// graph.
const LanguageStatusConcept = "memql:language"

// languageStatus is the row. Field names are the wire contract MemQL OS decodes
// (clients/os/src/apps/settings/language.ts).
type languageStatus struct {
	Language                string               `json:"language"`
	Edition                 string               `json:"edition"`
	Status                  string               `json:"status"`
	GrammarVersion          string               `json:"grammarVersion"`
	EditorRelease           string               `json:"editorRelease"`
	DeprecationWindowMinors int                  `json:"deprecationWindowMinors"`
	Forms                   []languageStatusForm `json:"forms"`
}

// languageStatusForm is one deprecated form, where it stands in its window, and
// every use of it the last load found.
type languageStatusForm struct {
	Rule        string `json:"rule"`
	Spelling    string `json:"spelling"`
	Replacement string `json:"replacement"`
	Migrator    string `json:"migrator"`
	// DeprecatedIn is the release whose load first warned about the form.
	DeprecatedIn string `json:"deprecatedIn"`
	// RefusedFrom is the first release ALLOWED to stop loading it -- a FLOOR,
	// not a date, which is why the name says "from" and the surface says "may
	// be refused from". Empty when the form's deprecation point is unreadable,
	// and an empty floor is an absent sentence rather than a guessed one.
	RefusedFrom string `json:"refusedFrom"`
	// State is what THIS node's loader does with the form now: "deprecated"
	// (loads, warns, counted) or "refused" (the window is spent).
	State string              `json:"state"`
	Uses  []languageStatusUse `json:"uses"`
}

// languageStatusUse is where one use starts: the file's path in the DSL tree
// (never a path on the node's disk), 1-based line and column, and the spelling
// as written -- `array(string)` where the form is `array(T)`, which is the part
// the generic spelling cannot show.
type languageStatusUse struct {
	File   string `json:"file"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
	Text   string `json:"text"`
}

// The two states a form can be in from a loader's point of view. Strings on the
// wire because they are read by a browser; constants here because a typo in one
// would be a state the client refuses the whole row over.
const (
	languageFormDeprecated = "deprecated"
	languageFormRefused    = "refused"
)

// evaluateLanguageStatusExpression is the languageStatus builtin. The capability
// was checked before it ran.
//
// Every registered form is listed, in the table's rule order, whether or not
// anything uses it: "no use" is an answer, so its `uses` is an empty list and
// never a null. A form is listed from the TABLE rather than from the uses, so a
// form nothing loaded spells still shows its window -- which is the version of
// this page a person wants to read BEFORE they write the form.
func (e *MemQLEngine) evaluateLanguageStatusExpression(_ context.Context) ([]memorynodes.MemoryNode, error) {
	usesByRule := map[string][]languageStatusUse{}
	if e != nil {
		// DeprecatedUses is sorted by file, then line, then column, and the
		// grouping below keeps that order within each form.
		for _, use := range e.DeprecatedUses() {
			usesByRule[use.Rule] = append(usesByRule[use.Rule], languageStatusUse{
				File: use.File, Line: use.Line, Column: use.Column, Text: use.Text,
			})
		}
	}

	// ONE READ OF THE RELEASE for every form, so a table with two forms cannot
	// answer from two different releases.
	release := deprecation.Current()

	registered := deprecation.Forms()
	forms := make([]languageStatusForm, 0, len(registered))
	for _, f := range registered {
		uses := usesByRule[f.Rule]
		if uses == nil {
			uses = []languageStatusUse{}
		}
		state := languageFormDeprecated
		if f.RefusesAt(release) {
			state = languageFormRefused
		}
		forms = append(forms, languageStatusForm{
			Rule:         f.Rule,
			Spelling:     f.Spelling,
			Replacement:  f.Replacement,
			Migrator:     f.Migrator,
			DeprecatedIn: f.DeprecatedIn,
			RefusedFrom:  f.RefusedFrom(),
			State:        state,
			Uses:         uses,
		})
	}

	payload, err := json.Marshal(languageStatus{
		Language:                languageParser.LanguageVersion,
		Edition:                 languageParser.Edition,
		Status:                  languageParser.EditionStatus,
		GrammarVersion:          languageParser.GrammarVersion,
		EditorRelease:           languageParser.EditorRelease,
		DeprecationWindowMinors: deprecation.MinimumMinorReleases,
		Forms:                   forms,
	})
	if err != nil {
		return nil, fmt.Errorf("languageStatus: %w", err)
	}
	return []memorynodes.MemoryNode{{
		ID:        LanguageStatusConcept,
		Concept:   LanguageStatusConcept,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payload,
	}}, nil
}
