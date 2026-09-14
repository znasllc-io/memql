package database

import (
	"context"
	"database/sql/driver"
	"errors"
	"log/slog"
	"math/rand"
	"net"
	"strings"
	"time"
)

// conn_retry.go makes physical connection establishment resilient to two
// transient failures a rolling deploy produces (znasllc-io/memql#1076, and
// design record 2026-09-14-readiness-convergence D7):
//
//   - SQLSTATE 53300 ("remaining connection slots are reserved ..." / "too
//     many clients already"), raised by the SERVER at Connect() when the
//     cluster's pods collectively demand more than max_connections. A small
//     install runs ONE Postgres instance at max_connections=200, and a rolling
//     deploy overlaps the old pods with the new ones -- each at up to eight
//     connections across its two pools -- which is how the ceiling is reached
//     for tens of seconds.
//   - A dial or handshake I/O TIMEOUT, which is what the same window looks
//     like from a pod that never got a slot at all. On 2026-09-13 the pods
//     that booted into it logged `read tcp ...: i/o timeout` against the -rw
//     Service, and the connector passed that through on the first try.
//
// Both are transient: a slot frees as old pods drain. database/sql does NOT
// retry Connect on its own, so a single rejected open surfaces as a failed
// query (a dropped seed materialization, health write, readiness write ...).
// Wrapping the driver.Connector so Connect() retries inside a BOUNDED budget
// turns those into a brief wait instead of a hard failure.
//
// THE BUDGET IS WALL-CLOCK, ~15 s, and a context deadline always wins. Boot
// paths call with no deadline and get the whole budget, which is what covers
// the overlap window; a request path carries its own shorter deadline and is
// bounded by it first. The ladder deliberately does NOT retry forever -- that
// would pile pressure onto an exhausted server.
//
// This is defense-in-depth: the primary mitigation is right-sizing the pool
// (MAX_OPEN_CONNS) so steady+surge demand stays under max_connections.

const (
	defaultConnRetryAttempts = 12
	defaultConnRetryBase     = 100 * time.Millisecond
	defaultConnRetryMax      = 2 * time.Second
	defaultConnRetryBudget   = 15 * time.Second
)

// isConnSlotExhaustion reports whether err is a Postgres connection-slot
// exhaustion (SQLSTATE 53300). Matched on the error text so it works across
// driver error types (bun's pgdriver surfaces the SQLSTATE + message).
func isConnSlotExhaustion(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "53300") ||
		strings.Contains(s, "too many clients already") ||
		strings.Contains(s, "remaining connection slots are reserved")
}

// isConnectTimeout reports whether err is a dial or handshake that ran out of
// time: a net.Error with Timeout(), or the text form pgdriver wraps it in.
// "connection refused" is NOT one: that is a server that is not there, and
// waiting on it is not a repair.
func isConnectTimeout(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return strings.Contains(err.Error(), "i/o timeout")
}

// isRetryableConnectError is the union the ladder retries.
func isRetryableConnectError(err error) bool {
	return isConnSlotExhaustion(err) || isConnectTimeout(err)
}

// retryingConnector wraps a driver.Connector and retries Connect() on a
// transient failure (53300, a dial timeout) inside a bounded budget. All other
// errors pass through unchanged.
type retryingConnector struct {
	base     driver.Connector
	logger   *slog.Logger
	attempts int
	baseWait time.Duration
	maxWait  time.Duration
	// budget is the wall-clock ceiling over every attempt and wait; a context
	// deadline that comes first wins.
	budget time.Duration
}

// newRetryingConnector wraps base so Connect() retries transient failures. A
// nil base returns nil (caller falls back to the unwrapped connector).
func newRetryingConnector(base driver.Connector, logger *slog.Logger) driver.Connector {
	if base == nil {
		return nil
	}
	return &retryingConnector{
		base:     base,
		logger:   logger,
		attempts: defaultConnRetryAttempts,
		baseWait: defaultConnRetryBase,
		maxWait:  defaultConnRetryMax,
		budget:   defaultConnRetryBudget,
	}
}

func (r *retryingConnector) Connect(ctx context.Context) (driver.Conn, error) {
	deadline := time.Now().Add(r.budget)
	var lastErr error
	for attempt := 0; attempt <= r.attempts; attempt++ {
		conn, err := r.base.Connect(ctx)
		if err == nil {
			if attempt > 0 && r.logger != nil {
				r.logger.Warn("db connect recovered after a transient failure",
					"component", "memoryNodes", "attempt", attempt+1)
			}
			return conn, nil
		}
		if !isRetryableConnectError(err) {
			return nil, err
		}
		lastErr = err
		if attempt == r.attempts {
			break
		}
		wait := backoffWithJitter(r.baseWait, r.maxWait, attempt)
		if time.Now().Add(wait).After(deadline) {
			if r.logger != nil {
				r.logger.Warn("db connect retry budget exhausted",
					"component", "memoryNodes", "attempt", attempt+1, "budget", r.budget.String(), "error", err)
			}
			break
		}
		if r.logger != nil {
			r.logger.Warn("db connect hit a transient failure; retrying",
				"component", "memoryNodes", "attempt", attempt+1, "backoff", wait.String(),
				"slotExhaustion", isConnSlotExhaustion(err), "timeout", isConnectTimeout(err))
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	return nil, lastErr
}

func (r *retryingConnector) Driver() driver.Driver { return r.base.Driver() }

// backoffWithJitter returns an exponential backoff (base * 2^attempt) capped at
// maxWait, with full jitter to avoid a thundering herd of pods all retrying in
// lockstep (which would re-exhaust the server the instant a slot frees).
func backoffWithJitter(base, maxWait time.Duration, attempt int) time.Duration {
	d := base << attempt
	if d <= 0 || d > maxWait {
		d = maxWait
	}
	return time.Duration(rand.Int63n(int64(d)) + 1)
}
