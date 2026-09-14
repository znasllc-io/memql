package dslgate

// v1.go -- how the contract gates read an edition-2026 clause (epic
// memql#5363, task memql#5368).
//
// A filter is a lambda, and each gate reads it as a TREE: the v1 parser
// (ParseV1Lambda) builds it, and the questions -- which fields does it read,
// does every row it admits satisfy a property -- are answered on the tree
// (component/language/ast v1_predicates.go).
//
// Two rules hold across every gate here, and both are the fail-CLOSED choice:
//
//   - A clause that does not parse guarantees nothing. The parse failure
//     itself is reported by the loader; what a gate must never do is read an
//     unparseable clause as an unguarded one that happens to be clean.
//   - A leaf reaches a TEXT leaf predicate (AdminGateLeaf, OwnerScopeLeaf) as
//     its canonical source with string contents blanked, so a quoted word is
//     never read as a reference (`row.note == "actor.userId"` is not an
//     ownership check).

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/dslclause"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// v1Clause parses a clause written in edition 2026. isV1 reports whether the
// clause opens with a lambda header at all; when it does, err is the parse
// failure, if any.
func v1Clause(clause string) (lam *ast.LambdaExpr, isV1 bool, err error) {
	if !dslclause.OpensLambda(clause) {
		return nil, false, nil
	}
	lam, err = languageParser.ParseV1Lambda(clause)
	return lam, true, err
}

// v1LeafText is the text a leaf predicate is asked about for a tree leaf.
func v1LeafText(n ast.ExpressionNode) string {
	return StructureOf(ast.FormatExpr(n))
}

// isReservedRoot reports whether a name is one the engine binds itself. A
// filter lambda whose parameter takes one of these names makes that name a
// ROW: under `actor => actor.userId == args.x`, `actor.userId` is the row's
// column and no caller check at all. The parser accepts the spelling (a spec
// over an @actor shape is written `actor => ...`, so which names a position
// allows is decided where the lambda is bound), so a gate must not read the
// root by its name alone.
func isReservedRoot(name string) bool {
	switch name {
	case "actor", "args", "now", "config", "partition", "trace", "event":
		return true
	}
	return false
}

// UserScopeFields are the payload fields whose comparison against a caller-
// supplied value SELECTS rows by user. UserScopeFieldRe is the same list for
// the `id:` lines of an update block.
var UserScopeFields = []string{"ownerUserId", "userId", "actorUserId", "targetId", "createdBy", "requestedBy"}

// v1ReadsUserScopeField reports whether a v1 filter reads a user-scope field
// of its row: a member chain rooted at the lambda parameter whose first field
// is one of UserScopeFields.
//
// A ROW INTRINSIC IS EXCLUDED: `row.createdBy` reads the createdBy intrinsic
// (memql#2779), which this gate never counted as a user-scope selection, and
// the migration classified every construct exactly as it was classified before.
// A dotted payload path (`row.credentials.userId`) is likewise not the row's
// own column.
func v1ReadsUserScopeField(lam *ast.LambdaExpr) bool {
	if lam == nil || len(lam.Params) != 1 {
		return false
	}
	param := lam.Params[0]
	found := false
	ast.MemberPaths(lam.Body, func(root string, fields []string) {
		if found || root != param || len(fields) == 0 {
			return
		}
		found = isRowUserScopeField(fields[0])
	})
	return found
}

func isRowUserScopeField(name string) bool {
	if isRowIntrinsic(name) {
		return false
	}
	for _, f := range UserScopeFields {
		if name == f {
			return true
		}
	}
	return false
}

// isRowIntrinsic reports whether a field name is a row intrinsic, which a v1
// member path reads through the parameter exactly as it reads a payload field.
// Mirrors component/memql intrinsicFieldRegistry, which this package must not
// import (component/memql imports it).
func isRowIntrinsic(name string) bool {
	switch strings.ToLower(name) {
	case "id", "concept", "type", "createdat", "createdby", "provenance":
		return true
	}
	return false
}

// v1TextReadsUserScopeField is v1ReadsUserScopeField for a clause that does
// not parse: it looks for `<param>.<field>` in the clause text.
func v1TextReadsUserScopeField(clause string) bool {
	params, body, ok := dslclause.SplitLambdaHeader(clause)
	if !ok {
		return false
	}
	body = StructureOf(body)
	for _, p := range params {
		re := regexp.MustCompile(`(^|[^\w.])` + regexp.QuoteMeta(p) + `\s*\.\??\s*([A-Za-z_][A-Za-z0-9_]*)`)
		for _, m := range re.FindAllStringSubmatch(body, -1) {
			if isRowUserScopeField(m[2]) {
				return true
			}
		}
	}
	return false
}

// v1RetiredFormViolation reports the retired form a filter clause spells, if
// its parse fails on one. The v1 grammar cannot express any of the retired
// connectives, so a clause that parses carries none -- the parser, not a
// second list of spellings, is the authority on what a clause may say, and
// that is what lets `(a, b) => ...` and a ternary `?` through where a text
// check would read a comma and a `?.`. A clause that fails to parse for any
// OTHER reason is not this gate's finding: the loader refuses it with the
// parser's own message.
//
// clause is the clause text with its physical lines joined by newlines, the
// first holding what followed the `filter` keyword on clauseLine, so a line
// the parser reports maps back onto the file.
func v1RetiredFormViolation(path string, clauseLine int, clause string) (Violation, bool) {
	_, err := languageParser.ParseV1Lambda(clause)
	var rf *languageParser.RetiredFormError
	if err == nil || !errors.As(err, &rf) {
		return Violation{}, false
	}
	line := clauseLine
	if rf.Parse != nil && rf.Parse.Line > 0 {
		line = clauseLine + rf.Parse.Line - 1
	}
	return Violation{
		Gate:   GateRetiredOperator,
		File:   path,
		Line:   line,
		Detail: fmt.Sprintf("retired filter form (%s): %s is retired in edition 2026 -- write %s", rf.Form.Rule, rf.Form.Spelling, rf.Form.Replacement),
	}, true
}
