package parser

import (
	"strings"
	"testing"
)

// duplicate_attribute_arg_test.go -- memql#2968.
//
// parseAttribute accumulates an annotation's named arguments into a
// map[string]any, so a repeated key used to collapse LAST-WINS before any
// per-annotation validator could see it had been written twice. A reader
// scanning left to right saw the first value; the engine used the last.
//
// Rejected in parseAttribute rather than in any one validator, because the
// collapse happened before the validator ran -- so every validator written
// against attr.Args inherited the blind spot, including ParseRowAuthz's
// `len(attr.Args) > 1` arity check, which catches
// @rowAuthz(public, clusterOwner) and was blind to @rowAuthz(public, public).

// The four shapes memql#2968 measured, each on an annotation whose argument is
// a real boundary.
func TestDuplicateAttributeArgumentIsRejected(t *testing.T) {
	for _, tc := range []struct{ name, src, wantArg string }{
		{
			name:    "relationship target -- decides what an edge canonicalizes against",
			src:     "@relationship(type=\"parent\", field=\"a\", field=\"b\")\nconcept probe {\n  a string\n}\n",
			wantArg: "field",
		},
		{
			name:    "displayCard primary",
			src:     "@displayCard(primary=\"x\", primary=\"y\")\nconcept probe {\n  a string\n}\n",
			wantArg: "primary",
		},
		{
			name:    "trigger event -- decides what fires the automation",
			src:     "@trigger(event=\"a\", event=\"b\")\nautomation probe {\n  steps { }\n}\n",
			wantArg: "event",
		},
		{
			name:    "rowAuthz owner -- names the field compared against actor.userId",
			src:     "@rowAuthz(owner=\"x\", owner=\"y\")\nconcept probe {\n  a string\n}\n",
			wantArg: "owner",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseFile(tc.src)
			if err == nil {
				t.Fatalf("a repeated %q was accepted. It collapses last-wins, so the value a "+
					"reader sees is not the value the engine uses -- and on this annotation that "+
					"is a boundary.\n\nsource:\n%s", tc.wantArg, tc.src)
			}
			msg := err.Error()
			if !strings.Contains(msg, "duplicate argument") || !strings.Contains(msg, tc.wantArg) {
				t.Errorf("the error must name the repeated argument so the author can find it; "+
					"got: %v", err)
			}
		})
	}
}

// The bare-flag form goes through the SAME map, so it collapsed the same way.
// memql#2968 asked for this to be confirmed against the corpus before making it
// an error; the corpus loads clean, so it is one.
func TestDuplicateBareFlagArgumentIsRejected(t *testing.T) {
	src := "@allowedRoles(owner, owner)\ntool probe {\n  a string\n}\n"
	_, err := ParseFile(src)
	if err == nil {
		t.Fatal("a repeated bare flag was accepted. It lands in attr.Args as `true` exactly like " +
			"a key=value argument, so it collapses the same way and is invisible to any arity " +
			"check written against the map.")
	}
	if !strings.Contains(err.Error(), "duplicate argument") {
		t.Errorf("got: %v", err)
	}
}

// The arity blind spot the issue calls out by name: two spellings of "more than
// one tier" that used to arrive indistinguishable.
//
// They are rejected at DIFFERENT layers, which is the whole point and is why
// this test drives both rather than one:
//
//   - @rowAuthz(public, clusterOwner) parses fine and is refused by
//     ParseRowAuthz's `len(attr.Args) > 1` arity check.
//   - @rowAuthz(public, public) never reached that check, because the two
//     entries collapsed into ONE map key before it ran. It is now refused by
//     the parser.
//
// Asserting only the first would have left the gap exactly where it was.
func TestRepeatedIdenticalTierIsNoLongerInvisible(t *testing.T) {
	t.Run("distinct tiers are caught by the arity check", func(t *testing.T) {
		attr := &Attribute{Name: RowAuthzAnnotation, Args: map[string]any{
			"public": true, "clusterOwner": true,
		}}
		if _, err := ParseRowAuthz(attr); err == nil {
			t.Error("two distinct tiers is a contradiction and must be refused -- this is the " +
				"case that was ALREADY caught, kept as the control")
		}
	})

	t.Run("a repeated identical tier is caught by the parser", func(t *testing.T) {
		src := "@rowAuthz(public, public)\nconcept probe {\n  a string\n}\n"
		_, err := ParseFile(src)
		if err == nil {
			t.Fatal("@rowAuthz(public, public) was accepted. It used to collapse to a single map " +
				"entry, so ParseRowAuthz's len(attr.Args) > 1 check never saw it -- the arity " +
				"gate was blind to exactly one of the two spellings (memql#2968).")
		}
		if !strings.Contains(err.Error(), "duplicate argument") {
			t.Errorf("refused, but not as a duplicate, so this proves nothing about the "+
				"collapse: %v", err)
		}
	})
}

// The direction that keeps the change honest: DISTINCT arguments on the same
// annotation must still parse, and the values must be the ones written. A fix
// that rejected every multi-argument annotation would pass every test above.
func TestDistinctAttributeArgumentsStillParse(t *testing.T) {
	src := "@relationship(type=\"parent\", field=\"parentId\", target=\"v1:probe:thing\")\n" +
		"concept probe {\n  a string\n}\n"
	file, err := ParseFile(src)
	if err != nil {
		t.Fatalf("distinct arguments must still parse: %v", err)
	}
	var attr *Attribute
	for _, def := range file.Definitions {
		decl, ok := def.(*ConceptDecl)
		if !ok {
			continue
		}
		for _, a := range decl.Attributes {
			if a != nil && a.Name == "relationship" {
				attr = a
			}
		}
	}
	if attr == nil {
		t.Fatal("the @relationship attribute did not survive parsing, so this measures nothing")
	}
	for k, want := range map[string]any{
		"type": "parent", "field": "parentId", "target": "v1:probe:thing",
	} {
		if got := attr.Args[k]; got != want {
			t.Errorf("Args[%q] = %#v, want %#v", k, got, want)
		}
	}
}

// Repeating an argument across two SEPARATE annotations is not a duplicate --
// each @ gets its own Args map. Pinned because a fix that hoisted the seen-set
// out of parseAttribute would break every construct carrying two annotations
// that happen to share an argument name.
func TestSameArgumentNameOnTwoAnnotationsIsFine(t *testing.T) {
	src := "@trigger(event=\"a\")\n@precondition(event=\"b\")\nautomation probe {\n  steps { }\n}\n"
	if _, err := ParseFile(src); err != nil && strings.Contains(err.Error(), "duplicate argument") {
		t.Errorf("two different annotations may each carry an argument of the same name; the "+
			"duplicate check is per-annotation.\n  got: %v", err)
	}
}
