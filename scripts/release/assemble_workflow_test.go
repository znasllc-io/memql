// Build-speed C6 (#1511): static guard over the lockfile-assembly workflow
// (.github/workflows/release-lockfile-assemble.yml). It resolves the 8
// components' digests from ACR by tag, runs assemble-lockfile.sh, and opens the
// release PR. These assertions keep the workflow in sync with the assembler's
// component set + invariants.
package release

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func assembleWorkflow(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	p := filepath.Join(filepath.Dir(thisFile), "..", "..", ".github", "workflows", "release-lockfile-assemble.yml")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read release-lockfile-assemble.yml: %v", err)
	}
	return string(raw)
}

func TestAssembleWorkflowResolvesAllComponents(t *testing.T) {
	wf := assembleWorkflow(t)
	// The 8 lockfile components (== ACR repo names) -- must match
	// assemble-lockfile.sh's COMPONENTS.
	for _, comp := range []string{
		"memql-identity", "memql-cognition", "memql-voice", "memql-agent",
		"memql-planner", "memql-workbench", "memql-bff-copresent", "copresent",
	} {
		if !strings.Contains(wf, comp) {
			t.Errorf("assemble workflow must resolve the %q component digest", comp)
		}
	}
	// Resolves digests from ACR by tag, then assembles + validates.
	if !strings.Contains(wf, "az acr repository show") || !strings.Contains(wf, "--query digest") {
		t.Error("workflow must resolve @sha256 digests from ACR (az acr repository show --query digest)")
	}
	if !strings.Contains(wf, "scripts/release/assemble-lockfile.sh") {
		t.Error("workflow must run assemble-lockfile.sh")
	}
}

func TestAssembleWorkflowOpensPRViaOIDC(t *testing.T) {
	wf := assembleWorkflow(t)
	if !strings.Contains(wf, "workflow_dispatch:") {
		t.Error("assemble workflow must be workflow_dispatch (OIDC subject ref:refs/heads/main)")
	}
	if !strings.Contains(wf, "id-token: write") || !strings.Contains(wf, "azure/login@") {
		t.Error("workflow must authenticate to ACR via OIDC")
	}
	if !strings.Contains(wf, "create-pull-request@") {
		t.Error("workflow must open the lockfile PR")
	}
}
