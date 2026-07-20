package parser

import "testing"

func runRewrite(t *testing.T, fn func([]byte) ([]byte, error), in string) string {
	t.Helper()
	got, err := fn([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	return string(got)
}

func TestRewriteRequiredSigil(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{
			name: "args field",
			in:   "  args {\n    slug string @required\n  }",
			want: "  args {\n    slug string!\n  }",
		},
		{
			name: "concept field keeps other annotations and alignment gap",
			in:   "  ownerUserId   string  @required  @description(\"owner\")",
			want: "  ownerUserId   string!  @description(\"owner\")",
		},
		{
			name: "array shorthand",
			in:   "  tags []string @required",
			want: "  tags []string!",
		},
		{
			name: "enum type keeps values",
			in:   "  status enum(\"a\", \"b\") @required",
			want: "  status enum(\"a\", \"b\")!",
		},
		{
			name: "existing sigil plus annotation dedupes",
			in:   "  slug string! @required",
			want: "  slug string!",
		},
		{
			name: "commented prose untouched",
			in:   "  // the old form used @required here",
			want: "  // the old form used @required here",
		},
		{
			name: "annotation-only mention untouched",
			in:   "  @requiredX string",
			want: "  @requiredX string",
		},
	}
	for _, tc := range cases {
		if got := runRewrite(t, RewriteRequiredSigil, tc.in); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

func TestRewriteEnumTypeArgs(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{
			name: "sigiled string with enum annotation folds",
			in:   "query q {\n  args {\n    status string! @enum(\"open\", \"closed\")\n  }\n}",
			want: "query q {\n  args {\n    status enum(\"open\", \"closed\")!\n  }\n}",
		},
		{
			name: "optional field folds without sigil",
			in:   "query q {\n  args {\n    kind string @enum(\"a\", \"b\") @description(\"keep\")\n  }\n}",
			want: "query q {\n  args {\n    kind enum(\"a\", \"b\") @description(\"keep\")\n  }\n}",
		},
		{
			name: "non-string typed enum annotation stays",
			in:   "query q {\n  args {\n    level int @enum(\"1\", \"2\")\n  }\n}",
			want: "query q {\n  args {\n    level int @enum(\"1\", \"2\")\n  }\n}",
		},
		{
			name: "outside args blocks untouched",
			in:   "concept w {\n  status string @enum(\"a\")\n}",
			want: "concept w {\n  status string @enum(\"a\")\n}",
		},
	}
	for _, tc := range cases {
		if got := runRewrite(t, RewriteEnumTypeArgs, tc.in); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

func TestRewriteCachePositional(t *testing.T) {
	in := "@cache(ttl=\"300\")\nquery widget listWidgets {\n  filter widget.kind == args.kind\n}\n@cache(600)\n"
	want := "@cache(300)\nquery widget listWidgets {\n  filter widget.kind == args.kind\n}\n@cache(600)\n"
	if got := runRewrite(t, RewriteCachePositional, in); got != want {
		t.Errorf("cache:\n got %q\nwant %q", got, want)
	}
}

// TestSigilRewritesParseEquivalent pins the property behind all three:
// the rewritten spelling parses to the same representation.
func TestSigilRewritesParseEquivalent(t *testing.T) {
	long := "args {\n  slug string @required\n  status string! @enum(\"open\", \"closed\")\n  note string\n}"
	step1 := runRewrite(t, RewriteRequiredSigil, long)
	short := runRewrite(t, RewriteEnumTypeArgs, step1)

	lb, err := parseArgsSafe(long)
	if err != nil {
		t.Fatal(err)
	}
	sb, err := parseArgsSafe(short)
	if err != nil {
		t.Fatalf("rewritten form fails to parse: %v\n%s", err, short)
	}
	if len(lb.Fields) != len(sb.Fields) {
		t.Fatalf("field count diverged: %d vs %d", len(lb.Fields), len(sb.Fields))
	}
	for i := range lb.Fields {
		l, s := lb.Fields[i], sb.Fields[i]
		if l.Name != s.Name || l.Type != s.Type || l.Optional != s.Optional || len(l.Enum) != len(s.Enum) {
			t.Errorf("field %s diverged: %+v vs %+v", l.Name, l, s)
		}
	}
}

func TestRewriteEnumTypeToolBody(t *testing.T) {
	in := "tool probe {\n  mode string! @enum(\"fast\", \"slow\") @description(\"keep\")\n}"
	want := "tool probe {\n  mode enum(\"fast\", \"slow\")! @description(\"keep\")\n}"
	if got := runRewrite(t, RewriteEnumTypeArgs, in); got != want {
		t.Errorf("tool body:\n got %q\nwant %q", got, want)
	}
}
