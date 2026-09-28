package k3d

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// progress_lines_test.go -- the phases the long capabilities report.
//
// Creating the cluster and rebuilding the images are most of an install's and
// a rebuild's wait. Each now says where it has got to with `cap_progress`
// (scripts/lib/capability.sh), which the editor turns into the status line
// under its progress bar ("Starting services 5 of 9") and keeps out of the log.
//
// What is asserted: the counts are REAL -- the wait reports how many services
// are Available, the rebuild how many images it has built and imported -- and
// every phase the scripts report is named in the operator's words. The flow
// through up.sh's main() is too heavy to drive from a test (it creates a
// cluster), so its section-level phases are checked where they are written.

// availableFakeKubectl answers the Deployments' Available conditions from
// $FAKE_AVAILABLE ("name True|False" per line) and fails any `wait` naming a
// Deployment in $FAKE_UNREADY, the way a Deployment that never comes up does.
const availableFakeKubectl = `#!/usr/bin/env bash
case "$*" in
  *"get deployments"*jsonpath*)
    printf '%s\n' "$FAKE_AVAILABLE"
    exit 0 ;;
  *"get deployments"*custom-columns*)
    while read -r name _; do
      [ -n "$name" ] && printf '%s   1\n' "$name"
    done <<< "$FAKE_AVAILABLE"
    exit 0 ;;
  *"get deployments"*)
    while read -r name _; do
      [ -n "$name" ] && printf 'deployment.apps/%s\n' "$name"
    done <<< "$FAKE_AVAILABLE"
    exit 0 ;;
  *"get pods"*)
    printf '{"items":[]}\n'
    exit 0 ;;
  *wait*)
    for unready in $FAKE_UNREADY; do
      case "$*" in *"/$unready"*) exit 1 ;; esac
    done
    exit 0 ;;
esac
exit 0
`

func runWaitWithAvailability(t *testing.T, available, unready string) string {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	root := repoRoot(t)
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "kubectl"), []byte(availableFakeKubectl), 0o755); err != nil {
		t.Fatalf("write fake kubectl: %v", err)
	}
	harness := filepath.Join(tmp, "harness.sh")
	body := "#!/usr/bin/env bash\n" +
		"set -uo pipefail\n" +
		"source \"" + filepath.Join(root, "scripts", "k3d", "up.sh") + "\"\n" +
		"NAMESPACE=memql\n" +
		"wait_for_workloads\n"
	if err := os.WriteFile(harness, []byte(body), 0o755); err != nil {
		t.Fatalf("write harness: %v", err)
	}
	cmd := exec.Command("bash", harness)
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"PATH="+tmp+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_AVAILABLE="+available,
		"FAKE_UNREADY="+unready,
		"MEMQL_K3D_WORKLOAD_TIMEOUT=1s",
	)
	out, err := cmd.CombinedOutput()
	if _, ok := err.(*exec.ExitError); !ok && err != nil {
		t.Fatalf("harness failed: %v\n%s", err, out)
	}
	return string(out)
}

// While the wait runs, the phase is the count of services that are up.
func TestTheWorkloadWaitReportsHowManyServicesAreUp(t *testing.T) {
	out := runWaitWithAvailability(t, "bff True\nidentity False\nedge True", "identity")
	if !strings.Contains(out, "::memql-progress:: 2/3 Starting services\n") {
		t.Fatalf("the wait did not report the real count of Available services (2 of 3):\n%s", out)
	}
	if strings.Contains(out, "3/3 Starting services") {
		t.Fatalf("the wait reported every service up while one never became Available:\n%s", out)
	}
}

// When everything comes up the last word is the whole count.
func TestTheWorkloadWaitEndsOnTheWholeCount(t *testing.T) {
	out := runWaitWithAvailability(t, "bff True\nidentity True", "")
	lines := progressLines(out)
	if len(lines) == 0 || lines[len(lines)-1] != "::memql-progress:: 2/2 Starting services" {
		t.Fatalf("the last phase is not 2 of 2: %q\n%s", lines, out)
	}
}

// The rebuild counts images through both passes.
func TestTheRebuildCountsImagesThroughBothPasses(t *testing.T) {
	_, ok, out := runBuildAndImport(t, "", "identity", "bff", "agent")
	if !ok {
		t.Fatalf("build_and_import_nodes failed with no failing build:\n%s", out)
	}
	want := []string{
		"::memql-progress:: 0/3 Building images",
		"::memql-progress:: 1/3 Building images",
		"::memql-progress:: 2/3 Building images",
		"::memql-progress:: 0/3 Importing images",
		"::memql-progress:: 1/3 Importing images",
		"::memql-progress:: 2/3 Importing images",
	}
	if got := progressLines(out); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("progress lines:\n  got  %q\n  want %q\noutput:\n%s", got, want, out)
	}
}

// capProgressCall matches a literal `cap_progress "<label>"` call.
var capProgressCall = regexp.MustCompile(`cap_progress "([^"$]+)"`)

// Every phase a script reports is a status line an operator reads: sentence
// case, six words at most, and in their words. And the phases the flow cannot
// be driven through in a test are at least all still written down.
func TestEveryReportedPhaseIsNamedForTheOperator(t *testing.T) {
	root := repoRoot(t)
	want := map[string][]string{
		"scripts/k3d/up.sh": {
			"Creating the cluster", "Installing ArgoCD", "Registering services",
			"Seeding secrets", "Starting services",
		},
		"scripts/k3d/dev.sh":              {"Building images", "Importing images", "Restarting services"},
		"scripts/install/update-stack.sh": {"Downloading updates"},
	}
	for file, labels := range want {
		b, err := os.ReadFile(filepath.Join(root, file))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		found := map[string]bool{}
		for _, m := range capProgressCall.FindAllStringSubmatch(string(b), -1) {
			label := m[1]
			found[label] = true
			if words := strings.Fields(label); len(words) > 6 {
				t.Errorf("%s: phase %q is more than six words", file, label)
			}
			if !sentenceCase(label) {
				t.Errorf("%s: phase %q is not sentence case", file, label)
			}
			for _, jargon := range []string{"k3d", "kubectl", "capability", "envelope", "graph", "receipt", "wave"} {
				if strings.Contains(strings.ToLower(label), jargon) {
					t.Errorf("%s: phase %q uses the internal word %q", file, label, jargon)
				}
			}
		}
		for _, label := range labels {
			if !found[label] {
				t.Errorf("%s no longer reports the phase %q", file, label)
			}
		}
	}
}

func progressLines(out string) []string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "::memql-progress::") {
			lines = append(lines, l)
		}
	}
	return lines
}

// sentenceCase: a capital first word, and every later word lower case unless it
// is a product name.
func sentenceCase(label string) bool {
	words := strings.Fields(label)
	if len(words) == 0 || words[0][:1] != strings.ToUpper(words[0][:1]) {
		return false
	}
	for _, w := range words[1:] {
		switch w {
		case "ArgoCD", "MemQL", "Docker":
			continue
		}
		if w != strings.ToLower(w) {
			return false
		}
	}
	return true
}
