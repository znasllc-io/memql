// responsibility_intake.go
//
// Responsibility intake dispatcher (issue #637 -- epic #631 / program
// #629).
//
// When a user authors a v1:planner:responsibility, createResponsibility
// lands it in status='draft' with intakeStatus=''. This dispatcher runs a
// LIGHTWEIGHT reasoning pass over the freeform statement -- the
// responsibilityIntake prompt -- to (a) infer the structured field set the
// responsibility needs (trigger / schedule / condition / targetKind /
// roleSlug / successCriteria / notifyHow) and (b) surface ONLY the
// genuinely-ambiguous clarifying questions (0-2, hard cap 2).
//
// Flow on a freshly created draft:
//
//   1. Claim the row (CAS guard so a created+updated double-fire, or a
//      multi-node race, runs the intake prompt once). intakeStatus '' ->
//      'pending' (markResponsibilityIntakePending).
//   2. Invoke ai("responsibilityIntake", {statement, roleCatalog, now}).
//   3. Parse the JSON result.
//   4a. ZERO questions -> apply the inferred fields with status='active' +
//       intakeStatus='clear'. The responsibility goes straight live
//       (applyResponsibilityIntake).
//   4b. 1-2 questions -> apply the inferred fields with status='draft' +
//       intakeStatus='awaitingAnswers' + intakeRequest carrying the
//       questions. The row parks for the user; the intakeRequest shape
//       mirrors Plan.feedbackRequest so the same awaitingFeedback-style
//       surfacing renders it.
//
// Folding answers back: once the user answers the parked questions
// (the CoPresent intake card calls foldResponsibilityIntakeAnswers
// directly, OR a follow-up updated-event arrives with intakeResponse set
// and intakeStatus still 'awaitingAnswers'), the dispatcher re-runs the
// prompt with the answers folded in and writes the final field set + flips
// the row to active.
//
// The split mirrors the trainSpecialist dispatcher in this package and the
// harness consolidation handler: the LLM call + JSON parse + branch live in
// Go because the MemQL DSL can't express them; the mutations it calls are
// plain DSL writes. The dispatcher runs under the system:planner actor, so
// the intake mutations do NOT re-stamp ownerUserId (the row stays owned by
// the human author).

package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
)

// maxIntakeQuestions is the hard cap on clarifying questions, enforced
// Go-side regardless of what the model returns. The prompt asks for 0-2;
// this guarantees the surfaced set never exceeds 2 even on a misbehaving
// model.
const maxIntakeQuestions = 2

// ResponsibilityIntakeDispatcher claims freshly-created responsibilities
// and runs the responsibilityIntake reasoning pass against them. It
// subscribes to created + updated topics for v1:planner:responsibility; the
// created path runs first-pass intake, the updated path picks up an
// answers-folded row to finalize.
type ResponsibilityIntakeDispatcher struct {
	engine Engine
	logger *slog.Logger

	// claimed guards against double-dispatch of the FIRST-pass intake: a
	// created+updated double-fire for the same row, or a multi-node race,
	// must run the prompt once. Best-effort in-process set; cross-node
	// dedup relies on the intakeStatus='pending' transition being observed
	// before a second node claims.
	mu      sync.Mutex
	claimed map[string]struct{}
	// dispatched is the PERMANENT (never-released) set of responsibility ids
	// this process has already run first-pass intake for. Distinct from
	// `claimed`, which releases after the run so the row can be re-evaluated.
	//
	// memQL is append-only: every successful update() (including the
	// routing-only assignResponsibility) materialises as a new row
	// that re-fires graph.node.created. Without a permanent guard, assigning
	// an agent to a still-draft responsibility (intakeStatus=='') re-enters
	// first-pass intake, which re-infers trigger/schedule from the bare
	// statement and CLOBBERS the user's explicit cadence (recurring->standing,
	// schedule blanked) -- memql#1645. First-pass intake is a once-per-row
	// authoring step; this set enforces that within the process even when a
	// prior run failed (leaving intakeStatus=='') or the claim was released.
	// Cross-replica re-fire is still gated durably by the intakeStatus!='' read.
	dispatched map[string]struct{}
}

