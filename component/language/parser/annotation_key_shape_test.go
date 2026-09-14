package parser

import (
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/annotations"
)

// TestKeywordKeyShapeIsChecked: a keyword key is written the way its
// placement declares it -- a flag bare, a valued key with a value -- or the
// parser refuses it with annotation_key (memql#5359). The parser stores a bare
// key as `true`, so a check that read key NAMES only let
// `@rateLimit(maxCalls, periodSeconds)` through; the tool parser then read ""
// for both and registered the tool with no rate limit at all (the memql#3625
// silent-drop class).
func TestKeywordKeyShapeIsChecked(t *testing.T) {
	cases := []struct {
		name  string
		parse func() error
		want  []string
	}{
		{
			name: "a valued key written bare on a tool",
			parse: func() error {
				_, err := ParseToolDecl("@handler(type=\"function\", name=\"x\")\n@rateLimit(maxCalls, periodSeconds)\ntool probe {\n  x string\n}\n")
				return err
			},
			want: []string{"maxCalls takes a value", "maxCalls=<number>", "@rateLimit(maxCalls=10, periodSeconds=60)"},
		},
		{
			name: "a valued key written bare on a query",
			parse: func() error {
				lowered, err := NormaliseAll("@cache(ttl)\nquery thing probe {\n  filter row.id != \"\"\n}\n")
				if err != nil {
					return err
				}
				_, err = ParseFile(lowered)
				return err
			},
			want: []string{"ttl takes a value", `ttl="..."`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.parse()
			var ref *annotations.Refusal
			if err == nil || !errors.As(err, &ref) || ref.Code != annotations.CodeKey {
				t.Fatalf("want an annotation_key refusal, got: %v", err)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("the refusal must say %q: %v", w, err)
				}
			}
		})
	}
}
