package worker

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

func connectionRecoveryFixture(t *testing.T) (*Fleet, string, time.Time, *managedHome, *managedHome) {
	t.Helper()
	f, path := controlFixture(t)
	f.logger = quietLogger()
	now := time.Now()
	f.connectionClock = func() time.Time { return now }
	a := &managedHome{home: f.workers.Homes[0], enabled: true, failedSince: now, wake: make(chan struct{}, 1), cancel: func() {}}
	b := &managedHome{home: f.workers.Homes[1], enabled: true, failedSince: now, wake: make(chan struct{}, 1), cancel: func() {}}
	f.managed["a"], f.managed["b"] = a, b
	return f, path, now, a, b
}

// Exercise the real supervisor, registration, retry loop, refusal hold and
// heartbeat after a twelve-hour outage. Only transport and elapsed time are
// controlled; no resume command, config rewrite or process restart is allowed.
func TestFleetReconnectsAfterLongOutageWithoutManualResume(t *testing.T) {
	for _, reason := range []string{"network", "credential-refusal"} {
		t.Run(reason, func(t *testing.T) {
			f, path := controlFixture(t)
			if err := saveHomeEnabled(path, "b", false); err != nil {
				t.Fatal(err)
			}
			workers, err := LoadWorkers(path, "")
			if err != nil {
				t.Fatal(err)
			}
			f, err = NewFleet(FleetOptions{Workers: workers, WorkersPath: path, Logger: quietLogger()})
			if err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(path)
			clock := newFakeClock()
			start := clock.Now()
			f.connectionClock = clock.Now
			f.connectionTicks = make(chan time.Time)
			var recovered atomic.Bool
			cluster := newFakeCluster(func(n int) answer {
				if n == 0 || recovered.Load() {
					return accept()
				}
				if reason == "credential-refusal" {
					return refuseToken()
				}
				return unreachable()
			})
			cluster.dialed = make(chan *scriptStream, 10000)
			other := newFakeCluster(func(int) answer { return accept() })
			pointAt(f, map[string]*fakeCluster{"a": cluster, "b": other})
			build := f.newRunner
			var builds atomic.Int32
			var longest atomic.Int64
			f.newRunner = func(opts Options) (*Runner, error) {
				builds.Add(1)
				r, err := build(opts)
				if err != nil {
					return nil, err
				}
				r.now = clock.Now
				r.heartbeat = time.Millisecond
				r.pause = func(ctx context.Context, delay time.Duration) bool {
					if int64(delay) > longest.Load() {
						longest.Store(int64(delay))
					}
					clock.advance(delay)
					f.reconnectUnresponsiveHomes(clock.Now())
					if clock.Now().Sub(start) >= 12*time.Hour {
						recovered.Store(true)
					}
					return ctx.Err() == nil
				}
				return r, nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- f.Run(ctx) }()
			defer func() { cancel(); <-done }()
			initial := cluster.next(t)
			initial.await(t, "heartbeat", func(m *memqlv1.WorkerClientMessage) bool { return m.GetHeartbeat() != nil })
			initial.drop(errors.New("internet outage"))
			eventuallyControl(t, func() bool { return recovered.Load() && f.LocalStatus().Homes[0].State == "Connected" })
			cluster.mu.Lock()
			restored := cluster.streams[len(cluster.streams)-1]
			cluster.mu.Unlock()
			restored.await(t, "heartbeat after outage", func(m *memqlv1.WorkerClientMessage) bool { return m.GetHeartbeat() != nil })
			if initial.register().GetLabels()[LabelMachineId] != restored.register().GetLabels()[LabelMachineId] {
				t.Fatal("recovery changed machine identity")
			}
			if builds.Load() != 1 {
				t.Fatalf("watchdog replaced a working retry loop %d times", builds.Load()-1)
			}
			if other.dialCount() != 0 {
				t.Fatal("manually paused sibling connected")
			}
			if reason == "credential-refusal" && time.Duration(longest.Load()) != defaultRefusalHoldMax {
				t.Fatalf("credential hold did not reach its normal ceiling: %s", time.Duration(longest.Load()))
			}
			status := f.LocalStatus()
			if !status.Homes[0].Enabled || status.Homes[0].AutoPaused || status.Homes[1].Enabled {
				t.Fatalf("outage changed connection choices: %+v", status.Homes)
			}
			after, _ := os.ReadFile(path)
			if string(before) != string(after) {
				t.Fatal("outage rewrote enrollment or connection policy")
			}
		})
	}
}