// NewResponsibilityIntakeDispatcher constructs a dispatcher pinned to the
// planner integration's engine adapter + logger.
func NewResponsibilityIntakeDispatcher(engine Engine, logger *slog.Logger) *ResponsibilityIntakeDispatcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &ResponsibilityIntakeDispatcher{
		engine:     engine,
		logger:     logger,
		claimed:    make(map[string]struct{}),
		dispatched: make(map[string]struct{}),
	}
}

// HandleResponsibilityCreated is the event-bus subscriber for
// graph.node.created.v1:planner:responsibility. Runs first-pass intake on a
// freshly-authored draft.
func (d *ResponsibilityIntakeDispatcher) HandleResponsibilityCreated(ev events.Event) {
	id, status, intakeStatus, intakeResponse, ok := extractResponsibilityIntakeFields(ev)
	if !ok {
		return
	}
	// First-pass intake only fires on a fresh draft that hasn't been
	// through intake yet.
	if status != "draft" || intakeStatus != "" {
		return
	}
	d.dispatchFirstPass(id)
	_ = intakeResponse
}

// HandleResponsibilityUpdated is the event-bus subscriber for
// graph.node.updated.v1:planner:responsibility. It handles exactly one case:
//   - a row parked in intakeStatus='awaitingAnswers' that now carries an
//     intakeResponse (the user answered) -> fold the answers back.
//
// First-pass intake is NOT (re-)triggered from the updated path. It belongs
// to the create path (HandleResponsibilityCreated): createResponsibility
// always inserts in status='draft' intakeStatus=”, so the created event fully
// covers genuine first authoring. Re-triggering first-pass on an arbitrary
// update is the memql#1645 footgun -- a routing-only assignResponsibility
// materialises a fresh row version and would otherwise re-run the inference
// cascade, clobbering the user's explicit trigger/schedule.
func (d *ResponsibilityIntakeDispatcher) HandleResponsibilityUpdated(ev events.Event) {
	id, _, intakeStatus, hasResponse, ok := extractResponsibilityIntakeFields(ev)
	if !ok {
		return
	}
	if intakeStatus == "awaitingAnswers" && hasResponse {
		// The user answered the parked questions; finalize in a detached
		// goroutine (it makes a second LLM call).
		go d.runFoldAnswers(context.Background(), id)
	}
}

// dispatchFirstPass claims the row and runs first-pass intake in a detached
// goroutine (the bus dispatches subscribers synchronously and the intake
// prompt is a multi-second LLM call).
func (d *ResponsibilityIntakeDispatcher) dispatchFirstPass(id string) {
	// Permanent once-per-row guard (memql#1645): first-pass intake is an
	// authoring step that must run at most once per responsibility. An
	// update() such as the routing-only assignResponsibility re-fires
	// graph.node.created (append-only materialisation), which would otherwise
	// re-enter first-pass on a still-draft row and clobber the user's explicit
	// trigger/schedule. Skip if we've already dispatched first-pass for this id.
	d.mu.Lock()
	if _, done := d.dispatched[id]; done {
		d.mu.Unlock()
		return
	}
	d.dispatched[id] = struct{}{}
	d.mu.Unlock()

	if !d.claim(id) {
		return
	}
	d.logger.Info("responsibility intake: claimed", "responsibilityId", id)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				d.logger.Error("responsibility intake: PANIC",
					"responsibilityId", id, "recover", fmt.Sprintf("%v", r))
			}
			d.release(id)
		}()
		if err := d.runFirstPass(context.Background(), id); err != nil {
			d.logger.Warn("responsibility intake: first pass failed",
				"responsibilityId", id, "error", err)
		}
	}()
}

func (d *ResponsibilityIntakeDispatcher) claim(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.claimed[id]; ok {
		return false
	}
	d.claimed[id] = struct{}{}
	return true
}

func (d *ResponsibilityIntakeDispatcher) release(id string) {
	d.mu.Lock()
	delete(d.claimed, id)
	d.mu.Unlock()
}

