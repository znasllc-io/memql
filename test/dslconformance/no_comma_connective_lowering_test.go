package dslconformance

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/compiler"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// TestNoLoweringCarriesTheCommaConnective is the memql#5439 lock over every
// tree the repository carries: no struct-form lowering emits `,` as OR.
//
// The procedural grammar -- what the rewriter lowers a query and a mutation
// into, and what a client sends to Execute -- refuses the comma connective,
// and an author cannot write one in a .memql file either: the edition-2026
// grammar refuses it there first. What neither catches in the author's terms
// is a LOWERING that emits one, the way `;` survived memql#5375's author-side
// retirement in the rewriter's own glue. Such a regression refuses at boot on
// text nobody wrote. This lowers every tracked .memql file outside the
// conformance corpus -- the embedded dsl/ tree, the fleet bundle, every pack
// and every example -- through the loaders' own rewrite chain and parse
// (compiler.ParseFileSource), and names the file whose lowering carries one.
//
// The corpus is excluded because its refused cases carry the comma on purpose;
// dsl/_reference is excluded because its skeletons are retired forms kept as
// don't-do-this examples.
func TestNoLoweringCarriesTheCommaConnective(t *testing.T) {
	root := repoRoot(t)
	out, err := exec.Command("git", "-C", root, "ls-files", "*.memql").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	frontEnd, err := langparser.FrontEndFor(langparser.Edition)
	if err != nil {
		t.Fatalf("the edition %s front end: %v", langparser.Edition, err)
	}

	checked, parsed := 0, 0
	for _, rel := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if rel == "" || strings.HasPrefix(rel, "test/conformance/") || strings.HasPrefix(rel, "dsl/_reference/") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		prepared, err := frontEnd.Prepare(string(src))
		if err != nil {
			t.Errorf("%s: the edition %s front end refused it: %v", rel, langparser.Edition, err)
			continue
		}
		checked++
		_, err = compiler.ParseFileSource(prepared)
		switch {
		case err == nil:
			parsed++
		case isCommaConnectiveRefusal(err):
			t.Errorf("%s: its lowering carries the retired `,` connective (memql#5439) -- the rewriter, not the author, wrote it: %v", rel, err)
		}
		// Any other error is another gate's: a file of dedicated-parser
		// constructs lowers to nothing (ErrEmptyInput), and the loaders' own
		// tests own the rest.
	}

	// A checker reports its own coverage: a walk that found nothing, or
	// lowered nothing, passes vacuously.
	if checked < 300 || parsed < 300 {
		t.Fatalf("checked %d files and fully parsed the lowering of %d: the walk found less of the tree than it exists", checked, parsed)
	}
	t.Logf("lowered %d .memql files; %d parsed whole under the grammar that refuses `,` as OR", checked, parsed)

	// The detector, shown to fire: the same parse over a lowering that does
	// carry the comma refuses it with the rule this gate keys on.
	_, err = compiler.ParseFileSource("func (Query) q(ctx any) (any, error) {\n  return (a == args.a, b == args.b), nil\n}\n")
	if !isCommaConnectiveRefusal(err) {
		t.Fatalf("a lowering with `,` as OR must be refused as retired_comma_connective, got: %v", err)
	}
}

// isCommaConnectiveRefusal reports whether err is the retired `,` connective's
// refusal, by its rule code.
func isCommaConnectiveRefusal(err error) bool {
	var rf *langparser.RetiredFormError
	return errors.As(err, &rf) && rf.RuleCode() == "retired_comma_connective"
}
