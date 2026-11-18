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
	Action string
	Seat   string
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

// SeatGone decides a seat in a call whose identity the media server no longer shows.
// Its last sign of life is the later of the last time it was seen there and the time it
// was let in, so a seat let in again after leaving is judged from its new admission and
// never from the stint before. It is gone once that sign is grace old -- before then it
// may still be connecting on the pass it was handed -- and it left at that sign: the
// participant list is read every so often, so the last time a seat was seen is the
// closest the record comes to when it went. A seat with no sign of life at all is gone
// now.
func SeatGone(lastSeenAt, joinedAt, now time.Time, grace time.Duration) (bool, time.Time) {
	at := lastSeenAt
	if joinedAt.After(at) {
		at = joinedAt
	}
	if at.IsZero() {
		return true, now
	}
	return !now.Before(at.Add(grace)), at
}
