package main

// allowed_roles.go is the `--rewrite=allowed-roles` codemod: the migration
// channel of the deprecated_allowed_roles window (memql#5438).
//
// @allowedRoles compared one role string whose meaning depended on the
// caller, so a list was either AGENT roles or PERSON roles. The rewrite
// carries each use across to the annotation that says what it meant --
// @requiresAgentRole with the same values, or @requiresRank at the list's
// lowest rung -- and LEAVES, reporting why on stderr, every list it cannot
// carry across exactly: one mixing the two, one naming a value neither
// vocabulary knows (a roleSlug, a custom role, a typo), one that is not a
// floor, and one whose construct already carries the replacement. The
// decision is the parser's (langparser.RewriteAllowedRoles), the same one the
// language server's quick fix makes, and the two vocabularies it decides with
// are read from their declarations in the embedded tree -- dsl/agents' agent
// concept and dsl/rbac's role seeds -- rather than restated here.
//
// A path rewrite rather than a plain one only so a report can name the file.

import (
	"fmt"
	"io"
	"os"
	"sync"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// allowedRolesReport is where the rewrite says what it left; stderr, so a
// report never lands in a -w diff or a piped file. A variable so a test can
// read it.
var allowedRolesReport io.Writer = os.Stderr

// allowedRolesVocabulary is read once per run from the embedded tree.
var allowedRolesVocabulary = sync.OnceValues(func() (*langparser.RoleVocabulary, error) {
	return langparser.RoleVocabularyFromTree(memqldsl.Tree())
})

func rewriteAllowedRoles(path string, src []byte) ([]byte, error) {
	vocabulary, err := allowedRolesVocabulary()
	if err != nil {
		return nil, fmt.Errorf("allowed-roles: reading the agent roles and the role ladder from the embedded tree: %w", err)
	}
	out, left := langparser.RewriteAllowedRoles(string(src), vocabulary)
	for _, f := range left {
		fmt.Fprintf(allowedRolesReport, "memqlmigrate: %s:%d:%d: left %s as written: %s\n", path, f.Line, f.Column, f.Text, f.Reason)
	}
	return []byte(out), nil
}
