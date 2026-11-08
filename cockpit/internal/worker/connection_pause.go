package worker

import (
	"context"
	"time"
)

// A home gets a fresh window only after explicit resume or a normal heartbeat
// on its registered stream. A TCP dial or a short-lived RegisterAck must not
// keep a permanently broken connection retrying forever. Once paused, persist
// the same disabled choice as manual pause plus its reason; no token is removed.
const connectionRetryWindow = 10 * time.Minute

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
		h.pauseFailed = false
	} else {
		h.loseConnectionHealth(f.connectionNow())
	}
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
			f.pauseUnresponsiveHomes(now)
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

func (f *Fleet) pauseUnresponsiveHomes(now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// The updater has already accepted a snapshot of the saved connection
	// choices. Defer policy changes until that lease ends, retaining elapsed
	// outage time so cancel/expiry does not buy another retry window.
	if f.connectionPolicyReserved() {
		return
	}
	for _, h := range f.managed {
		if !h.enabled || h.duplicateOf != "" {
			continue
		}
		if h.healthy {
			if now.Sub(h.lastHealthy) < connectionRetryWindow {
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
		if now.Sub(h.failedSince) < connectionRetryWindow {
			continue
		}
		// Write before changing the live choice. A failed write must not leave
		// the menu saying paused while a restart silently reconnects the home.
		if err := saveHomeConnectionState(f.workersPath, h.home.ID, false, true); err != nil {
			if !h.pauseFailed {
				f.logger.Warn("could not save automatic connection pause; connection retries continue", "home", h.home.ID, "error", err)
				h.pauseFailed = true
			}
			continue
		}
		h.enabled, h.autoPaused, h.pauseFailed = false, true, false
		h.generation++
		if h.cancel != nil {
			h.cancel()
		}
		select {
		case h.wake <- struct{}{}:
		default:
		}
		f.logger.Warn("connection paused automatically after ten minutes without a healthy heartbeat; choose Try again to reconnect", "home", h.home.ID)
	}
}

// Caller holds f.mu; match the existing status/update lock order.
func (f *Fleet) connectionPolicyReserved() bool {
	f.update.mu.Lock()
	defer f.update.mu.Unlock()
	return f.update.finishing || time.Now().Before(f.update.until)
}