func TestConnectionRecoveryRestartsOnlyStalledHomeWithoutWritingPolicy(t *testing.T) {
	f, path, now, a, b := connectionRecoveryFixture(t)
	before, _ := os.ReadFile(path)
	stopsA, stopsB := 0, 0
	a.cancel = func() { stopsA++ }
	b.cancel = func() { stopsB++ }
	runner := &Runner{}
	conn := &Connection{RegistrationId: "healthy-idle"}
	runner.conn.Store(conn)
	b.runner = runner
	clock := now.Add(connectionStallWindow - time.Second)
	f.connectionClock = func() time.Time { return clock }
	f.connectionHealth(b, runner, b.generation, conn, true)
	f.reconnectUnresponsiveHomes(clock)
	if stopsA != 0 {
		t.Fatal("restarted before the stall window")
	}
	f.reconnectUnresponsiveHomes(now.Add(connectionStallWindow))
	if !a.enabled || a.autoPaused || stopsA != 1 || !b.enabled || stopsB != 0 {
		t.Fatal("recovery changed policy or crossed home boundaries")
	}
	status := f.LocalStatus()
	if status.Homes[0].State != "Disconnected / retrying" || status.Homes[1].State != "Connected" {
		t.Fatalf("incorrect recovery state: %+v", status.Homes)
	}
	f.reconnectUnresponsiveHomes(now.Add(connectionStallWindow + time.Second))
	if stopsA != 1 {
		t.Fatal("cancellation repeated before the replacement's window")
	}
	// A read-only/invalid registry does not prevent transport recovery: no write
	// is needed until the person changes a choice.
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	clock = now.Add(2 * connectionStallWindow)
	f.connectionHealth(b, runner, b.generation, conn, true)
	f.reconnectUnresponsiveHomes(clock)
	if stopsA != 2 || stopsB != 0 {
		t.Fatal("recovery depended on persisting a pause")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("watchdog wrote the workers file")
	}
}

func TestConnectionRecoveryRestartsStalledDialThroughSupervisor(t *testing.T) {
	f, path := controlFixture(t)
	if err := saveHomeEnabled(path, "b", false); err != nil {
		t.Fatal(err)
	}
	workers, err := LoadWorkers(path, "")
	if err != nil {
		t.Fatal(err)
	}
	f, err = NewFleet(FleetOptions{Workers: workers, WorkersPath: path, Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	clock := newFakeClock()
	f.connectionClock = clock.Now
	ticks := make(chan time.Time)
	f.connectionTicks = ticks
	cluster := newFakeCluster(func(int) answer { return accept() })
	pointAt(f, map[string]*fakeCluster{"a": cluster})
	build := f.newRunner
	entered := make(chan struct{})
	var builds atomic.Int32
	f.newRunner = func(opts Options) (*Runner, error) {
		r, err := build(opts)
		if err != nil {
			return nil, err
		}
		r.heartbeat = time.Millisecond
		if builds.Add(1) == 1 {
			r.dial = func(ctx context.Context, _ Config) (stream, error) {
				close(entered)
				<-ctx.Done()
				return nil, ctx.Err()
			}
		}
		return r, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx) }()
	defer func() { cancel(); <-done }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("dial did not start")
	}
	// Exercise the actual monitor without treating the outage as machine sleep.
	for elapsed := time.Second; elapsed <= connectionStallWindow; elapsed += time.Second {
		clock.advance(time.Second)
		ticks <- clock.Now()
	}
	restored := cluster.next(t)
	restored.await(t, "heartbeat after stalled dial", func(m *memqlv1.WorkerClientMessage) bool { return m.GetHeartbeat() != nil })
	if builds.Load() != 2 || !f.LocalStatus().Homes[0].Enabled {
		t.Fatal("supervisor did not recover its stalled runner")
	}
}

