//go:build clustere2e

package clustere2e

// site_edge_invalidation_test.go -- the live-cluster half of the site edge's
// cross-node cache invalidation gate (memql#3714, Task 9, controller
// additions 7 + 8).
//
// CONTRACT UNDER TEST. Each edge replica runs its OWN process-local Resolver
// cache (component/edge/resolve.go) mapping a request Host to a
// v1:platform:site row. A write to that row -- createSite, updateSiteStatus,
// updateSiteBundle -- happens on WHICHEVER node handles the mutation, not
// necessarily any given edge replica, and must reach every OTHER edge
// replica's independent cache. component/edge.SiteInvalidationSubscriber
// does that by subscribing to the site concept's graph.node.{created,updated}
// events, forwarded cross-node by the routing rules in
// component/node/routing.go. The deterministic, always-in-CI proof that the
// ROUTING RULE forwards those two topics lives in
// component/node/routing_test.go's TestEvaluateRouting_SiteEdgeInvalidationBroadcast;
// this test is the live-cluster confirmation that the whole wired path --
// write, forward, subscribe, invalidate, re-resolve -- actually reaches a
// SECOND, INDEPENDENTLY IDENTIFIED replica within the TTL backstop.
//
// ADDITION 8 -- ASSERT ON THE REASON, NOT THE OUTCOME. A status code cannot
// distinguish "this replica always had the new value" from "this replica
// picked it up via invalidation" from "the TTL happened to expire before I
// looked" -- and a test that dials the `edge` Service (which load-balances
// across replicas) cannot even promise it asked a SPECIFIC replica twice. So
// this test does NOT go through the Service or the front door: it opens a
// `kubectl port-forward` directly to each of two named edge PODS, confirms
// each one's OWN identity via its OWN /healthz (nodeId == pod name, the same
// discriminator verify-frontdoor.sh's `precedence` check uses), warms BOTH
// replicas' caches with the pre-write value, writes once, and then asserts
// that the replica identified as "B" -- not "whichever replica answered" --
// now serves the NEW value, well under the 30s TTL backstop
// (MEMQL_EDGE_SITE_CACHE_TTL_SECONDS) so a TTL-driven refresh cannot explain
// a pass.
//
// WHY /runtime-config.json AND NOT THE BUNDLE. The status gate (draft=404,
// disabled=503, live=serves) in component/edge/handler.go's ServeHTTP runs
// BEFORE the bundle is opened, and GET /runtime-config.json is dispatched
// right after that gate and before the bundle lookup (runtimeconfig.go) --
// so this probe exercises the exact same resolver + status-gate path a real
// asset request would, without needing a real bundle on disk for a synthetic
// test hostname.
//
// PREREQUISITES, ALL SKIPPED GRACEFULLY WHEN ABSENT (mirroring token(t)):
//   - MEMQL_E2E_TOKEN for a CLUSTER OWNER. v1:platform:site is
//     @rowAuthz(clusterOwner) (dsl/platform/concepts.memql), so a token for an
//     ordinary registered user is refused by createSite/updateSiteStatus. The
//     token scripts/test/cluster-e2e.sh's seed_token() mints during a FRESH
//     cluster's first run is the cluster owner (it is whoever completes
//     /setup); a token from a cluster that already has an owner and was
//     seeded via plain /login is not, and this test will fail loud on the
//     mutation's error rather than silently no-op.
//   - kubectl on PATH, pointed at the cluster, with >=2 Running pods labeled
//     app.kubernetes.io/name=edge (`make up SERVERS=2 && make scale N=2`).
//
// RUN
//
//	MEMQL_E2E_TOKEN=<cluster owner JWT> go test -tags clustere2e -count=1 \
//	  -timeout=300s ./test/clustere2e/... -run TestSiteEdgeInvalidation_CrossReplica -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/core/id"
	memqlclient "github.com/znasllc-io/memql/sdk/go/client"
)

