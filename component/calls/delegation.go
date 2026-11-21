package calls

import (
	"slices"
	"strings"
	"time"
	"unicode"
)

// Funding authority is separate from usage authority. Crediting a wallet never
// replenishes these spending ceilings. All periods are UTC calendar months.
type AutoTopupLimit struct {
	Enabled             bool  `json:"enabled"`
	AmountCents         int64 `json:"amountCents"`
	ThresholdCents      int64 `json:"thresholdCents"`
	PerChargeLimitCents int64 `json:"perChargeLimitCents"`
	PeriodLimitCents    int64 `json:"periodLimitCents"`
	LifetimeLimitCents  int64 `json:"lifetimeLimitCents"`
}

type SkillVersionAuthority struct {
	SkillID string `json:"skillId"`
	Version int    `json:"version"`
}

type DelegationRequest struct {
	PortraitID     string `json:"portraitId,omitempty"`
	PortraitDigest string `json:"portraitDigest,omitempty"`
	PortraitData   string `json:"portraitData,omitempty"`
	Purpose        string `json:"purpose,omitempty"`
	ConductVersion string `json:"conductVersion,omitempty"`
	ReviewDigest   string `json:"reviewDigest,omitempty"`
	// Communication and booking authority are explicit. A proposed participant
	// label is not proof of identity or permission to email arbitrary recipients.
	InvitationRecipients []string                `json:"invitationRecipients,omitempty"`
	AllowAnyRecipient    bool                    `json:"allowAnyRecipient,omitempty"`
	ScheduleStart        time.Time               `json:"scheduleStart,omitempty"`
	ProposalID           string                  `json:"proposalId,omitempty"`
	MaxCalls             int                     `json:"maxCalls,omitempty"`
	SkillVersions        []SkillVersionAuthority `json:"skillVersions,omitempty"`
	ResourceIDs          []string                `json:"resourceIds,omitempty"`
	RoomIDs              []string                `json:"roomIds,omitempty"`
	Terms                string                  `json:"terms"`
	Participation        string                  `json:"participation"`
	AuthorityAccepted    bool                    `json:"authorityAccepted"`
	Action               string                  `json:"action"`
	GrantID              string                  `json:"grantId,omitempty"`
	Label                string                  `json:"label,omitempty"`
	Scopes               []string                `json:"scopes,omitempty"`
	MaxCallCents         int64                   `json:"maxCallCents,omitempty"`
	PeriodLimitCents     int64                   `json:"periodLimitCents,omitempty"`
	LifetimeLimitCents   int64                   `json:"lifetimeLimitCents,omitempty"`
	MaxParticipants      int                     `json:"maxParticipants,omitempty"`
	MaxDurationMinutes   int                     `json:"maxDurationMinutes,omitempty"`
	ExpiresAt            time.Time               `json:"expiresAt,omitempty"`
	Topup                AutoTopupLimit          `json:"topup"`
}

type DelegationInfo struct {
	DelegationRequest
	ApprovalRevision      int       `json:"approvalRevision"`
	ApprovalDigest        string    `json:"approvalDigest"`
	ID                    string    `json:"id"`
	Status                string    `json:"status"`
	CreatedAt             time.Time `json:"createdAt"`
	SpentCents            int64     `json:"spentCents"`
	PeriodSpentCents      int64     `json:"periodSpentCents"`
	TopupSpentCents       int64     `json:"topupSpentCents"`
	TopupPeriodSpentCents int64     `json:"topupPeriodSpentCents"`
}

type DelegationResult struct {
	Portrait   *PortraitInfo    `json:"portrait,omitempty"`
	Policy     AgentPolicy      `json:"policy"`
	Grants     []DelegationInfo `json:"grants"`
	Proposal   *AgentProposal   `json:"proposal,omitempty"`
	Credential string           `json:"credential,omitempty"`
}