func TestConnectionRecoveryMigratesAutomaticPauseButKeepsManualPause(t *testing.T) {
	_, path := controlFixture(t)
	if err := saveHomeConnectionState(path, "a", false, true); err != nil {
		t.Fatal(err)
	}
	if err := saveHomeEnabled(path, "b", false); err != nil {
		t.Fatal(err)
	}
	original, err := LoadWorkers(path, "")
	if err != nil {
		t.Fatal(err)
	}
	for restart := 0; restart < 2; restart++ {
		workers, err := LoadWorkers(path, "")
		if err != nil {
			t.Fatal(err)
		}
		f, err := NewFleet(FleetOptions{Workers: workers, WorkersPath: path, Logger: quietLogger()})
		if err != nil {
			t.Fatal(err)
		}
		a := newFakeCluster(func(int) answer { return accept() })
		b := newFakeCluster(func(int) answer { return accept() })
		pointAt(f, map[string]*fakeCluster{"a": a, "b": b})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- f.Run(ctx) }()
		func() {
			defer func() { cancel(); <-done }()
			a.next(t).await(t, "registration", func(m *memqlv1.WorkerClientMessage) bool { return m.GetRegister() != nil })
			if b.dialCount() != 0 {
				t.Fatal("migration revived a manual pause")
			}
			saved, err := LoadWorkers(path, "")
			if err != nil {
				t.Fatal(err)
			}
			if !saved.Homes[0].IsEnabled() || saved.Homes[0].AutoPaused || saved.Homes[1].IsEnabled() {
				t.Fatal("migration did not persist the recovered choice")
			}
			for i := range saved.Homes {
				if saved.Homes[i].Token != original.Homes[i].Token {
					t.Fatal("migration changed enrollment")
				}
			}
			body, _ := os.ReadFile(path)
			if !strings.Contains(string(body), "custom_setting: preserve-me") {
				t.Fatal("migration lost unrelated configuration")
			}
			info, _ := os.Stat(path)
			if info.Mode().Perm() != 0600 {
				t.Fatal("migration exposed credentials")
			}
		}()
	}
}

func TestConnectionRecoveryMigrationFailureAndPausedDuplicateKeepPolicy(t *testing.T) {
	for _, fault := range []string{"file-mode", "manual-duplicate"} {
		t.Run(fault, func(t *testing.T) {
			_, path := controlFixture(t)
			if err := saveHomeConnectionState(path, "a", false, true); err != nil {
				t.Fatal(err)
			}
			if fault == "manual-duplicate" {
				body, _ := os.ReadFile(path)
				body = []byte(strings.ReplaceAll(string(body), "https://b.example", "https://a.example"))
				if err := os.WriteFile(path, body, 0600); err != nil {
					t.Fatal(err)
				}
				if err := saveHomeEnabled(path, "b", false); err != nil {
					t.Fatal(err)
				}
			}
			workers, err := LoadWorkers(path, "")
			if err != nil {
				t.Fatal(err)
			}
			f, err := NewFleet(FleetOptions{Workers: workers, WorkersPath: path, Logger: quietLogger()})
			if err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(path)
			if fault == "file-mode" {
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			}
			f.recoverAutomaticallyPausedHomes()
			if f.workers.Homes[0].IsEnabled() || !f.workers.Homes[0].AutoPaused {
				t.Fatal("failed migration changed live choice")
			}
			if f.workers.Homes[1].IsEnabled() != workers.Homes[1].IsEnabled() {
				t.Fatal("migration changed sibling choice")
			}
			after, _ := os.ReadFile(path)
			if string(before) != string(after) {
				t.Fatal("failed migration changed saved choices")
			}
		})
	}
}

