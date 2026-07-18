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
	src := `mutate space createSpace {
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
	src := `mutate space createSpace {
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
	src := `mutate space createSpace {
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
	src := `mutate space createSpace {
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
	src := `mutate space createSpace {
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

// --- nested form (memql#2592) -------------------------------------------------
//
// The bare form above spells no write kind and means insert (#2035). Nesting
// accept/stamp INSIDE a write block spells the kind where it has always been
// spelled, which is what lets an `update` adopt the sugar. Before #2592 the
// rewriter pinned writeKind = "insert" unconditionally, so an update could not
// use accept/stamp at all -- and a nested attempt tripped the mix guard.

func TestNormaliseMutation_AcceptStamp_UpdateForm(t *testing.T) {
	src := `mutate conversation setConversationInsight {
  args {
    conversationId string @required
    summary        string @required
    sentiment      string @required
  }
  update {
    accept { summary, sentiment }
    stamp  { id: args.conversationId }
  }
}`
	out, err := NormaliseMutationSource(src)
	if err != nil {
		t.Fatalf("accept/stamp nested under update should rewrite cleanly, got: %v", err)
	}
	// The load-bearing assertion: the emitted write kind is update, not insert.
	if !strings.Contains(out, "return update(") {
		t.Fatalf("expected an update write; got %q", out)
	}
	if strings.Contains(out, "return insert(") {
		t.Fatalf("update form must not emit an insert; got %q", out)
	}
	if !strings.Contains(out, "id=args.conversationId") {
		t.Fatalf("stamped id should hoist to the positional id=; got %q", out)
	}
	for _, want := range []string{"summary: args.summary", "sentiment: args.sentiment"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected auto-bound %q in payload; got %q", want, out)
		}
	}
}

func TestNormaliseMutation_AcceptStamp_InsertFormNested(t *testing.T) {
	src := `mutate space createSpace {
  args {
    id   string @required
    name string @required
  }
  insert {
    accept { name }
    stamp  { id: args.id }
  }
}`
	out, err := NormaliseMutationSource(src)
	if err != nil {
		t.Fatalf("accept/stamp nested under insert should rewrite cleanly, got: %v", err)
	}
	if !strings.Contains(out, "return insert(") {
		t.Fatalf("expected an insert write; got %q", out)
	}
	if !strings.Contains(out, "id=args.id") || !strings.Contains(out, "name: args.name") {
		t.Fatalf("expected hoisted id and auto-bound name; got %q", out)
	}
}

func TestNormaliseMutation_AcceptStamp_BareFormStillInserts(t *testing.T) {
	// Regression: the #2035 bare form spells no kind and must keep meaning
	// insert. #2592 must not silently retarget it.
	src := `mutate space createSpace {
  args { id string @required }
  accept { id }
}`
	out, err := NormaliseMutationSource(src)
	if err != nil {
		t.Fatalf("bare accept form should still rewrite cleanly, got: %v", err)
	}
	if !strings.Contains(out, "return insert(") {
		t.Fatalf("bare accept/stamp form must still mean insert; got %q", out)
	}
}

func TestNormaliseMutation_AcceptStamp_UpdateRequiresId(t *testing.T) {
	src := `mutate conversation setConversationInsight {
  args { summary string @required }
  update {
    accept { summary }
  }
}`
	_, err := NormaliseMutationSource(src)
	if err == nil {
		t.Fatal("expected an error: an update needs an id: line to identify the row")
	}
	if !strings.Contains(err.Error(), "id") {
		t.Fatalf("error should name the missing id; got %q", err.Error())
	}
}

func TestNormaliseMutation_AcceptStamp_NestedRejectsStrayFields(t *testing.T) {
	// A stray `key: value` beside a nested accept/stamp would be silently
	// dropped by the desugar, so it is rejected rather than lost.
	src := `mutate message notify {
  args { conversationId string @required, text string @required }
  insert {
    accept { conversationId, text }
    stamp  { id: args.conversationId }
    who: "agent"
  }
}`
	_, err := NormaliseMutationSource(src)
	if err == nil {
		t.Fatal("expected an error: a stray field beside nested accept/stamp would be dropped")
	}
	if !strings.Contains(err.Error(), "who") {
		t.Fatalf("error should name the stray field; got %q", err.Error())
	}
}

func TestNormaliseMutation_AcceptStamp_SameLineBlocksBothApply(t *testing.T) {
	// A block header anchors on a newline, so a second block written on the
	// SAME line as the first carries no anchor of its own. Both blocks must
	// still be seen: the failure mode is the desugar taking `accept` and
	// silently dropping `stamp` -- and the residue guard, which strips accept
	// first and thereby re-anchors stamp, stripping it too and reporting a
	// clean body. Guard and desugar must never disagree about what exists.
	for _, src := range []string{
		// nested form
		`mutate widget createWidget {
  args {
    name string @required
  }
  insert {
    accept { name } stamp { status: "active", createdBy: actor.userId }
  }
}`,
		// bare form
		`mutate widget createWidget {
  args {
    name string @required
  }
  accept { name } stamp { status: "active", createdBy: actor.userId }
}`,
		// stamp written first
		`mutate widget createWidget {
  args {
    name string @required
  }
  insert {
    stamp { status: "active", createdBy: actor.userId } accept { name }
  }
}`,
	} {
		out, err := NormaliseMutationSource(src)
		if err != nil {
			t.Fatalf("same-line accept/stamp should rewrite cleanly, got: %v\nsrc:\n%s", err, src)
		}
		// The accepted field must bind...
		if !strings.Contains(out, "name: args.name") {
			t.Fatalf("accepted field dropped; got %q\nsrc:\n%s", out, src)
		}
		// ...and the stamp block must NOT be silently discarded.
		for _, want := range []string{`status: "active"`, "createdBy: actor.userId"} {
			if !strings.Contains(out, want) {
				t.Fatalf("stamp field %q silently dropped; got %q\nsrc:\n%s", want, out, src)
			}
		}
	}
}

func TestNormaliseMutation_AcceptStamp_RejectsSplitAcrossBoundary(t *testing.T) {
	// accept/stamp must sit on ONE side of the write block. Splitting them --
	// one nested, the other at the top level -- must not silently drop the
	// outer block: the desugar only ever reads the nested region, so an
	// unnoticed outer `accept` yields a write with an EMPTY payload (an update
	// that writes nothing), which is the worst possible failure mode.
	cases := map[string]string{
		"accept nested, stamp outside": `mutate space createSpace {
  args {
    id   string @required
    name string @required
  }
  insert { accept { name } }
  stamp { id: args.id }
}`,
		"stamp nested, accept outside": `mutate space createSpace {
  args {
    id   string @required
    name string @required
  }
  insert { stamp { id: args.id } }
  accept { name }
}`,
		"update, stamp nested, accept outside": `mutate conversation setConversationInsight {
  args {
    conversationId string @required
    summary        string @required
  }
  update { stamp { id: args.conversationId } }
  accept { summary }
}`,
		// Same-line placement: the stray sits on the write block's own line, so
		// it carries no leading newline for the block headers to anchor on.
		// The excision must not consume that anchor either.
		"update, accept outside on the same line": `mutate conversation setConversationInsight {
  args {
    conversationId string @required
    summary        string @required
  }
  update { stamp { id: args.conversationId } } accept { summary }
}`,
		"insert, accept outside on the same line": `mutate space createSpace {
  args {
    id   string @required
    name string @required
  }
  insert { stamp { id: args.id } } accept { name }
}`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := NormaliseMutationSource(src)
			if err == nil {
				t.Fatalf("expected an error: accept/stamp split across the write-block boundary; got %q", out)
			}
			if !strings.Contains(err.Error(), "cannot mix") {
				t.Fatalf("error should explain the exclusion; got %q", err.Error())
			}
		})
	}
}

func TestNormaliseMutation_AcceptStamp_UpdateRequiresMatchingArg(t *testing.T) {
	// The accept auto-bind guard must fire on the update path too.
	src := `mutate conversation setConversationInsight {
  args { conversationId string @required }
  update {
    accept { summary }
    stamp  { id: args.conversationId }
  }
}`
	_, err := NormaliseMutationSource(src)
	if err == nil {
		t.Fatal("expected an error: accepted `summary` has no matching arg")
	}
	if !strings.Contains(err.Error(), "no matching arg") {
		t.Fatalf("error should explain the missing arg; got %q", err.Error())
	}
}

// --- framing robustness (the single-source scanMutationBlocks refactor) -------
//
// These pin the whole whack-a-mole class the newline-anchored regexes kept
// reintroducing: a block must be found identically no matter where it sits, and
// braces/keywords inside strings or comments must never throw off the framing.

func TestNormaliseMutation_AcceptStamp_StrayOnArgsCloseLine(t *testing.T) {
	// Round 4: a top-level `accept` sharing the args block's closing-brace line,
	// beside a nested write. The regex approach never anchored this and emitted
	// a silent no-op; it must be rejected as a mix.
	src := `mutate space createSpace {
  args {
    id   string @required
    name string @required
  } accept { name }
  update { accept { id } }
}`
	_, err := NormaliseMutationSource(src)
	if err == nil {
		t.Fatal("expected an error: top-level accept beside a nested write (mix)")
	}
	if !strings.Contains(err.Error(), "cannot mix") {
		t.Fatalf("error should explain the mix; got %q", err.Error())
	}
}

func TestNormaliseMutation_AcceptStamp_BraceInStampStringLiteral(t *testing.T) {
	// The brace is UNBALANCED inside the string (a single `}`): a brace-only
	// scanner nets it against a real `}` and truncates the construct. Only a
	// string-aware frame -- both the outer construct frame (rewriteEachBlock)
	// AND the inner scan -- survives this.
	src := `mutate widget createWidget {
  args {
    id   string @required
    name string @required
  }
  insert {
    accept { name }
    stamp  { id: args.id, note: "a } b" }
  }
}`
	out, err := NormaliseMutationSource(src)
	if err != nil {
		t.Fatalf("unbalanced brace inside a stamp string literal broke framing: %v", err)
	}
	for _, want := range []string{"name: args.name", `note: "a } b"`, "id=args.id", "return insert("} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q in output; got %q", want, out)
		}
	}
}

func TestNormaliseMutation_AcceptStamp_SlashSlashInStringValue(t *testing.T) {
	// A `//` inside a value string (a URL) must not be read as a line comment
	// that truncates the line and drops the fields after it.
	src := `mutate widget createWidget {
  args {
    id     string @required
    name   string @required
  }
  insert {
    accept { name }
    stamp  { source: "https://cdn.example.com/x", id: args.id }
  }
}`
	out, err := NormaliseMutationSource(src)
	if err != nil {
		t.Fatalf("`//` inside a string value broke the body: %v", err)
	}
	for _, want := range []string{"name: args.name", `source: "https://cdn.example.com/x"`, "id=args.id"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q to survive (no truncation at `//`); got %q", want, out)
		}
	}
}