// runFirstPass loads the row, marks intake pending, invokes the prompt, and
// applies the result -- either flipping straight to active (no questions)
// or parking with the clarifying questions on intakeRequest.
func (d *ResponsibilityIntakeDispatcher) runFirstPass(ctx context.Context, id string) error {
	row, err := d.loadResponsibility(ctx, id)
	if err != nil {
		return fmt.Errorf("load responsibility: %w", err)
	}
	statement := getString(row, "statement")
	if statement == "" {
		return fmt.Errorf("responsibility %s has an empty statement", id)
	}

	// Mark pending (best-effort -- a failed transition doesn't stop the
	// work; it just leaves a slightly stale intakeStatus a moment longer).
	if err := d.markPending(ctx, id); err != nil {
		d.logger.Warn("responsibility intake: markPending failed; continuing",
			"responsibilityId", id, "error", err)
	}

	result, err := d.invokeIntake(ctx, statement, nil)
	if err != nil {
		return fmt.Errorf("responsibilityIntake prompt: %w", err)
	}

	questions := result.cappedQuestions()
	if len(questions) == 0 {
		// Clear: apply the inferred fields and flip the row live.
		d.logger.Info("responsibility intake: clear -- activating",
			"responsibilityId", id, "trigger", result.Trigger,
			"targetKind", result.TargetKind)
		return d.applyIntake(ctx, id, result, "active", "clear", nil)
	}

	// Ambiguous: park with the clarifying questions on intakeRequest.
	d.logger.Info("responsibility intake: awaiting answers",
		"responsibilityId", id, "questionCount", len(questions))
	intakeRequest := map[string]any{
		"questions": questions,
		"askedAt":   time.Now().UTC().Format(time.RFC3339),
	}
	return d.applyIntake(ctx, id, result, "draft", "awaitingAnswers", intakeRequest)
}

// runFoldAnswers re-runs the prompt with the user's answers folded in and
// writes the final field set, flipping the row to active.
func (d *ResponsibilityIntakeDispatcher) runFoldAnswers(ctx context.Context, id string) error {
	if !d.claim(id) {
		return nil
	}
	defer d.release(id)

	row, err := d.loadResponsibility(ctx, id)
	if err != nil {
		return fmt.Errorf("load responsibility: %w", err)
	}
	// Re-read intakeStatus from the freshly-loaded row: a racing fold may
	// have already applied it.
	if getString(row, "intakeStatus") != "awaitingAnswers" {
		return nil
	}
	statement := getString(row, "statement")
	answers := buildAnswerList(row)
	if len(answers) == 0 {
		// No usable answers on the response object -- nothing to fold;
		// leave the row parked for the user to answer properly.
		d.logger.Warn("responsibility intake: fold requested but no answers present",
			"responsibilityId", id)
		return nil
	}

	result, err := d.invokeIntake(ctx, statement, answers)
	if err != nil {
		return fmt.Errorf("responsibilityIntake (fold) prompt: %w", err)
	}

	intakeResponse := mapField(row, "intakeResponse")
	if intakeResponse == nil {
		intakeResponse = map[string]any{"answers": answers}
	}
	d.logger.Info("responsibility intake: answers folded -- activating",
		"responsibilityId", id, "trigger", result.Trigger)
	return d.foldAnswers(ctx, id, result, intakeResponse)
}

// invokeIntake renders + runs the responsibilityIntake prompt and parses
// the structured-output result. On the fold pass, answers is non-nil.
func (d *ResponsibilityIntakeDispatcher) invokeIntake(ctx context.Context, statement string, answers []map[string]any) (intakeResult, error) {
	data := map[string]any{
		"statement": statement,
		"now":       time.Now().UTC().Format(time.RFC3339),
	}
	if len(answers) > 0 {
		data["answers"] = answers
	}
	resp, err := d.engine.InvokeAI(systemActorContext(ctx), "responsibilityIntake", data)
	if err != nil {
		return intakeResult{}, err
	}
	return parseIntakeResult(resp)
}

// --- loaders --------------------------------------------------------------

func (d *ResponsibilityIntakeDispatcher) loadResponsibility(ctx context.Context, id string) (map[string]any, error) {
	q := fmt.Sprintf(`query responsibilityById(responsibilityId:%q)`, id)
	res, err := d.engine.Execute(systemActorContext(ctx), q)
	if err != nil {
		return nil, err
	}
	rows := memql.MaterializeRows(res)
	if len(rows) == 0 {
		return nil, fmt.Errorf("responsibility %s not found", id)
	}
	return rows[0], nil
}

// --- mutations ------------------------------------------------------------

func (d *ResponsibilityIntakeDispatcher) markPending(ctx context.Context, id string) error {
	q := fmt.Sprintf(`mutation markResponsibilityIntakePending(responsibilityId:%q)`, id)
	_, err := d.engine.Execute(systemActorContext(ctx), q)
	return err
}

