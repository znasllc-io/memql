package annotations

// Receiver names a place an annotation can be written: a construct
// (`query`, `concept`, ...), the body of a concept (`@relationship`), or a
// field of one of the field lists (a concept field, an args field, a tool,
// prompt or builtin field). The string value is the receiver's stable name,
// and it is also the key of ByReceiver.
type Receiver string

// The receivers, one per place the parser reads annotations. A construct
// receiver is checked by its construct parser; a field receiver by the field
// list that reads the field; Concept, ConceptBody and ConceptField by the
// concept translator in component/database.
const (
	Query        Receiver = "Query"
	Mutation     Receiver = "Mutation"
	Logic        Receiver = "Logic"
	Automation   Receiver = "Automation"
	Action       Receiver = "Action"
	Capability   Receiver = "Capability"
	Spec         Receiver = "Spec" // spec and trait: one parser, one set
	Tool         Receiver = "Tool"
	Builtin      Receiver = "Builtin"
	Prompt       Receiver = "Prompt"
	Provider     Receiver = "Provider"
	Shape        Receiver = "Shape"
	Policy       Receiver = "Policy"
	Rule         Receiver = "Rule"
	Seed         Receiver = "Seed"
	Concept      Receiver = "Concept"
	ConceptBody  Receiver = "ConceptBody"
	ConceptField Receiver = "ConceptField"
	ArgsField    Receiver = "ArgsField"
	ToolField    Receiver = "ToolField"
	PromptField  Receiver = "PromptField"
	BuiltinField Receiver = "BuiltinField"
)

// receiverOrder is the order Receivers and Placements report in: the four
// function constructs, the declarative constructs, the concept, then the
// field lists.
var receiverOrder = []Receiver{
	Query, Mutation, Logic, Automation, Action, Capability, Spec, Tool, Builtin,
	Prompt, Provider, Shape, Policy, Rule, Seed, Concept, ConceptBody,
	ConceptField, ArgsField, ToolField, PromptField, BuiltinField,
}

// receiverPhrases are the receivers in words, with their article, for a
// refusal that has to say where the annotation was written.
var receiverPhrases = map[Receiver]string{
	Query:        "a query",
	Mutation:     "a mutation",
	Logic:        "a logic",
	Automation:   "an automation",
	Action:       "an action",
	Capability:   "a capability",
	Spec:         "a spec or trait",
	Tool:         "a tool",
	Builtin:      "a builtin",
	Prompt:       "a prompt",
	Provider:     "a provider",
	Shape:        "a shape",
	Policy:       "a policy",
	Rule:         "a rule",
	Seed:         "a seed",
	Concept:      "a concept",
	ConceptBody:  "a concept body",
	ConceptField: "a concept field",
	ArgsField:    "an args field",
	ToolField:    "a tool field",
	PromptField:  "a prompt field",
	BuiltinField: "a builtin field",
}

// Receivers returns every receiver, in the order Placements reports them.
// The result is a fresh slice the caller may keep.
func Receivers() []Receiver {
	return append([]Receiver(nil), receiverOrder...)
}

// Phrase returns the receiver in words, with its article: "a query", "an args
// field". An unknown receiver reads as its own name, so a refusal about one
// still says something.
func (r Receiver) Phrase() string {
	if p, ok := receiverPhrases[r]; ok {
		return p
	}
	return "a " + string(r)
}

// isField reports whether r is a field receiver -- a field of a concept, an
// args block, or a tool / prompt / builtin body -- rather than a construct or
// the concept body.
func (r Receiver) isField() bool {
	switch r {
	case ConceptField, ArgsField, ToolField, PromptField, BuiltinField:
		return true
	}
	return false
}

// receiverIndex is each receiver's position in receiverOrder.
var receiverIndex = func() map[Receiver]int {
	out := make(map[Receiver]int, len(receiverOrder))
	for i, r := range receiverOrder {
		out[r] = i
	}
	return out
}()
