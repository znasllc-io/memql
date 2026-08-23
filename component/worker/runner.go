package worker

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// runner.go orchestrates one delegated app run end to end
// (memql#4360): mint the back-channel credential, write the row,
// open the session, stream its output into a bounded transcript,
// renew the credential before it dies, and drive the row to a
// terminal status.

// Defaults for a run whose delegation policy says nothing.
const (
	// DefaultTranscriptBytes bounds what the ROW keeps. The full
	// transcript is pushed to the Library at end; the row is a
	// readable summary, not an archive.
	DefaultTranscriptBytes = 256 * 1024
	// transcriptFlushInterval is how often the accumulated transcript
	// is written. A chatty run emits thousands of chunks a minute and
	// a row-write each would saturate the engine to deliver text
	// nobody reads between flushes.
	transcriptFlushInterval = 2 * time.Second
	// transcriptTruncationNotice is appended once the bound is hit.
	// Marked rather than silent: a transcript that stops without
	// saying why reads as a run that stopped.
	transcriptTruncationNotice = "\n[transcript truncated -- the full output is in the produced artifacts]\n"
)

// SessionRunner runs app sessions on behalf of a caller.
type SessionRunner struct {
	Logger   *slog.Logger
	Registry *Registry
	Store    AppSessionStore
	Minter   CredentialMinter
	Auditor  Auditor
	// MCPEndpoint is the streamable-HTTP MCP URL handed to the app.
	MCPEndpoint string
	// Clock is injectable for tests.
	Clock func() time.Time
}

// RunSpec is one delegated run.
type RunSpec struct {
	SessionId   string
	OwnerUserId string
	App         string
	Kind        string
	Prompt      string
	Workspace   string
	Inputs      []string
	PlanId      string
	TaskId      string
	// AppSessionRef names the app's own session on the attach path.
	AppSessionRef string
	// RequireLabels narrows machine selection beyond the app: label.
	RequireLabels map[string]string
	// CredentialLifetime and MaxTranscriptBytes come from the
	// delegation policy; zero means the default.
	CredentialLifetime time.Duration
	MaxTranscriptBytes int64
	MaxDuration        time.Duration
}

// RunResult is what a completed run reports back.
type RunResult struct {
	SessionId           string
	WorkerId            string
	Status              string
	ExitCode            int
	Usage               AppSessionUsage
	Billing             string
	AppSessionRef       string
	ProducedArtifactIds []string
	Transcript          string
	TranscriptTruncated bool
	ErrorMessage        string
}

// ProgressFunc receives each chunk as it arrives, so a caller can
// bridge them to a live view without waiting for the run to finish.
type ProgressFunc func(chunk AppSessionChunk)

func (r *SessionRunner) now() time.Time {
	if r != nil && r.Clock != nil {
		return r.Clock()
	}
	return time.Now().UTC()
}

