// Getting the bootstrap values into the RUNNING mesh (znasllc-io/memql#3588).
//
// Writing the Secret was never the job. Container environment is read once, at
// container start; the install seeds this Secret AFTER `clusterUp` has waited for
// every pod to be Available, and the overlay mounts it `optional: true` -- so on
// a fresh install the values landed in a Secret that no running process would
// ever read, `bootstrapComplete` said true, no cluster owner was created, and
// `magicLink` failed two steps later holding a failure it did not cause.
//
// So the step now converges: write the Secret, then make sure the processes that
// read it are running with it.
//
// THE CASE THAT MATTERS MOST IS THE SECOND RUN. Restarting identity is
// DESTRUCTIVE to a cluster nobody has claimed yet:
//
//   - identity emits the owner's magic link into its log at boot, and
//     `magic-link.sh` recovers it with `kubectl logs deploy/identity` -- from a
//     LIVE pod. Delete that pod and the link goes with it.
//   - a restarted identity does NOT re-emit it. EvaluateAutoBootstrap returns
//     BootstrapActionSuppress once the clusterSettings row exists (memql#1829,
//     deliberately, so a restart does not spam the owner).
//
// Those two together mean an unnecessary roll makes the cluster permanently
// unclaimable by this path. The roll must therefore happen EXACTLY ONCE -- on the
// run that first supplies the values -- which is why the decision is recorded on
// the Secret rather than re-derived, and why "a re-run rolls nothing" is asserted
// as hard as "a first run rolls everything".
//
// Hermetic: kubectl is a stub over a state directory. No cluster is touched.
package install

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// -----------------------------------------------------------------------
// a cluster with Deployments, pods, and a Secret that remembers
// -----------------------------------------------------------------------

// sbMeshWorld is a stub kubectl backed by files:
//
//	deploys      one line per Deployment: "<name> <replicas> <secretRef>..."
//	pods         one line per pod:       "<name> <deployment> <ready>"
//	annotations  one line per annotation written to the Secret
//	argv         every invocation
//
// `delete pods` removes the matching pods and creates replacements whose
// readiness is $FAKE_REPLACEMENT_READY -- which is how the "replacements never
// come up" case is driven without waiting on anything real.
type sbMeshWorld struct {
	env   []string
	state string
}

const sbMeshKubectl = `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$FAKE_STATE/argv"
if [[ "${1:-}" == "--context" ]]; then shift 2; fi
if [[ "${1:-}" == --namespace=* || "${1:-}" == "-n" ]]; then
  [[ "${1:-}" == "-n" ]] && shift 2 || shift
fi

verb="${1:-}"; shift || true
args="$*"

case "$verb" in
  cluster-info) echo "Kubernetes control plane is running"; exit 0 ;;
  create)       echo "apiVersion: v1"; echo "kind: Secret"; exit 0 ;;
  apply)        cat > /dev/null; echo "secret/memql-bootstrap configured"; exit 0 ;;
  annotate)
    for a in $args; do
      case "$a" in *=*) [[ "$a" == memql* || "$a" == *"/"*"="* ]] && printf '%s\n' "$a" >> "$FAKE_STATE/annotations" ;; esac
    done
    echo "secret/memql-bootstrap annotated"; exit 0 ;;
  delete)
    # delete pods --selector=app.kubernetes.io/name=<deploy>
    d="${args##*name=}"; d="${d%% *}"
    grep -v " ${d} " "$FAKE_STATE/pods" > "$FAKE_STATE/pods.tmp" 2>/dev/null || true
    mv "$FAKE_STATE/pods.tmp" "$FAKE_STATE/pods"
    replicas="$(awk -v d="$d" '$1==d {print $2}' "$FAKE_STATE/deploys")"
    # A NEW NAME EVERY TIME, as a real ReplicaSet gives. Reusing one would make
    # the "no pod from before the roll is still running" check unsatisfiable, and
    # the test would be measuring the stub rather than the script.
    gen="$(cat "$FAKE_STATE/generation" 2>/dev/null || echo 0)"
    gen=$((gen + 1)); printf '%s' "$gen" > "$FAKE_STATE/generation"
    i=0
    while [ "$i" -lt "${replicas:-0}" ]; do
      printf '%s-g%s-%s %s %s\n' "$d" "$gen" "$i" "$d" "${FAKE_REPLACEMENT_READY:-true}" >> "$FAKE_STATE/pods"
      i=$((i + 1))
    done
    echo "pod deleted"; exit 0 ;;
  get)
    # DISPATCH ON THE RESOURCE, which is the first word. Matching anywhere in the
    # argument string is what a stub gets wrong: every real call carries
    # --namespace=<ns>, so a *namespace* pattern swallows all of them and the
    # script under test sees an empty answer to every question.
    resource="${args%% *}"
    case "$resource" in
      namespace|namespaces)
        exit 0 ;;
      secret|secrets)
        val="$(awk -F= '$1 ~ /rolled-for/ {print $2}' "$FAKE_STATE/annotations" 2>/dev/null | tail -1)"
        # go-template prints <no value> for a key that is not there.
        [ -n "$val" ] && printf '%s' "$val" || printf '<no value>'
        exit 0 ;;
      deployments)
        while read -r name replicas refs; do
          [ -n "$name" ] || continue
          printf '%s %s %s\n' "$name" "$replicas" "$refs"
        done < "$FAKE_STATE/deploys"
        exit 0 ;;
      deploy)
        # The pod selector, read off the Deployment.
        d="${args#deploy }"; d="${d%% *}"
        printf 'app.kubernetes.io/name=%s,' "$d"
        exit 0 ;;
      pods)
        d="${args##*name=}"; d="${d%%,*}"; d="${d%% *}"
        while read -r pod deploy ready; do
          [ "$deploy" = "$d" ] || continue
          printf '%s %s\n' "$pod" "$ready"
        done < "$FAKE_STATE/pods"
        exit 0 ;;
    esac
    exit 0 ;;
esac
exit 0
`