func TestNormaliseMutation_AcceptStamp_BraceInComment(t *testing.T) {
	// A `}` or block keyword inside a comment must not affect framing.
	src := `mutate widget createWidget {
  args {
    id   string @required
    name string @required
  }
  insert {
    // a stray } and a fake stamp { here, both in a comment
    accept { name }
    stamp  { id: args.id }
  }
}`
	out, err := NormaliseMutationSource(src)
	if err != nil {
		t.Fatalf("comment braces/keywords broke framing: %v", err)
	}
	if !strings.Contains(out, "name: args.name") || !strings.Contains(out, "id=args.id") {
		t.Fatalf("expected the real accept/stamp fields; got %q", out)
	}
}

func TestNormaliseMutation_LegacyObjectLiteralValue(t *testing.T) {
	// A legacy insert body whose value is an object literal must NOT be
	// mistaken for a nested block, and must pass through verbatim.
	src := `mutate widget createWidget {
  args {
    id   string @required
    meta object @required
  }
  insert {
    id: args.id
    meta: { nested: "v", n: 1 }
  }
}`
	out, err := NormaliseMutationSource(src)
	if err != nil {
		t.Fatalf("object-literal value broke the legacy path: %v", err)
	}
	if !strings.Contains(out, "return insert(") || !strings.Contains(out, `meta: { nested: "v", n: 1 }`) {
		t.Fatalf("legacy object-literal body should pass through; got %q", out)
	}
}

