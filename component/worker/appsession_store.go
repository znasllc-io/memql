package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
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
	ID                   string
	OwnerUserId          string
	WorkerId             string
	App                  string
	Kind                 string
	PlanId               string
	TaskId               string
	Status               string
	Workspace            string
	Prompt               string
	InputArtifactIds     []string
	Transcript           string
	TranscriptBytes      int
	TranscriptTruncated  bool
	Usage                AppSessionUsage
	Billing              string
	ExitCode             int
	ProducedArtifactIds  []string
	AppSessionRef        string
	CredentialIdentityId string
	CredentialExpiresAt  time.Time
	MCPEndpoint          string
	ErrorMessage         string
	CancelReason         string
	StartedAt            time.Time
	EndedAt              time.Time
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
		"sessionId":            row.ID,
		"ownerUserId":          row.OwnerUserId,
		"workerId":             row.WorkerId,
		"app":                  row.App,
		"kind":                 row.Kind,
		"planId":               row.PlanId,
		"taskId":               row.TaskId,
		"workspace":            row.Workspace,
		"prompt":               row.Prompt,
		"inputArtifactIds":     stringsOrEmpty(row.InputArtifactIds),
		"mcpEndpoint":          row.MCPEndpoint,
		"credentialIdentityId": row.CredentialIdentityId,
		"startedAt":            row.StartedAt.UTC().Format(time.RFC3339Nano),
	}
	if !row.CredentialExpiresAt.IsZero() {
		args["credentialExpiresAt"] = row.CredentialExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	return s.executeMutation(ctx, "createAppSession", args)
}

// AppendAppSessionTranscript flushes the accumulated transcript.
func (s *EngineStore) AppendAppSessionTranscript(ctx context.Context, sessionId, transcript string, bytes int, truncated bool, status string) error {
	if s == nil || s.Engine == nil {
		return nil
	}
	return s.executeMutation(ctx, "appendAppSessionTranscript", map[string]any{
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
	return s.executeMutation(ctx, "endAppSession", map[string]any{
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
