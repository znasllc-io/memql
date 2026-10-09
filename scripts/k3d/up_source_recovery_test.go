package k3d

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const droppedFetch = "ComparisonError: Failed to fetch default: git fetch origin --tags --force --prune failed exit status 128: error: RPC failed; curl 56 Recv failure: Connection reset by peer; fatal: early EOF; fatal: fetch-pack: invalid index-pack output"

// Advance the script's deadline instead of waiting real minutes. kubectl still
// runs as a process: a retry has to reach the Application, not just log a line.
const sourceWait = `function sleep() { SECONDS=$((SECONDS + $1)); }
APP_NAME=memql-local
TARGET_REVISION=0123456789abcdef
APP_COMPARE_TIMEOUT=90
wait_for_app_comparison
`

func sourceRecovery(t *testing.T, snippet string, env ...string) (string, int, string) {
	t.Helper()
	log := filepath.Join(t.TempDir(), "calls")
	env = append([]string{"FAKE_KUBECTL_LOG=" + log, "FAKE_APP_CONDITIONS=" + droppedFetch, "FAKE_APP_SYNC=OutOfSync"}, env...)
	out, code := runUpFunc(t, snippet, env...)
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	return out, code, string(calls)
}

func TestDroppedGitFetchIsRetriedBeforeOperatorTimeout(t *testing.T) {
	for _, app := range []string{"cert-manager", "cnpg-operator"} {
		t.Run(app, func(t *testing.T) {
			out, code, calls := sourceRecovery(t, `function sleep() { SECONDS=$((SECONDS + $1)); }
TARGET_REVISION=0123456789abcdef
MEMQL_K3D_OPERATOR_TIMEOUT=90
_wait_for_operator `+app+" "+app+" controller\n", "FAKE_CLEAR_ON_REFRESH=1", "FAKE_OPERATOR_AFTER_REFRESH=1")
			if code != 0 || !strings.Contains(calls, "rollout status deployment/controller") {
				t.Fatalf("operator failed to recover: %d\n%s\n%s", code, out, calls)
			}
			if strings.Count(calls, "annotate application "+app) != 1 || !strings.Contains(calls, "argocd.argoproj.io/refresh=hard") {
				t.Fatalf("must ask Argo for a fresh source comparison: %s", calls)
			}
		})
	}
}

func TestDroppedGitFetchRecoveryIsSharedByMesh(t *testing.T) {
	out, code, calls := sourceRecovery(t, sourceWait, "FAKE_CLEAR_ON_REFRESH=1")
	if code != 0 || strings.Count(calls, "annotate application") != 1 {
		t.Fatalf("mesh failed: %d\n%s\n%s", code, out, calls)
	}
}

func TestSourceRetriesAreBoundedAndKeepOriginalDiagnosis(t *testing.T) {
	out, code, calls := sourceRecovery(t, sourceWait)
	if code != 5 || strings.Count(calls, "annotate application") != 3 || !strings.Contains(out, droppedFetch) {
		t.Fatalf("unbounded/opaque retry: %d\n%s\n%s", code, out, calls)
	}
}

func TestInFlightRefreshDoesNotSpendRetriesOnOldFailure(t *testing.T) {
	out, code, calls := sourceRecovery(t, sourceWait, "FAKE_REFRESH_PENDING=1")
	if code != 5 || strings.Count(calls, "annotate application") != 1 || !strings.Contains(out, "deadline") {
		t.Fatalf("stale failure spent retries: %d\n%s\n%s", code, out, calls)
	}
}

func TestPermanentSourceErrorsDoNotRetry(t *testing.T) {
	for _, diagnosis := range []string{"ComparisonError: authentication required", "ComparisonError: app path does not exist", "ComparisonError: failed to unmarshal manifest", "InvalidSpecError: repository not permitted; connection reset by peer"} {
		t.Run(diagnosis, func(t *testing.T) {
			out, code, calls := sourceRecovery(t, sourceWait, "FAKE_APP_CONDITIONS="+diagnosis)
			if code != 5 || strings.Contains(calls, "annotate application") || !strings.Contains(out, diagnosis) {
				t.Fatalf("permanent error retried or lost: %d\n%s\n%s", code, out, calls)
			}
		})
	}
}

func TestUnknownSyncIsNotASuccessfulComparison(t *testing.T) {
	out, code, _ := sourceRecovery(t, sourceWait, "FAKE_APP_CONDITIONS=", "FAKE_APP_SYNC=Unknown")
	if code != 0 || strings.Contains(out, "reconciliation is under way") {
		t.Fatalf("Unknown was treated as success: %d\n%s", code, out)
	}
}

func TestSourceRefreshFailureIsReported(t *testing.T) {
	out, code, calls := sourceRecovery(t, sourceWait, "FAKE_REFRESH_EXIT=1")
	if code != 5 || strings.Count(calls, "annotate application") != 1 || !strings.Contains(out, "Could not request another source download") {
		t.Fatalf("refresh failure ignored: %d\n%s\n%s", code, out, calls)
	}
}

