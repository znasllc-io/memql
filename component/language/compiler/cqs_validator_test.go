package compiler

import "testing"

func TestValidateCQS_QueryCallingMutationIsRejected(t *testing.T) {
	source := `
args { id string @required }
func (Mutation) mutationCreateLog(args any) {
	insert("v1:log", { id: args.id, payload: { value: "x" } })
}

args { id string @required }
func (Query) queryGetUserAndLog(args any) {
	mutationCreateLog(id=args.id)
}`

	_, err := CompileSource(source)
	if err == nil {
		t.Fatalf("expected CQS violation")
	}
	if _, ok := err.(*CQSViolation); !ok {
		t.Fatalf("expected CQSViolation, got %T (%v)", err, err)
	}
}

func TestValidateCQS_QueryCallingQueryIsAllowed(t *testing.T) {
	source := `
args { id string @required }
func (Query) queryById(args any) {
	concept==v1:user;id==args.id
}

args { id string @required }
func (Query) queryWrapper(args any) {
	queryById(id=args.id)
}`

	if _, err := CompileSource(source); err != nil {
		t.Fatalf("expected no CQS violation, got %v", err)
	}
}

// TestValidateCQS_SpecCallingMutationIsRejected was retired alongside
// the spec/trait CompileSource path. Specs + traits now have a
// dedicated parser (component/memql/spec_parser.go) that builds Specs
// directly from `spec NAME { <bool-expr> }` syntax. CompileSource no
// longer handles spec sources, so the "mixed mutation + spec file"
// fixture this test used can't be parsed by this path -- and the CQS
// rule "spec must not mutate" is now enforced structurally: spec
// bodies must be boolean expressions, mutations don't return bool,
// so the call gets rejected at the expression validator instead of
// the CQS layer.
