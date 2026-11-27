package calls

import (
	"reflect"
	"time"
)

// PublicationPermissions limits permission to publish; it never switches on a
// device or unmutes a track. Screen-share audio follows ScreenShare.
type PublicationPermissions struct {
	Microphone  bool `json:"microphone"`
	Camera      bool `json:"camera"`
	ScreenShare bool `json:"screenShare"`
}
type PresentationRequest struct {
	VisitID          string                  `json:"visitId"`
	ExpectedRevision int64                   `json:"expectedRevision"`
	PresenterSeatID  string                  `json:"presenterSeatId,omitempty"`
	Audience         *PublicationPermissions `json:"audience,omitempty"`
	SpeakerSeatID    string                  `json:"speakerSeatId,omitempty"`
	SpeakerSeconds   int                     `json:"speakerSeconds,omitempty"`
}
type PresentationChoice struct {
	SeatID     string `json:"seatId"`
	Name       string `json:"name"`
	Identity   string `json:"identity"`
	Kind       string `json:"kind"`
	ResourceID string `json:"resourceId,omitempty"`
}
type PresentationStatus struct {
	RunID             string                 `json:"runId"`
	VisitID           string                 `json:"visitId"`
	Revision          int64                  `json:"revision"`
	Active            bool                   `json:"active"`
	PresenterSeatID   string                 `json:"presenterSeatId,omitempty"`
	PresenterName     string                 `json:"presenterName,omitempty"`
	PresenterIdentity string                 `json:"presenterIdentity,omitempty"`
	ResourceID        string                 `json:"resourceId,omitempty"`
	Audience          PublicationPermissions `json:"audience"`
	SpeakerSeatID     string                 `json:"speakerSeatId,omitempty"`
	SpeakerName       string                 `json:"speakerName,omitempty"`
	SpeakerIdentity   string                 `json:"speakerIdentity,omitempty"`
	SpeakerExpiresAt  *time.Time             `json:"speakerExpiresAt,omitempty"`
	Enforcement       string                 `json:"enforcement"`
	Reason            string                 `json:"reason,omitempty"`
	CanControl        bool                   `json:"canControl"`
	Choices           []PresentationChoice   `json:"choices,omitempty"`
	ServerNow         time.Time              `json:"serverNow"`
}

func PresentationAction(action string) bool {
	return action == "presentation_status" || action == "presentation_set" || action == "presentation_clear"
}
func ValidPresentationRequest(r WorkflowRequest) bool {
	if !PresentationAction(r.Action) {
		return false
	}
	if r.Action == "presentation_status" {
		return reflect.DeepEqual(r, WorkflowRequest{Action: r.Action})
	}
	if r.Presentation == nil || !reflect.DeepEqual(r, WorkflowRequest{Action: r.Action, Presentation: r.Presentation, RunID: r.RunID, ExpectedRevision: r.ExpectedRevision, ExpectedEpoch: r.ExpectedEpoch, IdempotencyKey: r.IdempotencyKey}) || !ValidWorkflowID(r.RunID) || !ValidWorkflowID(r.IdempotencyKey) || r.ExpectedRevision < 1 || r.ExpectedEpoch < 1 {
		return false
	}
	p := r.Presentation
	if !ValidWorkflowID(p.VisitID) || p.ExpectedRevision < 0 {
		return false
	}
	if r.Action == "presentation_clear" {
		return reflect.DeepEqual(*p, PresentationRequest{VisitID: p.VisitID, ExpectedRevision: p.ExpectedRevision})
	}
	if p.Audience == nil || p.PresenterSeatID == "" || len(p.PresenterSeatID) > 128 {
		return false
	}
	return p.SpeakerSeatID == "" && p.SpeakerSeconds == 0 || p.SpeakerSeatID != "" && len(p.SpeakerSeatID) <= 128 && p.SpeakerSeconds >= 1 && p.SpeakerSeconds <= 300
}
