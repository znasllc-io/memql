package calls

// SpotlightRequest changes only the room's shared focal participant. Identity
// binds both the actor and target to the admission visible on the requesting
// screen. A clear names neither target seat nor target identity.
type SpotlightRequest struct {
	RoomID        string `json:"roomId"`
	ActorIdentity string `json:"actorIdentity"`
	Revision      string `json:"revision"`
	RequestID     string `json:"requestId"`
	Seat          string `json:"seat"`
	Identity      string `json:"identity"`
}

type SpotlightParticipant struct {
	Seat        string `json:"seat"`
	Identity    string `json:"identity"`
	DisplayName string `json:"displayName"`
}

// Spotlight is shared layout guidance, not media authority. A private pin may
// take precedence in each viewer's own client without changing this state.
type Spotlight struct {
	Revision    string                `json:"revision"`
	Participant *SpotlightParticipant `json:"participant,omitempty"`
}
