package worker

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/cockpit/internal/worker/models"
	"github.com/znasllc-io/memql/cockpit/internal/worker/tools"
)

// fakeModelInventory is what the runner sees instead of Ollama. It also
// counts invalidations, because the re-advertise and the cache drop are
// one API call on purpose: a caller that got only half of it would
// re-register the label set from before the pull.
type fakeModelInventory struct {
	mu            sync.Mutex
	inv           models.Inventory
	invalidations int
}

func (f *fakeModelInventory) Models(ctx context.Context) models.Inventory {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inv
}

func (f *fakeModelInventory) Invalidate() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invalidations++
}

func (f *fakeModelInventory) serve(inv models.Inventory) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inv = inv
}

func (f *fakeModelInventory) invalidated() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.invalidations
}

// testRunner builds a runner with a clock the test moves by hand. The
// guard it exercises is TWO MINUTES of wall clock; a test that waited it
// out would spend two minutes of every CI run to assert one branch.
func testRunner(inv ModelInventory, now *time.Time) *Runner {
	return &Runner{
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		modelsInv:      inv,
		readvertiseNow: make(chan struct{}, 1),
		now:            func() time.Time { return *now },
		closed:         make(chan struct{}),
	}
}

// testConnection is a Connection with no stream under it. Close() is
// safe on one -- the SDK connection checks its own nil receiver -- which
// is what lets the re-advertise decision be asserted without a cluster.
// It registered with the fail-closed consent, which is what a runner with
// no policy reports, so only the labels differ unless a test says so.
func testConnection(fingerprint string) *Connection {
	return &Connection{ModelFingerprint: fingerprint}
}

func oneModel() models.Inventory {
	return servingInventory(offeredModel("llama3.1:8b", models.Attributes{ContextWindow: 8192, MaxConcurrent: 1}))
}

func twoModels() models.Inventory {
	return servingInventory(
		offeredModel("llama3.1:8b", models.Attributes{ContextWindow: 8192, MaxConcurrent: 1}),
		offeredModel("nomic-embed-text", models.Attributes{ContextWindow: 2048, Embeddings: true, MaxConcurrent: 4}),
	)
}

// TestMaybeReadvertiseModels_TheFloorHoldsWithoutARequest.
//
// A runtime flapping between up and down must not turn this worker into
// one that reconnects forever, which is worse than a stale label.
func TestMaybeReadvertiseModels_TheFloorHoldsWithoutARequest(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	inv := &fakeModelInventory{}
	inv.serve(oneModel())
	r := testRunner(inv, &clock)

	if !r.maybeReadvertiseModels(context.Background(), testConnection("")) {
		t.Fatal("a changed model set on an idle worker must re-advertise")
	}
	clock = clock.Add(30 * time.Second)
	if r.maybeReadvertiseModels(context.Background(), testConnection("")) {
		t.Error("a second re-advertise inside the two-minute floor must be refused")
	}
	clock = clock.Add(modelReadvertiseMinInterval)
	if !r.maybeReadvertiseModels(context.Background(), testConnection("")) {
		t.Error("once the floor has passed, a changed set re-advertises again")
	}
}

// TestMaybeReadvertiseModels_UnchangedLabelsChangeNothing. Discovery
// running again is not news; a model appearing is.
func TestMaybeReadvertiseModels_UnchangedLabelsChangeNothing(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	inv := &fakeModelInventory{}
	inv.serve(oneModel())
	r := testRunner(inv, &clock)

	same := advertisedFingerprint(oneModel().Labels())
	if r.maybeReadvertiseModels(context.Background(), testConnection(same)) {
		t.Error("re-discovering the same set must not spend a reconnect")
	}
}

// TestRequestImmediateReadvertise_BypassesTheFloorExactlyOnce.
//
// Somebody pressed Pull and is watching. Two minutes of a model that does
// not appear reads as a pull that failed, so the request skips the floor
// -- ONCE. If the bypass survived its own use, a flapping runtime plus
// one old request would reconnect this worker on every refresh tick,
// which is the failure the floor exists to prevent.
func TestRequestImmediateReadvertise_BypassesTheFloorExactlyOnce(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	inv := &fakeModelInventory{}
	inv.serve(oneModel())
	r := testRunner(inv, &clock)

	if !r.maybeReadvertiseModels(context.Background(), testConnection("")) {
		t.Fatal("the first re-advertise must happen")
	}
	clock = clock.Add(5 * time.Second)
	if r.maybeReadvertiseModels(context.Background(), testConnection("")) {
		t.Fatal("without a request the floor holds")
	}

	inv.serve(twoModels())
	r.RequestImmediateReadvertise()
	if !r.maybeReadvertiseModels(context.Background(), testConnection("")) {
		t.Error("a pull with somebody watching must re-advertise inside the floor")
	}
	clock = clock.Add(time.Second)
	if r.maybeReadvertiseModels(context.Background(), testConnection("")) {
		t.Error("the bypass is a one-shot: the second call inside the floor must be refused")
	}
}

