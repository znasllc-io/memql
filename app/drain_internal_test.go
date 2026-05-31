package app

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestApplyShutdownDrain_CallsBeginDrainThenSleeps pins the #552 contract:
// the drain hook fires first (flip readiness to 503), then we keep serving
// for the delay before the Stop sweep.
func TestApplyShutdownDrain_CallsBeginDrainThenSleeps(t *testing.T) {
	began := false
	var slept time.Duration
	applyShutdownDrain(nil,
		func() { began = true },
		3*time.Second,
		func(d time.Duration) { slept = d },
	)
	assert.True(t, began, "BeginDrain must be invoked")
	assert.Equal(t, 3*time.Second, slept, "must sleep for the configured delay")
}

// A zero delay falls back to the package default.
func TestApplyShutdownDrain_ZeroUsesDefault(t *testing.T) {
	var slept time.Duration
	applyShutdownDrain(nil, nil, 0, func(d time.Duration) { slept = d })
	assert.Equal(t, DefaultShutdownDrainDelay, slept)
}

// A negative delay disables the wait entirely (sleep never called).
func TestApplyShutdownDrain_NegativeSkipsSleep(t *testing.T) {
	began := false
	slept := false
	applyShutdownDrain(nil,
		func() { began = true },
		-1,
		func(time.Duration) { slept = true },
	)
	assert.True(t, began, "BeginDrain still fires even when the wait is skipped")
	assert.False(t, slept, "a negative delay must skip the sleep")
}

// A nil BeginDrain hook is tolerated (the delay still applies).
func TestApplyShutdownDrain_NilHook(t *testing.T) {
	var slept time.Duration
	applyShutdownDrain(nil, nil, time.Second, func(d time.Duration) { slept = d })
	assert.Equal(t, time.Second, slept)
}
