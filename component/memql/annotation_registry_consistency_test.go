package memql

import (
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/znasllc-io/memql/component/language/annotations"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// annotation_registry_consistency_test.go -- memql#5359.
//
// The registry (component/language/annotations) is the gate for every
// receiver only if every receiver's REAL gate reads it. The test this file
// replaces compared two views of the same registry for four receivers, which
// proved nothing about a gate: both sides aliased one map.
//
// This one drives the gates themselves. For every placement of every receiver
// it builds the smallest construct that carries the placement's example and
// requires the real path to accept it -- the struct-form rewriter and the
// parser for a construct or a field list, and additionally the concept
// translator (memoryNodes.BuildConceptFromDecl) for a concept, its body and
// its fields. It then requires a wrong-form variant and an unknown name to be
// refused by that same path with the registry's code. A receiver whose gate
// stopped reading the registry fails here by name.

// receiverFixture renders the smallest construct of the receiver carrying ann
// in the receiver's position. name is the placement being exercised, for the
// few receivers whose smallest valid construct depends on it.
func receiverFixture(r annotations.Receiver, name, ann string) string {
	switch r {
	case annotations.Query:
		return ann + "\nquery thing probe {\n  filter row.id != \"\"\n}\n"
	case annotations.Mutation:
		return ann + "\nmutate thing probe {\n  args {\n    id string!\n  }\n  update {\n    id: args.id\n  }\n}\n"
	case annotations.Logic:
		return ann + "\nlogic probe {\n  body {\n    return 1\n  }\n}\n"
	case annotations.Automation:
		return ann + "\nautomation probe {\n  step run {\n    mutation createThing (id: \"x\")\n  }\n}\n"
	case annotations.Action:
		return ann + "\naction probe {\n  capability script(script: \"x\")\n}\n"
	case annotations.Capability:
		return ann + "\ncapability integration.probe.run {\n}\n"
	case annotations.Spec:
		return ann + "\nspec thing probe {\n  return active == true\n}\n"
	case annotations.Tool:
		return ann + "\ntool probe {\n  x string\n}\n"
	case annotations.Builtin:
		return ann + "\nbuiltin probe {\n  x string\n}\n"
	case annotations.Prompt:
		return ann + "\nprompt probe {\n  x string\n}\n"
	case annotations.Provider:
		return ann + "\nprovider probe {\n}\n"
	case annotations.Shape:
		return ann + "\nshape thing probe {\n  row.id\n}\n"
	case annotations.Policy:
		return ann + "\npolicy probe { }\n"
	case annotations.Rule:
		// @policy is REQUIRED on a rule, so every other placement needs one
		// beside it -- and @policy's own case must not carry a second.
		if name == "policy" {
			return ann + "\nrule probe { }\n"
		}
		return ann + "\n@policy(\"localFirst\")\nrule probe { }\n"
	case annotations.Seed:
		return ann + "\nseed thing probe {\n  name: \"x\"\n}\n"
	case annotations.Concept:
		return ann + "\nconcept probe {\n  ownerUserId string\n  title string\n  status string\n}\n"
	case annotations.ConceptBody:
		return "concept probe {\n  ownerUserId string\n  " + ann + "\n}\n"
	case annotations.ConceptField:
		return "concept probe {\n  " + conceptFieldLine(name, ann) + "\n}\n"
	case annotations.ArgsField:
		typ := "string"
		if name == "minimum" || name == "maximum" {
			typ = "int"
		}
		return "query thing probe {\n  args {\n    x " + typ + " " + ann + "\n  }\n  filter row.id != \"\"\n}\n"
	case annotations.ToolField:
		return "tool probe {\n  x string " + ann + "\n}\n"
	case annotations.PromptField:
		return "prompt probe {\n  x string " + ann + "\n}\n"
	case annotations.BuiltinField:
		return "builtin probe {\n  x string " + ann + "\n}\n"
	}
	return ""
}

// conceptFieldLine is a field the annotation means something on: a numeric
// bound needs a number, @open needs a block to relax, @variant needs branches.
func conceptFieldLine(name, ann string) string {
	switch name {
	case "minimum", "maximum":
		return "count int " + ann
	case "open":
		return "settings object " + ann + " {\n    known string\n  }"
	case "variant":
		return "cred object " + ann + " {\n    a {\n      kind string\n    }\n  }"
	}
	return "title string " + ann
}

// isConceptReceiver reports whether r is gated by the concept translator.
func isConceptReceiver(r annotations.Receiver) bool {
	return r == annotations.Concept || r == annotations.ConceptBody || r == annotations.ConceptField
}

// runReceiverGate drives src through the receiver's real path: the rewriter
// and the parser, then -- for a concept receiver -- the concept translator.
func runReceiverGate(r annotations.Receiver, src string) error {
	normalised, err := languageParser.NormaliseAll(src)
	if err != nil {
		return err
	}
	file, err := languageParser.ParseFile(normalised)
	if err != nil {
		return err
	}
	if !isConceptReceiver(r) {
		return nil
	}
	declared := false
	for _, def := range file.Definitions {
		if _, ok := def.(*languageParser.ConceptDecl); ok {
			declared = true
		}
	}
	if !declared {
		return errors.New("fixture declared no concept")
	}
	// The PRODUCTION concept path, not a fixed id: BuildUnifiedConcepts reads
	// @version and @namespace to assemble the id before it builds the
	// concept, so an annotation it reads first must still be answered by the
	// registry. The file sits in the directory the @namespace example names.
	tree := fstest.MapFS{"support/concepts.memql": {Data: []byte(src)}}
	_, skips, err := BuildUnifiedConcepts(nil, tree)
	if err != nil {
		return err
	}
	if len(skips) > 0 {
		return skips[0].Err
	}
	return nil
}

// refusalCode returns the registry code an error carries, or "" when it
// carries none. The code must be both reachable as a *Refusal and the last
// bracketed word of the message, so neither a wrapper that drops the cause nor
// one that appends after the code passes.
func refusalCode(err error) string {
	var ref *annotations.Refusal
	if err == nil || !errors.As(err, &ref) {
		return ""
	}
	if !strings.HasSuffix(err.Error(), "["+ref.Code+"]") {
		return ""
	}
	return ref.Code
}

// wrongFormText renders the placement's annotation in a form it does not take,
// choosing among the forms the parser can produce for that name.
func wrongFormText(p annotations.Placement) (string, annotations.Form) {
	type candidate struct {
		form annotations.Form
		text string
	}
	candidates := []candidate{
		{annotations.FormNumber, "@" + p.Name + "(7)"},
		{annotations.FormString, "@" + p.Name + `("zz")`},
		{annotations.FormFlag, "@" + p.Name},
		{annotations.FormStrings, "@" + p.Name + `("zz", "yy")`},
		{annotations.FormEmpty, "@" + p.Name + "()"},
		{annotations.FormBool, "@" + p.Name + "(true)"},
	}
	for _, c := range candidates {
		// @filter captures anything that is not a string or an object as an
		// expression, so a number or a list never reaches the check as one.
		if p.Name == "filter" && c.form != annotations.FormFlag && c.form != annotations.FormString && c.form != annotations.FormEmpty {
			continue
		}
		if p.Forms&c.form == 0 {
			return c.text, c.form
		}
	}
	return "", 0
}

// TestEveryReceiverGateReadsTheRegistry is the behavioural consistency gate:
// every placement is accepted by its receiver's real gate, and that gate
// refuses a wrong form and an unknown key with the registry's code.
func TestEveryReceiverGateReadsTheRegistry(t *testing.T) {
	covered := map[annotations.Receiver]bool{}
	for _, p := range annotations.Placements() {
		p := p
		covered[p.Receiver] = true
		t.Run(string(p.Receiver)+"/"+p.Name, func(t *testing.T) {
			src := receiverFixture(p.Receiver, p.Name, p.Example)
			if src == "" {
				t.Fatalf("no fixture for receiver %s", p.Receiver)
			}
			if err := runReceiverGate(p.Receiver, src); err != nil {
				t.Fatalf("the example %s is refused by the %s gate:\n%s\nerror: %v", p.Example, p.Receiver, src, err)
			}

			text, form := wrongFormText(p)
			if text == "" {
				t.Fatalf("no wrong form is producible for %s @%s", p.Receiver, p.Name)
			}
			err := runReceiverGate(p.Receiver, receiverFixture(p.Receiver, p.Name, text))
			if got := refusalCode(err); got != annotations.CodeForm {
				t.Errorf("%s written %s (%s) on %s: code %q, want %q\nerror: %v", p.Name, text, form, p.Receiver, got, annotations.CodeForm, err)
			}

			if p.Forms&annotations.FormKeywords != 0 {
				keyed := "@" + p.Name + `(zzUnknownKey="x")`
				err := runReceiverGate(p.Receiver, receiverFixture(p.Receiver, p.Name, keyed))
				if got := refusalCode(err); got != annotations.CodeKey {
					t.Errorf("%s on %s: code %q, want %q\nerror: %v", keyed, p.Receiver, got, annotations.CodeKey, err)
				}
				// A key written in the wrong SHAPE: a flag given a value, a
				// valued key written bare. The parser stores a bare key as
				// `true`, so a reader that only looked at the key's NAME let
				// `@rateLimit(maxCalls, periodSeconds)` register a tool with
				// no limit at all.
				for _, k := range p.Keys {
					shaped := "@" + p.Name + "(" + k.Name + ")"
					if k.Type == "flag" {
						shaped = "@" + p.Name + "(" + k.Name + `="zz")`
					}
					err := runReceiverGate(p.Receiver, receiverFixture(p.Receiver, p.Name, shaped))
					if got := refusalCode(err); got != annotations.CodeKey {
						t.Errorf("%s on %s (key %s is %s): code %q, want %q\nerror: %v", shaped, p.Receiver, k.Name, k.Type, got, annotations.CodeKey, err)
					}
				}
			}
		})
	}
	for _, r := range annotations.Receivers() {
		if !covered[r] {
			t.Errorf("receiver %s has no placement, so this test drives no gate for it", r)
		}
	}
}

// TestEveryReceiverGateRefusesAnUnknownName: an annotation no receiver knows
// is refused by every receiver's gate as annotation_unknown -- the construct
// kinds that used to tolerate one (prompt and builtin fields, the action
// preamble, a capability) included.
func TestEveryReceiverGateRefusesAnUnknownName(t *testing.T) {
	const unknown = "@zzUnknownAnnotation"
	for _, r := range annotations.Receivers() {
		src := receiverFixture(r, "", unknown)
		if r == annotations.ConceptBody {
			// An annotation in body position that is not @relationship is the
			// next field's prefix annotation, so the concept-field gate is the
			// one that reads it.
			src = "concept probe {\n  " + unknown + "\n  title string\n}\n"
		}
		err := runReceiverGate(r, src)
		if got := refusalCode(err); got != annotations.CodeUnknown {
			t.Errorf("%s: %s -- code %q, want %q\nerror: %v", r, unknown, got, annotations.CodeUnknown, err)
			continue
		}
		if r != annotations.ConceptBody && !strings.Contains(err.Error(), r.Phrase()) {
			t.Errorf("%s: the refusal does not name the receiver: %v", r, err)
		}
	}
}

// TestRetiredAndMisplacedReachTheRealGates: the retirement hints moved out of
// the per-construct parsers and loaders into the registry; each still reaches
// an author through the gate that used to carry it.
func TestRetiredAndMisplacedReachTheRealGates(t *testing.T) {
	cases := []struct {
		receiver annotations.Receiver
		ann      string
		code     string
		want     string
	}{
		// The three construct-level bury gates (#2708 / #2709 / #2713) that
		// core/baseparser's text scan used to carry.
		{annotations.Query, "@internal", annotations.CodeRetired, "#2708"},
		{annotations.Mutation, "@internal", annotations.CodeRetired, "#2708"},
		{annotations.Mutation, `@role("admin")`, annotations.CodeRetired, "never enforced"},
		{annotations.Query, `@role("admin")`, annotations.CodeRetired, "#2709"},
		{annotations.Query, `@permission("read:users")`, annotations.CodeRetired, "#2713"},
		{annotations.Shape, "@useConcept(thing)", annotations.CodeRetired, "file-top `use"},
		{annotations.Action, `@sideEffect("write")`, annotations.CodeRetired, "CAPABILITY"},
		{annotations.Action, `@kind("primitive")`, annotations.CodeRetired, "composites are automations"},
		{annotations.Tool, "@clientExecution", annotations.CodeRetired, "client-tool relay"},
		{annotations.Spec, `@shape(thing)`, annotations.CodeRetired, "signature"},
		{annotations.Concept, `@scope("global")`, annotations.CodeRetired, "default partition"},
		{annotations.Concept, "@cache(300)", annotations.CodeRetired, "query that reads"},
		{annotations.ArgsField, `@default("x")`, annotations.CodeRetired, "??"},
		{annotations.ArgsField, `@description("x")`, annotations.CodeRetired, "///"},
		{annotations.Query, "@row", annotations.CodeMisplaced, "SHAPE kind marker"},
		{annotations.Query, "@row", annotations.CodeMisplaced, "filter row.id == args.<x>"},
		{annotations.Spec, "@actor", annotations.CodeRetired, "@actor shape in the signature"},
		{annotations.Tool, "@row", annotations.CodeMisplaced, "a shape"},
		{annotations.PromptField, "@pii", annotations.CodeMisplaced, "a concept field"},
		{annotations.Query, "@description(\"a\")\n@description(\"b\")", annotations.CodeRepeated, "more than once"},
	}
	for _, tc := range cases {
		err := runReceiverGate(tc.receiver, receiverFixture(tc.receiver, "", tc.ann))
		if got := refusalCode(err); got != tc.code {
			t.Errorf("%s %s: code %q, want %q\nerror: %v", tc.receiver, tc.ann, got, tc.code, err)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s %s: the refusal lost its hint %q: %v", tc.receiver, tc.ann, tc.want, err)
		}
	}
}