// applyIntake writes the inferred field set + intake outcome. status is
// "active" (clear) or "draft" (awaitingAnswers); intakeStatus is the
// matching marker; intakeRequest carries the questions when parking (nil
// when clear).
func (d *ResponsibilityIntakeDispatcher) applyIntake(ctx context.Context, id string, r intakeResult, status, intakeStatus string, intakeRequest map[string]any) error {
	args := map[string]any{
		"responsibilityId": id,
		"trigger":          r.Trigger,
		"schedule":         r.Schedule,
		"targetKind":       r.TargetKind,
		"assignedRoleSlug": r.RoleSlug,
		"successCriteria":  r.SuccessCriteria,
		"notifyHow":        r.NotifyHow,
		"status":           status,
		"intakeStatus":     intakeStatus,
	}
	if r.Condition != nil {
		args["condition"] = r.Condition
	}
	if intakeRequest != nil {
		args["intakeRequest"] = intakeRequest
	}
	q := fmt.Sprintf(`applyResponsibilityIntake(%s)`, encodeArgs(args))
	_, err := d.engine.Execute(systemActorContext(ctx), q)
	return err
}

// foldAnswers writes the re-inferred final field set + intakeResponse and
// flips the row to active (the mutation hard-codes status='active' +
// intakeStatus='applied').
func (d *ResponsibilityIntakeDispatcher) foldAnswers(ctx context.Context, id string, r intakeResult, intakeResponse map[string]any) error {
	args := map[string]any{
		"responsibilityId": id,
		"intakeResponse":   intakeResponse,
		"trigger":          r.Trigger,
		"schedule":         r.Schedule,
		"targetKind":       r.TargetKind,
		"assignedRoleSlug": r.RoleSlug,
		"successCriteria":  r.SuccessCriteria,
		"notifyHow":        r.NotifyHow,
	}
	if r.Condition != nil {
		args["condition"] = r.Condition
	}
	q := fmt.Sprintf(`foldResponsibilityIntakeAnswers(%s)`, encodeArgs(args))
	_, err := d.engine.Execute(systemActorContext(ctx), q)
	return err
}

// --- result type + parsing ------------------------------------------------

// intakeQuestion is one clarifying question the prompt emitted.
type intakeQuestion struct {
	ID       string           `json:"id"`
	Question string           `json:"question"`
	Kind     string           `json:"kind"`
	Options  []map[string]any `json:"options,omitempty"`
}

// intakeResult is the parsed structured output of the responsibilityIntake
// prompt.
type intakeResult struct {
	Trigger         string           `json:"trigger"`
	Schedule        string           `json:"schedule"`
	Condition       map[string]any   `json:"condition"`
	TargetKind      string           `json:"targetKind"`
	RoleSlug        string           `json:"roleSlug"`
	SuccessCriteria string           `json:"successCriteria"`
	NotifyHow       string           `json:"notifyHow"`
	Questions       []intakeQuestion `json:"questions"`
}

// cappedQuestions returns the clarifying questions as plain maps (for the
// intakeRequest object), hard-capped at maxIntakeQuestions and dropping any
// blank-text entries the model may have emitted.
func (r intakeResult) cappedQuestions() []map[string]any {
	out := make([]map[string]any, 0, len(r.Questions))
	for i, q := range r.Questions {
		if q.Question == "" {
			continue
		}
		id := q.ID
		if id == "" {
			id = fmt.Sprintf("q%d", i+1)
		}
		kind := q.Kind
		if kind != "choice" {
			kind = "text"
		}
		entry := map[string]any{
			"id":       id,
			"question": q.Question,
			"kind":     kind,
		}
		if kind == "choice" && len(q.Options) > 0 {
			entry["options"] = q.Options
		}
		out = append(out, entry)
		if len(out) >= maxIntakeQuestions {
			break
		}
	}
	return out
}

