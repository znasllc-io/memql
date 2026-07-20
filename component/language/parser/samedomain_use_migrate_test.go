package parser

import "testing"

func TestRewriteSameDomainUse(t *testing.T) {
	cases := []struct {
		name, domain, in, want string
	}{
		{
			name:   "same-domain line deleted",
			domain: "planner",
			in:     "use planner.concepts.{ plan }\nuse harness.concepts.{ run }\n\nquery plan allPlans {\n}\n",
			want:   "use harness.concepts.{ run }\n\nquery plan allPlans {\n}\n",
		},
		{
			name:   "cross-domain only untouched",
			domain: "planner",
			in:     "use harness.concepts.{ run }\n\nquery plan allPlans {\n}\n",
			want:   "use harness.concepts.{ run }\n\nquery plan allPlans {\n}\n",
		},
		{
			name:   "multi-line brace list deleted",
			domain: "cognition",
			in:     "use cognition.concepts.{\n  space,\n  participant\n}\nuse identity.concepts.{ user }\n",
			want:   "use identity.concepts.{ user }\n",
		},
		{
			name:   "prefix domain does not match",
			domain: "plan",
			in:     "use planner.concepts.{ plan }\n",
			want:   "use planner.concepts.{ plan }\n",
		},
		{
			name:   "commented use line untouched",
			domain: "planner",
			in:     "// use planner.concepts.{ plan }\n",
			want:   "// use planner.concepts.{ plan }\n",
		},
		{
			name:   "empty domain is a no-op",
			domain: "",
			in:     "use planner.concepts.{ plan }\n",
			want:   "use planner.concepts.{ plan }\n",
		},
	}
	for _, tc := range cases {
		got, err := RewriteSameDomainUse(tc.domain, []byte(tc.in))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if string(got) != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, string(got), tc.want)
		}
	}
}
