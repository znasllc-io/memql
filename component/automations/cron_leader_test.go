package automations

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/uptrace/bun"
)

// TestScheduleLeaderOK pins the #561 gating contract used by the cron-firing
// closure: ungated when no gate is set, otherwise follows the gate.
func TestScheduleLeaderOK(t *testing.T) {
	s := &Scheduler{} // nil leaderGate -> ungated (single-node / dev)
	assert.True(t, s.scheduleLeaderOK(), "nil gate must run (backward-compatible)")

	s.leaderGate = func() bool { return true }
	assert.True(t, s.scheduleLeaderOK(), "leader runs scheduled automations")

	s.leaderGate = func() bool { return false }
	assert.False(t, s.scheduleLeaderOK(), "a non-leader must NOT run scheduled automations")
}

// TestCronLeader_DefaultNotLeader: a freshly constructed leader is not the
// leader until it acquires the lock.
func TestCronLeader_DefaultNotLeader(t *testing.T) {
	cl := NewCronLeader(func() *bun.DB { return nil }, nil)
	assert.False(t, cl.IsLeader())
	assert.Equal(t, CronLeaderComponentName, cl.ComponentName())
}

// TestCronLeader_FailClosedWithoutDB: polling with no reachable DB leaves the
// node a non-leader (fail-closed -- skip a maintenance cron rather than risk
// a double-run). Also exercises demote() releasing a (here, never-held) lease.
func TestCronLeader_FailClosedWithoutDB(t *testing.T) {
	cl := NewCronLeader(func() *bun.DB { return nil }, nil)
	cl.poll(context.Background())
	assert.False(t, cl.IsLeader(), "no DB -> not leader")

	// Even if we were somehow marked leader, a failed poll demotes.
	cl.leader.Store(true)
	cl.poll(context.Background())
	assert.False(t, cl.IsLeader(), "lost DB connection -> demoted")
}