func TestTruncatedHTTPResponseIsRetried(t *testing.T) {
	out, code, calls := sourceRecovery(t, sourceWait, "FAKE_CLEAR_ON_REFRESH=1", "FAKE_APP_CONDITIONS=ComparisonError: RPC failed; curl 18 transfer closed with 33 bytes remaining to read; fatal: expected flush after ref listing")
	if code != 0 || strings.Count(calls, "annotate application") != 1 {
		t.Fatalf("truncated response did not recover: %d\n%s\n%s", code, out, calls)
	}
}

// Replay the Pop!_OS timeline: no status until a 300s Git timeout, then a
// successful retry at 612s. The old 300s deployment deadline ends before the
// first source failure is even published. No real sleeps or cluster needed.
func TestColdSourceRetryDoesNotConsumeDeploymentReadinessBudget(t *testing.T) {
	out, code, calls := sourceRecovery(t, `
function sleep() { SECONDS=$((SECONDS + $1)); }
function argocd_comparison_state() {
    if ((SECONDS < 300)); then printf '\n\n.'
    elif [[ ! -f "$FAKE_APP_READS.refreshed" ]]; then
        printf 'Unknown\n\nComparisonError: git fetch failed timeout after 5m0s.'
    elif ((SECONDS < 612)); then
        printf 'Unknown\nhard\nComparisonError: git fetch failed timeout after 5m0s.'
    else printf 'Synced\n\n.'; fi
}

unset SECONDS
SECONDS=0
_wait_for_operator cert-manager cert-manager controller
`, "FAKE_OPERATOR_AFTER_REFRESH=1")
	if code != 0 || strings.Count(calls, "annotate application") != 1 {
		t.Fatalf("cold fetch did not recover: %d\n%s\n%s", code, out, calls)
	}
	if !strings.Contains(calls, "rollout status deployment/controller -n cert-manager --timeout=300s") {
		t.Fatalf("source download consumed readiness time:\n%s", calls)
	}
	if !strings.Contains(out, "300s elapsed") || !strings.Contains(out, "600s elapsed") {
		t.Fatalf("cold fetch became a silent wait:\n%s", out)
	}
}

func TestOperatorStackDeadlineIsSharedAcrossNamespacesAndRollouts(t *testing.T) {
	out, code, calls := sourceRecovery(t, `
function _register_operator_app() { :; }
function kubectl() {
    command kubectl "$@"
    local result=$?
    case "$*" in *"rollout status"*) SECONDS=$((SECONDS + 20));; esac
    return "$result"
}
unset SECONDS
SECONDS=0
OPERATOR_STACK_TIMEOUT=75
install_operator_stack
`)
	if code != 5 || !strings.Contains(out, "deadline reached") {
		t.Fatalf("operator stack escaped its deadline: %d\n%s\n%s", code, out, calls)
	}
	if strings.Count(calls, "rollout status") != 4 || !strings.Contains(calls, "rollout status deployment/cnpg-controller-manager -n cnpg-system --timeout=15s") {
		t.Fatalf("second namespace received a fresh budget:\n%s", calls)
	}
	if strings.Contains(calls, "rollout status deployment/barman-cloud") {
		t.Fatalf("rollout started after the shared deadline:\n%s", calls)
	}
}

func TestPartialOperatorRetriesSourceForMissingSibling(t *testing.T) {
	out, code, calls := sourceRecovery(t, `
function sleep() { SECONDS=$((SECONDS + $1)); }
_wait_for_operator cert-manager cert-manager controller webhook
`, "FAKE_CLEAR_ON_REFRESH=1", "FAKE_OPERATOR_AFTER_REFRESH=1", "FAKE_OPERATOR_EXISTING=controller")
	if code != 0 || strings.Count(calls, "annotate application") != 1 || !strings.Contains(calls, "rollout status deployment/webhook") {
		t.Fatalf("partial operator did not recover: %d\n%s\n%s", code, out, calls)
	}
}

func TestUnresponsiveSourceStillStopsAtSharedDeadline(t *testing.T) {
	out, code, calls := sourceRecovery(t, `
function sleep() { SECONDS=$((SECONDS + $1)); }
unset SECONDS
SECONDS=0
OPERATOR_STACK_TIMEOUT=630
_wait_for_operator cert-manager cert-manager controller
`, "FAKE_OPERATOR_ABSENT=1", "FAKE_APP_SYNC=", "FAKE_APP_CONDITIONS=")
	if code != 5 || !strings.Contains(out, "timed out after 630s") || strings.Contains(calls, "rollout status") {
		t.Fatalf("unresponsive source escaped its deadline: %d\n%s\n%s", code, out, calls)
	}
}