// Run executes one app session to completion.
//
// The ORDER here is load-bearing. The credential is minted and the
// row is written BEFORE AppSessionStart goes on the wire, so a
// session the worker never acknowledges still leaves a row saying it
// was attempted -- without that, "nothing happened" and "it failed to
// start" read identically afterwards.
func (r *SessionRunner) Run(ctx context.Context, spec RunSpec, progress ProgressFunc) (RunResult, error) {
	if r == nil || r.Registry == nil {
		return RunResult{}, fmt.Errorf("worker: session runner not configured")
	}
	if strings.TrimSpace(spec.SessionId) == "" {
		return RunResult{}, fmt.Errorf("worker: run requires a session id")
	}
	if !IsKnownAppId(spec.App) {
		return RunResult{}, fmt.Errorf("worker: %q is not an app this engine drives", spec.App)
	}
	if !IsValidAppSessionKind(spec.Kind) {
		return RunResult{}, fmt.Errorf("worker: unknown app session kind %q", spec.Kind)
	}

	w, err := r.Registry.PickWorkerForApp(spec.OwnerUserId, spec.App, spec.RequireLabels)
	if err != nil {
		return RunResult{}, fmt.Errorf("worker: no machine online with %s allowed and signed in: %w", spec.App, err)
	}

	lifetime := spec.CredentialLifetime
	if lifetime <= 0 {
		lifetime = 4 * time.Hour
	}
	// A missing minter REFUSES the run rather than starting one with a
	// blank bearer. An app with no credential can reach nothing over
	// MCP and would report that as "MemQL's tools are broken", which
	// sends the reader looking in entirely the wrong place.
	if r.Minter == nil {
		return RunResult{}, fmt.Errorf("worker: no credential minter configured; an app session cannot be given a back-channel")
	}
	cred, err := r.Minter.Mint(ctx, CredentialRequest{
		SessionId:   spec.SessionId,
		OwnerUserId: spec.OwnerUserId,
		TTL:         lifetime,
	})
	if err != nil {
		return RunResult{}, fmt.Errorf("worker: mint back-channel credential: %w", err)
	}

	startedAt := r.now()
	row := AppSessionRow{
		ID:                  spec.SessionId,
		OwnerUserId:         spec.OwnerUserId,
		WorkerId:            w.RegistrationId,
		App:                 spec.App,
		Kind:                spec.Kind,
		PlanId:              spec.PlanId,
		TaskId:              spec.TaskId,
		Status:              AppSessionStatusStarting,
		Workspace:           spec.Workspace,
		Prompt:              spec.Prompt,
		InputArtifactIds:    spec.Inputs,
		Billing:             BillingUnknown,
		CredentialRef:       cred.IdentityId,
		CredentialExpiresAt: cred.ExpiresAt,
		MCPEndpoint:         r.MCPEndpoint,
		StartedAt:           startedAt,
	}
	if r.Store != nil {
		if err := r.Store.CreateAppSession(ctx, row); err != nil {
			return RunResult{}, fmt.Errorf("worker: write app session row: %w", err)
		}
	}
	r.audit(ctx, "app_session_started", spec, w, map[string]any{
		"app":                 spec.App,
		"kind":                spec.Kind,
		"workspace":           spec.Workspace,
		"mcpEndpoint":         r.MCPEndpoint,
		"credentialExpiresAt": cred.ExpiresAt.Format(time.RFC3339),
		"inputArtifactIds":    spec.Inputs,
	})

	maxTranscript := spec.MaxTranscriptBytes
	if maxTranscript <= 0 {
		maxTranscript = DefaultTranscriptBytes
	}

	handle, err := w.StartAppSession(ctx, AppSessionRequest{
		SessionId:     spec.SessionId,
		App:           spec.App,
		Kind:          spec.Kind,
		Prompt:        spec.Prompt,
		Inputs:        spec.Inputs,
		Workspace:     spec.Workspace,
		Credential:    cred.Token,
		MCPEndpoint:   r.MCPEndpoint,
		PlanId:        spec.PlanId,
		TaskId:        spec.TaskId,
		AppSessionRef: spec.AppSessionRef,
		Limits: AppSessionLimits{
			CredentialLifetime: lifetime,
			MaxDuration:        spec.MaxDuration,
			MaxTranscriptBytes: maxTranscript,
		},
	})
	if err != nil {
		result := RunResult{
			SessionId:    spec.SessionId,
			WorkerId:     w.RegistrationId,
			Status:       AppSessionStatusFailed,
			Billing:      BillingUnknown,
			ErrorMessage: err.Error(),
		}
		r.finishRow(ctx, row, result, spec, w)
		return result, err
	}

	collector := &transcriptCollector{max: maxTranscript}
	stopRenewal := r.startRenewal(ctx, handle, spec, cred, lifetime)
	defer stopRenewal()

	drained := make(chan struct{})
	go func() {
		defer close(drained)
		flush := time.NewTicker(transcriptFlushInterval)
		defer flush.Stop()
		for {
			select {
			case chunk, ok := <-handle.Chunks():
				if !ok {
					r.flushTranscript(ctx, spec.SessionId, collector, AppSessionStatusRunning)
					return
				}
				collector.append(chunk)
				if progress != nil {
					progress(chunk)
				}
			case <-flush.C:
				r.flushTranscript(ctx, spec.SessionId, collector, AppSessionStatusRunning)
			}
		}
	}()

	outcome, waitErr := handle.Wait(ctx)
	<-drained

	status := AppSessionStatusEnded
	errMessage := ""
	switch {
	case waitErr != nil && outcome.Error == "cancelled":
		status = AppSessionStatusCancelled
		errMessage = outcome.Error
	case waitErr != nil:
		status = AppSessionStatusFailed
		errMessage = firstNonEmpty(outcome.Error, waitErr.Error())
	case outcome.ExitCode != 0:
		// A non-zero exit is a FAILED run, not an ended one. Reading
		// it as ended would make a plan whose delegated step crashed
		// look like a plan whose step succeeded and produced nothing.
		status = AppSessionStatusFailed
		errMessage = fmt.Sprintf("app exited %d", outcome.ExitCode)
	}

	transcript, bytesSeen, truncated := collector.snapshot()
	result := RunResult{
		SessionId:           spec.SessionId,
		WorkerId:            w.RegistrationId,
		Status:              status,
		ExitCode:            int(outcome.ExitCode),
		Usage:               outcome.Usage,
		Billing:             DeriveBilling(outcome.Usage, w, spec.App),
		AppSessionRef:       outcome.AppSessionRef,
		ProducedArtifactIds: outcome.ProducedArtifactIds,
		Transcript:          transcript,
		TranscriptTruncated: truncated,
		ErrorMessage:        errMessage,
	}
	row.TranscriptBytes = bytesSeen
	r.finishRow(ctx, row, result, spec, w)
	if waitErr != nil {
		return result, waitErr
	}
	return result, nil
}

