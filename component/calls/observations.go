package calls

import (
	"reflect"
	"slices"
	"strings"
	"time"
)

const ObservationProtocol = "callkey.observations.v1"
const ObservationNotice = "untrusted_observations_v1"

// These operations authorize native text forwarding only. They never grant media,
// device, screenshot, transcription or trusted operator processing authority.
type ObservationRequest struct {
	RoomID              string   `json:"roomId"`
	Identity            string   `json:"identity"`
	ID                  string   `json:"id,omitempty"`
	Revision            int64    `json:"revision,omitempty"`
	IdempotencyKey      string   `json:"idempotencyKey,omitempty"`
	SubjectIdentity     string   `json:"subjectIdentity,omitempty"`
	RecipientIdentities []string `json:"recipientIdentities,omitempty"`
	Purpose             string   `json:"purpose,omitempty"`
	DurationSeconds     int      `json:"durationSeconds,omitempty"`
	Notice              string   `json:"notice,omitempty"`
	Text                string   `json:"text,omitempty"`
}
type ObservationPerson struct {
	Identity string `json:"identity"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}
type ObservationPermission struct {
	ID             string              `json:"id"`
	Revision       int64               `json:"revision"`
	Status         string              `json:"status"`
	Purpose        string              `json:"purpose"`
	Observer       ObservationPerson   `json:"observer"`
	Subject        ObservationPerson   `json:"subject"`
	Recipients     []ObservationPerson `json:"recipients"`
	ExpiresAt      time.Time           `json:"expiresAt"`
	CanConsent     bool                `json:"canConsent"`
	CanWithdraw    bool                `json:"canWithdraw"`
	CanPublish     bool                `json:"canPublish"`
	PublishedCount int                 `json:"publishedCount"`
}
type Observations struct {
	Available    bool                    `json:"available"`
	Reason       string                  `json:"reason,omitempty"`
	Protocol     string                  `json:"protocol"`
	Notice       string                  `json:"notice"`
	Participants []ObservationPerson     `json:"participants"`
	Permissions  []ObservationPermission `json:"permissions"`
	ActiveCount  int                     `json:"activeCount"`
}
type ObservationReceipt struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
	Status   string `json:"status"`
	EventID  string `json:"eventId,omitempty"`
}
type ObservationPacket struct {
	Protocol     string            `json:"protocol"`
	EventID      string            `json:"eventId"`
	RoomID       string            `json:"roomId"`
	PermissionID string            `json:"permissionId"`
	Revision     int64             `json:"revision"`
	Observer     ObservationPerson `json:"observer"`
	Subject      ObservationPerson `json:"subject"`
	Purpose      string            `json:"purpose"`
	Text         string            `json:"text"`
	PublishedAt  int64             `json:"publishedAt"`
	ExpiresAt    int64             `json:"expiresAt"`
}

func ObservationAction(action string) bool {
	return slices.Contains([]string{"observations", "observation_request", "observation_consent", "observation_withdraw", "observation_publish"}, action)
}
func ValidObservationRequest(r WorkflowRequest) bool {
	q := r.Observation
	if !ObservationAction(r.Action) || q == nil || !reflect.DeepEqual(r, WorkflowRequest{Action: r.Action, Observation: q}) || !messageIdentityPattern.MatchString(q.RoomID) || !messageIdentityPattern.MatchString(q.Identity) {
		return false
	}
	base := ObservationRequest{RoomID: q.RoomID, Identity: q.Identity}
	if r.Action == "observations" {
		return reflect.DeepEqual(*q, base)
	}
	if !ValidWorkflowID(q.IdempotencyKey) {
		return false
	}
	base.IdempotencyKey = q.IdempotencyKey
	if r.Action == "observation_request" {
		if !messageIdentityPattern.MatchString(q.SubjectIdentity) || q.SubjectIdentity == q.Identity || !ValidAgentPurpose(q.Purpose) || q.DurationSeconds < 60 || q.DurationSeconds > 300 || q.Notice != ObservationNotice || len(q.RecipientIdentities) < 2 || len(q.RecipientIdentities) > 8 || !slices.Contains(q.RecipientIdentities, q.Identity) || !slices.Contains(q.RecipientIdentities, q.SubjectIdentity) {
			return false
		}
		seen := map[string]bool{}
		for _, id := range q.RecipientIdentities {
			if !messageIdentityPattern.MatchString(id) || seen[id] {
				return false
			}
			seen[id] = true
		}
		base.SubjectIdentity, base.Purpose, base.DurationSeconds, base.Notice, base.RecipientIdentities = q.SubjectIdentity, q.Purpose, q.DurationSeconds, q.Notice, q.RecipientIdentities
		return reflect.DeepEqual(*q, base)
	}
	if !ValidWorkflowID(q.ID) || q.Revision < 1 || q.Revision > 3 {
		return false
	}
	base.ID, base.Revision = q.ID, q.Revision
	switch r.Action {
	case "observation_consent":
		if q.Notice != ObservationNotice {
			return false
		}
		base.Notice = q.Notice
	case "observation_publish":
		if !ValidQuestionText(q.Text) || q.Text != strings.TrimSpace(q.Text) {
			return false
		}
		base.Text = q.Text
	}
	return reflect.DeepEqual(*q, base)
}
