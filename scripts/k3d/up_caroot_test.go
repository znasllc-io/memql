package k3d

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// up_caroot_test.go -- znasllc-io/memql#4069.
//
// k3d.up is the middle of the chain the CAROOT has to travel down:
//
//	install graph  localCA   -> install.mkcert --caroot=$HOME/.memql/mkcert
//	               clusterUp -> k3d.up ...
//	                            -> seed-secrets.sh ...
//	                               -> install.mkcert  (the SAME CA, or the bug)
//
// The value is threaded rather than re-derived for the same reason --repo-root
// is (memql#3570): a step that re-derives what an earlier step decided is a
// second source of truth, and the two disagree on precisely the machine that
// matters -- one where mkcert's own default root is not where the install put
// its CA. See seed_secrets_caroot_test.go for the full failure narrative.
//
// up.sh cannot be driven end-to-end from a test (it creates clusters and
// installs ArgoCD), so it is sourced and the two seeding seams are exercised
// against a fake kubectl and a stub seed-secrets.sh -- the seam
// up_voice_gate_budget_test.go and up_workloads_wait_test.go already use.

// carootSeedStub stands in for seed-secrets.sh and records the argv it was
// handed, which is the entire question here.
const carootSeedStub = "#!/usr/bin/env bash\nprintf '%s\\n' \"$*\" >> \"$SEED_ARGV_LOG\"\n"

// carootUpFakeKubectl answers everything the two seams ask: the namespace
// probe, the apply, and `get deploy voice` (present immediately, so the voice
// gate reaches seed-secrets on its first probe rather than waiting).
const carootUpFakeKubectl = `#!/usr/bin/env bash
case "$*" in
  *"get deploy voice"*) printf 'deployment.apps/voice\n'; exit 0 ;;
esac
exit 0
`

// runUpSeedSeam sources up.sh and calls one seeding function with the given
// CAROOT, returning every argv the stub seed-secrets.sh was invoked with.
func runUpSeedSeam(t *testing.T, fn, caroot string) string {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	root := repoRoot(t)
	tmp := t.TempDir()
	argvLog := filepath.Join(tmp, "seed-argv.log")

	if err := os.WriteFile(filepath.Join(tmp, "kubectl"), []byte(carootUpFakeKubectl), 0o755); err != nil {
		t.Fatalf("write fake kubectl: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "seed-secrets.sh"), []byte(carootSeedStub), 0o755); err != nil {
		t.Fatalf("write seed-secrets stub: %v", err)
	}

	harness := filepath.Join(tmp, "harness.sh")
	body := "#!/usr/bin/env bash\n" +
		"set -uo pipefail\n" +
		"source \"" + filepath.Join(root, "scripts", "k3d", "up.sh") + "\"\n" +
		"NAMESPACE=memql\n" +
		"REPO_ROOT=" + root + "\n" +
		"SCRIPT_DIR=" + tmp + "\n" +
		"DOMAIN=memql.localhost\n" +
		"ARGOCD_TIMEOUT=30\n" +
		"MKCERT_CAROOT=" + caroot + "\n" +
		"function sleep() { :; }\n" +
		fn + "\n"
	if err := os.WriteFile(harness, []byte(body), 0o755); err != nil {
		t.Fatalf("write harness: %v", err)
	}

	cmd := exec.Command("bash", harness)
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"PATH="+tmp+string(os.PathListSeparator)+os.Getenv("PATH"),
		"SEED_ARGV_LOG="+argvLog,
	)
	out, err := cmd.CombinedOutput()
	if _, ok := err.(*exec.ExitError); !ok && err != nil {
		t.Fatalf("harness failed: %v\n%s", err, out)
	}
	t.Logf("%s output:\n%s", fn, out)

	raw, readErr := os.ReadFile(argvLog)
	if readErr != nil {
		if os.IsNotExist(readErr) {
			t.Fatalf("%s never invoked seed-secrets.sh at all; output:\n%s", fn, out)
		}
		t.Fatalf("read seed argv log: %v", readErr)
	}
	return string(raw)
}

// The contract surface the install graph is audited against: installSession's
// "every param the plan supplies is a flag the script declares" runs k3d.up
// --print-spec and fails when the plan passes something this list does not
// carry -- which is not a warning at run time but an immediate exit 2 from
// cap_parse_flags, at the step that passed it.
func TestUpDeclaresTheCarootParam(t *testing.T) {
	root := repoRoot(t)
	out, err := exec.Command("bash", filepath.Join(root, "scripts", "k3d", "up.sh"), "--print-spec").CombinedOutput()
	if err != nil {
		t.Fatalf("--print-spec failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), `"caroot"`) {
		t.Errorf("--print-spec does not declare --caroot, so the install graph has no way to tell k3d.up which CA the install created\n%s", out)
	}
}

// Both directions in one test, for the reason spelled out in
// seed_secrets_caroot_test.go: forwarding when asked is the fix, and forwarding
// NOTHING when not asked is what keeps `make up` resolving mkcert's own default
// exactly as it does today.
func TestUpForwardsTheCarootToSeedSecrets(t *testing.T) {
	const caroot = "/home/op/.memql/mkcert"

	given := runUpSeedSeam(t, "seed_secrets", caroot)
	if !strings.Contains(given, "--caroot="+caroot) {
		t.Errorf("seed_secrets invoked seed-secrets.sh without the CAROOT k3d.up was given,\n"+
			"so the front-door pair is issued from whatever CA the machine default happens to hold.\nargv:\n%s", given)
	}

	absent := runUpSeedSeam(t, "seed_secrets", "")
	if strings.Contains(absent, "--caroot") {
		t.Errorf("a --caroot was invented with none supplied, changing what `make up` resolves.\nargv:\n%s", absent)
	}
}

// The SECOND call site. It re-runs only the voice-lane gate, which short-
// circuits inside seed-secrets before any certificate work -- so the value is
// inert there TODAY. It is passed anyway, and deliberately: the defect being
// fixed IS a value that failed to travel, and a call site that omits it is
// where the next one starts. --repo-root is passed at both sites on the same
// reasoning.
func TestUpForwardsTheCarootFromTheVoiceGateToo(t *testing.T) {
	const caroot = "/home/op/.memql/mkcert"

	given := runUpSeedSeam(t, "gate_voice_lane_post_sync", caroot)
	if !strings.Contains(given, "--caroot="+caroot) {
		t.Errorf("the post-sync voice gate dropped the CAROOT; every seed-secrets call site "+
			"carries it or the next change re-opens memql#4069.\nargv:\n%s", given)
	}

	absent := runUpSeedSeam(t, "gate_voice_lane_post_sync", "")
	if strings.Contains(absent, "--caroot") {
		t.Errorf("a --caroot was invented with none supplied.\nargv:\n%s", absent)
	}
}