// DeriveBilling decides who paid for a run.
//
// The rule is deliberately conservative: it takes BOTH the app's own
// usage report and the subscription state the machine reported for
// that app, and falls to "unknown" whenever either is silent. It
// never infers. The number the owner asked for -- what the
// subscription covered -- is only worth having if silence is visible
// as silence rather than folded into one side.
func DeriveBilling(usage AppSessionUsage, w *Worker, appId string) string {
	if !usage.Known {
		return BillingUnknown
	}
	app, ok := w.App(appId)
	if !ok {
		return BillingUnknown
	}
	switch app.Subscription {
	case SubscriptionPresent:
		return BillingSubscription
	case SubscriptionNone:
		return BillingMetered
	}
	return BillingUnknown
}

func (r *SessionRunner) finishRow(ctx context.Context, row AppSessionRow, result RunResult, spec RunSpec, w *Worker) {
	row.Status = result.Status
	row.ExitCode = result.ExitCode
	row.Usage = result.Usage
	row.Billing = result.Billing
	row.Transcript = result.Transcript
	row.TranscriptTruncated = result.TranscriptTruncated
	row.ProducedArtifactIds = result.ProducedArtifactIds
	row.AppSessionRef = result.AppSessionRef
	row.ErrorMessage = result.ErrorMessage
	row.EndedAt = r.now()
	if row.TranscriptBytes == 0 {
		row.TranscriptBytes = len(result.Transcript)
	}

	if r.Store != nil {
		// A detached context: the caller's may already be cancelled
		// (that is one of the ways a run ends), and losing the
		// terminal row would leave a session that reads as still
		// running forever.
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := r.Store.EndAppSession(writeCtx, row); err != nil && r.Logger != nil {
			r.Logger.Warn("worker: persist app session end failed",
				"session_id", row.ID, "error", err)
		}
	}
	r.audit(ctx, "app_session_ended", spec, w, map[string]any{
		"app":          spec.App,
		"status":       result.Status,
		"exitCode":     result.ExitCode,
		"billing":      result.Billing,
		"usageKnown":   result.Usage.Known,
		"inputTokens":  result.Usage.InputTokens,
		"outputTokens": result.Usage.OutputTokens,
		"costUSD":      result.Usage.CostUSD,
		"errorMessage": result.ErrorMessage,
	})
}

