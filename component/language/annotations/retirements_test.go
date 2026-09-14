package annotations

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// TestRetirementsReadTheTablesCheckReads: Retirements is a read-only view of
// the tables Check consults, so every entry it reports is refused by Check
// with its code and hint, on every receiver it names -- and no table entry is
// missing from it. The generated attribute matrix renders its Retired table
// from this view, so a view that disagreed with Check would publish a
// retirement the parser does not enforce, or hide one it does.
func TestRetirementsReadTheTablesCheckReads(t *testing.T) {
	all := Retirements()
	if want := len(retiredEverywhere) + len(retiredOn) + 1; len(all) != want {
		t.Fatalf("Retirements() has %d entries, the tables %d (every name, every receiver-specific entry and the @use family)", len(all), want)
	}
	for i, ret := range all {
		if ret.Hint == "" {
			t.Errorf("%+v carries no hint", ret)
		}
		if i > 0 {
			prev := all[i-1]
			if prev.Name > ret.Name || (prev.Name == ret.Name && retirementRank(prev) >= retirementRank(ret)) {
				t.Errorf("Retirements out of order: %s/@%s before %s/@%s", prev.Receiver, prev.Name, ret.Receiver, ret.Name)
			}
		}
		name := ret.Name
		if ret.Prefix {
			name += "AnyMember"
		}
		receivers := []Receiver{ret.Receiver}
		if ret.Receiver == "" {
			receivers = Receivers()
		}
		refused := 0
		for _, r := range receivers {
			if _, live := Lookup(r, name); live {
				continue // an everywhere retirement leaves the receivers that accept the name alone
			}
			refused++
			ref := Check(r, Use{Name: name, Form: FormFlag})
			if ref == nil || ref.Code != CodeRetired || !strings.Contains(ref.Message, ret.Hint) {
				t.Errorf("%s @%s: Retirements reports it retired, but Check answers %v", r, name, ref)
			}
		}
		if refused == 0 {
			t.Errorf("@%s is reported retired on %q, where every receiver accepts it", ret.Name, ret.Receiver)
		}
	}

	// The family is a prefix rule, and the view says so rather than listing
	// members: a member nobody listed is refused the same way.
	var family *Retirement
	for i := range all {
		if all[i].Prefix {
			family = &all[i]
		}
	}
	if family == nil || !isUseFamily(family.Name+"Concept") || isUseFamily(family.Name+"r") {
		t.Errorf("the @use family entry does not describe the rule isUseFamily applies: %+v", family)
	}

	all[0].Name = "mutated"
	if Retirements()[0].Name == "mutated" {
		t.Error("Retirements returned the registry's own slice")
	}
}

// TestRefusalCodesListEveryCode: RefusalCodes names every Code* constant
// check.go declares, in declaration order, each with a one-sentence meaning.
// The constants are read from the source, so a seventh code added to check.go
// without a meaning fails here instead of going missing from the generated
// attribute matrix's Refusals table.
func TestRefusalCodesListEveryCode(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "check.go", nil, 0)
	if err != nil {
		t.Fatalf("parse check.go: %v", err)
	}
	var declared []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, ident := range vs.Names {
				if !strings.HasPrefix(ident.Name, "Code") || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok {
					t.Fatalf("%s is not a string literal", ident.Name)
				}
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("%s: %v", ident.Name, err)
				}
				declared = append(declared, value)
			}
		}
	}
	if len(declared) < 6 {
		t.Fatalf("found only %d Code* constants in check.go -- the declaration moved under this test", len(declared))
	}

	var listed []string
	for _, c := range RefusalCodes() {
		listed = append(listed, c.Code)
		if c.Meaning == "" || !strings.HasSuffix(c.Meaning, ".") {
			t.Errorf("%s: the meaning must be a sentence, got %q", c.Code, c.Meaning)
		}
	}
	if strings.Join(listed, ",") != strings.Join(declared, ",") {
		t.Errorf("RefusalCodes() lists %v; check.go declares %v", listed, declared)
	}
}
