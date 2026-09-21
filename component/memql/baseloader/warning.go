package baseloader

// warning.go -- what a DSL load pass found that does not fail it (memql#5390).
//
// A Skip is a construct the load DROPPED, and a strict boot refuses the tree
// for it. A Warning is the other kind of finding: everything loaded, and the
// author is still told what to change before a later release refuses it. The
// first of them is a use of a deprecated language form
// (component/language/deprecation), which loads until its window is spent.
//
// A Warning never counts toward the strict-boot decision: the memql-side
// LoadReport keeps warnings apart from its problems, so a tree that only warns
// still boots.

// Warning is one finding that does not fail the load, positioned in the file
// that carries it.
type Warning struct {
	Component string // loader scope, e.g. "memql.deprecatedForms"
	File      string // origin file path in the DSL tree
	// Line and Column are where the finding sits in File, 1-based, the column
	// counting runes; zero when it has no position.
	Line, Column int
	// Code is the finding's stable rule id -- a deprecated form's rule -- and
	// the field a consumer keys on without parsing prose.
	Code string
	// Message is the text an author reads, the rule id at its end.
	Message string
}
