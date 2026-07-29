package parser

import (
	"strings"
	"testing"
)

// memql#2906 was reported and fixed against the AUTOMATION lane, but two of its
// four fixes live in helpers every lane shares:
//
//	rewriteEachBlock   frames query, mutation, logic AND automation
//	extractArgsBlock   serves mutation, logic and automation
//
// So the fix reaches further than the story does, and the reach is unguarded
// unless something exercises it here. That is not hypothetical: a sweep across
// all four lanes found 14 cases outside the automation lane whose behaviour
// this change alters -- 12 that went from a hard rewrite error to a correct
// load, and 2 where the lane had been SILENTLY DROPPING a declared `args`
// block and now emits it.
//
// The silent ones are why this file exists. A dropped `args` block produces a
// program that compiles and is wrong, and nothing in the automation lane's 122
// subtests would notice if a future edit reverted that half.
//
// What is NOT fixed, and is deliberately absent here: the query lane's
// struct-field loop still rejects a comment line outright ("unknown
// struct-query field"), and parseStructQueryBody and emitLogic still locate
// their inner blocks on raw source. Those are memql#2948.
func TestCrossLane_CommentInAnOuterHeaderDoesNotRefuseTheLoad(t *testing.T) {
	for _, lane := range []struct {
		name, ctl, with string
		fn              func(string) (string, error)
	}{
		{
			name: "query",
			ctl:  "query widget q {\n  filter { id: args.id }\n}",
			with: "query widget /*c*/ q {\n  filter { id: args.id }\n}",
			fn:   NormaliseQuerySource,
		},
		{
			name: "mutation",
			ctl:  "mutate space createSpace {\n  args {\n    id string @required\n  }\n  insert { id: args.id }\n}",
			with: "mutate space /*c*/ createSpace {\n  args {\n    id string @required\n  }\n  insert { id: args.id }\n}",
			fn:   NormaliseMutationSource,
		},
		{
			name: "logic",
			ctl:  "logic doFoo {\n  args {\n    x string @required\n  }\n  body {\n    return args.x\n  }\n}",
			with: "logic /*c*/ doFoo {\n  args {\n    x string @required\n  }\n  body {\n    return args.x\n  }\n}",
			fn:   NormaliseLogicSource,
		},
	} {
		t.Run(lane.name, func(t *testing.T) {
			want, err := lane.fn(lane.ctl)
			if err != nil {
				t.Fatalf("control does not compile, so this lane proves nothing: %v", err)
			}
			got, err := lane.fn(lane.with)
			if err != nil {
				t.Fatalf("a comment in the outer header refused the load. rewriteEachBlock locates "+
					"the header on the blanked view and must take the name from there too "+
					"(memql#2906).\n  error: %v", err)
			}
			if compiledForm(got) != compiledForm(want) {
				t.Errorf("a comment in the outer header changed the compiled program.\n  got:\n%s\n  control:\n%s", got, want)
			}
		})
	}
}

// The half that fails SILENTLY. With the args header located on raw source,
// extractArgsBlock finds the header inside the comment, seeks its brace in the
// blanked view where that `{` is a space, and -- in these two lanes -- ends up
// emitting a program with no `args` block at all rather than erroring.
//
// Asserting on the output is therefore the whole point: an error-vs-nil check
// passes on both sides of this bug.
func TestCrossLane_CommentInAnArgsHeaderDoesNotDropTheArgsBlock(t *testing.T) {
	for _, lane := range []struct {
		name, ctl string
		fn        func(string) (string, error)
	}{
		{
			name: "mutation",
			ctl:  "mutate space createSpace {\n  args {\n    id string @required\n  }\n  insert { id: args.id }\n}",
			fn:   NormaliseMutationSource,
		},
		{
			name: "logic",
			ctl:  "logic doFoo {\n  args {\n    x string @required\n  }\n  body {\n    return args.x\n  }\n}",
			fn:   NormaliseLogicSource,
		},
	} {
		for _, pos := range []struct{ name, header string }{
			{"before args keyword", "  /*c*/args {"},
			{"after args keyword", "  args /*c*/ {"},
		} {
			t.Run(lane.name+"/"+pos.name, func(t *testing.T) {
				want, err := lane.fn(lane.ctl)
				if err != nil {
					t.Fatalf("control: %v", err)
				}
				if !strings.Contains(want, "args {") {
					t.Fatalf("the control itself emits no `args` block, so a dropped one is "+
						"indistinguishable from correct output:\n%s", want)
				}
				src := strings.Replace(lane.ctl, "  args {", pos.header, 1)
				if src == lane.ctl {
					t.Fatal("splice did not apply; this case would pass vacuously")
				}
				got, err := lane.fn(src)
				if err != nil {
					t.Fatalf("a comment in the args header refused the load (memql#2906).\n  error: %v", err)
				}
				if !strings.Contains(got, "args {") {
					t.Errorf("the `args` block was SILENTLY DROPPED -- no error, and a program that "+
						"compiles without the arguments it declares (memql#2906).\n  got:\n%s", got)
				}
				if compiledForm(got) != compiledForm(want) {
					t.Errorf("a comment in the args header changed the compiled program.\n  got:\n%s\n  control:\n%s", got, want)
				}
			})
		}
	}
}
