package main

// deprecated_forms_test.go -- the migration channel a deprecation promises
// (memql#5390).
//
// Every form in a window names the rewrite that carries a tree across it, and
// that name reaches an author in three places at once: the load warning, the
// lint line and the editor's squiggle. A name this tool does not hold is worse
// than no name at all -- it sends the author to a command that refuses, and the
// deprecation's whole offer is that there is a mechanical way out. The string
// lives in the registry (component/language/deprecation) because a leaf package
// cannot import this tool; this test is where the two meet.

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/deprecation"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

func TestEveryDeprecatedFormNamesARewriteThisToolHas(t *testing.T) {
	for _, f := range deprecation.Forms() {
		name, ok := strings.CutPrefix(f.Migrator, "memqlmigrate --rewrite=")
		if !ok {
			t.Errorf("form %s: Migrator %q is not a `memqlmigrate --rewrite=<name>` invocation, so an author "+
				"reading the warning has no command to run", f.Rule, f.Migrator)
			continue
		}
		if _, err := resolveRewrites(langparser.Edition, []string{name}); err != nil {
			t.Errorf("form %s names `%s`, which this tool does not hold: %v", f.Rule, f.Migrator, err)
		}
	}
}

// The rewrite a form names must actually write the form's replacement. A
// registered name that resolves and then leaves the spelling alone is the same
// dead end one letter later.
func TestTheArrayFormsRewriteWritesItsReplacement(t *testing.T) {
	f, ok := deprecation.Lookup(deprecation.ArrayType)
	if !ok {
		t.Fatalf("the %s form is not registered", deprecation.ArrayType)
	}
	name, _ := strings.CutPrefix(f.Migrator, "memqlmigrate --rewrite=")
	pipeline, err := resolveRewrites(langparser.Edition, []string{name})
	if err != nil {
		t.Fatalf("resolveRewrites(%q): %v", name, err)
	}
	const src = "concept ticket {\n  tags array(string)\n}\n"
	got := []byte(src)
	for _, r := range pipeline {
		if got, err = r.plain(got); err != nil {
			t.Fatalf("%s: %v", r.name, err)
		}
	}
	if want := "concept ticket {\n  tags []string\n}\n"; string(got) != want {
		t.Fatalf("`%s` rewrote\n %q\nto\n %q\nwant\n %q", f.Migrator, src, got, want)
	}
	// And what it wrote no longer spells the form -- otherwise the warning
	// would survive the fix the warning itself recommends.
	if uses := langparser.ScanDeprecatedUses(string(got)); len(uses) != 0 {
		t.Fatalf("the rewritten source still spells the form: %+v", uses)
	}
}
