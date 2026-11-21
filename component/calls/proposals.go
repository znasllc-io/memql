package calls

import "time"

type ProposalParticipant struct {
	Label  string `json:"label"`
	RoleID string `json:"roleId"`
	Email  string `json:"email,omitempty"`
}
type AgentProposalRequest struct {
	Label        string                `json:"label"`
	Grant        DelegationRequest     `json:"grant"`
	SkillID      string                `json:"skillId"`
	SkillVersion int                   `json:"skillVersion"`
	Participants []ProposalParticipant `json:"participants"`
	Start        time.Time             `json:"start"`
	Resources    []string              `json:"resources"`
	Analysis     bool                  `json:"analysis"`
}
type AgentProposal struct {
	AgentProposalRequest
	ID              string    `json:"id"`
	RequestingAgent string    `json:"requestingAgent"`
	Status          string    `json:"status"`
	ExpiresAt       time.Time `json:"expiresAt"`
	ApprovalPath    string    `json:"approvalPath"`
	ReviewDigest    string    `json:"reviewDigest"`
}
