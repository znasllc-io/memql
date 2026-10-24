package worker

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/cockpit/internal/worker/hardware"
)

// fleet_run_test.go drives Fleet.Run against one fake cluster per home
// (memql-cockpit#433, #434). Before these, the only fleet test checked how
// the registry projected into configs; the supervisor itself had never run
// under a test.

// fleetOf builds a registry with one enabled home per id, all under root.
func fleetOf(root string, ids ...string) WorkersFile {
	on := true
	w := WorkersFile{Version: 1, WorkerName: "test-machine", StateDir: root, Capabilities: []string{"HEADLESS"},
		Concurrency: map[string]uint32{"HEADLESS": 1}}
	for _, id := range ids {
		w.Homes = append(w.Homes, Home{ID: id, ClusterURL: "https://api." + id + ".example",
			Token: "mql_wkr_" + id + "_token_aaaaaaaaaaaa", Enabled: &on})
	}
	return w
}

// pointAt makes the fleet's runners dial the fake cluster for their home.
func pointAt(f *Fleet, clusters map[string]*fakeCluster) {
	f.newRunner = func(opts Options) (*Runner, error) {
		r, err := NewRunner(opts)
		if err != nil {
			return nil, err
		}
		r.dial = clusters[opts.Config.Home].dial
		r.scanHardware = func(context.Context) hardware.Inventory { return hardware.Inventory{Chip: "test"} }
		clock := newFakeClock()
		r.now = clock.Now
		r.pause = func(ctx context.Context, d time.Duration) bool {
			// A real pause, but a short one: long enough that a home held
			// by a refusing cluster does not spin, short enough to watch.
			// The runner's clock moves by what it asked for, so the
			// refusal grace passes as it would have.
			clock.advance(d)
			select {
			case <-ctx.Done():
				return false
			case <-time.After(2 * time.Millisecond):
				return true
			}
		}
		return r, nil
	}
}

// TestFleetBuildsEveryHomeBeforeStartingAny (memql-cockpit#433). A home
// that cannot be built used to be found after the homes before it were
// already connected -- and Run returned with them still running under a
// supervisor that had reported itself finished.
func TestFleetBuildsEveryHomeBeforeStartingAny(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	clusters := map[string]*fakeCluster{
		"alpha": newFakeCluster(func(int) answer { return accept() }),
		"beta":  newFakeCluster(func(int) answer { return accept() }),
	}
	f, err := NewFleet(FleetOptions{Logger: quietLogger(), Workers: fleetOf(root, "alpha", "beta")})
	if err != nil {
		t.Fatal(err)
	}
	pointAt(f, clusters)
	build := f.newRunner
	f.newRunner = func(opts Options) (*Runner, error) {
		if opts.Config.Home == "beta" {
			return nil, errors.New("beta cannot be built")
		}
		return build(opts)
	}

	done := make(chan error, 1)
	go func() { done <- f.Run(context.Background()) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "fleet: home beta: beta cannot be built") {
			t.Fatalf("Run = %v, want the build error naming beta", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return on a home it could not build")
	}
	if n := clusters["alpha"].dialCount(); n != 0 {
		t.Fatalf("alpha dialled %d times: nothing may start before every home is built", n)
	}
}

// TestFleetHomesAreIndependentOnOneMachineIdentity. Two clusters, one
// refusing this machine and one accepting it: the refusal holds only its
// own home, each home gets its own dispatcher (and so its own consent
// window), the metrics say which home is which -- and both clusters are
// told the SAME machine id, resolved once before either connected.
func TestFleetHomesAreIndependentOnOneMachineIdentity(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	clusters := map[string]*fakeCluster{
		"refusing":  newFakeCluster(func(int) answer { return refuseToken() }),
		"accepting": newFakeCluster(func(int) answer { return accept() }),
	}
	metrics := NewMetrics()
	var mu sync.Mutex
	var dispatchersFor []string
	f, err := NewFleet(FleetOptions{
		Logger:  quietLogger(),
		Workers: fleetOf(root, "refusing", "accepting"),
		Metrics: metrics,
		ToolsFor: func(id string) ToolDispatcher {
			mu.Lock()
			defer mu.Unlock()
			dispatchersFor = append(dispatchersFor, id)
			return &recordingTools{}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	pointAt(f, clusters)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx) }()

	accepted := clusters["accepting"].next(t)
	refused := clusters["refusing"].next(t)
	deadline := time.Now().Add(10 * time.Second)
	for {
		text := metrics.render()
		if strings.Contains(text, `worker_home_refused{home="refusing"} 1`) &&
			strings.Contains(text, `worker_home_connected{home="accepting"} 1`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("want refusing held and accepting connected, metrics:\n%s", text)
		}
		time.Sleep(time.Millisecond)
	}
	text := metrics.render()
	for _, want := range []string{
		`worker_home_connected{home="refusing"} 0`,
		`worker_home_refused{home="accepting"} 0`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics missing %s:\n%s", want, text)
		}
	}

	machineID, err := ResolveMachineID(root)
	if err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]*scriptStream{"accepting": accepted, "refusing": refused} {
		if got := s.register().GetLabels()[LabelMachineId]; got != machineID {
			t.Errorf("%s was told machineId %q, want the machine's %q", name, got, machineID)
		}
	}
	mu.Lock()
	got := strings.Join(dispatchersFor, ",")
	mu.Unlock()
	if got != "refusing,accepting" {
		t.Errorf("dispatchers built for %q, want one per home, in order", got)
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v after cancel, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel: something it started outlived it")
	}
}

// TestFleetOpensOneStreamPerCluster (memql-cockpit#433). Two enabled homes
// on one cluster used to open two streams and flap the machine's row.
// Now the later entry connects -- the newest enrollment, holding the
// newest token -- and the other is named in the log, never dialled.
func TestFleetOpensOneStreamPerCluster(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	on := true
	w := fleetOf(root)
	w.Homes = []Home{
		{ID: "local", ClusterURL: "https://api.example.com", Token: "mql_wkr_old_token_aaaaaaaaaaaa", Enabled: &on},
		{ID: "api.example.com", ClusterURL: "https://api.example.com:443", Token: "mql_wkr_new_token_aaaaaaaaaaaa", Enabled: &on},
	}
	clusters := map[string]*fakeCluster{
		"local":           newFakeCluster(func(int) answer { return accept() }),
		"api.example.com": newFakeCluster(func(int) answer { return accept() }),
	}
	logs := &logBuffer{}
	f, err := NewFleet(FleetOptions{Logger: slogJSON(logs), Workers: w})
	if err != nil {
		t.Fatal(err)
	}
	pointAt(f, clusters)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx) }()

	clusters["api.example.com"].next(t)
	time.Sleep(20 * time.Millisecond)
	if n := clusters["local"].dialCount(); n != 0 {
		t.Fatalf("the superseded home dialled %d times; one machine holds one stream per cluster", n)
	}
	if logs.count(`"level":"WARN"`, `only \"api.example.com\" connects`, `remove \"local\" from workers.yaml`) != 1 {
		t.Fatalf("the duplicate must be named once in the log:\n%s", logs)
	}
	cancel()
	<-done
}
