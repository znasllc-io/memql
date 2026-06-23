package steps

import (
	"strings"
	"testing"
)

// TestRenderMemQLValue_TypedSliceAndMap is the memql#344 Regression A
// regression guard. The cluster `system.startup` automations
// (bootstrapCluster / registerNode) fail at runtime with
// `parse error at line 1, column 394: expected '}', got "["` when
// the event payload carries typed slices / maps that aren't `[]any`
// or `map[string]any`. The Go fmt default-case rendering produces
// strings like `[a b]` / `map[k:v]` that the langparser rejects;
// renderMemQLValue must produce valid MemQL text for every value
// type the runtime arg-bag could carry.
//
// Pins both the immediate `[]string` / `map[string]string` cases
// (the actual triggers from the production logs in #344) and the
// reflection-fallback path for less common typed slice / map shapes.
func TestRenderMemQLValue_TypedSliceAndMap(t *testing.T) {
	cases := []struct {
		name     string
		value    any
		want     string
		wantSubs []string // substring assertions for unordered maps
	}{
		{
			name:  "[]string single element",
			value: []string{"mql"},
			want:  `["mql"]`,
		},
		{
			name:  "[]string multiple elements",
			value: []string{"aud1", "aud2"},
			want:  `["aud1", "aud2"]`,
		},
		{
			name:  "[]string empty",
			value: []string{},
			want:  `[]`,
		},
		{
			name:     "map[string]string sorted keys",
			value:    map[string]string{"env": "prod", "region": "us-east-1"},
			want:     `{"env": "prod", "region": "us-east-1"}`,
			wantSubs: []string{`"env": "prod"`, `"region": "us-east-1"`},
		},
		{
			name:  "map[string]string empty",
			value: map[string]string{},
			want:  `{}`,
		},
		{
			name:  "[]int reflection fallback",
			value: []int{1, 2, 3},
			want:  `[1, 2, 3]`,
		},
		{
			name:  "[]bool reflection fallback",
			value: []bool{true, false},
			want:  `[true, false]`,
		},
		{
			name:  "map[string]int reflection fallback",
			value: map[string]int{"a": 1, "b": 2},
			want:  `{"a": 1, "b": 2}`,
		},
		{
			name:  "nested map containing []string (the #344 shape)",
			value: map[string]any{"acceptedAudiences": []string{"mql"}, "name": "memql-identity"},
			want:  `{"acceptedAudiences": ["mql"], "name": "memql-identity"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderMemQLValue(tc.value)
			if got != tc.want {
				t.Errorf("renderMemQLValue:\n  got:  %s\n  want: %s", got, tc.want)
			}
			// Don't allow Go's default %v markers to leak into the
			// rendered output -- the load-bearing invariant the bug
			// violated.
			for _, bad := range []string{"map[", "[a "} {
				if strings.Contains(got, bad) {
					t.Errorf("renderMemQLValue produced Go-format leakage %q in output: %s", bad, got)
				}
			}
		})
	}
}

// TestRenderMemQLValue_BugReproductionFromIssue344 reproduces the
// exact shape from the #344 issue body: an arg-map value containing
// a `[]string` (acceptedAudiences) that the renderer mis-formatted
// as `[mql]` -- which the langparser then rejected with the column-
// 394 error. After the fix, the rendered output parses cleanly.
func TestRenderMemQLValue_BugReproductionFromIssue344(t *testing.T) {
	// Shape the cluster bootstrapCluster automation hits when
	// EmitSystemStartup includes identity-provider info: an
	// `identityProvider` map carrying acceptedAudiences as []string.
	args := map[string]any{
		"identityProvider": map[string]any{
			"name":              "memql-identity",
			"issuerUrl":         "http://identity:8081",
			"providerType":      "oidc",
			"acceptedAudiences": []string{"mql"},
		},
	}
	got := renderFunctionArgs(args)
	if strings.Contains(got, "[mql]") {
		t.Fatalf("renderFunctionArgs leaked Go-format `[mql]` (the #344 bug); got: %s", got)
	}
	if !strings.Contains(got, `["mql"]`) {
		t.Errorf("renderFunctionArgs should produce quoted-string array `[\"mql\"]`; got: %s", got)
	}
}
