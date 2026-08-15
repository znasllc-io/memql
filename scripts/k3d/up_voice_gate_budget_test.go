package k3d

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// up_voice_gate_budget_test.go -- znasllc-io/memql#3877.
//
// WHAT HAPPENED. A local install failed at `clusterUp` with
//
//	the script exited 0 but its verify did not hold:
//	result.workloadsReady did not satisfy resultTrue
//
// on a cluster where nine of eleven pods were healthy. The two that were not
// were voice and voice-agent, crash-looping against an empty livekit-secrets --
// which they do BY DESIGN (memql#2416), and which gate_voice_lane_post_sync
// exists to prevent by scaling the lane to 0 when no credentials are present.
//
// The gate was called. It simply lost a race, by four seconds:
//
//	05:52:14Z  ArgoCD Application created   (the gate begins waiting)
//	05:54:18Z  the Deployments appear       -> 124s
//	           the gate's hardcoded bound   -> 120s
//
// It returned 0, the lane started ungated, and wait_for_workloads could not
// pass. Confirmed independently by object generation: after the install `voice`
// was at generation=1 -- creation, nothing had scaled it.
//
// WHY A PRIVATE BUDGET WAS THE DEFECT, rather than the number being too small.
// What the gate waits for is ArgoCD materialising the Deployments, and the
// install ALREADY has an operator-tunable budget for exactly that:
// `--argocd-timeout` / MEMQL_K3D_ARGOCD_TIMEOUT. A machine slow enough to need
// `MEMQL_K3D_ARGOCD_TIMEOUT=600` is precisely the machine whose Deployments take
// longer than 120s to appear -- and raising that documented knob did nothing,
// because the gate was not listening to it. Two budgets for one wait is how a
// tunable stops tuning the thing it names.
//
// So the assertion is not "the bound is bigger". It is that the bound IS the
// ArgoCD budget, which is falsifiable in both directions: the gate must still
// give up when that budget is small, and must still be waiting past 120s when
// it is large.
//
// up.sh cannot be driven end-to-end from a test -- it creates clusters and
// installs ArgoCD -- so it is sourced and the gate exercised against a fake
// kubectl, the same seam up_workloads_wait_test.go uses.

// voiceGateFakeKubectl reports `get deploy voice` as ABSENT until the call
// count in $FAKE_APPEAR_AFTER is reached, which is how a slow ArgoCD looks from
// inside the gate. Each probe is recorded so the test can count them.
const voiceGateFakeKubectl = `#!/usr/bin/env bash
case "$*" in
  *"get deploy voice"*)
    n=0
    [ -f "$FAKE_PROBE_COUNT" ] && n="$(cat "$FAKE_PROBE_COUNT")"
    n=$((n + 1))
    printf '%s' "$n" > "$FAKE_PROBE_COUNT"
    if [ "$n" -ge "${FAKE_APPEAR_AFTER:-999}" ]; then
      printf 'deployment.apps/voice\n'
      exit 0
    fi
    exit 1 ;;
esac
exit 0
`