func TestConnectionRecoveryDefersDuringUpdateReservation(t *testing.T) {
	for _, state := range []string{"prepared", "finishing", "expired"} {
		t.Run(state, func(t *testing.T) {
			f, _, now, a, _ := connectionRecoveryFixture(t)
			stops := 0
			a.cancel = func() { stops++ }
			f.update.until = time.Now().Add(time.Minute)
			if state == "finishing" {
				f.update.finishing = true
				f.update.until = time.Now().Add(-time.Minute)
			}
			if state == "expired" {
				f.update.until = time.Now().Add(-time.Minute)
			}
			f.reconnectUnresponsiveHomes(now.Add(connectionStallWindow))
			if !a.enabled || a.autoPaused {
				t.Fatal("watchdog changed connection policy")
			}
			if state == "expired" {
				if stops != 1 {
					t.Fatal("expired reservation blocked recovery")
				}
				return
			}
			if stops != 0 {
				t.Fatal("replaced runner during updater handoff")
			}
			if err := f.SetHomeEnabled("a", false); err == nil {
				t.Fatal("manual change bypassed reservation")
			}
			f.update.finishing = false
			if err := f.controlUpdate("cancel-update", os.Getpid()); err != nil {
				t.Fatal(err)
			}
			f.reconnectUnresponsiveHomes(now.Add(connectionStallWindow))
			if stops != 1 {
				t.Fatal("canceling reservation did not restore watchdog")
			}
		})
	}
}

func TestConnectionObservationGapExcludesSleepAndPreservesManualPause(t *testing.T) {
	f, _, now, a, b := connectionRecoveryFixture(t)
	a.failedSince = now.Add(-9 * time.Minute)
	b.healthy, b.lastHealthy, b.failedSince = true, now.Add(-time.Second), time.Time{}
	stops := 0
	a.cancel = func() { stops++ }
	wake := now.Add(2 * time.Hour)
	f.discountConnectionObservationGap(now, wake)
	f.reconnectUnresponsiveHomes(wake)
	if stops != 0 || wake.Sub(a.failedSince) != 9*time.Minute || wake.Sub(b.lastHealthy) != time.Second {
		t.Fatal("sleep consumed the stall window")
	}
	f.reconnectUnresponsiveHomes(wake.Add(time.Minute))
	if stops != 1 || !a.enabled || !b.enabled {
		t.Fatal("watchdog failed to recover after active stall time")
	}
	if err := f.SetHomeEnabled("a", false); err != nil {
		t.Fatal(err)
	}
	f.discountConnectionObservationGap(wake.Add(time.Minute), wake.Add(3*time.Hour))
	f.reconnectUnresponsiveHomes(wake.Add(3 * time.Hour))
	if a.enabled {
		t.Fatal("wake resumed a manual pause")
	}
	b.lastHealthy = wake.Add(3 * time.Hour)
	f.discountConnectionObservationGap(wake, wake.Add(3*time.Hour))
	if !b.lastHealthy.Equal(wake.Add(3 * time.Hour)) {
		t.Fatal("fresh heartbeat shifted into future")
	}
}

func TestConnectionObservationGapCoversTransitionsAfterLastTick(t *testing.T) {
	f, _, now, a, b := connectionRecoveryFixture(t)
	// Both callbacks happened after the last tick, just before suspension.
	a.failedSince = now.Add(500 * time.Millisecond)
	b.healthy, b.lastHealthy, b.failedSince = true, now.Add(500*time.Millisecond), time.Time{}
	stopsA, stopsB := 0, 0
	a.cancel = func() { stopsA++ }
	b.cancel = func() { stopsB++ }
	wake := now.Add(2 * time.Hour)
	f.discountConnectionObservationGap(now, wake)
	f.reconnectUnresponsiveHomes(wake)
	if stopsA != 0 || stopsB != 0 || !a.failedSince.Equal(wake) || !b.lastHealthy.Equal(wake) {
		t.Fatal("a transition just before sleep consumed its stall window")
	}
	f.reconnectUnresponsiveHomes(wake.Add(connectionStallWindow - time.Second))
	if stopsA != 0 || stopsB != 0 {
		t.Fatal("wake-up did not allow an active stall window")
	}
	f.reconnectUnresponsiveHomes(wake.Add(connectionStallWindow))
	if stopsA != 1 || stopsB != 1 || !a.enabled || !b.enabled {
		t.Fatal("suspended observation prevented recovery or changed policy")
	}
}

