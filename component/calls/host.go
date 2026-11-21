package calls

import (
	"slices"
	"time"
)

// host.go -- the pure half of a call's host controls (design section 7.7, "Host
// controls") and of what ends a call (section 8, "End"): which controls there are,
// what each acts on, and when a seat the media server no longer shows has gone. The
// calls service reads the state these decide from (integrations/calls host.go and
// reconcile.go).

// HostActionRequest is one host control: what to do, and the seat it is done to. Lock,
// unlock and end act on the call and name no seat.
type HostActionRequest struct {
	Action       string
	Seat         string
	RequestID    string
	ReviewDigest string
}

// Roster is a call as a host seat in it sees it: what the host controls name. Locked is
// whether the call is locked now, so a host is offered the one of HostLock and HostUnlock
// that changes it. InCall is the call's admitted and in-call seats, in the order they were
// let in; Waiting is its waiting seats in line order, those held behind the lock included.
// Each seat is named by its own id, the one HostActionRequest.Seat takes, and an in-call
// seat by its media identity as well, so a client matches the media server's participants
// to seats. It holds no secret: no token, no digest and no key.
type Roster struct {
	Locked  bool
	InCall  []RosterSeat
	Waiting []RosterWaiter
}

// RosterSeat is one seat in the call.
type RosterSeat struct {
	Seat, DisplayName, Kind, Status, Identity string
	Host                                      bool
}

// RosterWaiter is one seat waiting to come in, and when it began waiting.
type RosterWaiter struct {
	Seat, DisplayName, Kind string
	Since                   time.Time
}

// LeaveRequest is an admission-bound explicit leave. Empty preserves legacy behavior.
// Mode is optional for guests; only occupying hosts may choose a room policy.
type LeaveRequest struct {
	Mode           string `json:"mode,omitempty"`
	LeaveToken     string `json:"leaveToken,omitempty"`
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
}

const (
	LeaveKeepRunning = "keep_running"
	LeaveWrapUp      = "wrap_up"
)

// SeatCall is what a session seated in an open call is told about that call when it asks
// after itself, which every seat's page does while it is in the call (design section 8,
// "End": the host-left countdown is shown on screen). Host is whether the seat hosts the
// call now: a host seat admitted or in the call, which is how a key holder the call was
// handed to learns it is theirs, since a hand-over hands out no new pass. CountdownEndsAt
// is the shared host-left or firm wrap-up deadline: the same instant for every seat, and still said once it has passed
// until a pass has ended the call; the zero time when no countdown runs. Roster is the
// call as its host controls name it, for a host and nobody else. The zero SeatCall is
// what a session seated nowhere, a seat that left or was removed, and a seat of an ended
// call are told. OpenedAt is the room's persisted start, shared by every admitted or
// in-call seat across late joins and reconnects; waiting seats are not given it.
// CountdownKind distinguishes a firm wrap_up from the legacy host_left grace.
// Identity and LeaveToken are given only to an occupying seat.
type SeatCall struct {
	PendingAgents     []PendingAgent
	Agents            []AgentParticipant
	ParticipationMode string
	Family            *FamilySession
	RoomID            string
	Identity          string
	LeaveToken        string
	CountdownKind     string
	ParticipantLimit  int
	Insights          bool
	Resources         bool
	DurationMinutes   int
	ReservedCents     *int64
	CostCents         *int64
	Title             string
	Host              bool
	OpenedAt          time.Time
	CountdownEndsAt   time.Time
	AllowanceEndsAt   time.Time
	FundingMode       string
	Roster            *Roster
}

// The host controls (design section 7.7, "Host controls"). Each is stable: a client
// sends it as written.
const (
	// HostMute mutes the seat's microphone. Only they can unmute themselves.
	HostMute = "mute"
	// HostCameraOff turns the seat's camera off.
	HostCameraOff = "camera_off"
	// HostStopShare stops the seat's screen share, and the share's audio with it.
	HostStopShare = "stop_share"
	// HostRemove takes the seat out of the call for the rest of it.
	HostRemove = "remove"
	// HostLock locks the call: a newcomer waits until a host admits them.
	HostLock = "lock"
	// HostUnlock unlocks it again.
	HostUnlock = "unlock"
	// HostAdmit lets one waiting seat in past the lock.
	HostAdmit = "admit"
	// HostEnd ends the call for everyone in it.
	HostEnd = "end"
)