// parseIntakeResult unmarshals the prompt's structured output, tolerating
// both a string (raw model text) and a map (schema-enforced) return shape,
// and stripping a stray markdown fence. Mirrors parsePlannerDecision.
func parseIntakeResult(resp any) (intakeResult, error) {
	if resp == nil {
		return intakeResult{}, fmt.Errorf("responsibilityIntake returned nil")
	}
	var raw []byte
	switch r := resp.(type) {
	case string:
		raw = []byte(r)
	default:
		b, err := json.Marshal(resp)
		if err != nil {
			return intakeResult{}, err
		}
		raw = b
	}
	raw = stripJSONFence(raw)
	var res intakeResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return intakeResult{}, fmt.Errorf("parse intake JSON: %w (raw=%s)", err, truncate(string(raw), 200))
	}
	res.normalize()
	return res, nil
}

// normalize defaults the inferred discriminators to safe values so a
// malformed model result still produces a valid responsibility (the row
// stays usable rather than failing the enum-validated mutation).
func (r *intakeResult) normalize() {
	switch r.Trigger {
	case "reactive", "standing", "recurring":
	default:
		r.Trigger = "standing"
	}
	switch r.TargetKind {
	case "assistant", "specialist", "unassigned":
	default:
		r.TargetKind = "assistant"
	}
	// schedule only meaningful for recurring; condition only for reactive.
	if r.Trigger != "recurring" {
		r.Schedule = ""
	}
	if r.Trigger != "reactive" {
		r.Condition = nil
	}
	if r.TargetKind != "specialist" {
		r.RoleSlug = ""
	}
}

// --- helpers --------------------------------------------------------------

// extractResponsibilityIntakeFields pulls the id + the intake-relevant
// payload fields off a graph event. hasResponse is true when the row
// carries a non-empty intakeResponse object.
func extractResponsibilityIntakeFields(ev events.Event) (id, status, intakeStatus string, hasResponse, ok bool) {
	if ev.Payload == nil {
		return "", "", "", false, false
	}
	id = getString(ev.Payload, "id")
	if id == "" {
		return "", "", "", false, false
	}
	payload, _ := ev.Payload["payload"].(map[string]any)
	if payload == nil {
		return id, "", "", false, true
	}
	status = getString(payload, "status")
	intakeStatus = getString(payload, "intakeStatus")
	if resp, ok := payload["intakeResponse"].(map[string]any); ok && len(resp) > 0 {
		hasResponse = true
	}
	return id, status, intakeStatus, hasResponse, true
}

// buildAnswerList projects the row's intakeRequest questions + the user's
// intakeResponse answers into the [{id, question, answer}] list the prompt's
// `answers` input expects. Pairs answers to questions by id; an answer
// without a matching question still passes through (the model tolerates a
// bare {id, answer}).
func buildAnswerList(row map[string]any) []map[string]any {
	resp := mapField(row, "intakeResponse")
	if resp == nil {
		return nil
	}
	rawAnswers, _ := resp["answers"].([]any)
	if len(rawAnswers) == 0 {
		return nil
	}
	// Index question text by id for enrichment.
	questionText := map[string]string{}
	if req := mapField(row, "intakeRequest"); req != nil {
		if qs, ok := req["questions"].([]any); ok {
			for _, q := range qs {
				qm, ok := q.(map[string]any)
				if !ok {
					continue
				}
				questionText[getString(qm, "id")] = getString(qm, "question")
			}
		}
	}
	out := make([]map[string]any, 0, len(rawAnswers))
	for _, a := range rawAnswers {
		am, ok := a.(map[string]any)
		if !ok {
			continue
		}
		id := getString(am, "id")
		entry := map[string]any{
			"id":     id,
			"answer": getString(am, "answer"),
		}
		if qt := questionText[id]; qt != "" {
			entry["question"] = qt
		}
		out = append(out, entry)
	}
	return out
}

// encodeArgs renders a MemQL named-argument object literal from a Go map.
// encodeArgs renders a mutation/query argument map as the named-args
// invocation body `k: v, ...` (Story 9 / #2335: NOT the legacy object-literal
// wrapper `{...}` -- the parser rejects it). It is always used as the FULL
// argument list of a `name(<encodeArgs(args)>)` call, so the braces are
// dropped; nested object VALUES keep their JSON braces via json.Marshal. Keys
// sorted for a deterministic call string. Empty map renders "" -> `name()`.
func encodeArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	first := true
	for _, k := range keys {
		v, err := json.Marshal(args[k])
		if err != nil {
			continue
		}
		if !first {
			b.WriteString(", ")
		}
		first = false
		b.WriteString(k)
		b.WriteString(": ")
		b.Write(v)
	}
	return b.String()
}
