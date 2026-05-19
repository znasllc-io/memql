package memql

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// validateNamingPrefix enforces the naming-prefix convention at
// engine load time. Query / mutation / spec functions must use
// their kind's prefix (query*, mutation*, spec*). A mismatch is a
// hard error in strict mode (the default); the MEMQL_NAMING_LENIENT
// env var downgrades it to a logged warning so a one-off legacy
// file doesn't block engine startup during the migration window.
//
// Strict mode is the default per the locked Decision 3 in
// docs/planning/dsl-engine-mvp-foundation.md. The escape valve
// preserves bootability for authors mid-migration; it is not a
// permanent operating mode.
//
// Naming linting is enforced for canonical struct-form functions.
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

	msg := fmt.Sprintf(
		"function %q violates %s: %s functions must use %q prefix; rename to %q (or set MEMQL_NAMING_LENIENT=1 to downgrade to warning)",
		funcDef.Name, ruleName, kindName, prefix, suggested,
	)

	if namingLenient() {
		namingLogger().Warn("naming lint downgraded by MEMQL_NAMING_LENIENT",
			"component", "memql.namingValidator",
			"rule", ruleName,
			"function", funcDef.Name,
			"suggested", suggested,
		)
		return nil
	}
	return fmt.Errorf("%s", msg)
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

// namingLenient reports whether the strict-mode escape valve is set.
// Cached on first read so we don't syscall on every parse.
//
// Truthy values: 1 / true / yes / on (case-insensitive). Everything
// else (including empty / unset) means strict mode.
var (
	namingLenientOnce sync.Once
	namingLenientVal  bool
)

func namingLenient() bool {
	namingLenientOnce.Do(func() {
		switch strings.ToLower(strings.TrimSpace(os.Getenv("MEMQL_NAMING_LENIENT"))) {
		case "1", "true", "yes", "on":
			namingLenientVal = true
		}
	})
	return namingLenientVal
}

// namingLogger returns a slog handle suitable for naming lint
// warnings. We use the global default logger to avoid plumbing a
// logger through every parser entry; naming lint is a rare event
// on engine boot, not per-request hot-path.
func namingLogger() *slog.Logger {
	return slog.Default()
}
