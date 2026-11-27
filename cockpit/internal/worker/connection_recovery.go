package worker

import (
	"context"
	"time"
)

// Bound a stalled runner, not the lifetime of an enabled enrollment. A network
// outage must never become a saved owner pause: retries continue until the
// connection recovers or the owner disables it. Healthy heartbeats reset this
// watchdog; ordinary retry/credential backoff remains owned by Runner.
const connectionStallWindow = 10 * time.Minute

func (f *Fleet) connectionNow() time.Time {
	if f.connectionClock != nil {
		return f.connectionClock()
	}
	return time.Now()
}

func (f *Fleet) connectionHealth(h *managedHome, runner *Runner, generation uint64, conn *Connection, healthy bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if h.runner != runner || h.generation != generation || !h.enabled {
		return
	}
	if healthy {
		// A heartbeat that raced stream teardown cannot revive that stream's
		// health. The disconnect clears the atomic pointer before calling us.
		if runner.conn.Load() != conn {
			return
		}
		h.healthy = true
		h.lastHealthy = f.connectionNow()
		h.failedSince = time.Time{}
	} else {
		h.loseConnectionHealth(f.connectionNow())
	}
}

func (f *Fleet) connectionRetryScheduled(h *managedHome, runner *Runner, generation uint64, delay time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if h.runner != runner || h.generation != generation || !h.enabled {
		return
	}
	// A completed attempt proves the retry loop is running. The next attempt
	// gets the stall window after its intended wait, including the jitter's
	// upper bound. In particular a fifteen-minute credential hold is not a
	// ten-minute stall, and must not be replaced by a fresh fast retry loop.
	h.healthy = false
	h.lastHealthy = time.Time{}
	h.failedSince = f.connectionNow().Add(delay + delay/4)
}

// Caller holds f.mu. Teardown is not new progress: a stream that stalled
// before disconnecting must retain the age of its last successful heartbeat.
func (h *managedHome) loseConnectionHealth(now time.Time) {
	h.healthy = false
	if h.failedSince.IsZero() {
		h.failedSince = h.lastHealthy
		if h.failedSince.IsZero() {
			h.failedSince = now
		}
	}
}

func (f *Fleet) monitorConnections(ctx context.Context) {
	ticks := f.connectionTicks
	realTicks := ticks == nil
	if ticks == nil {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		ticks = ticker.C
	}
	previous := f.connectionNow()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticks:
			if realTicks {
				// A ticker can deliver an old buffered tick after sleep. Use
				// observation time so wake-up is recognized before expiring homes.
				now = f.connectionNow()
			}
			f.discountConnectionObservationGap(previous, now)
			previous = now
			f.reconnectUnresponsiveHomes(now)
		}
	}
}

// If the process could not observe connections for over a minute (for example,
// while the machine slept), do not count that gap as failed active retries.
// Preserve time already spent retrying, and never revive a persisted pause.
func (f *Fleet) discountConnectionObservationGap(previous, now time.Time) {
	gap := now.Sub(previous)
	if gap <= time.Minute {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	shift := func(at time.Time) time.Time {
		if at.IsZero() || !at.Before(now) {
			return at
		}
		if at.After(previous) {
			// A transition just before sleep can fall after the last tick.
			// Its active age is unobserved; grant the first post-wake window
			// without moving a fresh callback into the future.
			return now
		}
		return at.Add(gap)
	}
	for _, h := range f.managed {
		if !h.enabled {
			continue
		}
		h.failedSince = shift(h.failedSince)
		h.lastHealthy = shift(h.lastHealthy)
	}
}

func (f *Fleet) reconnectUnresponsiveHomes(now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Let the updater finish its reserved handoff before replacing a runner.
	// This watchdog never changes saved connection choices.
	if f.connectionPolicyReserved() {
		return
	}
	for _, h := range f.managed {
		if !h.enabled || h.duplicateOf != "" || h.cancel == nil {
			continue
		}
		if h.healthy {
			if now.Sub(h.lastHealthy) < connectionStallWindow {
				continue
			}
			// The transport may still be registered while a local probe or
			// inventory operation stalls before Send. Only fresh completed
			// normal heartbeats prove health; incoming idle traffic is irrelevant.
			h.healthy = false
			h.failedSince = h.lastHealthy
		}
		if h.failedSince.IsZero() {
			h.failedSince = now
			continue
		}
		if now.Sub(h.failedSince) < connectionStallWindow {
			continue
		}
		// Cancel only this runner. Its supervisor joins it and creates the next
		// one with the same enrollment. Fence late health callbacks and grant
		// the replacement a full window, even if teardown takes another tick.
		h.generation++
		h.healthy, h.retrying = false, true
		h.lastHealthy = time.Time{}
		h.failedSince = now
		h.cancel()
		f.logger.Warn("worker connection stalled; restarting its runner to reconnect", "home", h.home.ID)
	}
}

// Version 0.23.3 saved outages as enabled:false, auto_paused:true. Recover only
// that recorded automatic decision at startup; an owner's manual pause has no
// marker and must survive. Persist first so the menu and the next start agree.
func (f *Fleet) recoverAutomaticallyPausedHomes() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, home := range f.workers.Homes {
		if !home.AutoPaused {
			continue
		}
		// An explicitly paused duplicate can represent an owner stopping the
		// whole server. Do not revive an old enrollment around that decision.
		manualDuplicate := false
		for j, other := range f.workers.Homes {
			if i != j && !other.IsEnabled() && !other.AutoPaused && sameClusterURL(home.ClusterURL, other.ClusterURL) {
				manualDuplicate = true
				break
			}
		}
		if manualDuplicate {
			f.logger.Warn("automatic connection recovery deferred; resolve the manually paused duplicate enrollment", "home", home.ID)
			continue
		}
		if err := saveHomeEnabled(f.workersPath, home.ID, true); err != nil {
			f.logger.Warn("could not recover saved automatic connection pause", "home", home.ID, "error", err)
			continue
		}
		enabled := true
		f.workers.Homes[i].Enabled = &enabled
		f.workers.Homes[i].AutoPaused = false
		f.logger.Info("resuming connection paused automatically by an earlier worker", "home", home.ID)
	}
}

// Caller holds f.mu; match the existing status/update lock order.
func (f *Fleet) connectionPolicyReserved() bool {
	f.update.mu.Lock()
	defer f.update.mu.Unlock()
	return f.update.finishing || time.Now().Before(f.update.until)
}
