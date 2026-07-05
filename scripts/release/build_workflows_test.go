// Build-speed C3 (#1508, epic #1505): static guard over the engine image-build
// workflow (.github/workflows/build-engine-images.yml). It builds the two
// engine images (memql-identity, memql-voice) and pushes them to ACR via OIDC,
// feeding the release lockfile (#1511). These string assertions keep the
// workflow's invariants from silently regressing; the real build is exercised
// by a workflow_dispatch on main.
package release

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func engineBuildWorkflow(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// scripts/release/ -> repo root is two directories up.
	p := filepath.Join(filepath.Dir(thisFile), "..", "..", ".github", "workflows", "build-engine-images.yml")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read build-engine-images.yml: %v", err)
	}
	return string(raw)
}

func TestEngineBuildWorkflowInvariants(t *testing.T) {
	wf := engineBuildWorkflow(t)

	// Builds BOTH engine images the engine-image build owns
	// (memql-identity + memql-voice).
	for _, node := range []string{"identity", "voice"} {
		if !strings.Contains(wf, "node: "+node) {
			t.Errorf("workflow must build the %q engine node", node)
		}
	}
	// Pushes to the shared ACR.
	if !strings.Contains(wf, "acrmemql") {
		t.Error("workflow must target the acrmemql registry")
	}
	if !strings.Contains(wf, "push: true") {
		t.Error("workflow must push the images (push: true)")
	}
	// OIDC auth (no long-lived secret) -- needs the id-token permission +
	// azure/login.
	if !strings.Contains(wf, "id-token: write") {
		t.Error("workflow must request id-token: write for OIDC")
	}
	if !strings.Contains(wf, "azure/login@") {
		t.Error("workflow must authenticate via azure/login (OIDC)")
	}
	// The OIDC federated-credential subject is ref:refs/heads/main, so the
	// build must run on main -- workflow_dispatch (runs on the default branch).
	if !strings.Contains(wf, "workflow_dispatch:") {
		t.Error("workflow must be workflow_dispatch (the OIDC subject is ref:refs/heads/main)")
	}
	// Release tags stay immutable (mirror the operator's ensure_tag_immutable).
	if !strings.Contains(wf, "Immutability guard") {
		t.Error("workflow must guard against overwriting an existing release tag")
	}
	// voice is the CGO/voice-runtime exception.
	if !strings.Contains(wf, "voice-runtime") {
		t.Error("the voice build must target the voice-runtime stage (CGO)")
	}
}
