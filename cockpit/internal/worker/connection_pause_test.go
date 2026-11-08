package worker

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func connectionPauseFixture(t *testing.T) (*Fleet, string, time.Time, *managedHome, *managedHome) {
	t.Helper()
	f, path := controlFixture(t)
	f.logger = quietLogger()
	now := time.Now()
	f.connectionClock = func() time.Time { return now }
	a := &managedHome{home: f.workers.Homes[0], enabled: true, failedSince: now, wake: make(chan struct{}, 1)}
	b := &managedHome{home: f.workers.Homes[1], enabled: true, failedSince: now, wake: make(chan struct{}, 1)}
	f.managed["a"], f.managed["b"] = a, b
	return f, path, now, a, b
}

func TestConnectionAutoPausePersistsAndKeepsHealthyIdleSibling(t *testing.T) {
	f, path, now, a, b := connectionPauseFixture(t)
	stopsA, stopsB := 0, 0
	a.cancel = func() { stopsA++ }
	b.cancel = func() { stopsB++ }
	runner := &Runner{}
	conn := &Connection{RegistrationId: "b-registration"}
	runner.conn.Store(conn)
	b.runner = runner
	f.connectionHealth(b, runner, b.generation, conn, true)
	clock := now.Add(connectionRetryWindow - time.Second)
	f.connectionClock = func() time.Time { return clock }
	f.connectionHealth(b, runner, b.generation, conn, true)
	f.pauseUnresponsiveHomes(now.Add(connectionRetryWindow - time.Second))
	if !a.enabled || stopsA != 0 {
		t.Fatal("paused a brief connection failure")
	}
	f.pauseUnresponsiveHomes(now.Add(connectionRetryWindow))
	if a.enabled || !a.autoPaused || stopsA != 1 || !b.enabled || stopsB != 0 {
		t.Fatal("automatic pause crossed home boundaries")
	}
	status := f.LocalStatus()
	if !status.Homes[0].AutoPaused || status.Homes[0].State != "Paused automatically" || status.Homes[1].State != "Connected" {
		t.Fatalf("wrong status: %+v", status.Homes)
	}
	workers, err := LoadWorkers(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if workers.Homes[0].IsEnabled() || !workers.Homes[0].AutoPaused || !workers.Homes[1].IsEnabled() {
		t.Fatal("automatic pause was not durable")
	}
	for i := range workers.Homes {
		if workers.Homes[i].Token != f.workers.Homes[i].Token {
			t.Fatal("pause changed enrollment")
		}
	}
	body, _ := os.ReadFile(path)
	if !strings.Contains(string(body), "custom_setting: preserve-me") {
		t.Fatal("pause lost unrelated configuration")
	}
	// An idle stream remains healthy without incoming tool messages or jobs,
	// as long as its normal outgoing heartbeats continue making progress.
	for clock = clock.Add(time.Minute); clock.Before(now.Add(24 * time.Hour)); clock = clock.Add(time.Minute) {
		f.connectionHealth(b, runner, b.generation, conn, true)
		f.pauseUnresponsiveHomes(clock)
	}
	if !b.enabled || stopsB != 0 || stopsA != 1 {
		t.Fatal("idle healthy worker was paused or pause repeated")
	}
}

func TestConnectionAutoPauseRestartAndExplicitRetry(t *testing.T) {
	f, path, now, a, b := connectionPauseFixture(t)
	if err := f.SetHomeEnabled("b", false); err != nil {
		t.Fatal(err)
	}
	f.pauseUnresponsiveHomes(now.Add(connectionRetryWindow))
	if a.enabled || b.enabled || b.autoPaused {
		t.Fatal("manual pause changed reason")
	}
	workers, err := LoadWorkers(path, "")
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewFleet(FleetOptions{Workers: workers, WorkersPath: path, Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	fresh := now.Add(time.Hour)
	reloaded.connectionClock = func() time.Time { return fresh }
	reloaded.connectionTicks = make(chan time.Time)
	started := make(chan string, 2)
	reloaded.runHome = func(ctx context.Context, h *managedHome) error { started <- h.home.ID; <-ctx.Done(); return ctx.Err() }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = reloaded.Run(ctx) }()
	defer func() { cancel(); <-done }()
	eventuallyControl(t, func() bool { reloaded.mu.Lock(); defer reloaded.mu.Unlock(); return len(reloaded.managed) == 2 })
	if s := reloaded.LocalStatus(); !s.Homes[0].AutoPaused || s.Homes[1].AutoPaused {
		t.Fatal("restart lost pause reasons")
	}
	select {
	case <-started:
		t.Fatal("paused home restarted without retry")
	default:
	}
	if err := reloaded.SetHomeEnabled("a", true); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-started:
		if id != "a" {
			t.Fatal("retried wrong home")
		}
	case <-time.After(time.Second):
		t.Fatal("retry did not start")
	}
	reloaded.pauseUnresponsiveHomes(fresh.Add(connectionRetryWindow - time.Second))
	if s := reloaded.LocalStatus(); !s.Homes[0].Enabled || s.Homes[0].AutoPaused || s.Homes[1].Enabled {
		t.Fatal("retry did not start a fresh window")
	}
	reloaded.pauseUnresponsiveHomes(fresh.Add(connectionRetryWindow))
	eventuallyControl(t, func() bool { return reloaded.LocalStatus().Homes[0].State == "Paused automatically" })
	// A manual Pause on an automatically paused entry deliberately replaces
	// its reason; it must stay a manual pause on the next launch.
	if err := reloaded.SetHomeEnabled("a", false); err != nil {
		t.Fatal(err)
	}
	workers, err = LoadWorkers(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if workers.Homes[0].AutoPaused || workers.Homes[0].IsEnabled() {
		t.Fatal("manual pause did not replace automatic reason")
	}
}

func TestConnectionHealthRequiresNormalHeartbeatAndRejectsStaleGeneration(t *testing.T) {
	f, _, now, a, _ := connectionPauseFixture(t)
	if err := f.SetHomeEnabled("b", false); err != nil {
		t.Fatal(err)
	}
	r := &Runner{}
	c := &Connection{RegistrationId: "accepted-only"}
	r.conn.Store(c)
	a.runner = r
	// RegisterAck/TCP success alone does not reset a failed retry window.
	f.pauseUnresponsiveHomes(now.Add(connectionRetryWindow))
	if a.enabled {
		t.Fatal("registration without a heartbeat reset the deadline")
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

func TestConnectionAutoPausePersistenceFailureLeavesPolicyAndRetries(t *testing.T) {
	f, path, now, a, _ := connectionPauseFixture(t)
	if err := f.SetHomeEnabled("b", false); err != nil {
		t.Fatal(err)
	}
	logs := &logBuffer{}
	f.logger = slog.New(slog.NewJSONHandler(logs, nil))
	stops := 0
	a.cancel = func() { stops++ }
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	f.pauseUnresponsiveHomes(now.Add(connectionRetryWindow))
	f.pauseUnresponsiveHomes(now.Add(2 * connectionRetryWindow))
	if !a.enabled || a.autoPaused || stops != 0 {
		t.Fatal("failed persistence changed live policy")
	}
	if logs.count("could not save automatic connection pause") != 1 {
		t.Fatal("repeated identical persistence warnings")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	f.pauseUnresponsiveHomes(now.Add(2 * connectionRetryWindow))
	if a.enabled || !a.autoPaused || stops != 1 {
		t.Fatal("did not pause after persistence recovered")
	}
}

func TestConnectionAutoPauseDefersDuringUpdateReservation(t *testing.T) {
	for _, state := range []string{"prepared", "finishing", "expired"} {
		t.Run(state, func(t *testing.T) {
			f, _, now, a, _ := connectionPauseFixture(t)
			if err := f.SetHomeEnabled("b", false); err != nil {
				t.Fatal(err)
			}
			f.update.until = time.Now().Add(time.Minute)
			if state == "finishing" {
				f.update.finishing = true
				f.update.until = time.Now().Add(-time.Minute)
			}
			if state == "expired" {
				f.update.until = time.Now().Add(-time.Minute)
			}
			f.pauseUnresponsiveHomes(now.Add(connectionRetryWindow))
			if state == "expired" {
				if a.enabled {
					t.Fatal("expired reservation kept retries running")
				}
				return
			}
			if !a.enabled || !a.failedSince.Equal(now) {
				t.Fatal("reservation changed policy or reset outage time")
			}
			if err := f.SetHomeEnabled("a", false); err == nil {
				t.Fatal("manual policy change bypassed updater reservation")
			}
			f.update.finishing = false
			if err := f.controlUpdate("cancel-update", os.Getpid()); err != nil {
				t.Fatal(err)
			}
			f.pauseUnresponsiveHomes(now.Add(connectionRetryWindow))
			if a.enabled || !a.autoPaused {
				t.Fatal("canceling update did not resume expired outage evaluation")
			}
		})
	}
}

func TestConnectionAutoPauseCancelsStalledDialAndKeepsRetryWindowAcrossRunnerExit(t *testing.T) {
	for _, stalled := range []bool{true, false} {
		t.Run(map[bool]string{true: "stalled", false: "runner-exited"}[stalled], func(t *testing.T) {
			f, _, now, _, _ := connectionPauseFixture(t)
			ticks := make(chan time.Time)
			f.connectionTicks = ticks
			var attempts atomic.Int32
			f.runHome = func(ctx context.Context, h *managedHome) error {
				if h.home.ID == "b" {
					<-ctx.Done()
					return ctx.Err()
				}
				attempts.Add(1)
				if stalled {
					<-ctx.Done()
					return ctx.Err()
				}
				return errors.New("fixture runner exit")
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); _ = f.Run(ctx) }()
			defer func() { cancel(); <-done }()
			eventuallyControl(t, func() bool { return attempts.Load() > 0 })
			if !stalled {
				eventuallyControl(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.managed["a"].retrying })
			}
			// Keep the monitor observing; a single ten-minute jump models a
			// suspended process and correctly discounts that inactive interval.
			for elapsed := time.Second; elapsed <= connectionRetryWindow; elapsed += time.Second {
				ticks <- now.Add(elapsed)
			}
			eventuallyControl(t, func() bool { return f.LocalStatus().Homes[0].State == "Paused automatically" })
			if !f.LocalStatus().Homes[0].AutoPaused {
				t.Fatal("stalled/exited runner escaped pause")
			}
		})
	}
}

func TestConnectionAutoPauseAfterHeartbeatProgressStalls(t *testing.T) {
	f, _, now, a, _ := connectionPauseFixture(t)
	if err := f.SetHomeEnabled("b", false); err != nil {
		t.Fatal(err)
	}
	r := &Runner{}
	c := &Connection{RegistrationId: "registered-but-probe-stalled"}
	r.conn.Store(c)
	a.runner = r
	stops := 0
	a.cancel = func() { stops++ }
	f.connectionHealth(a, r, a.generation, c, true)
	// Recv and the registration remain alive. A probe before Send can stall
	// without starting the Send watchdog, but its last health proof expires.
	f.pauseUnresponsiveHomes(now.Add(connectionRetryWindow - time.Second))
	if !a.enabled || !a.healthy || stops != 0 {
		t.Fatal("healthy heartbeat expired before the retry window")
	}
	f.pauseUnresponsiveHomes(now.Add(connectionRetryWindow))
	if a.enabled || !a.autoPaused || stops != 1 || r.RegistrationId() == "" {
		t.Fatal("stalled heartbeat progress escaped automatic pause")
	}
}

func TestConnectionDisconnectDoesNotResetStalledHeartbeatAge(t *testing.T) {
	f, _, now, a, _ := connectionPauseFixture(t)
	if err := f.SetHomeEnabled("b", false); err != nil {
		t.Fatal(err)
	}
	r := &Runner{}
	c := &Connection{RegistrationId: "registered-before-stall"}
	r.conn.Store(c)
	a.runner = r
	f.connectionHealth(a, r, a.generation, c, true)
	f.connectionClock = func() time.Time { return now.Add(9 * time.Minute) }
	r.conn.Store(nil)
	f.connectionHealth(a, r, a.generation, c, false)
	if !a.failedSince.Equal(now) {
		t.Fatal("disconnect reset age of the last successful heartbeat")
	}
	f.pauseUnresponsiveHomes(now.Add(connectionRetryWindow))
	if a.enabled || !a.autoPaused {
		t.Fatal("teardown extended a stalled stream's retry window")
	}
}

func TestConnectionObservationGapExcludesSleepWithoutResettingOutage(t *testing.T) {
	f, _, now, a, b := connectionPauseFixture(t)
	a.failedSince = now.Add(-9 * time.Minute)
	b.healthy, b.lastHealthy, b.failedSince = true, now.Add(-time.Second), time.Time{}
	wake := now.Add(2 * time.Hour)
	f.discountConnectionObservationGap(now, wake)
	f.pauseUnresponsiveHomes(wake)
	if !a.enabled || !b.enabled || wake.Sub(a.failedSince) != 9*time.Minute || wake.Sub(b.lastHealthy) != time.Second {
		t.Fatal("sleep consumed the retry window or lost prior outage duration")
	}
	f.pauseUnresponsiveHomes(wake.Add(time.Minute))
	if a.enabled || !a.autoPaused || !b.enabled {
		t.Fatal("sleep reset the whole window or paused a recently healthy home")
	}
	f.discountConnectionObservationGap(wake.Add(time.Minute), wake.Add(3*time.Hour))
	if a.enabled || !a.autoPaused {
		t.Fatal("sleep revived a saved pause")
	}
	// A heartbeat that completed after wake-up but before the monitor's lock
	// must not be shifted into the future by the older observation gap.
	b.lastHealthy = wake.Add(3 * time.Hour)
	f.discountConnectionObservationGap(wake, wake.Add(3*time.Hour))
	if !b.lastHealthy.Equal(wake.Add(3 * time.Hour)) {
		t.Fatal("fresh post-wake heartbeat was moved into the future")
	}
}

func TestConnectionObservationGapCoversTransitionsAfterLastTick(t *testing.T) {
	f, _, now, a, b := connectionPauseFixture(t)
	// Both callbacks happened after the last monitor tick, immediately before
	// suspension. Neither should age by the unobserved two hours on wake-up.
	a.failedSince = now.Add(500 * time.Millisecond)
	b.healthy, b.lastHealthy, b.failedSince = true, now.Add(500*time.Millisecond), time.Time{}
	wake := now.Add(2 * time.Hour)
	f.discountConnectionObservationGap(now, wake)
	f.pauseUnresponsiveHomes(wake)
	if !a.enabled || !b.enabled || !a.failedSince.Equal(wake) || !b.lastHealthy.Equal(wake) {
		t.Fatal("a transition just before sleep consumed its retry window")
	}
	f.pauseUnresponsiveHomes(wake.Add(connectionRetryWindow - time.Second))
	if !a.enabled || !b.enabled {
		t.Fatal("wake-up did not allow an active retry window")
	}
	f.pauseUnresponsiveHomes(wake.Add(connectionRetryWindow))
	if a.enabled || b.enabled {
		t.Fatal("suspended observation granted an unbounded retry window")
	}
}