// TestRequestImmediateReadvertise_DoesNotOverrideBusy.
//
// A reconnect that interrupts a running model call or app session is
// worse than a stale label, and "somebody is watching" is not a reason to
// kill their own in-flight work. The request is not spent by a busy
// return either: the next evaluation, once the work is done, still gets
// its bypass -- otherwise the busiest machines would be the ones that
// waited out the floor.
func TestRequestImmediateReadvertise_DoesNotOverrideBusy(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	inv := &fakeModelInventory{}
	inv.serve(oneModel())
	r := testRunner(inv, &clock)
	// A re-advertise a moment ago, so the floor is live for this test.
	r.lastReadvertise.Store(clock.UnixNano())
	clock = clock.Add(10 * time.Second)

	r.activeCalls.Add(1)
	r.RequestImmediateReadvertise()
	if r.maybeReadvertiseModels(context.Background(), testConnection("")) {
		t.Fatal("a busy worker must not reconnect, watched or not")
	}

	r.activeCalls.Add(-1)
	if !r.maybeReadvertiseModels(context.Background(), testConnection("")) {
		t.Error("the request must survive the busy window and bypass the floor once the work is done")
	}
	if r.maybeReadvertiseModels(context.Background(), testConnection("")) {
		t.Error("and then be spent")
	}
}

// TestRequestImmediateReadvertise_InvalidatesTheInventory.
//
// The two halves are one call because a caller that did only the
// re-advertise would reconnect and re-register the label set from BEFORE
// the pull: the reconnect happens, nothing changes, and the pull reads as
// one that did nothing.
func TestRequestImmediateReadvertise_InvalidatesTheInventory(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	inv := &fakeModelInventory{}
	inv.serve(oneModel())
	r := testRunner(inv, &clock)

	r.RequestImmediateReadvertise()
	if got := inv.invalidated(); got != 1 {
		t.Errorf("inventory invalidations = %d, want 1", got)
	}
}

// TestRequestImmediateReadvertise_CollapsesABurst. `setup --inference`
// pulls a default PAIR of models, and a machine being set up may pull
// several. Each finished pull is one request; they must collapse into one
// evaluation rather than queueing one reconnect per model.
func TestRequestImmediateReadvertise_CollapsesABurst(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	inv := &fakeModelInventory{}
	inv.serve(oneModel())
	r := testRunner(inv, &clock)

	r.RequestImmediateReadvertise()
	r.RequestImmediateReadvertise()
	r.RequestImmediateReadvertise()

	select {
	case <-r.readvertiseNow:
	default:
		t.Fatal("a request must wake the heartbeat loop rather than wait out the refresh ticker")
	}
	select {
	case <-r.readvertiseNow:
		t.Error("a burst of requests must collapse into one wake-up")
	default:
	}
}

// TestRequestImmediateReadvertise_SafeWithoutAModelInventory. A build
// that reports no models at all passes nil, and the pull path must not
// have to know which build it is running in.
func TestRequestImmediateReadvertise_SafeWithoutAModelInventory(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	r := testRunner(nil, &clock)
	r.RequestImmediateReadvertise()

	var nilRunner *Runner
	nilRunner.RequestImmediateReadvertise()

	if r.maybeReadvertiseModels(context.Background(), testConnection("")) {
		t.Error("a runner with no inventory has nothing to re-advertise")
	}
}

// The hardware inventory rides Register and then every tenth beat.
//
// At the 15-second default that is a refresh every two and a half
// minutes -- "within minutes", which is what the design record asks for
// -- and it keeps a scan that shells out to nvidia-smi and `docker
// version` off the other nine.
func TestHardwareOnBeat(t *testing.T) {
	for _, tc := range []struct {
		beat int
		want bool
	}{
		// Register already carried one, so the first nine beats add
		// nothing. A machine that reported again on beat 1 would send
		// the same payload twice inside fifteen seconds of connecting.
		{0, false}, {1, false}, {2, false}, {9, false},
		{10, true}, {11, false}, {19, false}, {20, true},
		{100, true}, {101, false},
	} {
		if got := hardwareOnBeat(tc.beat); got != tc.want {
			t.Errorf("hardwareOnBeat(%d) = %v, want %v", tc.beat, got, tc.want)
		}
	}
}

// The cadence is stated in BEATS, so the wall-clock interval follows
// from the heartbeat rather than being a second number that can drift
// from it. Asserted so that changing the heartbeat is visibly also a
// change to how often a machine re-describes itself.
//
// It reads the PRODUCTION constant. A local copy of 15s would let
// loop.go move to 30s while this stayed green, and the refresh would
// silently become five minutes -- the precise drift this is here to
// prevent, committed by the test that claims to prevent it.
func TestHardwareRefreshInterval(t *testing.T) {
	if got := time.Duration(hardwareRefreshBeats) * DefaultHeartbeat; got != 150*time.Second {
		t.Fatalf("hardware refresh interval = %s (%d beats of %s), want 2m30s",
			got, hardwareRefreshBeats, DefaultHeartbeat)
	}
}

