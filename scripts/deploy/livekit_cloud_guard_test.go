// No-cloud-leak guard for the telephony/voice media plane (Epic #2184,
// issue #2186). The locked architecture is:
//
//   - local dev  -> LiveKit Cloud (a *.livekit.cloud project, outbound).
//   - staging/prod -> self-hosted, open-source livekit-server +
//     livekit/sip (deploy/k8s/base/livekit*.yaml), exactly as today.
//
// The non-negotiable acceptance criterion is that the dev change must NOT
// put a LiveKit Cloud URL / key / secret into any staging or prod overlay
// (or the shared base they inherit). A doc note is insufficient -- this is
// the automated guard that fails the build if it ever happens.
//
// Why scan the raw YAML and not a rendered overlay: cloud creds in
// staging/prod would arrive via ESO from Key Vault at runtime, never from
// git, so the statically-checkable leak surface is a hard-coded
// *.livekit.cloud host (the cloud project URL) committed into base or the
// staging/prod overlays. The local overlay is deliberately exempt -- that
// is the one plane that is SUPPOSED to point at LiveKit Cloud (and even
// there the URL is secret-sourced via seed-secrets.sh, never committed).
//
// Same package as the other scripts/deploy tests; names are lkc-prefixed to
// avoid collisions. Runs under `GOWORK=off go test ./...` with no cluster.
package deploy

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// lkcCloudMarker is the definitive LiveKit Cloud host fragment. Every cloud
// project URL is wss://<project>.livekit.cloud, and cloud API keys/secrets
// are only ever presented alongside that host, so the host substring is the
// reliable static signal. Case-insensitive match.
const lkcCloudMarker = "livekit.cloud"

// lkcRepoRoot resolves the repo root from this test file's location
// (scripts/deploy/ -> ../..), mirroring aks_deploy_test.go.
func lkcRepoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

// lkcViolation is one offending line found by the scanner.
type lkcViolation struct {
	File string
	Line int
	Text string
}

// scanForLiveKitCloud walks every .yaml/.yml file under root and returns each
// line that mentions the LiveKit Cloud host. It is the reusable core the
// guard test and the planted-value self-test both exercise (#2190 requires
// that the guard provably catches a planted cloud value).
func scanForLiveKitCloud(root string) ([]lkcViolation, error) {
	var out []lkcViolation
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(b), "\n") {
			if strings.Contains(strings.ToLower(line), lkcCloudMarker) {
				out = append(out, lkcViolation{File: path, Line: i + 1, Text: strings.TrimSpace(line)})
			}
		}
		return nil
	})
	return out, err
}

// TestNoLiveKitCloudInStagingProd is the guard: base + the staging and prod
// overlays must carry zero LiveKit Cloud references. Staging/prod stay
// self-hosted (ws://livekit:7880 + wss://livekit.staging.copresent.ai), and
// base is shared with those overlays so it must be clean too.
func TestNoLiveKitCloudInStagingProd(t *testing.T) {
	root := lkcRepoRoot(t)
	scanDirs := []string{
		filepath.Join(root, "deploy", "k8s", "base"),
		filepath.Join(root, "deploy", "k8s", "overlays", "staging"),
		filepath.Join(root, "deploy", "k8s", "overlays", "prod"),
	}
	for _, dir := range scanDirs {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("expected manifest dir missing: %s: %v", dir, err)
		}
		violations, err := scanForLiveKitCloud(dir)
		if err != nil {
			t.Fatalf("scan %s: %v", dir, err)
		}
		for _, v := range violations {
			t.Errorf("LiveKit Cloud reference leaked into a self-hosted overlay (#2186): %s:%d: %q\n"+
				"staging/prod must stay self-hosted; the cloud plane is local-dev only.", v.File, v.Line, v.Text)
		}
	}
}

// TestLiveKitCloudGuardCatchesPlantedValue proves the scanner is not a no-op:
// a planted *.livekit.cloud URL is detected. This is the #2190 acceptance
// check ("Confirm the #2186 guard catches a planted cloud value") expressed
// as a unit test, so a future regression that neuters the scanner fails CI.
func TestLiveKitCloudGuardCatchesPlantedValue(t *testing.T) {
	dir := t.TempDir()
	planted := "" +
		"apiVersion: apps/v1\n" +
		"kind: Deployment\n" +
		"spec:\n" +
		"  template:\n" +
		"    spec:\n" +
		"      containers:\n" +
		"        - name: voice\n" +
		"          env:\n" +
		"            - { name: LIVEKIT_URL, value: \"wss://my-staging-proj.livekit.cloud\" }\n"
	if err := os.WriteFile(filepath.Join(dir, "leak.yaml"), []byte(planted), 0o644); err != nil {
		t.Fatalf("write planted file: %v", err)
	}
	// A clean file alongside it must NOT be flagged.
	clean := "        - { name: LIVEKIT_URL, value: \"ws://livekit:7880\" }\n"
	if err := os.WriteFile(filepath.Join(dir, "clean.yaml"), []byte(clean), 0o644); err != nil {
		t.Fatalf("write clean file: %v", err)
	}
	violations, err := scanForLiveKitCloud(dir)
	if err != nil {
		t.Fatalf("scan tmp: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("guard should flag exactly the planted cloud URL, got %d violations: %+v", len(violations), violations)
	}
	if !strings.Contains(violations[0].Text, "livekit.cloud") {
		t.Errorf("flagged the wrong line: %q", violations[0].Text)
	}
}
