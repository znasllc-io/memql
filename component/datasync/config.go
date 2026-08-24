package datasync

import (
	"time"

	"github.com/znasllc-io/memql/core/env"
)

// Config is the sync runtime's tunables. Every one is optional with a
// working default: a cluster that has never heard of a connector still
// boots, and one that has does not need a configuration pass to get
// sensible behaviour.
type Config struct {
	// Enabled turns the whole runtime off. A node with it false loads
	// every declaration and enforces every guard -- the mirror is still
	// read-only, the outbox is still appended -- and simply does not
	// DELIVER. That is the useful shape of an off switch here: turning
	// off the worker must not turn off the invariants.
	Enabled bool
	// Poll is how often a drain worker looks for entries.
	Poll time.Duration
	// StartupDelay staggers the first pass so a rolling restart does not
	// have every replica contend for the same claim at once.
	StartupDelay time.Duration
	// BatchSize bounds one drain pass.
	BatchSize int
	// MaxAttempts is how many delivery attempts an entry gets before it
	// is dead-lettered. Reached, an entry stops costing the worker time
	// and starts costing an operator attention, which is the correct
	// trade in that order.
	MaxAttempts int
	// BackoffBase is the first retry delay; each further attempt doubles
	// it, capped at BackoffMax.
	BackoffBase time.Duration
	// BackoffMax caps the doubling. Without a cap the eighth attempt on a
	// base of 30s is over an hour away, which for a mirror that is merely
	// rate-limited is a long time to be wrong.
	BackoffMax time.Duration
	// ClaimTTL is how long a drain claim is held. Longer than a pass by
	// enough that a slow pass does not lose its claim mid-delivery, short
	// enough that a dead replica's claim frees up promptly.
	ClaimTTL time.Duration
}

// The environment keys, spelled in FULL at the call sites below.
//
// Not composed from a prefix and a suffix, and that is not a style
// choice: cmd/envscan resolves a read by folding its key argument to a
// constant, and a key assembled at runtime -- or passed through a helper
// as a parameter -- resolves to nothing. The scanner then reports every
// one of these as a registry entry appearing nowhere in the repo, which
// is a drift failure that is really a failure to be readable. Whole
// literals keep the manifest and the code checkable against each other.
const (
	envEnabled      = "MEMQL_SYNC_ENABLED"
	envPoll         = "MEMQL_SYNC_POLL_SECONDS"
	envStartupDelay = "MEMQL_SYNC_STARTUP_DELAY_SECONDS"
	envBatchSize    = "MEMQL_SYNC_BATCH_SIZE"
	envMaxAttempts  = "MEMQL_SYNC_OUTBOX_MAX_ATTEMPTS"
	envBackoffBase  = "MEMQL_SYNC_BACKOFF_BASE_SECONDS"
	envBackoffMax   = "MEMQL_SYNC_BACKOFF_MAX_SECONDS"
	envClaimTTL     = "MEMQL_SYNC_CLAIM_TTL_SECONDS"
)

// LoadConfig reads the runtime's environment.
//
// Every value falls back to its default on absence OR on a malformed
// value. A malformed tunable must not stop a node from delivering: the
// failure mode of "the operator typed 'ten'" should be the default
// interval, not a mirror that silently stops being filled.
func LoadConfig() Config {
	r := env.NewEnvReader("")
	return Config{
		Enabled:      boolOr(r, envEnabled, true),
		Poll:         secondsOr(r, envPoll, 15),
		StartupDelay: secondsOr(r, envStartupDelay, 10),
		BatchSize:    intOr(r, envBatchSize, 50),
		MaxAttempts:  intOr(r, envMaxAttempts, 8),
		BackoffBase:  secondsOr(r, envBackoffBase, 30),
		BackoffMax:   secondsOr(r, envBackoffMax, 900),
		ClaimTTL:     secondsOr(r, envClaimTTL, 120),
	}
}

func boolOr(r env.EnvReader, key string, def bool) bool {
	v, err := r.OptionalBool(key)
	if err != nil || v == nil {
		return def
	}
	return *v
}

func intOr(r env.EnvReader, key string, def int) int {
	v, err := r.OptionalInt(key)
	if err != nil || v == nil || *v <= 0 {
		return def
	}
	return *v
}

func secondsOr(r env.EnvReader, key string, def int) time.Duration {
	return time.Duration(intOr(r, key, def)) * time.Second
}

// backoffFor returns how long to wait before attempt n+1, doubling from
// BackoffBase and capped at BackoffMax.
//
// Attempts are 1-based as the entry counts them, so the FIRST failure
// waits BackoffBase rather than twice it -- a transient blip should cost
// one interval, not two.
func (c Config) backoffFor(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	d := c.BackoffBase
	for i := 1; i < attempts; i++ {
		d *= 2
		if d >= c.BackoffMax {
			return c.BackoffMax
		}
	}
	if d > c.BackoffMax {
		return c.BackoffMax
	}
	return d
}