// The reasons a call ends, as v1:calls:room.endReason records them. Each is stable.
const (
	// EndReasonEnded: a host ended the call.
	EndReasonEnded = "ended"
	// EndReasonHostLeft: nobody hosting it was in it for the host-left grace
	// (HostLeft), and nobody entitled to took over.
	EndReasonHostLeft = "host_left"
	// EndReasonWrapUp: an explicit firm host departure deadline arrived.
	EndReasonWrapUp = "wrap_up"
	// EndReasonEmpty: the last person in it left, and nobody was waiting to come in.
	EndReasonEmpty = "empty"
)

// The reason codes of a host control's refusals, each stable for a client to switch
// on. An ended call is refused with ReasonCallEnded.
const (
	// ReasonNotHost: the session holds no host seat in the call now -- a guest's, a
	// session seated nowhere, a host still waiting to get in or one who has left.
	ReasonNotHost = "not_host"
	// ReasonNotWaiting: admit names a seat that is not waiting to come in.
	ReasonNotWaiting = "not_waiting"
)

// The controls done to a seat, and the sources each track control acts on: exactly the
// sources a person's pass may publish them on (GrantFor), so a control never reaches a
// track it does not name.
var (
	seatControls = map[string]bool{HostMute: true, HostCameraOff: true, HostStopShare: true, HostRemove: true, HostAdmit: true}
	callControls = map[string]bool{HostLock: true, HostUnlock: true, HostEnd: true}
	trackSources = map[string][]string{
		HostMute:      {sourceMicrophone},
		HostCameraOff: {sourceCamera},
		HostStopShare: {sourceScreenShare, sourceScreenShareAudio},
	}
)

// IsHostAction reports whether action is one of the host controls, spelled exactly.
func IsHostAction(action string) bool { return seatControls[action] || callControls[action] }

// HostActionNamesSeat reports whether a host control is done to a seat, and so whether
// a request that names none is malformed.
func HostActionNamesSeat(action string) bool { return seatControls[action] }

// HostActionSources is the track sources a track control acts on, and false for an
// action that is not one. The slice is the caller's own.
func HostActionSources(action string) ([]string, bool) {
	sources, ok := trackSources[action]
	return slices.Clone(sources), ok
}

// SeatAbsence is what a reconciliation pass does with a seat in a call whose identity the
// media server does not show (SeatAbsent).
type SeatAbsence struct {
	// Mark: no pass has found the seat missing in its current stint, so this pass is the
	// first to: it records now as when the seat went missing, and closes nothing.
	Mark bool
	// Gone: the seat has been missing for the grace, and is closed as having left At.
	Gone bool
	// At is the seat's last sign of life: the later of the last time it was seen and the
	// time it was let in, or, for a seat with neither, when it was first found missing.
	At time.Time
}

// SeatAbsent decides a seat in a call whose identity the media server does not show on a
// pass. ABSENCE DECIDES, NOT THE AGE OF A STAMP: a present seat's
// lastSeenAt is refreshed only every so often, so it is routinely as old as the grace, and
// a rule on its age alone closed a seat on the first pass that missed it -- every seat of a
// call at once when a media server restarted and the pass landed before anybody was back.
// So the first pass to miss a seat only records when (absentSince, Mark), and the seat is
// closed once it has been missing for the grace from then, or from a later sign of life (a
// media server's event about it), whichever is later (Gone). Before then it may still be
// connecting on the pass it was handed, or coming back.
//
// It left AT ITS LAST SIGN OF LIFE, the later of lastSeenAt and joinedAt: the participant
// list is read every so often, so the last time a seat was seen is the closest the record
// comes to when it went. A record of going missing from before the seat's latest admission
// is the stint before's, and is no record for this one, so a seat let in again is judged
// from its new stint and never from the one before.
func SeatAbsent(lastSeenAt, joinedAt, absentSince, now time.Time, grace time.Duration) SeatAbsence {
	at := lastSeenAt
	if joinedAt.After(at) {
		at = joinedAt
	}
	if absentSince.IsZero() || absentSince.Before(joinedAt) {
		return SeatAbsence{Mark: true, At: at}
	}
	from := absentSince
	if at.After(from) {
		from = at
	}
	if now.Before(from.Add(grace)) {
		return SeatAbsence{At: at}
	}
	if at.IsZero() {
		at = absentSince
	}
	return SeatAbsence{Gone: true, At: at}
}
