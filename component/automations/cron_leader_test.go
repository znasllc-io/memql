package automations

import (
	"context"
	"testing"

	"github.com/robfig/cron/v3"
	"github.com/stretchr/testify/assert"
	"github.com/uptrace/bun"
)

// TestScheduleLeaderOK pins the #561 gating contract used by the cron-firing
// closure: ungated when no gate is set, otherwise follows the gate.
func TestScheduleLeaderOK(t *testing.T) {
	s := &Scheduler{} // nil leaderGate -> ungated (single-node / dev)
	assert.True(t, s.scheduleLeaderOK("ordinary"), "nil gate must run (backward-compatible)")

	s.leaderGate = func() bool { return true }
	assert.True(t, s.scheduleLeaderOK("ordinary"), "leader runs scheduled automations")

	s.leaderGate = func() bool { return false }
	assert.False(t, s.scheduleLeaderOK("ordinary"), "a non-leader must NOT run scheduled automations")
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

func TestScheduleGateSelectsIndependentLeadership(t *testing.T) {
	s := &Scheduler{leaderGate: func() bool { return true }, scheduleGate: func(name string) bool { return name == "special" }}
	assert.True(t, s.scheduleLeaderOK("special"))
	assert.False(t, s.scheduleLeaderOK("ordinary"), "per-automation policy must not fall through to the general leader")
	s.leaderGate = func() bool { return false }
	assert.True(t, s.scheduleLeaderOK("special"), "an independent lease must not require general leadership too")
}

func TestCronFiringConsultsItsNamedScheduleGate(t *testing.T) {
	var seen string
	s := &Scheduler{
		cron: cron.New(), entryIds: map[string]cron.EntryID{},
		leaderGate:   func() bool { return true },
		scheduleGate: func(name string) bool { seen = name; return false },
	}
	for _, name := range []string{"special", "ordinary"} {
		if err := s.scheduleAutomation(&Automation{Name: name, Schedule: "*/10 * * * *"}); err != nil {
			t.Fatal(err)
		}
		s.cron.Entry(s.entryIds[name]).Job.Run()
		if seen != name {
			t.Fatalf("cron did not consult its named gate: %q, want %q", seen, name)
		}
	}
	// No executor is installed: reaching execution despite a denied gate
	// would panic, so this also proves refusal happens before running work.
}
