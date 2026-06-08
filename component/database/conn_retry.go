package database

import (
	"context"
	"database/sql/driver"
	"log/slog"
	"math/rand"
	"strings"
	"time"
)

// conn_retry.go makes physical connection establishment resilient to transient
// Postgres connection-slot exhaustion (SQLSTATE 53300, "remaining connection
// slots are reserved ..." / "too many clients already"). znasllc-io/memql#1076.
//
// 53300 is raised by the SERVER at Connect() time when the cluster's pods
// collectively demand more connections than the instance's max_connections --
// e.g. during a rollout when surge pods + old pods overlap. It is transient:
// a slot frees within milliseconds-to-seconds as other work completes or old
// pods drain. database/sql does NOT retry Connect on its own, so a single
// rejected open surfaces as a failed query (dropped seed materialization,
// health write, automation, checkpoint, ...). Wrapping the driver.Connector so
// Connect() retries 53300 with bounded jittered backoff turns those transient
// rejections into a brief wait instead of a hard failure.
//
// This is defense-in-depth: the primary mitigation is right-sizing the pool
// (MAX_OPEN_CONNS) so steady+surge demand stays under max_connections. The
// retry covers the residual spike; it deliberately does NOT retry forever
// (that would just pile pressure onto an exhausted server).

const (
	defaultConnRetryAttempts = 5
	defaultConnRetryBase     = 100 * time.Millisecond
	defaultConnRetryMax      = 2 * time.Second
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

// retryingConnector wraps a driver.Connector and retries Connect() on transient
// connection-slot exhaustion (53300). All other errors pass through unchanged.
type retryingConnector struct {
	base     driver.Connector
	logger   *slog.Logger
	attempts int
	baseWait time.Duration
	maxWait  time.Duration
}

// newRetryingConnector wraps base so Connect() retries 53300. A nil base
// returns nil (caller falls back to the unwrapped connector).
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
	}
}

func (r *retryingConnector) Connect(ctx context.Context) (driver.Conn, error) {
	var lastErr error
	for attempt := 0; attempt <= r.attempts; attempt++ {
		conn, err := r.base.Connect(ctx)
		if err == nil {
			if attempt > 0 && r.logger != nil {
				r.logger.Warn("db connect recovered after connection-slot exhaustion",
					"component", "memoryNodes", "attempt", attempt+1)
			}
			return conn, nil
		}
		if !isConnSlotExhaustion(err) {
			return nil, err
		}
		lastErr = err
		if attempt == r.attempts {
			break
		}
		wait := backoffWithJitter(r.baseWait, r.maxWait, attempt)
		if r.logger != nil {
			r.logger.Warn("db connect hit connection-slot exhaustion (53300); retrying",
				"component", "memoryNodes", "attempt", attempt+1, "backoff", wait.String())
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