// AgentRequest exposes named product operations, never an arbitrary HTTP, SQL,
// shell, or engine call. The bearer credential cannot perform account ceremonies.
type AgentRequest struct {
	Spotlight         *SpotlightRequest         `json:"spotlight,omitempty"`
	Report            *ReportRequest            `json:"report,omitempty"`
	Processing        *ProcessingConsentRequest `json:"processing,omitempty"`
	PortraitID        string                    `json:"portraitId,omitempty"`
	PortraitData      string                    `json:"portraitData,omitempty"`
	Acceptance        *AgentAcceptance          `json:"acceptance,omitempty"`
	GuestToken        string                    `json:"guestToken,omitempty"`
	RequestID         string                    `json:"requestId,omitempty"`
	Invitation        *InviteRequest            `json:"invitation,omitempty"`
	Proposal          *AgentProposalRequest     `json:"proposal,omitempty"`
	ProposalID        string                    `json:"proposalId,omitempty"`
	Workflow          *WorkflowRequest          `json:"workflow,omitempty"`
	Resource          *ResourceRequest          `json:"resource,omitempty"`
	Action            string                    `json:"action"`
	IdempotencyKey    string                    `json:"idempotencyKey,omitempty"`
	ParticipantLimit  int                       `json:"participantLimit,omitempty"`
	DurationMinutes   int                       `json:"durationMinutes,omitempty"`
	Insights          bool                      `json:"insights,omitempty"`
	Resources         bool                      `json:"resources,omitempty"`
	QuoteID           string                    `json:"quoteId,omitempty"`
	Title             string                    `json:"title,omitempty"`
	Name              string                    `json:"name,omitempty"`
	Terms             string                    `json:"terms,omitempty"`
	Participation     string                    `json:"participation,omitempty"`
	AuthorityAccepted bool                      `json:"authorityAccepted,omitempty"`
	Start             time.Time                 `json:"start,omitempty"`
	CallID            string                    `json:"callId,omitempty"`
	Control           string                    `json:"control,omitempty"`
	Seat              string                    `json:"seat,omitempty"`
	ConfirmationToken string                    `json:"confirmationToken,omitempty"`
	Acknowledged      bool                      `json:"acknowledged,omitempty"`
}

var DelegationScopes = []string{"read", "join", "host", "schedule", "control", "topup", "skills_read", "skills_write", "skills_control", "resources_read", "resources_publish", "resources_control", "invite", "portraits_write", "processing", "report"}