// sbNewMeshWorld builds the cluster an install actually meets: nine engine nodes
// carrying the bootstrap Secret plus two that do not, each with a running pod.
func sbNewMeshWorld(t *testing.T) sbMeshWorld {
	t.Helper()
	bin := t.TempDir()
	state := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(sbMeshKubectl), 0o755); err != nil {
		t.Fatalf("write kubectl stub: %v", err)
	}

	var deploys, pods strings.Builder
	// The nine the local overlay patches, exactly as
	// deploy/k8s/overlays/local/kustomization.yaml names them.
	for _, d := range []string{"identity", "bff", "cognition", "agent", "planner", "workbench", "mcp"} {
		fmt.Fprintf(&deploys, "%s 1 memql-secrets memql-bootstrap\n", d)
		fmt.Fprintf(&pods, "%s-old-0 %s true\n", d, d)
	}
	// The voice lane: gated to zero on a machine with no LiveKit credentials
	// (memql#2416), and still a consumer. Nothing to roll and nothing to wait
	// for -- a wait that counted "at least one ready pod" would hang here.
	for _, d := range []string{"voice", "voice-agent"} {
		fmt.Fprintf(&deploys, "%s 0 memql-secrets memql-bootstrap\n", d)
	}
	// Infrastructure, which reads no bootstrap values.
	for _, d := range []string{"postgres", "azurite"} {
		fmt.Fprintf(&deploys, "%s 1 memql-secrets\n", d)
		fmt.Fprintf(&pods, "%s-old-0 %s true\n", d, d)
	}

	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(state, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("deploys", deploys.String())
	write("pods", pods.String())
	write("annotations", "")
	write("argv", "")

	return sbMeshWorld{
		env: []string{
			"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
			"FAKE_STATE=" + state,
			"STUB_ARGV=" + filepath.Join(state, "argv"),
			// Nothing here should ever wait long; a hang is a failure, not patience.
			"MEMQL_BOOTSTRAP_ROLL_TIMEOUT=5",
		},
		state: state,
	}
}

func (w sbMeshWorld) read(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(w.state, name))
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

/** Every pod currently "running", as "<pod> <deployment>" lines. */
func (w sbMeshWorld) pods(t *testing.T) string { return w.read(t, "pods") }

/** The deployments whose pods were deleted, in order. */
func (w sbMeshWorld) rolled(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(w.read(t, "argv"), "\n") {
		if !strings.HasPrefix(line, "delete pods") && !strings.Contains(line, " delete pods") {
			continue
		}
		if i := strings.Index(line, "name="); i >= 0 {
			out = append(out, strings.TrimSpace(line[i+len("name="):]))
		}
	}
	return out
}

