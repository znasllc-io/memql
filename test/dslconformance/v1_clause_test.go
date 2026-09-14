package dslconformance

// v1_clause_test.go -- reading an edition-2026 filter as a tree, for the gates
// in this package that ask what a filter COMPARES (epic memql#5363, task
// memql#5368).
//
// A legacy filter names a payload field bare, so these gates found a
// comparison with a regex anchored on the bare name. An edition-2026 filter
// names every field through the lambda parameter -- `row.deploymentId ==
// args.x` -- where a bare-name regex cannot see it at all, so a v1 clause is
// parsed and the comparison is found on the tree.

import (
	"regexp"
	"strings"

	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/dslclause"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// rowFieldComparedToArg reports whether a filter clause compares the row's
// `field` against a caller-supplied argument: `field == args.x` in a legacy
// clause (the bare spelling, left to right, as these gates always read it),
// and in an edition-2026 clause `<param>.field == args.x` in EITHER operand
// order, or `<param>.field in args.xs` when withIn is set.
//
// A v1 clause that does not parse is read as text, for `<anything>.field ==
// args.`: the loader refuses it anyway, and the gate must not pass over a
// comparison because the parse failed.
func rowFieldComparedToArg(clause, field string, withIn bool) bool {
	bare := regexp.MustCompile(`(?:^|[^.\w])` + regexp.QuoteMeta(field) + `[ \t]*==[ \t]*args\.[A-Za-z_]`)
	if !dslclause.OpensLambda(clause) {
		return bare.MatchString(clause)
	}
	lam, err := langparser.ParseV1Lambda(clause)
	if err != nil || len(lam.Params) != 1 {
		dotted := regexp.MustCompile(`\.\??` + regexp.QuoteMeta(field) + `[ \t]*==[ \t]*args\.[A-Za-z_]`)
		return dotted.MatchString(clause)
	}
	param := lam.Params[0]
	isRowField := func(n ast.ExpressionNode) bool {
		root, fields, ok := ast.MemberPath(ast.Unparen(n))
		return ok && root == param && len(fields) == 1 && fields[0] == field
	}
	isArg := func(n ast.ExpressionNode) bool {
		root, fields, ok := ast.MemberPath(ast.Unparen(n))
		return ok && root == "args" && len(fields) > 0
	}
	found := false
	ast.WalkV1(lam.Body, func(n ast.ExpressionNode) bool {
		if found {
			return false
		}
		if b, ok := n.(*ast.BinaryExpr); ok {
			switch {
			case b.Op == "==" && ((isRowField(b.Left) && isArg(b.Right)) || (isArg(b.Left) && isRowField(b.Right))):
				found = true
			case b.Op == "in" && withIn && isRowField(b.Left) && isArg(b.Right):
				found = true
			}
		}
		return !found
	})
	return found
}

// filterClauseTexts returns the text of every filter clause in src -- what
// follows the keyword, plus every continuation line dslclause.ClauseExtent
// folds into it -- each joined onto one line. src must be comment-blanked.
//
// It replaces the single-line `^\s*filter\s+(.+)$` read these gates carried:
// that read has missed a wrapped clause's later lines since memql#4123 made
// the normaliser fold them, and the edition-2026 codemod wraps every long
// filter at its top-level `&&`.
func filterClauseTexts(src string) []string {
	var out []string
	lines := strings.Split(src, "\n")
	for i := 0; i < len(lines); i++ {
		trim := strings.TrimSpace(lines[i])
		if !dslclause.StartsWith(trim, "filter") {
			continue
		}
		last := dslclause.ClauseExtent(lines, i)
		parts := []string{strings.TrimSpace(strings.TrimPrefix(trim, "filter"))}
		for j := i + 1; j <= last; j++ {
			if t := strings.TrimSpace(lines[j]); t != "" {
				parts = append(parts, t)
			}
		}
		out = append(out, strings.Join(parts, " "))
		i = last
	}
	return out
}

// bareIdents returns every identifier in text that is not a member read --
// not preceded by a `.`. In an edition-2026 filter every field is
// `row.<field>`, so what remains is a construct reference (`isX(row)`), a
// parameter, or a reserved root.
func bareIdents(re *regexp.Regexp, text string) []string {
	var out []string
	for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
		if m[2] > 0 && text[m[2]-1] == '.' {
			continue
		}
		out = append(out, text[m[2]:m[3]])
	}
	return out
}
