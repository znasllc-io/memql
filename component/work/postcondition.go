package work

// postcondition.go -- every step has a postcondition (design record
// docs/superpowers/specs/2026-09-05-work-spine-design.md, section E
// "Verify").
//
// A spec over the result, a schema, or a deterministic check. Most
// deterministic steps get one for free: a mutation's postcondition is
// that the row exists with the fields it wrote, a query's is its shape.
// A postcondition failure is a FAILED step with the symptom `contract`,
// which the classifier's table already encodes.
//
// THE RULE THAT MAKES THIS WORTH ANYTHING: a step with no postcondition
// cannot be called deterministic. Without it "deterministic" degrades
// into "we did not check" -- the step is replayed on the strength of a
// promise nobody verified, and a template that silently stopped working
// keeps reporting success. RequirePostcondition is that rule.

import "fmt"

// Postcondition kinds.
const (
	// PostconditionSpec names a DSL spec evaluated over the result.
	PostconditionSpec = "spec"
	// PostconditionSchema names a shape the result must satisfy.
	PostconditionSchema = "schema"
	// PostconditionCheck names a deterministic engine-side check.
	PostconditionCheck = "check"
)

// Postcondition is the declared or derived contract of one step. It is
// written onto v1:work:step.postcondition with `passed` and `message`
// filled in at the receipt.
type Postcondition struct {
	// Kind is one of the Postcondition* constants.
	Kind string `json:"kind,omitempty"`
	// Ref names the spec, shape or check.
	Ref string `json:"ref,omitempty"`
	// Passed is filled in at the receipt.
	Passed bool `json:"passed,omitempty"`
	// Message explains a failure.
	Message string `json:"message,omitempty"`
}

// Declared reports whether this postcondition says anything.
func (p Postcondition) Declared() bool { return p.Kind != "" }

// DerivePostcondition returns the free postcondition for a step, when
// there is one. ok=false means the template must declare one -- it is
// not an error here, because whether that is fatal depends on the step's
// KIND, which is RequirePostcondition's question.
func DerivePostcondition(stepType, target string, reg Registry) (Postcondition, bool) {
	t, known := reg[target]
	switch stepType {
	case "mutation":
		concept := ""
		if known {
			concept = t.Concept
		}
		if concept == "" {
			return Postcondition{}, false
		}
		// The row exists, with the fields the mutation wrote. The engine
		// already knows both halves, so this needs nothing from the author.
		return Postcondition{Kind: PostconditionCheck, Ref: "rowWritten:" + concept}, true
	case "query", "shape":
		if !known {
			return Postcondition{}, false
		}
		// A read's contract is the shape it projects.
		return Postcondition{Kind: PostconditionSchema, Ref: target}, true
	}
	return Postcondition{}, false
}

// RequirePostcondition is the spec's rule. Only a DETERMINISTIC step owes
// a postcondition: a reasoning step's output is judged by the loop, and a
// human step's by a person.
func RequirePostcondition(kind Kind, pc Postcondition, stepKey string) error {
	if kind != KindDeterministic || pc.Declared() {
		return nil
	}
	return fmt.Errorf("work: step %q is deterministic and declares no postcondition -- a deterministic step is one whose result is CHECKED, and without a postcondition a template that quietly stopped working still reports success; declare a spec, a schema or a check, or let the kind derive to reasoning", stepKey)
}
