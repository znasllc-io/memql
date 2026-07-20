package parser

import (
	"strings"
	"testing"
)

func TestRewriteAcceptStamp(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // empty = expect byte-identical passthrough
	}{
		{
			name: "mirrors plus literals collapse into accept and stamp",
			in: `mutate role createRole {
  args {
    slug string @required
    name string @required
    rank number
  }
  insert {
    args.slug
    args.name
    rank: args.rank
    status: "open"
    id: makeId(args.slug)
  }
}`,
			want: `mutate role createRole {
  args {
    slug string @required
    name string @required
    rank number
  }
  insert {
    accept { slug, name, rank }
    stamp {
      status: "open"
      id: makeId(args.slug)
    }
  }
}`,
		},
		{
			name: "all mirrors, no stamp block emitted",
			in: `mutate widget addWidget {
  args {
    label string @required
    kind string
  }
  insert {
    args.label
    args.kind
  }
}`,
			want: `mutate widget addWidget {
  args {
    label string @required
    kind string
  }
  insert {
    accept { label, kind }
  }
}`,
		},
		{
			name: "update block migrates too (form shipped by #2593)",
			in: `mutate widget renameWidget {
  args {
    id string @required
    label string @required
    kind string
  }
  update {
    id: args.id
    args.label
    args.kind
  }
}`,
			want: `mutate widget renameWidget {
  args {
    id string @required
    label string @required
    kind string
  }
  update {
    accept { id, label, kind }
  }
}`,
		},
		{
			name: "single mirror is not worth the sugar",
			in: `mutate widget touchWidget {
  args {
    id string @required
  }
  update {
    id: args.id
    touchedAt: now
  }
}`,
		},
		{
			name: "already migrated is idempotent",
			in: `mutate role createRole {
  args {
    slug string @required
    name string @required
  }
  insert {
    accept { slug, name }
    stamp {
      status: "open"
    }
  }
}`,
		},
		{
			name: "comments in the block are preserved by skipping",
			in: `mutate role createRole {
  args {
    slug string @required
    name string @required
  }
  insert {
    args.slug
    args.name // human note worth keeping
  }
}`,
		},
		{
			// #2660 review: splitInsertFields glues depth>0 newlines to
			// spaces, so the multi-line check must run on the raw inner
			// -- otherwise the codemod reflows the expression with its
			// indentation frozen as space runs.
			name: "paren-continued multi-line field stays longhand",
			in: `mutate space addAgentToSpace {
  args {
    spaceId string @required
    agentId string @required
  }
  insert {
    id: concat("si-", hash(concat(
      canonicalId(args.agentId, agent), ":",
      canonicalId(args.spaceId, space)
    )))
    args.spaceId
    args.agentId
  }
}`,
		},
		{
			// #2660 delta review: an escaped quote inside a string must
			// not desync hasMultilineField's depth -- the multi-line
			// coalesce below must still be detected and skipped.
			name: "escaped quote before a multi-line field stays longhand",
			in: `mutate widget addWidget {
  args {
    a string @required
    b string @required
  }
  insert {
    args.a
    args.b
    q: concat("say \")(", args.a)
    c: coalesce(
      args.a,
      args.b
    )
  }
}`,
		},
		{
			name: "nested object value stays longhand",
			in: `mutate identity addCredential {
  args {
    userId string @required
    secret string @required
  }
  insert {
    args.userId
    args.secret
    credentials: { hash: args.secret }
  }
}`,
		},
		{
			name: "mirror of an undeclared arg stays longhand",
			in: `mutate widget addWidget {
  args {
    label string @required
  }
  insert {
    args.label
    args.phantom
  }
}`,
		},
		{
			name: "key collision between mirror and stamp stays longhand",
			in: `mutate widget addWidget {
  args {
    label string @required
    kind string
  }
  insert {
    args.label
    args.kind
    label: "override"
  }
}`,
		},
		{
			name: "verbose mirror with a different key is a stamp field",
			in: `mutate widget addWidget {
  args {
    label string @required
    kind string
  }
  insert {
    args.label
    args.kind
    displayKind: args.kind
  }
}`,
			want: `mutate widget addWidget {
  args {
    label string @required
    kind string
  }
  insert {
    accept { label, kind }
    stamp {
      displayKind: args.kind
    }
  }
}`,
		},
		{
			name: "non-mutation constructs untouched",
			in: `query listWidgets {
  args {
    kind string
  }
  filter { payload.kind == args.kind }
}`,
		},
	}
	for _, tc := range cases {
		got, err := RewriteAcceptStamp([]byte(tc.in))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		want := tc.want
		if want == "" {
			want = tc.in
		}
		if string(got) != want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, string(got), want)
		}
	}
}

// TestRewriteAcceptStamp_EmitEquivalence is the property behind the
// codemod's safety: for every rewritten fixture the engine's own
// emitter must produce the same procedural text (payload order-
// insensitive) for both spellings.
func TestRewriteAcceptStamp_EmitEquivalence(t *testing.T) {
	src := `mutate role createRole {
  args {
    slug string @required
    name string @required
    rank number
  }
  insert {
    args.slug
    status: "open"
    args.name
    rank: args.rank
    id: makeId(args.slug)
  }
}`
	got, err := RewriteAcceptStamp([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) == src {
		t.Fatal("interleaved mirrors must still migrate")
	}
	if !strings.Contains(string(got), "accept { slug, name, rank }") {
		t.Fatalf("accept fold missing:\n%s", got)
	}
	oldEmit, err := NormaliseMutationSource(src)
	if err != nil {
		t.Fatalf("old emit: %v", err)
	}
	newEmit, err := NormaliseMutationSource(string(got))
	if err != nil {
		t.Fatalf("new emit: %v", err)
	}
	if canonicalEmit(oldEmit) != canonicalEmit(newEmit) {
		t.Errorf("emits diverge:\n old %s\n new %s", oldEmit, newEmit)
	}
}

// TestRewriteAcceptStamp_MultibyteUpstream pins rune-space discipline
// (the #2658 lesson): a multibyte char before the write block must not
// skew the splice.
func TestRewriteAcceptStamp_MultibyteUpstream(t *testing.T) {
	src := "// clause § 4\nmutate widget addWidget {\n  args {\n    label string @required\n    kind string\n  }\n  insert {\n    args.label\n    args.kind\n  }\n}"
	got, err := RewriteAcceptStamp([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "accept { label, kind }") || !strings.Contains(string(got), "§") {
		t.Errorf("multibyte splice damaged output:\n%s", got)
	}
	if _, err := NormaliseMutationSource(strings.TrimPrefix(string(got), "// clause § 4\n")); err != nil {
		t.Errorf("rewritten construct fails to emit: %v", err)
	}
}
