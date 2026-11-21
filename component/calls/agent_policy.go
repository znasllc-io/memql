package calls

import (
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const AgentConductVersion = "callkey-agent-conduct-v1"
const AgentConductURL = "/agent-code-of-conduct.md"
const AgentConductDigest = "d0368d220c693bdf0ae07d0668470ecb99d72ccab30f227fd38e71be8722561b"
const AgentTermsVersion = "draft-2026-11-20"

// Policy descriptions are public; authority remains in native approval and
// admission gates. Declared purpose is untrusted text, never an intent verdict.
type AgentPolicy struct {
	ConductDigest    string           `json:"conductDigest"`
	TermsVersion     string           `json:"termsVersion"`
	ConductVersion   string           `json:"conductVersion"`
	ConductURL       string           `json:"conductUrl"`
	PurposeMinLength int              `json:"purposeMinLength"`
	PurposeMaxLength int              `json:"purposeMaxLength"`
	Media            AgentMediaPolicy `json:"media"`
}
type AgentMediaPolicy struct {
	Publish   []string `json:"publish"`
	Subscribe string   `json:"subscribe"`
}

func CurrentAgentPolicy() AgentPolicy {
	return AgentPolicy{ConductDigest: AgentConductDigest, TermsVersion: AgentTermsVersion, ConductVersion: AgentConductVersion, ConductURL: AgentConductURL, PurposeMinLength: 10, PurposeMaxLength: 500, Media: AgentMediaPolicy{Publish: []string{"camera", "microphone", "screen", "data"}, Subscribe: "room_media"}}
}
func ValidAgentPurpose(purpose string) bool {
	n := utf8.RuneCountInString(purpose)
	return utf8.ValidString(purpose) && purpose == strings.TrimSpace(purpose) && n >= 10 && n <= 500 && strings.IndexFunc(purpose, unicode.IsControl) < 0
}

type AgentAcceptance struct {
	ApprovalRevision int    `json:"approvalRevision"`
	ApprovalDigest   string `json:"approvalDigest"`
	ConductVersion   string `json:"conductVersion"`
	Terms            string `json:"terms"`
}
type AgentAuthority struct {
	Grant  DelegationInfo `json:"grant"`
	Policy AgentPolicy    `json:"policy"`
}

// Only already-admitted seats expose this public, approved identity. Account
// identifiers, bearer credentials, balances and private grant scopes stay private.
type AgentParticipant struct {
	PortraitID       string `json:"portraitId,omitempty"`
	PortraitDigest   string `json:"portraitDigest,omitempty"`
	Seat             string `json:"seat"`
	Identity         string `json:"identity"`
	Name             string `json:"name"`
	Purpose          string `json:"purpose"`
	ApprovalRevision int    `json:"approvalRevision"`
	ConductVersion   string `json:"conductVersion"`
}

// A human host reviews this exact immutable purpose and media envelope. It is
// not an assertion of the agent's intentions and does not grant host control.
type PendingAgent struct {
	PortraitID       string           `json:"portraitId,omitempty"`
	PortraitDigest   string           `json:"portraitDigest,omitempty"`
	RequestID        string           `json:"requestId"`
	ReviewDigest     string           `json:"reviewDigest"`
	Name             string           `json:"name"`
	Purpose          string           `json:"purpose"`
	ConductVersion   string           `json:"conductVersion"`
	ApprovalRevision int              `json:"approvalRevision"`
	ExpiresAt        time.Time        `json:"expiresAt"`
	Media            AgentMediaPolicy `json:"media"`
}
type AgentJoinStatus struct {
	CallID    string    `json:"callId"`
	RequestID string    `json:"requestId"`
	Status    string    `json:"status"`
	ExpiresAt time.Time `json:"expiresAt"`
}
