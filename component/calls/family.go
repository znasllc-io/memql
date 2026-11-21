package calls

import "time"

// Family is an explicit local consent simulation. Adult verification is never
// represented as parental consent; no live provider is enabled by this contract.
const FamilyMockConsentVersion = "family-mock-v1"

type FamilyRequest struct {
	RoomID            string `json:"roomId,omitempty"`
	Identity          string `json:"identity,omitempty"`
	Action            string `json:"action"`
	InvitationID      string `json:"invitationId,omitempty"`
	InvitationToken   string `json:"invitationToken,omitempty"`
	IdempotencyKey    string `json:"idempotencyKey,omitempty"`
	ConsentVersion    string `json:"consentVersion,omitempty"`
	ParentalAuthority bool   `json:"parentalAuthority,omitempty"`
}
type FamilyInvitation struct {
	ID        string    `json:"id"`
	Token     string    `json:"token,omitempty"`
	Status    string    `json:"status"`
	Claimed   bool      `json:"claimed"`
	ExpiresAt time.Time `json:"expiresAt"`
}
type FamilyResult struct {
	Mode        string             `json:"mode"`
	Status      string             `json:"status"`
	Invitation  *FamilyInvitation  `json:"invitation,omitempty"`
	Invitations []FamilyInvitation `json:"invitations,omitempty"`
	RetryAfter  int                `json:"retryAfter,omitempty"`
	Session     *Session           `json:"-"`
}
type FamilySession struct {
	Mode         string `json:"mode"`
	Role         string `json:"role"`
	Status       string `json:"status"`
	InvitationID string `json:"invitationId,omitempty"`
}
