package main

import "testing"

// #2614 review: the codemod's domain derivation must match the loader/gate
// (first segment under the dsl root), not the immediate parent -- nested
// files (dsl/agents/tools/x.memql) belong to their top-level domain.
func TestDomainForDSLPath(t *testing.T) {
	for path, want := range map[string]string{
		"dsl/agents/tools/askSpecialist.memql": "agents",
		"dsl/cognition/concepts.memql":         "cognition",
		"dsl/deployment/concepts.memql":        "deployment",
		"repo/dsl/agents/roles/x.memql":        "agents",
		"elsewhere/fylo/concepts.memql":        "fylo",
	} {
		if got := domainForDSLPath(path); got != want {
			t.Errorf("domainForDSLPath(%q) = %q, want %q", path, got, want)
		}
	}
}
