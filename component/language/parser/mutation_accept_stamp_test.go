package parser

import (
	"strings"
	"testing"
)

// mutation_accept_stamp_test.go covers the C5 (memql#2035) accept/stamp
// mutation sugar. `accept { f1, f2 }` lists the public concept fields a
// mutation accepts from its caller; each auto-binds to the same-named
// arg (`f1` -> `f1: args.f1`). `stamp { k: v }` carries the server-set
// fields. The two desugar into a synthetic `insert { ... }` so the
// rest of the rewriter (id hoist, payload translation) is unchanged.

func TestNormaliseMutation_AcceptStamp_AutoBinds(t *testing.T) {
	src := `mutation space createSpace {
  args {
    id          string @required
    name        string @required
    description string
  }
  accept { id, name, description }
  stamp {
    status:    "active"
    createdBy: actor.userId
  }
}`
	out, err := NormaliseMutationSource(src)
	if err != nil {
		t.Fatalf("accept/stamp form should rewrite cleanly, got: %v", err)
	}
	if !strings.Contains(out, "func (Mutation) createSpace") {
		t.Fatalf("expected procedural form; got %q", out)
	}
	// id auto-binds and is hoisted to the positional id= argument.
	if !strings.Contains(out, "id=args.id") {
		t.Fatalf("accepted `id` should hoist to id=args.id; got %q", out)
	}
	// Public fields auto-bind from same-named args.
	for _, want := range []string{"name: args.name", "description: args.description"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected auto-bound %q in payload; got %q", want, out)
		}
	}
	// Stamp fields carry through verbatim (server context, not args).
	for _, want := range []string{`"active"`, "createdBy: actor.userId"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected stamp field %q; got %q", want, out)
		}
	}
}

func TestNormaliseMutation_AcceptOnly_NoStamp(t *testing.T) {
	src := `mutation space createSpace {
  args {
    id   string @required
    name string @required
  }
  accept { id, name }
}`
	out, err := NormaliseMutationSource(src)
	if err != nil {
		t.Fatalf("accept-only form should rewrite, got: %v", err)
	}
	if !strings.Contains(out, "name: args.name") {
		t.Fatalf("expected auto-bound name; got %q", out)
	}
}

func TestNormaliseMutation_Accept_RequiresMatchingArg(t *testing.T) {
	// `description` is accepted but never declared in args -> error.
	src := `mutation space createSpace {
  args {
    id   string @required
    name string @required
  }
  accept { id, name, description }
}`
	_, err := NormaliseMutationSource(src)
	if err == nil {
		t.Fatal("expected an error: accepted field has no matching arg")
	}
	if !strings.Contains(err.Error(), "description") || !strings.Contains(err.Error(), "no matching arg") {
		t.Fatalf("error should name the unmatched field; got %q", err.Error())
	}
}

func TestNormaliseMutation_Accept_RejectsKeyValueEntry(t *testing.T) {
	// A `key: value` pair in accept is a mistake -- it belongs in stamp.
	src := `mutation space createSpace {
  args { id string @required }
  accept { id, status: "active" }
}`
	_, err := NormaliseMutationSource(src)
	if err == nil {
		t.Fatal("expected an error: accept entry looks like key:value")
	}
	if !strings.Contains(err.Error(), "stamp") {
		t.Fatalf("error should point the author at stamp; got %q", err.Error())
	}
}

func TestNormaliseMutation_Accept_RejectsMixWithInsert(t *testing.T) {
	src := `mutation space createSpace {
  args { id string @required }
  accept { id }
  insert { id: args.id }
}`
	_, err := NormaliseMutationSource(src)
	if err == nil {
		t.Fatal("expected an error: accept/stamp cannot mix with insert")
	}
	if !strings.Contains(err.Error(), "cannot mix") {
		t.Fatalf("error should explain the mutual exclusion; got %q", err.Error())
	}
}
