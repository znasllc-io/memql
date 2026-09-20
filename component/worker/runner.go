package worker

import (
	"context"
	"errors"
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
	// DefaultTranscriptBytes bounds the session's PROSE, which since epic
	// memql#5396 is one content-addressed Library file rather than a field on
	// the row. It rose from 256 KiB with the destination: 256 KiB was a bound
	// on what a ROW should carry, and a file has no such reason to be small.
	//
	// It is also the limit the cockpit is told on AppSessionStart, so raising
	// it means a chatty run streams more -- which is the intent. The prose
	// now has somewhere to live.
	DefaultTranscriptBytes = MaxTranscriptFileBytes
	// recordingPublishInterval is how often the recording's counters are
	// written when no action has arrived. A chatty run emits thousands of
	// stdout chunks a minute and a row-write each would saturate the engine
	// to deliver a count nobody reads between ticks.
	recordingPublishInterval = 2 * time.Second
	// transcriptTruncationNotice is appended once the bound is hit.
	// Marked rather than silent: a transcript that stops without
	// saying why reads as a run that stopped.
	transcriptTruncationNotice = "\n[transcript truncated -- the full output is in the produced artifacts]\n"
)

// SessionRunner runs app sessions on behalf of a caller.
type SessionRunner struct {
	Logger  *slog.Logger
	Store   AppSessionStore
	Minter  CredentialMinter
	Auditor Auditor
	// MCPEndpoint is the streamable-HTTP MCP URL handed to the app.
	MCPEndpoint string
	// Recorder turns the session's action events into rows of the work spine
	// (epic memql#5396). NIL IS A WORKING SESSION: a node without it runs the
	// session and records nothing, which is what it did before the recording
	// existed.
	Recorder SessionRecorder
	// Contents stores the file contents an action read or wrote, and the
	// session's prose, as content-addressed Library files. Nil records the
	// actions and omits the bytes, saying so on each observation.
	Contents ContentStore
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
	RunId       string
	StepId      string
	// AppSessionRef names the app's own session on the attach path.
	AppSessionRef string
	// RequireLabels narrows machine selection beyond the app: label.
	RequireLabels map[string]string
	// CredentialLifetime and MaxTranscriptBytes come from the
	// delegation policy; zero means the default.
	CredentialLifetime time.Duration
	MaxTranscriptBytes int64
	MaxDuration        time.Duration
	// ResponseSchema is the JSON Schema the harness is asked to answer
	// against. EMPTY MEANS NONE WAS ASKED FOR, which is not the same state
	// as asking and getting nothing back -- only a run that asked can be
	// disappointed by a session that ends with no structured answer.
	ResponseSchema string
	// RecordingRunId is the v1:work:run the session's actions are recorded
	// into -- the subrun epic memql#5391's delegate opened and stamped on the
	// delegating step. EMPTY asks the recorder to open one, which is the
	// delegated-task path: nothing handed a step over, so nothing opened a
	// run for it.
	RecordingRunId string
	// Level is the call's LEVEL, one of core/airoute's closed four (epic
	// memql#5391, design D8). It rides AppSessionStart and THE COCKPIT owns
	// the translation into the app's own knobs. Empty means no level was
	// named and the app runs at its own defaults.
	Level string
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
	// TranscriptFileId is the Library file the prose went to, empty when the
	// write failed or nothing stores content on this node.
	TranscriptFileId string
	// RecordedActions and DroppedActions are the recording's own accounting.
	// A caller reads them to say whether what it is about to lift is the
	// whole session.
	RecordedActions int
	DroppedActions  int
	ErrorMessage    string
	// Result is the harness's structured final answer as raw JSON, when it
	// produced one. Carried whatever the exit code says: a harness can
	// answer the schema and still fail, and dropping the answer because the
	// run failed loses the only part of it that can be read.
	Result []byte
	// Model and Effort are what the APP REPORTED serving this run with (epic
	// memql#5391, design D9), never what was asked for. Empty means it did not
	// say, which every reader records as unknown -- and for effort that is the
	// common case, since Claude Code's headless output states none.
	Model  string
	Effort string
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

// Run executes one app session to completion on the supplied worker.
//
// The CALLER selects the machine. Choosing between machines is the Fleet
// router's job (memql#4350) -- strategy, policy labels, load, the
// cross-node forward -- and a runner that picked for itself would be a
// second router disagreeing with the first. This one runs the session it
// is given.
//
// The ORDER here is load-bearing. The credential is minted and the row is
// written BEFORE AppSessionStart goes on the wire, so a session the
// worker never acknowledges still leaves a row saying it was attempted --
// without that, "nothing happened" and "it failed to start" read
// identically afterwards.
func (r *SessionRunner) Run(ctx context.Context, w *Worker, spec RunSpec, progress ProgressFunc) (RunResult, error) {
	if r == nil {
		return RunResult{}, fmt.Errorf("worker: session runner not configured")
	}
	if w == nil {
		return RunResult{}, fmt.Errorf("worker: session runner needs a selected machine")
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
	// AN UNRESOLVABLE OWNER IS REFUSED BEFORE THE SESSION STARTS (epic
	// memql#5396, task memql#5398), which is the workbench's
	// workspace_owner_unresolved rule applied here (memql#4354).
	//
	// Every row this run writes -- the session, its steps, its observations,
	// the Library files holding what it read and wrote -- is owner-tiered and
	// stamps ownerUserId from the actor. Under a blank one they are written
	// and readable by NOBODY, including the operator answering "what did this
	// agent do on my machine". CockpitAppExecutor already refuses this, and
	// so does the delegate; the check is here as well because this is the
	// last frame before the credential is minted and the row is written, and
	// a third caller arriving later would otherwise inherit the hole.
	if strings.TrimSpace(spec.OwnerUserId) == "" {
		return RunResult{}, fmt.Errorf(
			"worker: workspace_owner_unresolved: an app session with no owner cannot run; every row it " +
				"would write is owner-tiered and a row written under a blank actor is readable by nobody")
	}

	// The router matched on the `app:` label; RunsApp is the same test
	// asked of this worker directly. Re-checking is not redundant: the
	// machine may have signed out between the router's read and now, and
	// starting anyway would hand the app a credential it cannot use.
	if !w.RunsApp(spec.App) {
		return RunResult{}, fmt.Errorf("worker: %s is no longer allowed and signed in on %s",
			spec.App, w.RegistrationId)
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

	// Opened BEFORE the session row so the row can name the run it records
	// into: the MCP node reads that field off the row to write an `mcp` step
	// into the same run, and a row written without it would leave an app's
	// own calls back into MemQL unrecorded for the life of the session.
	recording := newSessionRecording(ctx, r, spec)

	startedAt := r.now()
	row := AppSessionRow{
		ID:                  spec.SessionId,
		SessionRunId:        recording.RunId(),
		OwnerUserId:         spec.OwnerUserId,
		WorkerId:            w.RegistrationId,
		App:                 spec.App,
		Kind:                spec.Kind,
		RunId:               spec.RunId,
		StepId:              spec.StepId,
		Status:              AppSessionStatusStarting,
		Workspace:           spec.Workspace,
		Prompt:              spec.Prompt,
		InputArtifactIds:    spec.Inputs,
		Billing:             BillingUnknown,
		CredentialRef:       cred.IdentityId,
		CredentialExpiresAt: cred.ExpiresAt,
		MCPEndpoint:         r.MCPEndpoint,
		ResponseSchema:      spec.ResponseSchema,
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
		SessionId:      spec.SessionId,
		App:            spec.App,
		Kind:           spec.Kind,
		Prompt:         spec.Prompt,
		Inputs:         spec.Inputs,
		Workspace:      spec.Workspace,
		Credential:     cred.Token,
		MCPEndpoint:    r.MCPEndpoint,
		ResponseSchema: spec.ResponseSchema,
		// The level the CALL declared (design D8). The cockpit translates it
		// into this app's own knobs; the engine never names a model here.
		Level:         spec.Level,
		RunId:         spec.RunId,
		StepId:        spec.StepId,
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

	// The drain goroutine must be able to exit on the CALLER's context as
	// well as on the chunk channel closing. Without that arm this
	// deadlocks: Wait returns as soon as ctx dies, but the chunk channel
	// closes only when the session ends, so a worker that never answers
	// the cancel leaves Run parked on <-drained for the life of the
	// process. Cancel is a request to a machine that may be asleep,
	// wedged or gone -- it is not a guarantee of an AppSessionEnd.
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		flush := time.NewTicker(recordingPublishInterval)
		defer flush.Stop()
		for {
			select {
			case chunk, ok := <-handle.Chunks():
				if !ok {
					recording.Publish(ctx, r.Store, AppSessionStatusRunning)
					return
				}
				// THE STREAM DECIDES WHERE THE CHUNK GOES. An `event` is a
				// completed action the cockpit normalized and becomes rows;
				// stdout and stderr are the model's prose and become the
				// transcript. Flattening both into one string, which is what
				// this did, is what made a session a black box.
				if chunk.Stream == AppSessionStreamEvent {
					recording.Observe(ctx, chunk)
					recording.Publish(ctx, r.Store, AppSessionStatusRunning)
				} else {
					collector.append(chunk)
				}
				if progress != nil {
					progress(chunk)
				}
			case <-flush.C:
				recording.Publish(ctx, r.Store, AppSessionStatusRunning)
			case <-ctx.Done():
				// Publish what was recorded before giving up, so a cancelled
				// run still says how far it got. That count is often the
				// reason somebody cancelled.
				recording.Publish(ctx, r.Store, AppSessionStatusRunning)
				return
			}
		}
	}()

	outcome, waitErr := handle.Wait(ctx)
	<-drained

	status := AppSessionStatusEnded
	errMessage := ""
	switch {
	case errors.Is(waitErr, context.Canceled), errors.Is(waitErr, context.DeadlineExceeded):
		// The CALLER gave up -- their plan was cancelled, or their
		// deadline passed. That is a cancelled session, not a failed
		// one: nothing on the machine misbehaved, and recording it as
		// failed would put a retry-shaped signal on a run that did
		// exactly what it was told.
		status = AppSessionStatusCancelled
		errMessage = firstNonEmpty(outcome.Error, waitErr.Error())
	case waitErr != nil && isCancellationReason(outcome.Error):
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

	transcript, _, truncated := collector.snapshot()
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
		Result:              outcome.Result,
		Model:               outcome.Model,
		Effort:              outcome.Effort,
	}
	// The prose goes to the Library as ONE file per session and the row names
	// it (design D5). Stored before finishRow so the terminal row carries the
	// pointer: a row that said "ended" and named no transcript would read as
	// a session that produced no output.
	row.TranscriptFileId = recording.StoreTranscript(ctx, transcript, truncated, result.Model, result.Effort)
	row.TranscriptTruncated = truncated
	row.RecordedSteps, row.DroppedActions = recording.Close(ctx, result, row.TranscriptFileId)
	result.TranscriptFileId = row.TranscriptFileId
	result.RecordedActions, result.DroppedActions = row.RecordedSteps, row.DroppedActions
	r.finishRow(ctx, row, result, spec, w)
	r.stampProduced(ctx, spec, result)
	if waitErr != nil {
		return result, waitErr
	}
	return result, nil
}

// stampProduced records WHICH INTELLIGENCE made each artifact this session
// produced (epic memql#5391, design D9).
//
// EVERY artifact, the transcript included. A lifted construct's source reads
// this stamp, so a session whose transcript carried no provenance would produce
// procedures whose origin is unanswerable -- and the transcript is the one a
// recording pass reads first.
//
// A FAILED STAMP DOES NOT FAIL THE RUN, and that is deliberate rather than
// lenient. The session already happened on somebody's machine and already spent
// their subscription; losing the back-pointer on a delivered artifact costs a
// reader a journey, while refusing the run would cost them the work. It is
// logged at WARN because it is the kind of silence that otherwise goes
// unnoticed for months.
func (r *SessionRunner) stampProduced(ctx context.Context, spec RunSpec, result RunResult) {
	if r == nil || len(result.ProducedArtifactIds) == 0 {
		return
	}
	stamper, ok := r.Store.(ArtifactProvenanceStamper)
	if !ok || stamper == nil {
		return
	}
	provenance := ArtifactProvenance{
		App: spec.App,
		// The APP'S REPORT, and empty when it said nothing (design D9). The
		// level the session was RUN AT is deliberately not substituted here:
		// a level is what was asked for, and this object is what happened.
		Model:     result.Model,
		Effort:    result.Effort,
		SessionId: result.SessionId,
	}
	if err := stamper.StampArtifactProvenance(ctx, spec.OwnerUserId, result.ProducedArtifactIds, provenance); err != nil && r.Logger != nil {
		r.Logger.Warn("app session: could not stamp provenance on produced artifacts",
			"session_id", result.SessionId, "artifacts", len(result.ProducedArtifactIds), "error", err)
	}
}

// Message sends a FOLLOW-UP into a running session (design D7).
//
// It is on the RUNNER rather than only on the handle because a caller that
// holds a run holds a session id and a worker, not a handle: the handle lives
// inside Run's stack for the life of the session. The runner keeps no session
// table -- that would be a second registry disagreeing with the stream's --
// so the caller supplies the worker it already selected.
//
// A machine whose descriptor says its harness cannot take a follow-up is
// refused BEFORE the wire, because the far side answers by not answering: a
// cockpit with no way to continue a session has nowhere to put the prompt and
// no turn to end, so the caller would wait out its own deadline for a
// capability the registration already reported absent.
func (r *SessionRunner) Message(w *Worker, handle *AppSessionHandle, prompt string) error {
	if r == nil {
		return fmt.Errorf("worker: session runner not configured")
	}
	if w == nil || handle == nil {
		return ErrAppSessionNotFound
	}
	return handle.Message(prompt)
}

// isCancellationReason reports whether a worker-reported error names a
// cancellation.
//
// Matched loosely, and on purpose. The reason string comes from another
// process on somebody else's machine, and the cost of the two mistakes is
// asymmetric: reading a cancel as a failure puts a retry-shaped signal on a
// run that did what it was told, while reading a genuine failure as a cancel
// only loses a retry the caller can ask for again. Neither spelling of the
// word is worth a wrong classification.
func isCancellationReason(reason string) bool {
	lowered := strings.ToLower(strings.TrimSpace(reason))
	return strings.Contains(lowered, "cancel")
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
	row.TranscriptTruncated = result.TranscriptTruncated
	row.ProducedArtifactIds = result.ProducedArtifactIds
	// Written only when the END carried one. A session that ended with
	// nothing leaves the row's existing value alone, so a `submit` the app
	// made over MCP mid-run survives the end that followed it.
	row.Result = result.Result
	row.AppSessionRef = result.AppSessionRef
	row.ErrorMessage = result.ErrorMessage
	row.EndedAt = r.now()

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

func (r *SessionRunner) audit(ctx context.Context, action string, spec RunSpec, w *Worker, detail map[string]any) {
	if r.Auditor == nil {
		return
	}
	detail["sessionId"] = spec.SessionId
	detail["runId"] = spec.RunId
	detail["stepId"] = spec.StepId
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

// transcriptCollector accumulates the session's PROSE under a byte bound.
//
// It sees stdout and stderr only. An `event` chunk is a completed action and
// goes to the recording instead -- the split this epic exists for, because
// flattening both into one string is what made a session a black box.
type transcriptCollector struct {
	mu        sync.Mutex
	buf       strings.Builder
	seen      int
	max       int64
	truncated bool
}

func (c *transcriptCollector) append(chunk AppSessionChunk) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen += len(chunk.Data)
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
