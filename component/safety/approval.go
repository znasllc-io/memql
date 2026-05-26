package safety

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
)

// ApprovalSink is the substrate the Gate consults when the decision
// policy returns DecisionAsk in enforce mode (memql#232). The DSL-
// backed implementation creates a v1:safety:approvalRequest row on
// first encounter and reuses it on subsequent retries via the
// descriptor's correlation key.
//
// Pattern: Gate.Evaluate calls Check on every Ask verdict (enforce
// mode). The verdict is one of:
//
//   - ApprovalStateApproved -- bypass token live; Gate flips
//     Decision to Allow.
//   - ApprovalStatePending  -- row exists / was created; Gate keeps
//     Decision=Ask. Refusal text includes the request id so the
//     human knows what to resolve.
//   - ApprovalStateDenied   -- a human said no; Gate flips Decision
//     to Deny (the request remains visible until a new dispatch
//     creates a fresh pending row).
//   - ApprovalStateUnconfigured -- no sink wired (default). Gate
//     behavior unchanged; the current "requires user approval"
//     refusal still fires. Keeps the gate safe-by-default when app
//     boot hasn't wired the sink yet.
//
// Implementations MUST be safe for concurrent use and MUST NOT block
// the dispatch path for more than the bounded time required by their
// backing store. Errors should be recovered + logged internally and
// returned as Unconfigured (fail-open with no behavior change) -- a
// dead approval sink must not crash live traffic.
type ApprovalSink interface {
	Check(ctx context.Context, desc ActionDescriptor, cls Classification) ApprovalVerdict
}

// ApprovalState enumerates the possible Gate outcomes from
// consulting the sink.
type ApprovalState string

const (
	// ApprovalStateUnconfigured: no sink wired (or sink internal
	// error). Gate behavior is unchanged -- Decision stays Ask and
	// EnforceDecision returns the "requires user approval" refusal.
	ApprovalStateUnconfigured ApprovalState = "unconfigured"

	// ApprovalStateApproved: a v1:safety:approvalRequest with
	// status="approved" exists for this descriptor's correlation key.
	// Gate flips Decision to Allow; the recorder still writes the
	// row with reason="approved bypass via approvalRequest=...".
	ApprovalStateApproved ApprovalState = "approved"

	// ApprovalStatePending: a v1:safety:approvalRequest with
	// status="pending" exists (newly created or pre-existing). Gate
	// keeps Decision=Ask; refusal text includes the request id.
	ApprovalStatePending ApprovalState = "pending"

	// ApprovalStateDenied: a v1:safety:approvalRequest with
	// status="denied" exists for this correlation key. Gate flips
	// Decision to Deny. A subsequent dispatch can create a fresh
	// pending row (the sink's lookup query excludes denied rows).
	ApprovalStateDenied ApprovalState = "denied"
)

// ApprovalVerdict is what the sink returns to the Gate. The
// ApprovalRequestID is populated for every non-Unconfigured state so
// the Gate can reference it in refusal text + the audit trail.
type ApprovalVerdict struct {
	State             ApprovalState
	ApprovalRequestID string
}

// NoopApprovalSink is the zero-config default the Gate gets when app
// boot hasn't wired a real sink. Returns Unconfigured for every
// Check, which keeps the pre-#232 Ask refusal behavior intact.
type NoopApprovalSink struct{}

func (NoopApprovalSink) Check(_ context.Context, _ ActionDescriptor, _ Classification) ApprovalVerdict {
	return ApprovalVerdict{State: ApprovalStateUnconfigured}
}

// ApprovalCorrelationKey returns a stable SHA-256 hex of the
// descriptor's identifying surface fields. Used by ApprovalSink
// implementations to look up existing approvals + create new ones
// idempotently -- the same shell command, retried by the same agent,
// should collapse to a single approvalRequest row.
//
// Inputs (in order): surface, action, Command, URL, sorted Paths,
// Method, ToolName, sorted Args keys+values. The Body is
// intentionally excluded: bodies vary in legitimate ways for
// "approve once, retry many" workflows (think pagination tokens).
// The Args map IS included because tool-call args usually pin down
// the action's semantics.
//
// Payload is passed through RedactedPayload first so the key is
// stable across credential-rotation scenarios -- two dispatches that
// differ only in the secret value should collapse to the same
// approvalRequest. (The Gate then includes the redacted-but-not-
// scrubbed payload in argsRedacted so the human sees what they're
// approving without seeing the credential.)
func ApprovalCorrelationKey(desc ActionDescriptor) string {
	payload := RedactedPayload(desc.Payload)
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|", desc.Surface, desc.Action)
	fmt.Fprintf(h, "cmd=%s|url=%s|method=%s|tool=%s|",
		payload.Command, payload.URL, payload.Method, payload.ToolName)
	sortedPaths := append([]string(nil), payload.Paths...)
	sort.Strings(sortedPaths)
	for _, p := range sortedPaths {
		fmt.Fprintf(h, "path=%s;", p)
	}
	keys := make([]string, 0, len(payload.Args))
	for k := range payload.Args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(h, "arg:%s=%v;", k, payload.Args[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}
