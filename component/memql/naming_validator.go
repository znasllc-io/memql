package memql

import (
	"fmt"
	"strings"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// validateNamingPrefix enforces the naming-prefix convention at
// engine load time. Query / mutation / spec functions must use
// their kind's prefix (query*, mutation*, spec*). A mismatch is a
// hard load-time error -- there is no escape valve, since the
// project hasn't deployed yet and starts fresh under the new rule.
//
// Naming linting applies to canonical struct-form functions.
// Procedural-form receivers (legacy `func (Query|Mutation|Spec) NAME`)
// already trip the dedicated parsers' rewriters; nothing reaches
// this validator with a procedural-form name today.
func validateNamingPrefix(funcDef *languageParser.FunctionDef) error {
	if funcDef == nil || funcDef.Name == "" {
		return nil
	}

	prefix, kindName, ruleName, ok := namingPrefixForKind(funcDef.Type)
	if !ok {
		return nil
	}
	if strings.HasPrefix(funcDef.Name, prefix) {
		return nil
	}

	// Build the suggested rename: prefix + Pascal-cased original.
	suggested := prefix + strings.ToUpper(funcDef.Name[:1]) + funcDef.Name[1:]

	return fmt.Errorf(
		"function %q violates %s: %s functions must use %q prefix; rename to %q",
		funcDef.Name, ruleName, kindName, prefix, suggested,
	)
}

// namingPrefixForKind returns the canonical prefix + rule name for
// a function kind. The OK return distinguishes "no rule for this
// kind" from "the empty prefix" (which would never match anyway).
func namingPrefixForKind(t languageParser.FunctionType) (prefix, kindName, ruleName string, ok bool) {
	switch t {
	case languageParser.FunctionTypeQuery:
		return "query", "Query", "naming.query-prefix", true
	case languageParser.FunctionTypeMutation:
		return "mutation", "Mutation", "naming.mutation-prefix", true
	case languageParser.FunctionTypeSpec:
		return "spec", "Spec", "naming.spec-prefix", true
	default:
		return "", "", "", false
	}
}
