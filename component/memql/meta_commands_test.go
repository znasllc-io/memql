package memql

import (
	"errors"
	"strings"
	"testing"
)

func TestSplitMetaCommandCall(t *testing.T) {
	cases := []struct {
		query, wantName, wantArgs string
		wantOk                    bool
	}{
		{`help()`, "help", "", true},
		{`help("foo")`, "help", `"foo"`, true},
		{`help({"name": "foo"})`, "help", `{"name": "foo"}`, true},
		{` help(  "foo"  ) `, "help", `  "foo"  `, true},
		{`validate({"concept": "v1:x", "payload": {"a": 1}})`, "validate", `{"concept": "v1:x", "payload": {"a": 1}}`, true},
		// Nested parens / brackets inside args must be respected.
		{`validate({"payload": {"nested": ["a", "b"]}})`, "validate", `{"payload": {"nested": ["a", "b"]}}`, true},
		// Quoted strings with `)` inside must not end the call early.
		{`help("foo)bar")`, "help", `"foo)bar"`, true},
		// Whole input must be the call. Trailing tokens reject the match
		// so a comparison like `id==help` doesn't get hijacked.
		{`help() and more`, "", "", false},
		{`name without parens`, "", "", false},
		{`concept==memql:version`, "", "", false},
		{``, "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			name, args, ok := splitMetaCommandCall(tc.query)
			if ok != tc.wantOk {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOk)
			}
			if !ok {
				return
			}
			if name != tc.wantName {
				t.Errorf("name = %q, want %q", name, tc.wantName)
			}
			if args != tc.wantArgs {
				t.Errorf("args = %q, want %q", args, tc.wantArgs)
			}
		})
	}
}

func TestQuoteBareIdentifierKeys(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// Bare-identifier keys at top level get wrapped.
		{`{name: "foo"}`, `{"name": "foo"}`},
		// Already-quoted keys are untouched.
		{`{"name": "foo"}`, `{"name": "foo"}`},
		// Mixed keys.
		{`{name: "foo", "other": 1}`, `{"name": "foo", "other": 1}`},
		// Nested objects get the same treatment.
		{`{name: "foo", payload: {inner: 1}}`, `{"name": "foo", "payload": {"inner": 1}}`},
		// String VALUES containing key-shaped tokens are not rewritten.
		{`{name: "foo: bar"}`, `{"name": "foo: bar"}`},
		// Quoted key followed by quoted value with `:` inside.
		{`{"label": "foo:bar"}`, `{"label": "foo:bar"}`},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := quoteBareIdentifierKeys(tc.in)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseMetaCommandArgs_Profiles(t *testing.T) {
	type tc struct {
		name     string
		argsSrc  string
		contract *BuiltinArgContract
		wantArgs map[string]any
		wantErr  bool
	}
	cases := []tc{
		{
			name:     "none-empty-ok",
			argsSrc:  "",
			contract: &BuiltinArgContract{Profile: BuiltinArgProfileNone},
		},
		{
			name:     "none-nonempty-errors",
			argsSrc:  `"bogus"`,
			contract: &BuiltinArgContract{Profile: BuiltinArgProfileNone},
			wantErr:  true,
		},
		{
			name:     "object-json-keys",
			argsSrc:  `{"concept": "v1:x", "payload": {}}`,
			contract: &BuiltinArgContract{Profile: BuiltinArgProfileObject, Required: []string{"concept", "payload"}},
			wantArgs: map[string]any{"concept": "v1:x", "payload": map[string]any{}},
		},
		{
			name:     "object-bare-keys",
			argsSrc:  `{concept: "v1:x", payload: {}}`,
			contract: &BuiltinArgContract{Profile: BuiltinArgProfileObject, Required: []string{"concept", "payload"}},
			wantArgs: map[string]any{"concept": "v1:x", "payload": map[string]any{}},
		},
		{
			name:     "optional-string-empty",
			argsSrc:  "",
			contract: &BuiltinArgContract{Profile: BuiltinArgProfileOptionalString, StringKey: "pattern"},
		},
		{
			name:     "optional-string-present",
			argsSrc:  `"foo"`,
			contract: &BuiltinArgContract{Profile: BuiltinArgProfileOptionalString, StringKey: "pattern"},
			wantArgs: map[string]any{"pattern": "foo"},
		},
		{
			name:     "string-or-object-string",
			argsSrc:  `"foo"`,
			contract: &BuiltinArgContract{Profile: BuiltinArgProfileStringOrObject, StringKey: "name", Required: []string{"name"}},
			wantArgs: map[string]any{"name": "foo"},
		},
		{
			name:     "string-or-object-object",
			argsSrc:  `{name: "foo"}`,
			contract: &BuiltinArgContract{Profile: BuiltinArgProfileStringOrObject, StringKey: "name", Required: []string{"name"}},
			wantArgs: map[string]any{"name": "foo"},
		},
		{
			name:     "string-or-object-empty-errors",
			argsSrc:  "",
			contract: &BuiltinArgContract{Profile: BuiltinArgProfileStringOrObject, StringKey: "name", Required: []string{"name"}},
			wantErr:  true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args, err := parseMetaCommandArgs(c.name, c.argsSrc, c.contract)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got args=%v", args)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !mapDeepEqual(args, c.wantArgs) {
				t.Errorf("args = %#v, want %#v", args, c.wantArgs)
			}
		})
	}
}

