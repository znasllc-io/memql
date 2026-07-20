package parser

import "testing"

func TestActorRefInSource(t *testing.T) {
	for name, tc := range map[string]struct {
		src  string
		want bool
	}{
		"filter read":       {"filter { ownerUserId == actor.userId }", true},
		"line start":        {"actor.role", true},
		"event envelope":    {"cond(event.actor.id == args.x, 1, 2)", false},
		"comment prose":     {"// gated by actor.rank ordering", false},
		"string prose":      {"note: \"uses actor.userId internally\"", false},
		"identifier suffix": {"reactor.core", false},
		"bare actor no dot": {"actor", false},
		"stamp read":        {"stamp {\n  ownerUserId: actor.userId\n}", true},
	} {
		if got := ActorRefInSource(tc.src); got != tc.want {
			t.Errorf("%s: ActorRefInSource = %v, want %v", name, got, tc.want)
		}
	}
}

func TestRewriteActorBinding(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{
			name: "insert above the header, below other annotations",
			in: `@description("Owned list.")
query todo todos {
  filter { ownerUserId == actor.userId }
}`,
			want: `@description("Owned list.")
@actor
query todo todos {
  filter { ownerUserId == actor.userId }
}`,
		},
		{
			name: "already declared untouched",
			in: `@actor
query todo todos {
  filter { ownerUserId == actor.userId }
}`,
		},
		{
			name: "no actor read untouched",
			in: `query todo openTodos {
  filter { payload.done == false }
}`,
		},
		{
			name: "prose-only mention untouched",
			in: `// ranked by actor.rank
query todo ranked {
  filter { payload.done == false }
}`,
		},
		{
			name: "seed construct untouched",
			in: `@actor("system")
seed platform bootstrapVars {
  value: "actor.userId"
}`,
		},
		{
			name: "mutation stamp gains the annotation",
			in: `@description("Create.")
mutate todo createTodo {
  insert {
    accept { title }
    stamp {
      ownerUserId: actor.userId
    }
  }
}`,
			want: `@description("Create.")
@actor
mutate todo createTodo {
  insert {
    accept { title }
    stamp {
      ownerUserId: actor.userId
    }
  }
}`,
		},
	}
	for _, tc := range cases {
		got, err := RewriteActorBinding([]byte(tc.in))
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
