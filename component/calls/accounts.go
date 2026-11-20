package calls

import (
	"encoding/json"
	"time"
)

// AccountRequest is one bounded account ceremony or credential-management act.
// Credential is the browser's WebAuthn response, verified by the service.
type AccountRequest struct {
	Action         string          `json:"action"`
	Key            string          `json:"key,omitempty"`
	ChallengeID    string          `json:"challengeId,omitempty"`
	Credential     json.RawMessage `json:"credential,omitempty"`
	CredentialID   string          `json:"credentialId,omitempty"`
	Label          string          `json:"label,omitempty"`
	AccountID      string          `json:"accountId,omitempty"`
	AmountCents    int64           `json:"amountCents,omitempty"`
	Plan           string          `json:"plan,omitempty"`
	IdempotencyKey string          `json:"idempotencyKey,omitempty"`
}

type AccountCapabilities struct {
	Passkeys         bool `json:"passkeys"`
	RequirePasskeys  bool `json:"requirePasskeys"`
	Payments         bool `json:"payments"`
	SimulatedFunding bool `json:"simulatedFunding"`
	UsagePreview     bool `json:"usagePreview"`
}

// AccountID is the durable calls key id, not a MemQL operator identity or the
// site's owning organization. A separate random handle identifies it to WebAuthn.
type AccountInfo struct {
	AccountID       string       `json:"accountId"`
	KeyID           string       `json:"keyId"`
	KeyLabel        string       `json:"keyLabel"`
	CreatedAt       time.Time    `json:"createdAt"`
	BalanceCents    *int64       `json:"balanceCents"`
	CredentialCount int          `json:"credentialCount"`
	Unclaimed       bool         `json:"unclaimed"`
	Legacy          bool         `json:"legacy"`
	CanHost         bool         `json:"canHost"`
	FundingMode     string       `json:"fundingMode"`
	HostingMode     string       `json:"hostingMode,omitempty"`
	ActivePass      *AccountPass `json:"activePass,omitempty"`
}

// AccountPass is a simulated hosting entitlement. Its fixed grant comes from
// the server catalog; remaining room time is derived from authoritative rooms.
type AccountPass struct {
	ID                    string    `json:"id"`
	Plan                  string    `json:"plan"`
	ActivatedAt           time.Time `json:"activatedAt"`
	ExpiresAt             time.Time `json:"expiresAt"`
	TotalSeconds          int64     `json:"totalSeconds"`
	DailySeconds          int64     `json:"dailySeconds"`
	MaxPeople             int       `json:"maxPeople"`
	TotalRemainingSeconds int64     `json:"totalRemainingSeconds"`
	TodayRemainingSeconds int64     `json:"todayRemainingSeconds"`
	DailyResetsAt         time.Time `json:"dailyResetsAt"`
}

type FundingReceipt struct {
	ID             string    `json:"id"`
	IdempotencyKey string    `json:"idempotencyKey"`
	Kind           string    `json:"kind"`
	Status         string    `json:"status"`
	Simulated      bool      `json:"simulated"`
	CreditedCents  int64     `json:"creditedCents"`
	DebitedCents   int64     `json:"debitedCents"`
	BalanceCents   int64     `json:"balanceCents"`
	CreatedAt      time.Time `json:"createdAt"`
	Plan           string    `json:"plan,omitempty"`
}

type AccountCredential struct {
	ID         string     `json:"id"`
	Label      string     `json:"label"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
}

type AccountResult struct {
	Authenticated   bool                `json:"authenticated"`
	Capabilities    AccountCapabilities `json:"capabilities"`
	Account         *AccountInfo        `json:"account,omitempty"`
	Credentials     []AccountCredential `json:"credentials"`
	Key             string              `json:"key,omitempty"`
	ChallengeID     string              `json:"challengeId,omitempty"`
	ExpiresAt       *time.Time          `json:"expiresAt,omitempty"`
	CreationOptions json.RawMessage     `json:"creationOptions,omitempty"`
	RequestOptions  json.RawMessage     `json:"requestOptions,omitempty"`
	Session         *Session            `json:"-"`
	ClearSession    bool                `json:"-"`
	Receipt         *FundingReceipt     `json:"receipt,omitempty"`
	Replayed        bool                `json:"replayed"`
}