/** Pretends a previous run already rolled the mesh for this exact digest. */
func (w sbMeshWorld) markRolledFor(t *testing.T, digest string) {
	t.Helper()
	p := filepath.Join(w.state, "annotations")
	if err := os.WriteFile(p, []byte("memql.io/rolled-for="+digest+"\n"), 0o644); err != nil {
		t.Fatalf("write annotations: %v", err)
	}
}

/** The digest the step stamped, or empty. */
func (w sbMeshWorld) rolledFor(t *testing.T) string {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(w.read(t, "annotations")), "\n") {
		if _, v, ok := strings.Cut(line, "="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// -----------------------------------------------------------------------
// a fresh install: the values have to reach the processes that read them
// -----------------------------------------------------------------------

func TestSeedBootstrapRestartsTheNodesThatReadTheSecret(t *testing.T) {
	w := sbNewMeshWorld(t)
	stdout, stderr, code := sbRun(t, w.env, sbCompleteArgs()...)
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, stdout, stderr)
	}
	env, res := sbParse(t, stdout)
	if !env.OK || !res.BootstrapComplete {
		t.Fatalf("ok=%v bootstrapComplete=%v, want both true: %s", env.OK, res.BootstrapComplete, stdout)
	}

	rolled := w.rolled(t)
	// Every consumer, and only consumers. identity is the one that creates the
	// owner; the rest need the AI provider key in the same Secret.
	for _, want := range []string{"identity", "bff", "cognition", "agent", "planner", "workbench", "mcp"} {
		if !containsString(rolled, want) {
			t.Errorf("%s consumes the bootstrap Secret and was not restarted, so it is still running\n"+
				"without the values -- which is the whole defect (rolled: %v)", want, rolled)
		}
	}
	for _, never := range []string{"postgres", "azurite"} {
		if containsString(rolled, never) {
			t.Errorf("%s does not read the bootstrap Secret and must not be restarted for it", never)
		}
	}

	// No CONSUMER pod from before the seed is still running. postgres and azurite
	// keep theirs, which is the other half of the assertion above.
	for _, line := range strings.Split(strings.TrimSpace(w.pods(t)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.Contains(fields[0], "-old-") {
			continue
		}
		if fields[1] == "postgres" || fields[1] == "azurite" {
			continue
		}
		t.Errorf("%s reads the bootstrap Secret and is still running a pod that predates it: %s",
			fields[1], fields[0])
	}
}

// The decision is RECORDED, so the next run can know the mesh already has these
// values without asking every pod what its environment says.
func TestSeedBootstrapRecordsWhatTheMeshWasRolledFor(t *testing.T) {
	w := sbNewMeshWorld(t)
	if _, stderr, code := sbRun(t, w.env, sbCompleteArgs()...); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if w.rolledFor(t) == "" {
		t.Fatalf("nothing was recorded on the Secret, so the next run cannot tell whether the mesh\n" +
			"is already running with these values -- and will either roll again (destroying the\n" +
			"recoverable magic link) or skip when it should not")
	}
}

// -----------------------------------------------------------------------
// THE CASE THAT MATTERS MOST: a second run must roll NOTHING
// -----------------------------------------------------------------------
//
// Re-running the install IS the repair in this installer, so this path is
// ordinary rather than exotic. And an unnecessary roll is not a wasted minute:
// it deletes the identity pod whose log holds the only copy of the owner's magic
// link, and a restarted identity will not emit another (BootstrapActionSuppress,
// memql#1829). The cluster becomes unclaimable.
func TestSeedBootstrapDoesNotRollTwiceForTheSameValues(t *testing.T) {
	w := sbNewMeshWorld(t)
	if _, stderr, code := sbRun(t, w.env, sbCompleteArgs()...); code != 0 {
		t.Fatalf("first run: exit %d: %s", code, stderr)
	}
	first := len(w.rolled(t))
	if first == 0 {
		t.Fatalf("the first run rolled nothing, so this case proves nothing")
	}
	podsAfterFirst := w.pods(t)

	stdout, stderr, code := sbRun(t, w.env, sbCompleteArgs()...)
	if code != 0 {
		t.Fatalf("second run: exit %d: %s", code, stderr)
	}
	_, res := sbParse(t, stdout)
	if !res.BootstrapComplete {
		t.Errorf("the second run must still report the bootstrap complete -- it is, and nothing about\n" +
			"re-running should read as a regression")
	}
	if len(w.rolled(t)) != first {
		t.Errorf("the second run restarted %d more Deployment(s). Restarting identity deletes the pod\n"+
			"whose log holds the owner's magic link, and a restarted identity does not emit another\n"+
			"(BootstrapActionSuppress) -- so the cluster can no longer be claimed.",
			len(w.rolled(t))-first)
	}
	if w.pods(t) != podsAfterFirst {
		t.Errorf("the running pods changed on a no-op re-run:\nbefore:\n%s\nafter:\n%s", podsAfterFirst, w.pods(t))
	}
}

// The other half of the same rule: when the VALUES change, the mesh must be
// rolled again. A recorded marker that never invalidates would leave an operator
// who corrected the owner's email with a cluster still running the old one.
func TestSeedBootstrapRollsAgainWhenTheValuesChange(t *testing.T) {
	w := sbNewMeshWorld(t)
	if _, stderr, code := sbRun(t, w.env, sbCompleteArgs()...); code != 0 {
		t.Fatalf("first run: exit %d: %s", code, stderr)
	}
	first := len(w.rolled(t))

	changed := append([]string{}, sbCompleteArgs()...)
	for i, a := range changed {
		if strings.HasPrefix(a, "--owner-email=") {
			changed[i] = "--owner-email=grace@example.com"
		}
	}
	if _, stderr, code := sbRun(t, w.env, changed...); code != 0 {
		t.Fatalf("second run: exit %d: %s", code, stderr)
	}
	if len(w.rolled(t)) <= first {
		t.Errorf("the owner email changed and nothing was restarted, so the mesh is still running the\n" +
			"previous values while the Secret claims the new ones")
	}
}

// A marker left by an interrupted run must not be trusted as "already rolled"
// when it names different values. Belt to the braces above: the recorded digest
// is compared, never merely checked for presence.
func TestSeedBootstrapIgnoresAMarkerForDifferentValues(t *testing.T) {
	w := sbNewMeshWorld(t)
	w.markRolledFor(t, "0000000000000000000000000000000000000000000000000000000000000000")
	if _, stderr, code := sbRun(t, w.env, sbCompleteArgs()...); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !containsString(w.rolled(t), "identity") {
		t.Errorf("a stale marker was treated as proof the mesh had the current values")
	}
}

// -----------------------------------------------------------------------
// a roll that does not come back is not a success
// -----------------------------------------------------------------------
//
// The failure this whole issue is about was a step reporting success for the half
// of its job it could see. If the replacements never become ready, the values are
// no more present than before, and saying otherwise sends the operator to
// magicLink to be told something unrelated.
func TestSeedBootstrapFailsWhenTheRestartedNodesNeverComeBack(t *testing.T) {
	w := sbNewMeshWorld(t)
	w.env = append(w.env, "FAKE_REPLACEMENT_READY=false")

	stdout, _, code := sbRun(t, w.env, sbCompleteArgs()...)
	if code == 0 {
		t.Fatalf("exit 0 on a mesh whose restarted nodes never became ready: %s", stdout)
	}
	env, res := sbParse(t, stdout)
	if env.OK || res.BootstrapComplete {
		t.Errorf("ok=%v bootstrapComplete=%v -- neither may be true when the values did not reach a\n"+
			"single running process", env.OK, res.BootstrapComplete)
	}
	if env.Error == nil || env.Error.Code != 5 {
		t.Errorf("error = %+v, want code 5 (the operation failed)", env.Error)
	}
	// cap_fail's message rides the ENVELOPE, and it has to name which node so the
	// operator has somewhere to look.
	if env.Error != nil && !strings.Contains(env.Error.Message, "identity") {
		t.Errorf("the failure does not name the Deployment that did not come back: %q", env.Error.Message)
	}
	if w.rolledFor(t) != "" {
		t.Errorf("the mesh was marked as rolled for values it is not running -- the next run would\n" +
			"skip the roll and the cluster would never bootstrap")
	}
}

// ONE budget over N consumers has to buy N ROLLS IN PARALLEL, not N rolls one
// after another (znasllc-io/memql#3880).
//
// `_SB_ROLL_DEADLINE` is computed once for the whole roll, but it used to be
// consulted inside a per-consumer wait that ran immediately after that
// consumer's own delete. So the budget covered roughly ONE restart while the
// work needed N of them, and which consumer got blamed was just whichever one
// the clock happened to run out on. Measured on a real failing install, where
// each node took about 100s to come back:
//
//	agent      deleted 07:39:45  ready 07:41:25   cumulative 100s
//	bff        deleted 07:41:29  ready 07:43:09   cumulative 204s
//	cognition  deleted 07:43:12  ready 07:44:52   cumulative 307s   <- 240s budget
//
// Nothing was wrong with cognition. Raising the budget only moves the boundary,
// because the requirement grows with the number of consumers -- which is why
// the fix is to issue every delete FIRST and then wait on all of them, so the
// restarts overlap and the elapsed time is one restart rather than N.
//
// That is observable without a clock: with the rolls overlapping, the FIRST
// Deployment is still being waited on after the LAST one has been deleted.
// Serially it is long finished by then.
func TestSeedBootstrapRollsTheMeshInParallelSoOneBudgetCoversEveryConsumer(t *testing.T) {
	w := sbNewMeshWorld(t)

	stdout, stderr, code := sbRun(t, w.env, sbCompleteArgs()...)
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, stdout, stderr)
	}

	rolled := w.rolled(t)
	if len(rolled) < 2 {
		t.Fatalf("rolled %v -- this test cannot say anything about ordering unless the mesh has\n"+
			"several consumers to roll", rolled)
	}
	first := rolled[0]

	// Split the call log at the last delete. Everything after it is work the step
	// did once every restart was already in flight.
	lines := strings.Split(strings.TrimSpace(w.read(t, "argv")), "\n")
	lastDelete := -1
	for i, line := range lines {
		if strings.Contains(line, "delete pods") {
			lastDelete = i
		}
	}
	if lastDelete < 0 {
		t.Fatalf("no delete in the call log:\n%s", strings.Join(lines, "\n"))
	}

	waitedOnFirstAfterLastDelete := false
	for _, line := range lines[lastDelete+1:] {
		if strings.Contains(line, "get pods") && strings.Contains(line, "name="+first) {
			waitedOnFirstAfterLastDelete = true
			break
		}
	}
	if !waitedOnFirstAfterLastDelete {
		t.Errorf("the roll is SERIAL: %q was already waited out before %q was even deleted, so the\n"+
			"single roll budget has to cover %d restarts end to end instead of one.\n"+
			"call log after the last delete:\n%s",
			first, rolled[len(rolled)-1], len(rolled),
			strings.Join(lines[lastDelete+1:], "\n"))
	}

	// Guard the guard: overlapping the restarts must not lose any of them, and it
	// must not turn "wait for it to come back" into "delete and hope".
	if len(rolled) != 7 {
		t.Errorf("rolled %v -- want all seven consumers of the bootstrap Secret", rolled)
	}
	_, res := sbParse(t, stdout)
	if !res.BootstrapComplete {
		t.Errorf("bootstrapComplete=false after a roll every node came back from")
	}
}

// A cluster with no Deployments yet is not a failure: it is what seeding BEFORE
// the workloads exist looks like, and the values will be read at first boot.
func TestSeedBootstrapAcceptsAClusterWithNoWorkloadsYet(t *testing.T) {
	w := sbNewMeshWorld(t)
	if err := os.WriteFile(filepath.Join(w.state, "deploys"), []byte(""), 0o644); err != nil {
		t.Fatalf("empty the deployments: %v", err)
	}
	stdout, stderr, code := sbRun(t, w.env, sbCompleteArgs()...)
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, stdout, stderr)
	}
	_, res := sbParse(t, stdout)
	if !res.BootstrapComplete {
		t.Error("a cluster whose workloads do not exist yet will read the Secret at first boot, which\n" +
			"is the ordering this step would prefer to have")
	}
	if len(w.rolled(t)) != 0 {
		t.Errorf("something was restarted in a cluster with no Deployments: %v", w.rolled(t))
	}
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