// k3dNamespace mirrors scripts/k3d/*.sh's own env var so this test targets
// the same namespace a manual `make up` / `make scale` run used.
func k3dNamespace() string {
	if v := os.Getenv("MEMQL_K3D_NAMESPACE"); v != "" {
		return v
	}
	return "memql"
}

// edgePodNames returns exactly two Running edge pod names, or skips. Skips
// rather than fails: this test needs direct pod addressability that only a
// real cluster with kubectl configured provides, the same posture token(t)
// takes for MEMQL_E2E_TOKEN.
func edgePodNames(t *testing.T) []string {
	t.Helper()
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl not on PATH -- this test needs direct pod access to a live cluster")
	}
	out, err := exec.Command("kubectl", "-n", k3dNamespace(), "get", "pods",
		"-l", "app.kubernetes.io/name=edge",
		"--field-selector=status.phase=Running",
		"-o", "jsonpath={.items[*].metadata.name}").Output()
	if err != nil {
		t.Skipf("kubectl get pods -l app.kubernetes.io/name=edge failed (no live cluster?): %v", err)
	}
	names := strings.Fields(string(out))
	if len(names) < 2 {
		t.Skipf("found %d Running edge pod(s) in namespace %q, want >=2 -- run "+
			"`make up SERVERS=2 && make scale N=2` (or `make up-refresh SERVERS=2 AGENTS=1`) first",
			len(names), k3dNamespace())
	}
	return names[:2] // exactly two named replicas -- see the file header.
}

// freeLocalPort allocates an ephemeral TCP port and releases it immediately
// for kubectl port-forward to bind. A small race window (another process
// could grab it first) is an accepted cost of this pattern in Go tests.
func freeLocalPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate a free local port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// healthzInfo is the subset of GetHealthz200JSONResponse
// (component/server/http_contract.go) this test needs to identify a replica.
type healthzInfo struct {
	Code     int
	NodeId   string
	NodeType string
}

func healthzProbe(port int) (healthzInfo, error) {
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
	if err != nil {
		return healthzInfo{}, err
	}
	defer resp.Body.Close()
	var body struct {
		NodeId   string `json:"nodeId"`
		NodeType string `json:"nodeType"`
	}
	code := resp.StatusCode
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return healthzInfo{Code: code}, err
	}
	return healthzInfo{Code: code, NodeId: body.NodeId, NodeType: body.NodeType}, nil
}

// podForward is a live `kubectl port-forward pod/<name>` process pinned to
// ONE named pod for its whole lifetime -- unlike dialing the `edge` Service,
// which load-balances, this is what makes "replica A" and "replica B" mean
// something across the whole test.
type podForward struct {
	podName string
	port    int
	nodeID  string
}

