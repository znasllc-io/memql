package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/num"
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
	ID               string
	OwnerUserId      string
	WorkerId         string
	App              string
	Kind             string
	RunId            string
	StepId           string
	Status           string
	Workspace        string
	Prompt           string
	InputArtifactIds []string
	// SessionRunId names the v1:work:run this session's actions are recorded
	// into -- the subrun the delegating step's childRunId points at. It is on
	// the ROW because the MCP node is a different replica and the row is the
	// only state both can see.
	SessionRunId string
	// TranscriptFileId names the Library file the session's prose went to.
	// It replaced Transcript and TranscriptBytes, which flattened every chunk
	// into one bounded string with the stream and the sequence discarded.
	TranscriptFileId    string
	TranscriptTruncated bool
	// RecordedSteps is how many steps the recording wrote, and the SEQ
	// ALLOCATOR both writers read. DroppedActions is how many actions were
	// lost.
	RecordedSteps       int
	DroppedActions      int
	Usage               AppSessionUsage
	Billing             string
	ExitCode            int
	ProducedArtifactIds []string
	AppSessionRef       string
	CredentialRef       string
	CredentialExpiresAt time.Time
	MCPEndpoint         string
	// ResponseSchema is the JSON Schema the harness was asked to answer
	// against, kept on the row so an app reaching back over MCP can be told
	// the shape its answer must take -- the wire told the cockpit, and
	// nothing told the app.
	ResponseSchema string
	// Result is the harness's structured final answer, as raw JSON. It does
	// NOT imply success and is written from EITHER the session's end or a
	// `submit` the app made over MCP.
	Result       []byte
	ErrorMessage string
	CancelReason string
	StartedAt    time.Time
	EndedAt      time.Time
}

// AppSessionStore is the persistence surface for session rows. Kept
// separate from Store so a binary that does not run app sessions is
// not obliged to implement it.
type AppSessionStore interface {
	CreateAppSession(ctx context.Context, row AppSessionRow) error
	RecordAppSessionProgress(ctx context.Context, sessionId string, recordedSteps, droppedActions int, status string) error
	EndAppSession(ctx context.Context, row AppSessionRow) error
	// ClaimRecordingSlot reads where this session's steps go -- its owner and
	// its recording run -- and takes the next step position, advancing the
	// shared counter.
	//
	// IT IS ON THE ROW BECAUSE TWO REPLICAS WRITE INTO ONE RUN. The replica
	// holding the session writes its actions; the MCP node writes an `mcp`
	// step when the app calls back into MemQL, and the two never speak. A
	// run's step order is what a client sorts by, so a counter in either
	// one's memory is a second opinion about that order -- and not a
	// theoretical one: an app that calls MemQL before its first recorded
	// action, which is ordinary, would collide on seq 0 every time.
	// ownerUserId is the actor to borrow for the read. EMPTY means "use the
	// actor ctx already carries", which is the MCP node's case: an
	// app-session credential's subject IS the owning user, so the inbound
	// actor is already the right one and there is nothing to borrow.
	ClaimRecordingSlot(ctx context.Context, sessionId, ownerUserId string) (RecordingSlot, error)
}

// RecordingSlot is where one step of a session's recording goes.
type RecordingSlot struct {
	// OwnerUserId and RunId come off the session row. Both EMPTY means the
	// session predates the recording, or ran on a node with no recorder --
	// which is not the same as a session that did nothing, and the caller
	// records nothing rather than inventing a run.
	OwnerUserId string
	RunId       string
	// Seq is this step's position, already claimed.
	Seq int
}

// EngineStore serves both surfaces. Asserted rather than left to the
// one assignment in app/ to prove it: a method whose signature drifts
// should fail HERE, next to the interface, rather than in a wiring file
// that reads like configuration.
var _ AppSessionStore = (*EngineStore)(nil)

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
		"runId":            row.RunId,
		"stepId":           row.StepId,
		"sessionRunId":     row.SessionRunId,
		"workspace":        row.Workspace,
		"prompt":           row.Prompt,
		"inputArtifactIds": stringsOrEmpty(row.InputArtifactIds),
		"responseSchema":   row.ResponseSchema,
		"mcpEndpoint":      row.MCPEndpoint,
		"credentialRef":    row.CredentialRef,
		"startedAt":        row.StartedAt.UTC().Format(time.RFC3339Nano),
	}
	if !row.CredentialExpiresAt.IsZero() {
		args["credentialExpiresAt"] = row.CredentialExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	return s.executeMutation(appSessionWriteContext(ctx, row.OwnerUserId), "createAppSession", args)
}

