package functions_test

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/functions"
	"github.com/znasllc-io/memql/component/language/parser"
)

// The catalog's retired maps and the parser's refusal table both name retired
// spellings, and they must say the same thing about each one. The parser
// refuses a spelling at load; EvalExpr, the compiler and the in-process check
// read the catalog's maps; Sense's hover and the language reference print the
// parser's table. One retired spelling has one replacement wherever it is read.
func TestRetiredSpellingsAgreeWithTheParser(t *testing.T) {
	forms := parser.V1RetiredForms()
	byRule := map[string]parser.RetiredForm{}
	bySpelling := map[string]parser.RetiredForm{}
	for _, f := range forms {
		byRule[f.Rule] = f
		bySpelling[f.Spelling] = f
	}
	if len(forms) == 0 || len(functions.RetiredFunctions()) == 0 {
		t.Fatal("a retired table is empty: this test examined nothing, which is not a pass")
	}

	// A retired function is refused under retired_<name>_call, spelled as a
	// call of that name, with the replacement the catalog gives.
	for name, repl := range functions.RetiredFunctions() {
		form, ok := byRule["retired_"+name+"_call"]
		if !ok {
			t.Errorf("the catalog retires %s(), and the parser has no retired_%s_call to refuse it", name, name)
			continue
		}
		if !strings.HasPrefix(form.Spelling, name+"(") {
			t.Errorf("retired_%s_call is spelled %q, not as a call of %s", name, form.Spelling, name)
		}
		if form.Replacement != repl {
			t.Errorf("%s(): the catalog says write %q and the parser says write %q", name, repl, form.Replacement)
		}
	}

	// A spelling a catalog entry replaces is refused with exactly that
	// spelling, and the replacement calls the entry.
	for _, f := range functions.Catalog() {
		call := f.Name + "("
		if f.Receiver != "" {
			call = "." + call
		}
		for _, spelling := range f.Retired {
			form, ok := bySpelling[spelling]
			if !ok {
				t.Errorf("%s replaces %q, and the parser has no form spelled that way", f.Key(), spelling)
				continue
			}
			if !strings.Contains(form.Replacement, call) {
				t.Errorf("%s replaces %q, and the parser's replacement %q does not call it", f.Key(), spelling, form.Replacement)
			}
		}
	}

	// A retired method's refusal names the catalog's replacement.
	for key, repl := range functions.RetiredMethods() {
		_, name, _ := strings.Cut(key, ".")
		form, ok := byRule["retired_"+name+"_method"]
		if !ok {
			t.Errorf("the catalog retires %s, and the parser has no retired_%s_method to refuse it", key, name)
			continue
		}
		if !strings.Contains(form.Replacement, repl) {
			t.Errorf("%s: the catalog says write %q and the parser's replacement %q does not", key, repl, form.Replacement)
		}
	}

	// The other direction: every call the parser retires is one the catalog
	// knows, as a retired function or as a spelling an entry replaces.
	replaced := map[string]bool{}
	for _, f := range functions.Catalog() {
		for _, spelling := range f.Retired {
			replaced[spelling] = true
		}
	}
	for _, form := range forms {
		name, isCall := strings.CutSuffix(strings.TrimPrefix(form.Rule, "retired_"), "_call")
		if !isCall {
			continue
		}
		if _, ok := functions.RetiredFunctions()[name]; !ok && !replaced[form.Spelling] {
			t.Errorf("the parser retires %s (%s), and the catalog does not know it", form.Rule, form.Spelling)
		}
	}
}