// startPodForward starts the forward and blocks until the pod's OWN /healthz
// answers 200 naming nodeType=="edge" -- positive identification, the same
// discriminator verify-frontdoor.sh's `precedence` check uses, rather than
// inferring readiness from "the port accepted a TCP connection."
func startPodForward(t *testing.T, ctx context.Context, podName string) *podForward {
	t.Helper()
	port := freeLocalPort(t)

	cmd := exec.CommandContext(ctx, "kubectl", "-n", k3dNamespace(), "port-forward",
		"pod/"+podName, fmt.Sprintf("%d:8085", port))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start kubectl port-forward to pod/%s: %v", podName, err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	})

	pf := &podForward{podName: podName, port: port}
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	var last healthzInfo
	for {
		info, err := healthzProbe(port)
		if err == nil && info.Code == http.StatusOK && info.NodeType == "edge" {
			if info.NodeId != podName {
				t.Fatalf("pod/%s's own /healthz reports nodeId %q, want %q -- "+
					"MEMQL_NODE_ID (fieldRef: metadata.name) is not tracking the pod name",
					podName, info.NodeId, podName)
			}
			pf.nodeID = info.NodeId
			return pf
		}
		lastErr, last = err, info
		if time.Now().After(deadline) {
			t.Fatalf("port-forward to pod/%s on :%d never became healthy within 30s "+
				"(last healthz=%+v, err=%v; kubectl stderr: %s)",
				podName, port, last, lastErr, stderr.String())
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// siteStatusCode issues GET /runtime-config.json through pf with an explicit
// Host header naming hostname -- there is no DNS involved, and none is
// needed: the edge resolves purely off the Host header (resolve.go).
func siteStatusCode(t *testing.T, pf *podForward, hostname string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d/runtime-config.json", pf.port), nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Host = hostname
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /runtime-config.json via pod/%s (nodeId=%s): %v", pf.podName, pf.nodeID, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func TestSiteEdgeInvalidation_CrossReplica(t *testing.T) {
	// memql#4212: a parity-cluster run failed this test after the negative-cache
	// prime. That is NOT the retired-contract class (createSite / updateSiteStatus
	// still exist on the engine SDK; nothing here calls mutationCreateSpace).
	// Do not invent a new product behavior to make the prime succeed -- the
	// create + invalidate path needs a live-cluster re-triage.
	tok := token(t)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	podNames := edgePodNames(t)
	pfA := startPodForward(t, ctx, podNames[0])
	pfB := startPodForward(t, ctx, podNames[1])
	if pfA.nodeID == pfB.nodeID || pfA.podName == pfB.podName {
		t.Fatalf("both port-forwards resolved to the same replica (pod=%s nodeId=%s) -- "+
			"not two distinct replicas", pfA.podName, pfA.nodeID)
	}
	t.Logf("replica A = pod/%s (nodeId=%s); replica B = pod/%s (nodeId=%s)",
		pfA.podName, pfA.nodeID, pfB.podName, pfB.nodeID)

	hostname := fmt.Sprintf("clustere2e-inv-%s.example.com", id.NewShortId())
	siteID := "v1:platform:site:" + id.NewShortId()

	// PRIME A NEGATIVE CACHE ENTRY ON BOTH REPLICAS BEFORE THE SITE EXISTS.
	// This is load-bearing, not decoration. Without it, a replica's FIRST
	// request for hostname after createSite is an ordinary cold-cache miss
	// that reads the database directly (resolve.go) -- which succeeds
	// whether or not any cross-node event ever arrives, because there is no
	// stale cache entry to invalidate. A test that resolves only AFTER
	// creating the site cannot tell "the created event crossed" apart from
	// "this replica had simply never been asked before" -- it passes
	// vacuously either way, which is exactly the failure mode addition 8
	// warns about applied to this test itself. Resolving the not-yet-real
	// hostname first forces a genuine miss to be CACHED (resolve.go: "a
	// miss is cached too") on each replica specifically, so the create that
	// follows has something on that replica it must actually evict.
	for _, pf := range []*podForward{pfA, pfB} {
		if code := siteStatusCode(t, pf, hostname); code != http.StatusNotFound {
			t.Fatalf("replica %s (pod/%s) returned %d for a hostname that does not exist yet, want 404 -- "+
				"cannot prime a negative cache entry to test invalidation against", pf.nodeID, pf.podName, code)
		}
	}
	t.Logf("primed a negative (404) cache entry for %s on both replicas", hostname)

	conns := openConnections(ctx, t, tok, 1)
	defer conns[0].Close()
	qc := memqlclient.NewQueryClient(conns[0].Dispatcher())

	// CREATE. Bundle content is irrelevant to this probe path (see the file
	// header), so a placeholder bundleRef is fine -- the assertions below
	// never open it.
	createdAt := time.Now()
	if _, err := qc.CreateSite(ctx, memqlclient.CreateSiteArgs{
		SiteId:    siteID,
		Hostname:  hostname,
		Kind:      "static",
		BundleRef: "file:///app/os",
		Status:    "live",
		Title:     "clustere2e invalidation probe",
	}); err != nil {
		t.Fatalf("createSite (requires a CLUSTER OWNER token -- see the file header): %v", err)
	}

	// THE DISCRIMINATING ASSERTION. Each replica's negative cache entry has
	// a 30s TTL; if it were left to expire on its own, "then serves 200"
	// would prove nothing about invalidation. Assert well under that TTL, so
	// only the graph.node.created.v1:platform:site forward -- not the
	// backstop timer -- can explain a 200 here.
	const assertWindow = 10 * time.Second
	const ttlBackstop = 30 * time.Second
	assertReflectsCreated := func(pf *podForward, label string) bool {
		t.Helper()
		deadline := time.Now().Add(assertWindow)
		var lastCode int
		for {
			lastCode = siteStatusCode(t, pf, hostname)
			if lastCode == http.StatusOK {
				t.Logf("replica %s (pod/%s, nodeId=%s) resolved the new site %s after createSite "+
					"-- well under the %s TTL backstop, so the created forward (not the TTL) explains it",
					label, pf.podName, pf.nodeID, time.Since(createdAt).Round(time.Millisecond), ttlBackstop)
				return true
			}
			if time.Now().After(deadline) {
				t.Errorf("replica %s (pod/%s, nodeId=%s) never resolved the new site within %s of "+
					"createSite (well under the %s TTL backstop) -- still serving %d against its PRIMED "+
					"negative cache entry. The graph.node.created.v1:platform:site forward did not reach "+
					"this replica.",
					label, pf.podName, pf.nodeID, assertWindow, ttlBackstop, lastCode)
				return false
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	createdCrossedB := assertReflectsCreated(pfB, "B")
	createdCrossedA := assertReflectsCreated(pfA, "A")
	if !createdCrossedA || !createdCrossedB {
		t.Fatalf("the created event did not cross to at least one replica (A=%v B=%v) -- "+
			"stopping before the update phase, which would be equally uninformative once "+
			"the more basic create-time invalidation is already known to fail",
			createdCrossedA, createdCrossedB)
	}

	// THE WRITE. Lands on whichever node this connection's mutation call
	// executes against -- irrelevant to what follows. What matters is
	// whether EDGE replica B, independently identified above, reflects it.
	// Reached only when the create-time invalidation above already proved
	// the mesh hop works for THIS hostname on THIS replica -- so a failure
	// here isolates the update path specifically, rather than restating a
	// mesh-wide problem the create-phase assertion already caught.
	writeAt := time.Now()
	if _, err := qc.UpdateSiteStatus(ctx, memqlclient.UpdateSiteStatusArgs{
		SiteId: siteID,
		Status: "disabled",
	}); err != nil {
		t.Fatalf("updateSiteStatus: %v", err)
	}

	// THE ASSERTION (addition 8): replica B must serve the NEW value (503,
	// per handler.go's status gate) within a window far under the 30s TTL
	// backstop, so a TTL-driven refresh cannot be what explains a pass --
	// only the change-feed invalidation can.
	assertReflectsDisabled := func(pf *podForward, label string) {
		t.Helper()
		deadline := time.Now().Add(assertWindow)
		var lastCode int
		for {
			lastCode = siteStatusCode(t, pf, hostname)
			if lastCode == http.StatusServiceUnavailable {
				t.Logf("replica %s (pod/%s, nodeId=%s) reflected disabled (503) %s after the write "+
					"-- well under the %s TTL backstop, so invalidation (not the TTL) explains it",
					label, pf.podName, pf.nodeID, time.Since(writeAt).Round(time.Millisecond), ttlBackstop)
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("replica %s (pod/%s, nodeId=%s) never reflected the disabled status "+
					"within %s of the write (well under the %s TTL backstop) -- last status %d. "+
					"Cross-node cache invalidation did not reach this replica.",
					label, pf.podName, pf.nodeID, assertWindow, ttlBackstop, lastCode)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	// B first: it is the replica this test exists to prove, and asserting it
	// before A means a failure here is never masked by A's own poll loop
	// having given the mesh extra time to converge.
	assertReflectsDisabled(pfB, "B")
	assertReflectsDisabled(pfA, "A")
}