func ValidDelegation(r DelegationRequest, now time.Time) bool {
	if r.PortraitData != "" || !ValidPortraitReference(r.PortraitID, r.PortraitDigest) {
		return false
	}
	if r.Terms == "" || len(r.Terms) > 64 || (r.Participation != "adults_only" && r.Participation != "children_present") || (r.Participation == "children_present" && !r.AuthorityAccepted) {
		return false
	}
	if r.Action != "create" || r.GrantID != "" || strings.TrimSpace(r.Label) == "" || len([]rune(r.Label)) > 64 || strings.IndexFunc(r.Label, unicode.IsControl) >= 0 || len(r.Scopes) == 0 || len(r.Scopes) > len(DelegationScopes) {
		return false
	}
	seen := map[string]bool{}
	for _, scope := range r.Scopes {
		if !slices.Contains(DelegationScopes, scope) || seen[scope] {
			return false
		}
		seen[scope] = true
	}
	if r.MaxCallCents <= 0 || r.MaxCallCents > 50000 || r.PeriodLimitCents < r.MaxCallCents || r.PeriodLimitCents > 10000000 || r.LifetimeLimitCents < r.PeriodLimitCents || r.LifetimeLimitCents > 10000000 || r.MaxParticipants < 1 || r.MaxParticipants > 100 || (r.MaxDurationMinutes != 10 && (r.MaxDurationMinutes < 15 || r.MaxDurationMinutes > 1440 || r.MaxDurationMinutes%15 != 0)) || !r.ExpiresAt.After(now) || r.ExpiresAt.After(now.Add(90*24*time.Hour)) {
		return false
	}
	if r.MaxCalls < 0 || r.MaxCalls > 1000 || r.ProposalID != "" {
		return false
	}
	if len(r.InvitationRecipients) > 100 || r.AllowAnyRecipient && len(r.InvitationRecipients) != 0 || !seen["invite"] && (r.AllowAnyRecipient || len(r.InvitationRecipients) != 0) {
		return false
	}
	recipients := map[string]bool{}
	for _, raw := range r.InvitationRecipients {
		mailbox, ok := InvitationRecipient(raw)
		if !ok || recipients[mailbox] {
			return false
		}
		recipients[mailbox] = true
	}
	if !r.ScheduleStart.IsZero() && (!seen["schedule"] || r.MaxCalls != 1 || !r.ScheduleStart.After(now) || r.ScheduleStart.Add(time.Duration(r.MaxDurationMinutes)*time.Minute).After(r.ExpiresAt)) {
		return false
	}
	if len(r.SkillVersions) > 32 || len(r.ResourceIDs) > 32 || len(r.RoomIDs) > 32 {
		return false
	}
	for _, ids := range [][]string{r.ResourceIDs, r.RoomIDs} {
		seenIDs := map[string]bool{}
		for _, id := range ids {
			if !ValidWorkflowID(id) || seenIDs[id] {
				return false
			}
			seenIDs[id] = true
		}
	}
	seenSkills := map[SkillVersionAuthority]bool{}
	for _, skill := range r.SkillVersions {
		if !ValidWorkflowID(skill.SkillID) || skill.Version < 1 || seenSkills[skill] {
			return false
		}
		seenSkills[skill] = true
	}
	t := r.Topup
	if !t.Enabled {
		return !seen["topup"] && t.AmountCents == 0 && t.ThresholdCents == 0 && t.PerChargeLimitCents == 0 && t.PeriodLimitCents == 0 && t.LifetimeLimitCents == 0
	}
	return seen["topup"] && t.AmountCents >= 1000 && t.AmountCents <= 50000 && t.ThresholdCents >= 0 && t.ThresholdCents <= 50000-t.AmountCents && t.PerChargeLimitCents >= t.AmountCents && t.PerChargeLimitCents <= 50000 && t.PeriodLimitCents >= t.PerChargeLimitCents && t.PeriodLimitCents <= 10000000 && t.LifetimeLimitCents >= t.PeriodLimitCents && t.LifetimeLimitCents <= 10000000
}

// Older persisted invite grants remain readable/revocable, but an empty recipient
// policy grants no delivery authority. Every new approval chooses it explicitly.
func ValidDelegationApproval(r DelegationRequest, now time.Time) bool {
	return ValidDelegation(r, now) && ValidAgentPurpose(r.Purpose) && r.Terms == AgentTermsVersion && r.ConductVersion == AgentConductVersion && r.ReviewDigest == "" && (!slices.Contains(r.Scopes, "invite") || r.AllowAnyRecipient || len(r.InvitationRecipients) > 0)
}

// DelegationRecipientAllowed does not infer permission from a participant name,
// a shared guest link, or an empty allowlist. The broad case must be approved.
func DelegationRecipientAllowed(r DelegationRequest, value string) bool {
	mailbox, ok := InvitationRecipient(value)
	if !ok || !slices.Contains(r.Scopes, "invite") {
		return false
	}
	if r.AllowAnyRecipient {
		return true
	}
	for _, raw := range r.InvitationRecipients {
		allowed, valid := InvitationRecipient(raw)
		if valid && mailbox == allowed {
			return true
		}
	}
	return false
}

// Subtraction avoids overflow even if a corrupted receipt names a huge amount.
func WithinDelegationBudget(amount, periodSpent, totalSpent, perCall, period, lifetime int64) bool {
	return amount >= 0 && periodSpent >= 0 && totalSpent >= 0 && amount <= perCall && periodSpent <= period && totalSpent <= lifetime && amount <= period-periodSpent && amount <= lifetime-totalSpent
}

func DelegationPeriod(t time.Time) string { return t.UTC().Format("2006-01") }