// startRenewal hands a long run a replacement bearer before its
// current one dies. Returns a stop function.
func (r *SessionRunner) startRenewal(ctx context.Context, handle *AppSessionHandle, spec RunSpec, cred Credential, lifetime time.Duration) func() {
	if r.Minter == nil || cred.ExpiresAt.IsZero() {
		return func() {}
	}
	stop := make(chan struct{})
	var once sync.Once
	go func() {
		current := cred
		for {
			wait := time.Until(current.ExpiresAt) - RenewBefore
			if wait < time.Second {
				wait = time.Second
			}
			timer := time.NewTimer(wait)
			select {
			case <-stop:
				timer.Stop()
				return
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			next, err := r.Minter.Mint(ctx, CredentialRequest{
				SessionId:   spec.SessionId,
				OwnerUserId: spec.OwnerUserId,
				TTL:         lifetime,
			})
			if err != nil {
				if r.Logger != nil {
					r.Logger.Warn("worker: app session credential renewal failed",
						"session_id", spec.SessionId, "error", err)
				}
				// Back off and retry rather than giving up: the run is
				// still going, and the alternative is the app's MCP
				// calls failing silently once the old bearer expires.
				select {
				case <-stop:
					return
				case <-ctx.Done():
					return
				case <-time.After(30 * time.Second):
				}
				continue
			}
			if err := handle.RenewCredential(next.Token); err != nil {
				return
			}
			current = next
		}
	}()
	return func() { once.Do(func() { close(stop) }) }
}

func (r *SessionRunner) flushTranscript(ctx context.Context, sessionId string, c *transcriptCollector, status string) {
	if r.Store == nil || !c.dirty() {
		return
	}
	text, bytesSeen, truncated := c.snapshot()
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := r.Store.AppendAppSessionTranscript(writeCtx, sessionId, text, bytesSeen, truncated, status); err != nil {
		if r.Logger != nil {
			r.Logger.Warn("worker: flush app session transcript failed",
				"session_id", sessionId, "error", err)
		}
		return
	}
	c.markClean()
}

func (r *SessionRunner) audit(ctx context.Context, action string, spec RunSpec, w *Worker, detail map[string]any) {
	if r.Auditor == nil {
		return
	}
	detail["sessionId"] = spec.SessionId
	detail["planId"] = spec.PlanId
	detail["taskId"] = spec.TaskId
	if w != nil {
		detail["machine"] = w.Name
	}
	target := ""
	if w != nil {
		target = w.RegistrationId
	}
	r.Auditor.Emit(ctx, AuditEvent{
		Action:      action,
		Actor:       "user:" + spec.OwnerUserId,
		Target:      target,
		TargetType:  "appSession",
		OwnerUserId: spec.OwnerUserId,
		Detail:      detail,
		Timestamp:   r.now(),
	})
}

// transcriptCollector accumulates chunk output under a byte bound.
type transcriptCollector struct {
	mu        sync.Mutex
	buf       strings.Builder
	seen      int
	max       int64
	truncated bool
	changed   bool
}

func (c *transcriptCollector) append(chunk AppSessionChunk) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen += len(chunk.Data)
	c.changed = true
	if c.truncated {
		return
	}
	remaining := int(c.max) - c.buf.Len()
	if remaining <= 0 {
		c.truncated = true
		c.buf.WriteString(transcriptTruncationNotice)
		return
	}
	if len(chunk.Data) <= remaining {
		c.buf.Write(chunk.Data)
		return
	}
	c.buf.Write(chunk.Data[:remaining])
	c.truncated = true
	c.buf.WriteString(transcriptTruncationNotice)
}

func (c *transcriptCollector) snapshot() (string, int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String(), c.seen, c.truncated
}

func (c *transcriptCollector) dirty() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.changed
}

func (c *transcriptCollector) markClean() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.changed = false
}
