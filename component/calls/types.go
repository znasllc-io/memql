// Package calls holds the pure rules of a generic calls capability:
// admission, queue order, the host-left countdown and schedule validation.
// Everything here is a function over values with the clock passed in, so a
// decision can be checked without an engine, a database or a wall clock
// (purity_test.go holds the package to the standard library).
package calls

import (
	"errors"
	"time"
)

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
	ProcessingEnabled    bool
	RequirePasskeys      bool
	SimulatedFunding     bool
	UsagePreview         bool
	CallConfiguration    bool
	MaxPeople            int
	MaxAgents            int
	RoomJoinsPerMinute   int
	MaxUpcomingScheduled int
	GuestInactiveDays    int
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
		GuestInactiveDays:    30,
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

// The reason codes a calls service adds to DecideJoin's own (call_ended,
// room_full, agent_seats_full, locked, capacity). Each is stable: a client
// switches on it, and a route maps ReasonRate to "try again after".
const (
	// ReasonRate: the room has taken its join attempts for this minute.
	ReasonRate = "rate"
	// ReasonInAnotherCall: the session holds a seat in another call that is
	// still open, and one browser is in one call at a time.
	ReasonInAnotherCall = "in_another_call"
	// ReasonKeyInCall: the key already has a call open, and Start opens no
	// second call on it.
	ReasonKeyInCall = "key_in_call"
	// ReasonNoMediaServer: no media server is taking new calls.
	ReasonNoMediaServer = "no_media_server"
	// ReasonRemoved: the host removed this seat from the call.
	ReasonRemoved = "removed"
	// ReasonKeyNotActive: the key is frozen or banned, and opens no new call.
	ReasonKeyNotActive = "key_not_active"
)

// SiteRef names the deployable a calls request is for: the site, its owner,
// as whom every calls row of the site is read and written, and the
// deployable's name, which a room records. A route builds it from the site it
// resolved and checked; the calls rows themselves are what bind a secret to
// this pair, so a site and owner that do not belong together find nothing.
type SiteRef struct {
	SiteID, OwnerUserID, Deployable string
}

// The two kinds of session. A host session is opened by a key, or by a call's
// host link; a guest session by the call's guest link.
const (
	SessionHost  = "host"
	SessionGuest = "guest"
)

// SessionLifetime is how long a session opens anything. It is the
// capability's constant, not a policy value: a session is a browser's hold on
// a seat, and a key holder unlocks again tomorrow.
const SessionLifetime = 24 * time.Hour

// Session is one bearer session as its holder presents it. Token is the
// secret the holder keeps (a cookie), handed out once; the store keeps only
// its digest. Site is where the session was opened, which is where every call
// it makes is answered.
type Session struct {
	Token                 string
	Kind                  string
	KeyID, SeatID, RoomID string
	ExpiresAt             time.Time
	Site                  SiteRef
}

// CallLinks is a call's id and its two link secrets, handed out once. The
// host link seats its holder as a host; the guest link as a guest.
type CallLinks struct {
	ID, GuestToken, HostToken string
}

// Confirmation is what every person confirms at every join: that they are 18
// or older, that no one under 18 will appear on their camera or screen, and
// the version of the terms they accept. Each seat records all three.
type Confirmation struct {
	Adult, NoMinors   bool
	Participation     string
	AuthorityAccepted bool
	Terms             string
}

// JoinBody names a call by one invitation, an account-owned history entry, or
// the session's current seat. HistoryID is a lookup, never a bearer capability.
type JoinBody struct {
	FamilyInvitationID                string
	Family                            bool
	FamilyToken                       string
	Token, HostToken, HistoryID, Name string
	Confirm                           Confirmation
}

// JoinResult answers a join, a start's host seat or a poll. An admit carries
// the media server's address, a pass and the seat's media identity, and the
// pass's lifetime; a wait or a queue says when to ask again and, for a queue,
// the place in line, counted from one; a refusal carries its reason.
type JoinResult struct {
	Outcome                     Outcome
	URL, Pass, Identity, Reason string
	Host                        bool
	ExpiresIn, RetryAfter       time.Duration
	Position                    int
}

// StartResult is a started call's links and what became of its host.
type StartResult struct {
	Call CallLinks
	Join JoinResult
}

// StartBody is what a host starts a call with: the host's display name and
// the confirmation every seat records.
type StartBody struct {
	QuoteID        string
	IdempotencyKey string
	Title          string
	Name           string
	Confirm        Confirmation
}

// ScheduleRequest books a call: when it starts, how many minutes it is booked
// for, and its title in the calendar file, at most MaxScheduledTitleRunes
// characters.
type ScheduleRequest struct {
	QuoteID        string
	IdempotencyKey string
	Start          time.Time
	Minutes        int
	Title          string
}

// ScheduledCall is a booked call as its scheduler is handed it, once: its id,
// its booked span, its title, the secrets of its two links, both links built
// on the deployable's public host, and its calendar file. The host link seats
// its holder as a host, the guest link as a guest. Only the links' digests are
// stored, so the tokens and links here are the only copies there are; the
// calendar file carries the guest link and never the host token.
type ScheduledCall struct {
	Configuration         *CallConfiguration
	ID                    string
	Start, End            time.Time
	Title                 string
	GuestToken, HostToken string
	GuestLink, HostLink   string
	ICS                   []byte
}

// The errors a calls service answers with, which a route maps to its replies.
// A refusal that has a reason returns its JoinResult beside the error, or,
// for an act that answers no JoinResult, is a *Refusal.
var (
	// ErrNoSession: the session presented is unknown, ended or expired.
	ErrNoSession = errors.New("calls: no session")
	// ErrNotFound: the key or link presented opens nothing on this deployable.
	ErrNotFound = errors.New("calls: not found")
	// ErrEnded: the call has ended.
	ErrEnded = errors.New("calls: the call has ended")
	// ErrForbidden: the session or key may not do this.
	ErrForbidden = errors.New("calls: not allowed")
	// ErrInvalid: the request itself is malformed or incomplete.
	ErrInvalid = errors.New("calls: invalid request")
	// ErrConflict: the request is well formed but the call's state refuses it.
	ErrConflict = errors.New("calls: conflict")
)

// Refusal is a refusal with a stable reason code, for an act whose answer has
// no Reason field of its own: booking, rescheduling and cancelling a call. (A
// join answers its reason in its JoinResult.) It unwraps to its class --
// ErrInvalid, ErrConflict, ErrEnded or ErrForbidden -- and to its cause when
// it has one, so errors.Is still finds the class a route maps to a reply, and
// ReasonOf finds the code a client switches on. Detail is a sentence for a log
// or a person; it never carries a secret.
type Refusal struct {
	Class  error
	Reason string
	Detail string
	Cause  error
}

func (r *Refusal) Error() string {
	text := r.Class.Error() + ": " + r.Reason
	if r.Detail != "" {
		text += ": " + r.Detail
	}
	return text
}

// Unwrap answers the class and, when there is one, the cause.
func (r *Refusal) Unwrap() []error {
	if r.Cause == nil {
		return []error{r.Class}
	}
	return []error{r.Class, r.Cause}
}

// Refuse is a refusal of the given class with a reason code and a sentence.
func Refuse(class error, reason, detail string) error {
	return &Refusal{Class: class, Reason: reason, Detail: detail}
}

// ReasonOf is the reason code of a refusal, or "" for an error that carries
// none.
func ReasonOf(err error) string {
	var refused *Refusal
	if errors.As(err, &refused) {
		return refused.Reason
	}
	return ""
}
