// Package calls holds the pure rules of a generic calls capability:
// admission, queue order, the host-left countdown and schedule validation.
// Everything here is a function over values with the clock passed in, so a
// decision can be checked without an engine, a database or a wall clock
// (purity_test.go holds the package to the standard library).
package calls

import "time"

// SeatKind says what occupies a seat. Only people count against MaxPeople and
// only agents against MaxAgents; avatars and samplers ride on their owner's
// seat and are never refused for room size.
type SeatKind string

const (
	KindPerson  SeatKind = "person"
	KindAgent   SeatKind = "agent"
	KindAvatar  SeatKind = "avatar"
	KindSampler SeatKind = "sampler"
)

// Policy carries every limit the rules consult. It is a value, not a global,
// so a deployment can override it and a test can pin it.
type Policy struct {
	MaxPeople            int
	MaxAgents            int
	RoomJoinsPerMinute   int
	MaxUpcomingScheduled int
	MaxScheduledMinutes  int

	HostLeftGrace   time.Duration
	PassTTL         time.Duration
	ScheduleHorizon time.Duration
	QueueEntryTTL   time.Duration
	QueuePoll       time.Duration
}

// DefaultPolicy is the shipped policy.
func DefaultPolicy() Policy {
	return Policy{
		MaxPeople:            8,
		MaxAgents:            2,
		RoomJoinsPerMinute:   30,
		MaxUpcomingScheduled: 20,
		MaxScheduledMinutes:  480,
		HostLeftGrace:        5 * time.Minute,
		PassTTL:              120 * time.Second,
		ScheduleHorizon:      30 * 24 * time.Hour,
		QueueEntryTTL:        10 * time.Minute,
		QueuePoll:            3 * time.Second,
	}
}

// RoomState is the snapshot admission is decided against.
type RoomState struct {
	Ended, Locked bool
	People        int
	Agents        int
}

// JoinAsk is one request to take a seat.
type JoinAsk struct {
	Kind         SeatKind
	Host, Rejoin bool
}

// Outcome is the answer to a join request.
type Outcome string

const (
	OutcomeAdmit  Outcome = "admit"
	OutcomeWait   Outcome = "wait"
	OutcomeQueue  Outcome = "queue"
	OutcomeRefuse Outcome = "refuse"
)

// Decision is an Outcome and, for anything but admit, a stable reason code a
// client can switch on.
type Decision struct {
	Outcome Outcome
	Reason  string
}