func TestParseMetaCommand_LangparserFlagDoesNotMatter(t *testing.T) {
	engine := newParserTestEngine(t)

	queries := []string{
		`help("myFunction")`,
		`help({"name": "myFunction"})`,
		`memqlDocs()`,
		`concepts()`,
		`concepts("v1:")`,
		`validate({"concept": "v1:test:concept", "payload": {}})`,
		`functions()`,
		`tools()`,
		`serviceVersion()`,
		`memqlVersion()`,
		`shapeTemplates()`,
		`shapeHelp("myShape")`,
		`shapeHelp({"name": "myShape"})`,
		`contentId({"concept": "v1:test:concept", "payload": {}})`,
		`previewInsert({"concept": "v1:test:concept", "payload": {}})`,
	}

	// The shim runs ABOVE both parsers, so the same query must produce
	// the same plan regardless of which parser is selected via
	// UseLangparserRuntime. Locks in the #256 contract that #249's
	// default-flip relies on.
	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			engine.UseLangparserRuntime(false)
			planOff, errOff := engine.Parse(q)
			if errOff != nil {
				t.Fatalf("Parse(off) error: %v", errOff)
			}
			engine.UseLangparserRuntime(true)
			planOn, errOn := engine.Parse(q)
			if errOn != nil {
				t.Fatalf("Parse(on) error: %v", errOn)
			}
			engine.UseLangparserRuntime(false)

			gotOff, ok := planOff.Root.(*BuiltinFunctionExpression)
			if !ok {
				t.Fatalf("Parse(off) root = %T, want *BuiltinFunctionExpression", planOff.Root)
			}
			gotOn, ok := planOn.Root.(*BuiltinFunctionExpression)
			if !ok {
				t.Fatalf("Parse(on) root = %T, want *BuiltinFunctionExpression", planOn.Root)
			}
			if gotOff.Executor != gotOn.Executor || gotOff.Name != gotOn.Name {
				t.Errorf("flag-dependent dispatch: off=%+v on=%+v", gotOff, gotOn)
			}
		})
	}
}

func TestParseMetaCommand_PropagatesArgErrors(t *testing.T) {
	engine := newParserTestEngine(t)

	cases := []struct {
		query      string
		wantSubstr string
	}{
		// Matched name + bad args must NOT fall through to the parser.
		{`help()`, "help"},
		{`help({"other": "x"})`, "help"},
		{`validate("just a string")`, "validate"},
	}
	for _, c := range cases {
		t.Run(c.query, func(t *testing.T) {
			_, err := engine.Parse(c.query)
			if err == nil {
				t.Fatalf("expected parse error for %q", c.query)
			}
			if !errors.Is(err, ErrInvalidArgument) {
				t.Errorf("expected ErrInvalidArgument, got %v", err)
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(c.wantSubstr)) {
				t.Errorf("error %q missing substring %q", err.Error(), c.wantSubstr)
			}
		})
	}
}

func mapDeepEqual(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok {
			return false
		}
		if !valueEqual(va, vb) {
			return false
		}
	}
	return true
}

func valueEqual(a, b any) bool {
	if a == nil && b == nil {
		return true
	}
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		return mapDeepEqual(am, bm)
	}
	return a == b
}