func TestNormaliseMutation_AcceptStamp_RejectsTopLevelStrayBareForm(t *testing.T) {
	// #2594: the bare form silently dropped a stray field beside the blocks.
	src := `mutate message notify {
  args { conversationId string @required, text string @required }
  accept { conversationId, text }
  stamp  { who: "agent" }
  suggested: false
}`
	_, err := NormaliseMutationSource(src)
	if err == nil {
		t.Fatal("expected an error: a top-level stray field beside bare accept/stamp")
	}
	if !strings.Contains(err.Error(), "suggested") {
		t.Fatalf("error should name the stray field; got %q", err.Error())
	}
}

// --- inner-block string awareness (round-2 review: args / body / step) --------

func TestNormaliseMutation_ArgsDefaultStringWithBrace(t *testing.T) {
	// A `}` inside an args `@default("...")` string must not truncate the args
	// block. The outer/mutation scans are string-aware; the args extractor must
	// agree with them (else it frames a different, corrupt block).
	src := `mutate widget createWidget {
  args {
    id  string @required
    tpl string @default("a}b")
  }
  insert {
    id: args.id
    tpl: args.tpl
  }
}`
	out, err := NormaliseMutationSource(src)
	if err != nil {
		t.Fatalf("`}` in an args @default string broke the args block: %v", err)
	}
	if !strings.Contains(out, `tpl string @default("a}b")`) {
		t.Fatalf("args @default string should survive intact; got %q", out)
	}
}

func TestNormaliseLogic_BodyReturnsStringWithBrace(t *testing.T) {
	// A `}` inside a string returned from a logic body must not truncate the
	// body block (string-aware inner framing).
	src := `logic mk {
  body {
    return "a}b"
  }
}`
	out, err := NormaliseLogicSource(src)
	if err != nil {
		t.Fatalf("`}` in a logic body return string broke the body block: %v", err)
	}
	if !strings.Contains(out, `return "a}b"`) {
		t.Fatalf("logic body should keep the full return string; got %q", out)
	}
}
