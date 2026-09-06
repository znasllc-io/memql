package work

// approval.go -- one concept for every human gate (design record
// docs/superpowers/specs/2026-09-05-work-spine-design.md, section D
// "approval", decision D6).
//
// v1:work:approval replaces the plan's feedbackRequest/feedbackResponse
// fields, the canvas cards the planner emitted onto a cognition space,
// and the safety gate's own ask sink (v1:safety:approvalRequest). One
// concept, six kinds, one inbox -- which is what makes a human gate
// VISIBLE in an engine-only cluster, where the canvas cards were not
// registered at all and every planner approval was already invisible.
//
// THE ARTIFACT HASH IS THE WHOLE GUARANTEE. An approval is a decision
// about a specific thing: this command, this patch, this draft template.
// Resume compares the hash before it acts, so an approval can never carry
// to a modified artifact -- approving one thing and running another is
// the failure mode a per-gate boolean cannot even detect.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Errors resume distinguishes. They are separate values because the
// operator response differs: a changed artifact needs a new approval, a
// pending one needs waiting, a rejected one needs the run to stop.
var (
	// ErrArtifactChanged means the thing approved is not the thing about
	// to run.
	ErrArtifactChanged = errors.New("the approved artifact has changed since it was approved")
	// ErrApprovalPending means nobody has decided yet.
	ErrApprovalPending = errors.New("the approval is still pending")
	// ErrApprovalRejected means a person said no.
	ErrApprovalRejected = errors.New("the approval was rejected")
)

// Approval kinds beyond the two the miss path raises (kind.go holds
// ApprovalKindPlanReview and ApprovalKindFeedback).
const (
	ApprovalKindSideEffect     = "sideEffect"
	ApprovalKindScopeElevation = "scopeElevation"
	ApprovalKindBudget         = "budget"
	ApprovalKindSkillMint      = "skillMint"
)

// ApprovalRequest is the row createWorkApproval writes.
type ApprovalRequest struct {
	RunId        string           `json:"runId"`
	StepKey      string           `json:"stepKey,omitempty"`
	Kind         string           `json:"kind"`
	Subject      map[string]any   `json:"subject,omitempty"`
	ArtifactHash string           `json:"artifactHash"`
	Question     string           `json:"question,omitempty"`
	Options      []map[string]any `json:"options,omitempty"`
	Evidence     Evidence         `json:"evidence"`
	RequestedAt  time.Time        `json:"requestedAt"`
	ExpiresAt    time.Time        `json:"expiresAt,omitempty"`
}

// ArtifactHash hashes the exact thing being approved. Encoded through
// encoding/json with sorted keys, so the digest does not depend on Go's
// randomized map iteration -- an order-sensitive hash would make every
// resume refuse, intermittently, which is the worst possible bug here.
func ArtifactHash(subject map[string]any) string {
	h := sha256.New()
	writeCanonical(h, subject)
	return hex.EncodeToString(h.Sum(nil))
}

func writeCanonical(h interface{ Write([]byte) (int, error) }, m map[string]any) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		_, _ = h.Write([]byte(k))
		_, _ = h.Write([]byte{0})
		if nested, ok := m[k].(map[string]any); ok {
			writeCanonical(h, nested)
			continue
		}
		b, err := json.Marshal(m[k])
		if err != nil {
			// A value that will not encode still has to contribute
			// something deterministic, or two different artifacts hash
			// the same.
			b = []byte(fmt.Sprintf("%#v", m[k]))
		}
		_, _ = h.Write(b)
		_, _ = h.Write([]byte{0})
	}
}

func newApproval(kind, runId, stepKey string, subject map[string]any, ev Evidence, now time.Time, ttl time.Duration) ApprovalRequest {
	a := ApprovalRequest{
		RunId:        runId,
		StepKey:      stepKey,
		Kind:         kind,
		Subject:      subject,
		ArtifactHash: ArtifactHash(subject),
		Evidence:     ev,
		RequestedAt:  now,
	}
	if ttl > 0 {
		a.ExpiresAt = now.Add(ttl)
	}
	return a
}

// BudgetApproval parks a run that reached a ceiling. The subject carries
// the figures because "over budget" with no numbers is not a decision
// anyone can make.
func BudgetApproval(runId, stepKey string, b CeilingBreach, now time.Time, ttl time.Duration) ApprovalRequest {
	return newApproval(ApprovalKindBudget, runId, stepKey, map[string]any{
		"ceiling": b.Ceiling,
		"limit":   b.Limit,
		"actual":  b.Actual,
		"reason":  b.Reason,
	}, Evidence{Tier: "budget", Reason: b.Reason, RuleId: "ceiling." + b.Ceiling, Source: EvidenceSourceRules}, now, ttl)
}

// SideEffectApproval is the safety gate's ask, as a row. artifactHash is
// supplied rather than derived because the gate already computes a
// correlation key over the redacted payload, and the two must agree.
func SideEffectApproval(runId, stepKey, artifactHash string, ev Evidence, subject map[string]any, now time.Time, ttl time.Duration) ApprovalRequest {
	a := newApproval(ApprovalKindSideEffect, runId, stepKey, subject, ev, now, ttl)
	a.ArtifactHash = artifactHash
	return a
}

// PlanReviewApproval carries the healing loop's typed patches to a person
// (D5: never a silent edit, even to the run's own draft template).
func PlanReviewApproval(runId, stepKey, artifactHash string, patches []map[string]any, ev Evidence, now time.Time, ttl time.Duration) ApprovalRequest {
	subject := map[string]any{"patches": patches}
	a := newApproval(ApprovalKindPlanReview, runId, stepKey, subject, ev, now, ttl)
	if artifactHash != "" {
		a.ArtifactHash = artifactHash
	}
	return a
}

// FeedbackApproval asks a person a question and parks the run.
func FeedbackApproval(runId, stepKey, question string, options []map[string]any, ev Evidence, now time.Time, ttl time.Duration) ApprovalRequest {
	subject := map[string]any{"question": question, "options": options}
	a := newApproval(ApprovalKindFeedback, runId, stepKey, subject, ev, now, ttl)
	a.Question = question
	a.Options = options
	return a
}

// ResumeAllowed is the gate resume runs before it acts on an approval.
func ResumeAllowed(approvedHash, currentHash, decision string) (bool, error) {
	switch decision {
	case "":
		return false, ErrApprovalPending
	case "rejected":
		return false, ErrApprovalRejected
	case "approved", "answered":
	default:
		return false, fmt.Errorf("work: unknown approval decision %q", decision)
	}
	if approvedHash != currentHash {
		return false, fmt.Errorf("%w: approved %s, now %s", ErrArtifactChanged, short(approvedHash), short(currentHash))
	}
	return true, nil
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