// And the runner actually USES that constant when the caller states no
// heartbeat -- otherwise the interval above is arithmetic about a
// number nothing reads.
func TestRunnerDefaultsToTheNamedHeartbeat(t *testing.T) {
	r, err := NewRunner(Options{
		Config: Config{
			ClusterURL: "https://api.example.com", Token: "mql_wkr_x", Name: "w",
			Capabilities: []string{"HEADLESS"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.heartbeat != DefaultHeartbeat {
		t.Fatalf("heartbeat = %s, want %s", r.heartbeat, DefaultHeartbeat)
	}
}

func TestNextBackoffCapsAtDefaultReconnectMax(t *testing.T) {
	got := nextBackoff(DefaultReconnectMaxBackoff, DefaultReconnectMaxBackoff)
	if got != DefaultReconnectMaxBackoff {
		t.Fatalf("nextBackoff at ceiling = %v, want %v", got, DefaultReconnectMaxBackoff)
	}
	got = nextBackoff(8*time.Second, DefaultReconnectMaxBackoff)
	if got != DefaultReconnectMaxBackoff {
		t.Fatalf("nextBackoff(8s) = %v, want cap %v", got, DefaultReconnectMaxBackoff)
	}
	if DefaultReconnectMaxBackoff > 15*time.Second {
		t.Fatalf("DefaultReconnectMaxBackoff must stay <= 15s; got %v", DefaultReconnectMaxBackoff)
	}
}

// pipelinesConnection registered with one model and the given pipeline policy.
func pipelinesConnection(pipelines bool) *Connection {
	return &Connection{ModelFingerprint: advertisedFingerprint(oneModel().Labels()), AdvertisedPipelines: (tools.PipelinesPolicy{Allow: pipelines}).AdvertisementFingerprint()}
}

// TestAChangedPipelinesPolicyIsAChangedAdvertisement (memql#5494). The
// pipelines label rides Register and nothing else, so a policy reload that
// grants or withdraws pipelines has to re-register for the cluster's router
// to see it -- under the same guards every other change of advertisement
// obeys: never while work is in flight, never twice inside the floor unless
// a reload asked.
func TestAChangedPipelinesPolicyIsAChangedAdvertisement(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	inv := &fakeModelInventory{}
	inv.serve(oneModel())
	allowed := false
	r := testRunner(inv, &clock)
	r.pipelines = func() tools.PipelinesPolicy { return tools.PipelinesPolicy{Allow: allowed} }

	if r.maybeReadvertiseModels(context.Background(), pipelinesConnection(false)) {
		t.Fatal("an unchanged pipelines policy is no reason to reconnect")
	}
	allowed = true
	if !r.maybeReadvertiseModels(context.Background(), pipelinesConnection(false)) {
		t.Fatal("a GRANTED pipelines policy must be re-registered")
	}
	clock = clock.Add(10 * time.Second)
	allowed = false
	if r.maybeReadvertiseModels(context.Background(), pipelinesConnection(true)) {
		t.Fatal("inside the floor, a change with no request waits")
	}
	r.RequestImmediateReadvertise()
	if !r.maybeReadvertiseModels(context.Background(), pipelinesConnection(true)) {
		t.Fatal("a WITHDRAWN pipelines policy, asked for by the reload, must be re-registered")
	}
	clock = clock.Add(modelReadvertiseMinInterval)
	allowed = true
	r.activeCalls.Add(1)
	if r.maybeReadvertiseModels(context.Background(), pipelinesConnection(false)) {
		t.Fatal("work in flight is never cut short for a re-registration")
	}
}

// A connection the runner is already closing to re-register is not
// evaluated again: the next tick on the same heartbeat loop would
// otherwise decide -- and log -- the same reconnect twice.
func TestAConnectionBeingClosedIsNotReEvaluated(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	inv := &fakeModelInventory{}
	inv.serve(oneModel())
	r := testRunner(inv, &clock)
	conn := &Connection{}
	if !r.maybeReadvertiseModels(context.Background(), conn) {
		t.Fatal("changed model labels must close the connection to re-register")
	}
	if r.maybeReadvertiseModels(context.Background(), conn) {
		t.Fatal("a connection already being closed must not be closed -- and announced -- again")
	}
}

func TestRepositoryOnlyPolicyChangeReadvertises(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	inv := &fakeModelInventory{}
	inv.serve(oneModel())
	policy := tools.PipelinesPolicy{Allow: true, Repos: []string{"o/a"}}
	r := testRunner(inv, &clock)
	r.pipelines = func() tools.PipelinesPolicy { return policy }
	conn := pipelinesConnection(true)
	conn.AdvertisedPipelines = policy.AdvertisementFingerprint()
	if r.maybeReadvertiseModels(context.Background(), conn) {
		t.Fatal("unchanged scope reconnects")
	}
	policy.Repos = []string{"o/b"}
	if !r.maybeReadvertiseModels(context.Background(), conn) {
		t.Fatal("repository-only policy change was hidden until a later restart")
	}
}