// RecordAppSessionProgress advances a live session's recording counters.
//
// It replaced AppendAppSessionTranscript, which flushed the whole bounded
// transcript string onto the row every two seconds. The prose is now one
// content-addressed Library file written at end, and what a live reader needs
// from the row is how far the recording has got -- which is also the seq the
// MCP node allocates its own step from.
//
// No owner to borrow here -- the progress write names only the session -- but
// the internal-origin stamp is still required: the mutation is @serverOnly and
// an unstamped context reads as a client call.
func (s *EngineStore) RecordAppSessionProgress(ctx context.Context, sessionId string, recordedSteps, droppedActions int, status string) error {
	if s == nil || s.Engine == nil {
		return nil
	}
	args := map[string]any{"sessionId": sessionId}
	// A NEGATIVE COUNT MEANS "DO NOT NAME THIS FIELD", and an empty status the
	// same. The mutation is a read-merge with no `?? 0` on either counter
	// precisely so a caller can advance one without resetting the other --
	// the allocator advances only recordedSteps, and naming droppedActions as
	// 0 beside it would erase a gap the drain had already counted.
	if recordedSteps >= 0 {
		args["recordedSteps"] = recordedSteps
	}
	if droppedActions >= 0 {
		args["droppedActions"] = droppedActions
	}
	if strings.TrimSpace(status) != "" {
		args["status"] = status
	}
	return s.executeMutation(appSessionWriteContext(ctx, ""), "recordAppSessionProgress", args)
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
		"transcriptFileId":    row.TranscriptFileId,
		"transcriptTruncated": row.TranscriptTruncated,
		"recordedSteps":       row.RecordedSteps,
		"droppedActions":      row.DroppedActions,
		"producedArtifactIds": stringsOrEmpty(row.ProducedArtifactIds),
		"appSessionRef":       row.AppSessionRef,
		"errorMessage":        row.ErrorMessage,
		"cancelReason":        row.CancelReason,
		"result":              resultArg(row.Result),
		"endedAt":             row.EndedAt.UTC().Format(time.RFC3339Nano),
	})
}

// ClaimRecordingSlot implements the shared allocator.
//
// ONE READ FOR ALL THREE ANSWERS. Both writers need the step position; the
// MCP node also needs the owner and the run, which it cannot know any other
// way -- its credential names a session and nothing else. A second reader for
// those would be a second read of the same row on the same call.
//
// Read-modify-write: the read borrows the owner (appSessionById is @actor and
// owner-tiered) and the write stamps internal origin. Two genuinely
// concurrent allocations can still take one value -- a tie in display order,
// never a lost row, because the step KEY is what the row id derives from and
// each writer's is distinct. What it removes is the SYSTEMATIC collision of
// two independent counters.
func (s *EngineStore) ClaimRecordingSlot(ctx context.Context, sessionId, ownerUserId string) (RecordingSlot, error) {
	if s == nil || s.Engine == nil {
		return RecordingSlot{}, fmt.Errorf("worker.store: engine not configured")
	}
	query, err := langparser.RenderCall("appSessionById", map[string]any{"sessionId": sessionId})
	if err != nil {
		return RecordingSlot{}, fmt.Errorf("worker.store: render appSessionById: %w", err)
	}
	nodes, err := s.executeAndExtract(appSessionReadContext(ctx, ownerUserId), "query "+query)
	if err != nil {
		return RecordingSlot{}, fmt.Errorf("worker.store: read the session's recording slot: %w", err)
	}
	var slot RecordingSlot
	var found bool
	for _, node := range nodes {
		if node == nil || node.Payload == nil {
			continue
		}
		payload := node.Payload.AsMap()
		slot.OwnerUserId, _ = payload["ownerUserId"].(string)
		slot.RunId, _ = payload["sessionRunId"].(string)
		slot.Seq = num.Float64Or(payloadNumber(payload, "recordedSteps"), 0)
		found = true
		break
	}
	if !found {
		return RecordingSlot{}, fmt.Errorf("worker.store: no session row is readable for %s", sessionId)
	}
	if err := s.RecordAppSessionProgress(ctx, sessionId, slot.Seq+1, -1, ""); err != nil {
		return slot, err
	}
	return slot, nil
}

// appSessionReadContext borrows the owner for a read and stamps NOTHING else.
//
// Deliberately not appSessionWriteContext: stamping internal origin on a read
// would widen it silently and move the decision away from the actor, which is
// the failure nobody notices (integrations/work's RULE 2, same reasoning).
func appSessionReadContext(ctx context.Context, ownerUserId string) context.Context {
	if strings.TrimSpace(ownerUserId) == "" {
		return ctx
	}
	return auth.ContextWithUserActor(ctx, ownerUserId)
}

