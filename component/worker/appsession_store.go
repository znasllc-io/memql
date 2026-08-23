package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/component/auth"
)

// appsession_store.go persists v1:worker:appSession rows (memql#4360).

// Session statuses.
const (
	AppSessionStatusStarting  = "starting"
	AppSessionStatusRunning   = "running"
	AppSessionStatusEnded     = "ended"
	AppSessionStatusFailed    = "failed"
	AppSessionStatusCancelled = "cancelled"
)

// Billing values on a session row and on the ledger.
const (
	BillingMetered      = "metered"
	BillingSubscription = "subscription"
	BillingUnknown      = "unknown"
)

// AppSessionRow is the persistence projection of v1:worker:appSession.
type AppSessionRow struct {
	ID                  string
	OwnerUserId         string
	WorkerId            string
	App                 string
	Kind                string
	PlanId              string
	TaskId              string
	Status              string
	Workspace           string
	Prompt              string
	InputArtifactIds    []string
	Transcript          string
	TranscriptBytes     int
	TranscriptTruncated bool
	Usage               AppSessionUsage
	Billing             string
	ExitCode            int
	ProducedArtifactIds []string
	AppSessionRef       string
	CredentialRef       string
	CredentialExpiresAt time.Time
	MCPEndpoint         string
	ErrorMessage        string
	CancelReason        string
	StartedAt           time.Time
	EndedAt             time.Time
}

// AppSessionStore is the persistence surface for session rows. Kept
// separate from Store so a binary that does not run app sessions is
// not obliged to implement it.
type AppSessionStore interface {
	CreateAppSession(ctx context.Context, row AppSessionRow) error
	AppendAppSessionTranscript(ctx context.Context, sessionId, transcript string, bytes int, truncated bool, status string) error
	EndAppSession(ctx context.Context, row AppSessionRow) error
}

// CreateAppSession writes the row that says a session was attempted.
func (s *EngineStore) CreateAppSession(ctx context.Context, row AppSessionRow) error {
	if s == nil || s.Engine == nil {
		return fmt.Errorf("worker.store: engine not configured")
	}
	args := map[string]any{
		"sessionId": row.ID,
		// ownerUserId is NOT passed: the mutation stamps it from the actor,
		// so a caller cannot forge the field @rowAuthz(owner=...) keys on.
		// The write runs under the owner's actor -- see writeCtx below.
		"workerId":         row.WorkerId,
		"app":              row.App,
		"kind":             row.Kind,
		"planId":           row.PlanId,
		"taskId":           row.TaskId,
		"workspace":        row.Workspace,
		"prompt":           row.Prompt,
		"inputArtifactIds": stringsOrEmpty(row.InputArtifactIds),
		"mcpEndpoint":      row.MCPEndpoint,
		"credentialRef":    row.CredentialRef,
		"startedAt":        row.StartedAt.UTC().Format(time.RFC3339Nano),
	}
	if !row.CredentialExpiresAt.IsZero() {
		args["credentialExpiresAt"] = row.CredentialExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	return s.executeMutation(appSessionWriteContext(ctx, row.OwnerUserId), "createAppSession", args)
}

// AppendAppSessionTranscript flushes the accumulated transcript.
func (s *EngineStore) AppendAppSessionTranscript(ctx context.Context, sessionId, transcript string, bytes int, truncated bool, status string) error {
	if s == nil || s.Engine == nil {
		return nil
	}
	// No owner to borrow here -- the transcript flush names only the session
	// -- but the internal-origin stamp is still required: the mutation is
	// @serverOnly and an unstamped context reads as a client call.
	return s.executeMutation(appSessionWriteContext(ctx, ""), "appendAppSessionTranscript", map[string]any{
		"sessionId":           sessionId,
		"transcript":          transcript,
		"transcriptBytes":     bytes,
		"transcriptTruncated": truncated,
		"status":              status,
	})
}

// EndAppSession drives the row to a terminal status.
func (s *EngineStore) EndAppSession(ctx context.Context, row AppSessionRow) error {
	if s == nil || s.Engine == nil {
		return nil
	}
	// Usage is written VERBATIM. An app that reported nothing gets
	// known=false and zeroes, and billing stays "unknown" -- folding
	// silence into either metered or subscription is precisely what
	// would make "what did the subscription cover" untrustworthy.
	return s.executeMutation(appSessionWriteContext(ctx, row.OwnerUserId), "endAppSession", map[string]any{
		"sessionId": row.ID,
		"status":    row.Status,
		"exitCode":  row.ExitCode,
		"usage": map[string]any{
			"inputTokens":  row.Usage.InputTokens,
			"outputTokens": row.Usage.OutputTokens,
			"costUSD":      row.Usage.CostUSD,
			"known":        row.Usage.Known,
		},
		"billing":             row.Billing,
		"transcript":          row.Transcript,
		"transcriptBytes":     row.TranscriptBytes,
		"transcriptTruncated": row.TranscriptTruncated,
		"producedArtifactIds": stringsOrEmpty(row.ProducedArtifactIds),
		"appSessionRef":       row.AppSessionRef,
		"errorMessage":        row.ErrorMessage,
		"cancelReason":        row.CancelReason,
		"endedAt":             row.EndedAt.UTC().Format(time.RFC3339Nano),
	})
}

// appSessionWriteContext prepares the context every app-session row write
// needs. It does TWO things, and dropping either one fails in a way that is
// hard to see:
//
//   - It borrows the owning user's authority. The three mutations stamp
//     ownerUserId from the actor (so a caller cannot forge the field
//     @rowAuthz keys on), which means the write must RUN as that user. The
//     engine never out-ranks the user whose row it is writing; it acts as
//     them, the same way the campaign sender does.
//
//   - It stamps INTERNAL origin. All three mutations are @serverOnly, and
//     OriginClient is the zero value -- so an unstamped context is treated
//     as an untrusted client call and the write is REFUSED. The refusal
//     carries only a WARN, so the visible symptom is a session row that
//     never appears, with the engine logging at a level nobody is watching
//     and the caller seeing success. This line is what makes the whole
//     @serverOnly decision workable rather than self-defeating.
func appSessionWriteContext(ctx context.Context, ownerUserId string) context.Context {
	ctx = auth.ContextWithInternalOrigin(ctx)
	if ownerUserId == "" {
		return ctx
	}
	return auth.ContextWithUserActor(ctx, ownerUserId)
}

func (s *EngineStore) executeMutation(ctx context.Context, name string, args map[string]any) error {
	body, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("worker.store: marshal %s args: %w", name, err)
	}
	if _, err := s.Engine.Execute(ctx, fmt.Sprintf("%s(%s)", name, string(body))); err != nil {
		return fmt.Errorf("worker.store: %s: %w", name, err)
	}
	return nil
}

// stringsOrEmpty renders a nil slice as [] rather than null, so the
// mutation's ?? default is never the thing that fires.
func stringsOrEmpty(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
