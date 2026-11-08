package dslconformance

// createAuthoringConstruct is the one mutation that writes v1:authoring:construct
// rows, so its `kind` argument and the concept's `kind` field must name ONE set:
// the contract identity_audit_enum_contract_test.go holds for the identity logs.
//
// They named two (memql#6022 follow-up). The concept listed `rule` and the
// mutation did not, so a runtime-authored routing rule's construct could not be
// written with its true kind, and no routing rule was ever armed. The other
// direction fails one layer later instead: a kind the mutation accepts and the
// concept does not passes the argument check and is refused at the row insert.

import (
	"fmt"
	"testing"

	languageAst "github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/memql/dslimports"
	"github.com/znasllc-io/memql/dsl"
)

func TestAuthoringConstructKindEnumsAgreeBetweenMutationAndConcept(t *testing.T) {
	tree, err := dslimports.Load(dsl.Tree())
	if err != nil {
		t.Fatalf("load tree: %v", err)
	}
	mutation, concept := enumSet{}, enumSet{}
	for _, file := range tree.Files {
		if file == nil {
			continue
		}
		for _, def := range file.Definitions {
			switch d := def.(type) {
			case *languageAst.FunctionDef:
				if d.Name != "createAuthoringConstruct" || d.Type != languageAst.FunctionTypeMutation || d.ArgsSchema == nil {
					continue
				}
				for _, f := range d.ArgsSchema.Fields {
					if f.Name != "kind" {
						continue
					}
					for _, v := range f.Enum {
						mutation[fmt.Sprint(v)] = true
					}
				}
			case *languageAst.ConceptDecl:
				if d.Name != "construct" {
					continue
				}
				for _, p := range d.Properties {
					if p.Name != "kind" || p.Type == nil || p.Type.Kind != "enum" {
						continue
					}
					for _, v := range p.Type.EnumValues {
						concept[v] = true
					}
				}
			}
		}
	}
	if len(mutation) == 0 || len(concept) == 0 {
		t.Fatalf("createAuthoringConstruct.kind (%v) or construct.kind (%v) declares no enum -- the field lost "+
			"its enum type or the parse narrowed; this gate cannot pass vacuously", mutation.sorted(), concept.sorted())
	}
	for _, v := range mutation.sorted() {
		if !concept[v] {
			t.Errorf("createAuthoringConstruct.kind accepts %q but concept construct.kind does not (%v); "+
				"the argument passes and the row insert refuses it", v, concept.sorted())
		}
	}
	for _, v := range concept.sorted() {
		if !mutation[v] {
			t.Errorf("concept construct.kind stores %q but createAuthoringConstruct.kind never accepts it (%v); "+
				"a construct of that kind cannot be written through the only writer", v, mutation.sorted())
		}
	}
}