// runVoiceGate sources up.sh and calls gate_voice_lane_post_sync with the given
// ArgoCD budget, against a fake kubectl that makes the voice Deployment appear
// only after appearAfter probes. It returns how many probes happened, whether
// the gate reached seed-secrets, and the combined output.
func runVoiceGate(t *testing.T, argocdTimeout, appearAfter int) (probes int, gated bool, out string) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	root := repoRoot(t)
	tmp := t.TempDir()

	if err := os.WriteFile(filepath.Join(tmp, "kubectl"), []byte(voiceGateFakeKubectl), 0o755); err != nil {
		t.Fatalf("write fake kubectl: %v", err)
	}
	// The gate shells out to seed-secrets.sh via $SCRIPT_DIR. Pointing SCRIPT_DIR
	// at a stub is what makes "did the gate actually run?" observable without
	// touching a cluster.
	stub := "#!/usr/bin/env bash\nprintf 'GATE-RAN %s\\n' \"$*\"\n"
	if err := os.WriteFile(filepath.Join(tmp, "seed-secrets.sh"), []byte(stub), 0o755); err != nil {
		t.Fatalf("write seed-secrets stub: %v", err)
	}

	probeCount := filepath.Join(tmp, "probes")
	harness := filepath.Join(tmp, "harness.sh")
	body := "#!/usr/bin/env bash\n" +
		"set -uo pipefail\n" +
		"source \"" + filepath.Join(root, "scripts", "k3d", "up.sh") + "\"\n" +
		"NAMESPACE=memql\n" +
		"REPO_ROOT=" + root + "\n" +
		"SCRIPT_DIR=" + tmp + "\n" +
		"ARGOCD_TIMEOUT=" + strconv.Itoa(argocdTimeout) + "\n" +
		// The gate sleeps between probes; a test that honoured that would take
		// as long as the budget. Overriding sleep keeps the LOGIC under test and
		// drops only the waiting.
		"function sleep() { :; }\n" +
		"gate_voice_lane_post_sync\n"
	if err := os.WriteFile(harness, []byte(body), 0o755); err != nil {
		t.Fatalf("write harness: %v", err)
	}

	cmd := exec.Command("bash", harness)
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"PATH="+tmp+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_PROBE_COUNT="+probeCount,
		"FAKE_APPEAR_AFTER="+strconv.Itoa(appearAfter),
	)
	b, err := cmd.CombinedOutput()
	if _, ok := err.(*exec.ExitError); !ok && err != nil {
		t.Fatalf("harness failed: %v\n%s", err, b)
	}
	out = string(b)

	if raw, readErr := os.ReadFile(probeCount); readErr == nil {
		probes, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
	}
	gated = strings.Contains(out, "GATE-RAN")
	t.Logf("probes=%d gated=%v output:\n%s", probes, gated, out)
	return probes, gated, out
}

// THE REGRESSION. With the ArgoCD budget raised -- which is what an operator on
// a slow machine is told to do -- the gate must still be waiting well past the
// old hardcoded 120s. At a 5s interval, 120s is 24 probes; a Deployment that
// appears on the 30th probe is the 124s case that failed in the field.
//
// Against the old code this fails: the gate gives up at 24 probes and never
// reaches seed-secrets, leaving the lane ungated.
func TestVoiceGateWaitsForTheArgocdBudgetNotAPrivate120s(t *testing.T) {
	probes, gated, out := runVoiceGate(t, 600, 30)

	if !gated {
		t.Fatalf("the gate never reached seed-secrets with a 600s ArgoCD budget -- "+
			"a Deployment appearing after ~150s is inside that budget and is the "+
			"case memql#3877 failed on (124s vs a hardcoded 120s).\noutput:\n%s", out)
	}
	if probes < 25 {
		t.Errorf("the gate stopped probing after %d probes (~%ds at a 5s interval); "+
			"with ARGOCD_TIMEOUT=600 it must not give up around the old 120s bound",
			probes, probes*5)
	}
}

// The other direction, so the fix is not just "wait longer". A SMALL budget must
// still expire -- otherwise the gate could hang a bring-up indefinitely on a
// cluster where ArgoCD is genuinely broken and the Deployments never arrive.
func TestVoiceGateStillGivesUpWhenTheArgocdBudgetIsSmall(t *testing.T) {
	_, gated, out := runVoiceGate(t, 10, 999)

	if gated {
		t.Errorf("the gate ran seed-secrets although the voice Deployment never appeared:\n%s", out)
	}
	if !strings.Contains(out, "NOT gated") {
		t.Errorf("expiry did not say the lane is ungated:\n%s", out)
	}
}

// The expiry must name the CONSEQUENCE, not just the fact.
//
// This is the half that cost the most time in the field. The gate deferring is
// invisible; what the operator sees is the install failing two lines later at
// `workloadsReady did not satisfy resultTrue` -- a different step, reporting a
// symptom, naming neither voice nor LiveKit nor the deferral. A warning that
// says "the workload wait below cannot pass" turns that into a five-second fix.
func TestVoiceGateExpirySaysTheReadinessWaitCannotPass(t *testing.T) {
	_, _, out := runVoiceGate(t, 10, 999)

	for _, want := range []string{"workloadsReady", "make secrets", "MEMQL_K3D_ARGOCD_TIMEOUT"} {
		if !strings.Contains(out, want) {
			t.Errorf("the expiry warning does not mention %q -- an operator reading only this "+
				"output has to guess what failed and how to fix it:\n%s", want, out)
		}
	}
}
