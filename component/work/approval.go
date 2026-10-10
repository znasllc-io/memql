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

// The two answers a failure-path question offers (FailureApproval). They are
// option VALUES rather than decisions: a feedback approval is decided
// `answered`, and which answer was chosen rides the answer object -- so the
// value is what a resume has to read to tell "carry on" from "stop".
const (
	FailureAnswerRetry   = "retry"
	FailureAnswerAbandon = "abandon"
)

// FailureApproval is the failure path's question to a person, as a row (epic
// memql#5127): a classified failure the system will not act on alone -- an
// environment symptom's planReview, a feedback question, or a budget one.
//
// THE SUBJECT IS THE FAILURE, AND THE HASH IS THE SUBJECT'S (memql#5664). The
// failure path used to store one map as the subject and hash a different one,
// with other keys, so the decide side -- which recomputes the hash over the
// stored subject, exactly as it does for every kind whose hash is derived
// (CurrentArtifactHash) -- answered "artifact changed" to every approve and
// every answer, and only a rejection could land. Built through newApproval,
// the subject and its hash are one map and cannot disagree.
//
// The run id is not in the subject, and needs not be: the row carries it, and
// a decision releases only the run waiting on THIS approval's id (the decide
// side's resumeParkedRun), so it can carry neither to another run nor to a
// later failure of this one, which parks on a new approval.
func FailureApproval(kind, runId, stepKey string, symptom Symptom, errorMessage, question string, ev Evidence, now time.Time, ttl time.Duration) ApprovalRequest {
	a := newApproval(kind, runId, stepKey, FailureSubject(symptom, stepKey, errorMessage), ev, now, ttl)
	a.Question = question
	label := "Retry"
	if symptom == SymptomPlan {
		label = "Revise plan"
	}
	a.Options = []map[string]any{
		{"label": label, "value": FailureAnswerRetry},
		{"label": "Abandon", "value": FailureAnswerAbandon},
	}
	return a
}

// FailureSubject is what a failure-path approval is a decision about: the
// symptom the classifier named, the step the run stopped at, and the failure
// in words. It is the one place that shape is spelled, so the row a person
// reads and the hash a decision is checked against are built from it alone.
func FailureSubject(symptom Symptom, stepKey, errorMessage string) map[string]any {
	return map[string]any{
		"symptom":      string(symptom),
		"stepKey":      stepKey,
		"errorMessage": errorMessage,
	}
}

// AnswerAbandons reports whether an `answered` decision chose to STOP the run:
// its value is FailureAnswerAbandon and the approval itself offered that
// option. Checking the offer as well as the value keeps the reading to the
// questions that ask it -- an answer to some other question that happens to
// carry the same word is somebody's answer to that question, not a stop.
func AnswerAbandons(options any, answer map[string]any) bool {
	if value, _ := answer["value"].(string); value != FailureAnswerAbandon {
		return false
	}
	return offers(options, FailureAnswerAbandon)
}

// IsFailureQuestion reports an approval the failure path raised
// (FailureApproval): one offering both Retry and Abandon. Its approve, and an
// answer other than Abandon, means "run the step that failed again", which is
// how the decide side tells a retry from the release of a step that is waiting
// on a person.
func IsFailureQuestion(options any) bool {
	return offers(options, FailureAnswerRetry) && offers(options, FailureAnswerAbandon)
}

// offers reports whether an approval's options -- as built, or as decoded
// from a row -- include value.
func offers(options any, value string) bool {
	var offered []map[string]any
	switch list := options.(type) {
	case []map[string]any:
		offered = list
	case []any:
		for _, o := range list {
			if m, ok := o.(map[string]any); ok {
				offered = append(offered, m)
			}
		}
	}
	for _, o := range offered {
		if v, _ := o["value"].(string); v == value {
			return true
		}
	}
	return false
}

// CurrentArtifactHash answers "what does the thing being approved hash to
// NOW", which is what a decision is checked against (ResumeAllowed). It lives
// beside the builders so the rule the decide side applies and the hash every
// builder raises with are one file's business: memql#5664 was the two drifting
// apart across a module boundary.
//
//   - For a kind whose hash is DERIVED from its subject -- budget, feedback,
//     inferenceUnavailable, a failure-path question, a planReview with no
//     explicit hash -- recomputing over the stored subject is the check: a
//     subject edited since the approval was raised hashes differently and the
//     decision is refused.
//   - sideEffect, scopeElevation and skillMint carry a hash that is NOT a
//     function of the stored subject (sideEffect's is the safety gate's
//     correlation key over the redacted descriptor), so it is passed through:
//     their modified-artifact protection lives where the artifact is, in the
//     next dispatch computing a different key.
//   - An approval with no subject passes its stored hash through: refusing it
//     would refuse every approval raised before the subject was recorded.
//
// A procedurePromotion is the one kind whose artifact is a row that keeps
// changing, so its caller reads the construct's current hash itself rather
// than asking here.
func CurrentArtifactHash(kind, storedHash string, subject map[string]any) string {
	switch kind {
	case ApprovalKindSideEffect, ApprovalKindScopeElevation, ApprovalKindSkillMint:
		return storedHash
	}
	if len(subject) == 0 {
		return storedHash
	}
	return ArtifactHash(subject)
}

// LegacyFailureHashes are the hashes a failure-path question was raised with
// before memql#5664 made its subject and its hash one map: the subject stored
// {symptom, stepKey, errorMessage} -- FailureSubject's shape -- and the hash
// covered {runId, stepKey, error, symptom}, the same failure under other keys
// plus the run's id as the executor spelled it. A question raised then may
// still be pending, so the decide side reads this shape too, for the run-id
// spellings it is given; the hash is still over the stored subject's own
// fields, so a failure edited since is still refused. There is no migration:
// the rows are rare, short-lived (workApprovalTTL), and decidable as they are.
func LegacyFailureHashes(subject map[string]any, runIds ...string) []string {
	symptom, _ := subject["symptom"].(string)
	stepKey, _ := subject["stepKey"].(string)
	errorMessage, _ := subject["errorMessage"].(string)
	out := make([]string, 0, len(runIds))
	for _, runId := range runIds {
		out = append(out, ArtifactHash(map[string]any{
			"runId":   runId,
			"stepKey": stepKey,
			"error":   errorMessage,
			"symptom": symptom,
		}))
	}
	return out
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