// payloadNumber reads a numeric payload field as a float64, whatever concrete
// type the decode produced. An absent key is 0, which for the step allocator
// is the honest answer: a session with no steps recorded starts at position 0.
func payloadNumber(payload map[string]any, key string) float64 {
	switch v := payload[key].(type) {
	case float64:
		return v
	case int64:
		return float64(v)
	case int:
		return float64(v)
	}
	return 0
}

// resultArg renders the harness's structured answer for the mutation, or nil
// to OMIT the argument entirely.
//
// The omission is the mechanism, not a shortcut. `endAppSession` is a
// read-merge and its body writes `args.result` with no `?? {}` default, so a
// session end carrying nothing leaves whatever a `submit` already recorded --
// which is exactly the case D7 describes: the app volunteered its answer
// mid-run and the harness had none to add. Passing an empty object instead
// would erase the answer at the moment the run finished producing it.
func resultArg(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		// Not JSON. Carried as a wrapper rather than dropped: the harness
		// answered something, and losing it because it did not parse would
		// hide the one piece of evidence that says the schema was not met.
		return map[string]any{"raw": string(raw)}
	}
	if m, ok := decoded.(map[string]any); ok {
		return m
	}
	// A non-object answer (an array, a bare string). The concept field is an
	// object, so it is wrapped rather than refused -- the alternative is
	// discarding an answer the caller may still be able to read.
	return map[string]any{"value": decoded}
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
	query, err := langparser.RenderCall(name, args)
	if err != nil {
		return fmt.Errorf("worker.store: render %s: %w", name, err)
	}
	if _, err := s.Engine.Execute(ctx, query); err != nil {
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

// ArtifactProvenance is what an app session stamps onto everything it produced
// (epic memql#5391, design D9).
//
// Model and Effort are the APP'S REPORT and may be EMPTY, which is the point:
// a lifted construct's provenance says "recorded from claude-code, model X,
// effort Y, session Z", and a model copied from what was requested would put a
// measurement into that sentence that nobody made.
type ArtifactProvenance struct {
	App       string
	Model     string
	Effort    string
	SessionId string
}

// AsMap renders the stamp for the mutation. Every key is written, empty ones
// included: a reader of the object can then tell "the app reported no effort"
// from "this stamp predates the field", and only one of those is a fact about
// the run.
func (p ArtifactProvenance) AsMap() map[string]any {
	return map[string]any{
		"app":       p.App,
		"model":     p.Model,
		"effort":    p.Effort,
		"sessionId": p.SessionId,
	}
}

// Empty reports a stamp with nothing worth writing. A session with no app and
// no id is not a session anything was produced by.
func (p ArtifactProvenance) Empty() bool {
	return strings.TrimSpace(p.App) == "" && strings.TrimSpace(p.SessionId) == ""
}

// ArtifactProvenanceStamper records which app session produced an artifact.
//
// It is a SEPARATE interface from AppSessionStore, and separate for the reason
// AppSessionStore is separate from Store: a binary that runs no app sessions is
// not obliged to implement it, and a caller that holds no stamper simply does
// not stamp. EngineStore serves all three.
type ArtifactProvenanceStamper interface {
	// StampArtifactProvenance writes the stamp onto each artifact id, and onto
	// the backing Library file row when one resolves by the same id.
	StampArtifactProvenance(ctx context.Context, ownerUserId string, artifactIds []string, p ArtifactProvenance) error
}

// StampArtifactProvenance implements ArtifactProvenanceStamper.
//
// It writes the INDEX row and the backing FILE row, and it writes them
// independently: the Files list reads the index and the analysis and sync
// passes read the file, so one stamped and the other not would make "which of
// these did an app produce" answerable in one place and not the other -- which
// is worse than neither, because the reader cannot tell which answer they got.
//
// A file id that does not resolve is NOT an error. The cockpit pushes
// artifacts, and an artifact id is not always a file id; the write is attempted
// and its refusal is a debug fact rather than a failure of the run.
func (s *EngineStore) StampArtifactProvenance(ctx context.Context, ownerUserId string, artifactIds []string, p ArtifactProvenance) error {
	if s == nil || s.Engine == nil || len(artifactIds) == 0 || p.Empty() {
		return nil
	}
	ctx = appSessionWriteContext(ctx, ownerUserId)
	stamp := p.AsMap()
	var firstErr error
	for _, artifactId := range artifactIds {
		artifactId = strings.TrimSpace(artifactId)
		if artifactId == "" {
			continue
		}
		if err := s.executeMutation(ctx, "stampArtifactProvenance", map[string]any{
			"artifactId": artifactId,
			"producedBy": stamp,
		}); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