func TestConnectionDisconnectDoesNotResetStalledHeartbeatAge(t *testing.T) {
	f, _, now, a, _ := connectionRecoveryFixture(t)
	r := &Runner{}
	c := &Connection{RegistrationId: "registered-before-stall"}
	r.conn.Store(c)
	a.runner = r
	stops := 0
	a.cancel = func() { stops++ }
	f.connectionHealth(a, r, a.generation, c, true)
	f.connectionClock = func() time.Time { return now.Add(9 * time.Minute) }
	r.conn.Store(nil)
	f.connectionHealth(a, r, a.generation, c, false)
	if !a.failedSince.Equal(now) {
		t.Fatal("disconnect reset age of the last successful heartbeat")
	}
	f.reconnectUnresponsiveHomes(now.Add(connectionStallWindow))
	if stops != 1 || !a.enabled || a.autoPaused {
		t.Fatal("teardown extended the stall window or changed connection policy")
	}
}

func TestConnectionRecoveryFencesLateHeartbeatAndRetryCallbacks(t *testing.T) {
	f, _, now, a, _ := connectionRecoveryFixture(t)
	r := &Runner{}
	c := &Connection{RegistrationId: "registered-before-stall"}
	r.conn.Store(c)
	a.runner = r
	generation := a.generation
	f.connectionHealth(a, r, generation, c, true)
	f.reconnectUnresponsiveHomes(now.Add(connectionStallWindow))
	if !a.enabled || a.healthy || a.generation == generation {
		t.Fatal("stalled heartbeat did not restart connection")
	}
	since := a.failedSince
	f.connectionHealth(a, r, generation, c, true)
	f.connectionRetryScheduled(a, r, generation, time.Hour)
	if a.healthy || !a.failedSince.Equal(since) {
		t.Fatal("old runner revived the replaced generation")
	}
	if err := f.SetHomeEnabled("a", false); err != nil {
		t.Fatal(err)
	}
	f.connectionHealth(a, r, a.generation, c, true)
	f.connectionRetryScheduled(a, r, a.generation, time.Hour)
	if a.enabled || a.healthy || !a.failedSince.IsZero() {
		t.Fatal("late callback changed a manual pause")
	}
}

func TestConnectionHealthRequiresNormalHeartbeatAndRejectsStaleGeneration(t *testing.T) {
	f, _, now, a, _ := connectionRecoveryFixture(t)
	if err := f.SetHomeEnabled("b", false); err != nil {
		t.Fatal(err)
	}
	r := &Runner{}
	c := &Connection{RegistrationId: "accepted-only"}
	r.conn.Store(c)
	a.runner = r
	a.cancel = func() {}
	// RegisterAck/TCP success alone does not reset a failed retry window.
	f.reconnectUnresponsiveHomes(now.Add(connectionStallWindow))
	if !a.enabled || a.healthy || a.generation == 0 {
		t.Fatal("registration without a heartbeat escaped recovery")
	}
	if err := f.SetHomeEnabled("a", true); err != nil {
		t.Fatal(err)
	}
	old := a.generation
	if err := f.SetHomeEnabled("a", false); err != nil {
		t.Fatal(err)
	}
	if err := f.SetHomeEnabled("a", true); err != nil {
		t.Fatal(err)
	}
	f.connectionHealth(a, r, old, c, true)
	if a.healthy || a.failedSince.IsZero() {
		t.Fatal("stale heartbeat revived a prior retry generation")
	}
	f.connectionHealth(a, r, a.generation, c, true)
	if !a.healthy || !a.failedSince.IsZero() {
		t.Fatal("normal healthy heartbeat did not clear outage")
	}
	r.conn.Store(nil)
	f.connectionHealth(a, r, a.generation, c, false)
	f.connectionHealth(a, r, a.generation, c, true)
	if a.healthy || a.failedSince.IsZero() {
		t.Fatal("heartbeat racing disconnect revived a lost stream")
	}
}
