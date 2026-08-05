package callgraph

import (
	"testing"

	"github.com/znasllc-io/memql/component/language/dslspec"
)

// The declaration keyword every restricted kind is scanned for must be the one
// dslspec carries -- not a literal restated in tree.go. memql#3043: the write
// function was renamed `mutation` -> `mutate` in memql#2041, headerRE was not
// moved with it, and every mutation rule ran against zero constructs for as
// long as the fixtures were written in the retired spelling too.
func TestHeaderKeywordsComeFromDslspec(t *testing.T) {
	for kind, receiver := range restrictedKinds {
		keyword, _, ok := kindKeyword(kind)
		if !ok {
			t.Errorf("kind %q (receiver %q) resolves to no dslspec construct -- the call-graph rules for it are dead", kind, receiver)
			continue
		}
		if headerRE(kind) == nil {
			t.Errorf("kind %q has no header matcher", kind)
		}
		if c := dslspec.Build().ConstructByKeyword(keyword); c == nil {
			t.Errorf("kind %q resolved to keyword %q, which dslspec does not carry", kind, keyword)
		}
	}
}

// The write function is declared `mutate`, and the retired `mutation` noun is
// the invocation-step prefix only. A header matcher that accepted the retired
// spelling would match nothing in the tree -- which is exactly the #3043
// defect, and is invisible unless asserted from both sides.
func TestMutationKeywordIsMutate(t *testing.T) {
	keyword, conceptInSignature, ok := kindKeyword("mutation")
	if !ok {
		t.Fatal("kind \"mutation\" resolves to no dslspec construct")
	}
	if keyword != "mutate" {
		t.Errorf("mutation declaration keyword = %q, want \"mutate\" (memql#2041)", keyword)
	}
	if !conceptInSignature {
		t.Error("mutate binds its concept in the signature (`mutate <Concept> <name>`)")
	}
}

// The retired spelling must produce NO constructs, so a fixture that drifts
// back to `mutation node x {` fails loudly instead of silently exercising the
// rules against a keyword the parser no longer accepts.
func TestRetiredMutationSpellingSplitsToNothing(t *testing.T) {
	retired := `use cluster.concepts.{ node }
mutation node twoWrites {
  args { id string @required }
  insert { id: args.id }
  update { id: args.id, health: "up" }
}`
	if got := splitConstructs("mutation", retired); len(got) != 0 {
		t.Fatalf("retired `mutation` spelling must split to nothing; got %d constructs", len(got))
	}
	if fs := CheckFile("dsl/cluster/mutations.memql", retired, nil); len(fs) != 0 {
		t.Fatalf("retired `mutation` spelling must yield no findings; got %v", rules(fs))
	}
}

// The live spelling splits and is judged. Guards the concept segment being
// optional (`mutate <name>` as well as `mutate <Concept> <name>`).
func TestLiveMutateSpellingSplits(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header string
	}{
		{"with bound concept", "mutate node twoWrites {"},
		{"without bound concept", "mutate twoWrites {"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := "use cluster.concepts.{ node }\n" + tc.header + `
  args { id string @required }
  insert { id: args.id }
  update { id: args.id, health: "up" }
}`
			got := splitConstructs("mutation", src)
			if len(got) != 1 || got[0].name != "twoWrites" {
				t.Fatalf("expected one construct named twoWrites; got %v", got)
			}
			if fs := CheckFile("dsl/cluster/mutations.memql", src, nil); !has(fs, "mutation-single-write") {
				t.Fatalf("expected mutation-single-write finding; got %v", rules(fs))
			}
		})
	}
}

// Every restricted kind must actually split its own live declaration form.
// #3043 DoD item 5: query / logic / action match live keywords, but that was
// asserted by inspection rather than by a test, which is how the mutation arm
// stayed broken.
func TestEveryRestrictedKindSplitsItsLiveForm(t *testing.T) {
	for _, tc := range []struct{ kind, src string }{
		{"query", "query node q {\n  filter row.id == args.id\n}"},
		{"mutation", "mutate node m {\n  insert { id: args.id }\n}"},
		{"logic", "logic decide {\n  body { return true }\n}"},
		{"action", "action run {\n  args { x string }\n}"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			got := splitConstructs(tc.kind, tc.src)
			if len(got) != 1 {
				t.Fatalf("kind %q: expected 1 construct from its live form, got %d -- its rules are dead against the tree", tc.kind, len(got))
			}
		})
	}
}
