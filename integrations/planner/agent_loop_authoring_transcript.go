package planner

// agent_loop_authoring_transcript.go -- DETERMINISTIC transcription capture
// (epic memql#1160, issue #1188).
//
// The original capture (agent_loop_authoring_capture.go) asked an LLM to
// RE-AUTHOR an automation from the task's goal (design -> emit -> Gate-1 ->
// repair). A live test proved that brittle: the model returned prose / `{ ... }`
// placeholders, over-decomposed, couldn't produce compiling MemQL, and burned
// ~$0.27/session producing ZERO bundles.
//
// This is the replacement the owner asked for: capture the LITERAL MemQL of
// what ACTUALLY ran. Every tool call an agent makes is already recorded, so on
// a completed RUN we just read those records and render them as a MemQL
// automation -- one statement per call. No LLM: it's transcription, not
// generation. Reliable, free, and it is genuinely "the MemQL that ran".
//
// # A tool call is written as the call its handler makes
//
// A tool is agent-only: no statement calls one. A tool whose handler is a
// function handler runs one construct call with the tool's arguments, so the
// statement is that call, `callN := <kind> <handler>(<args>)`. A query or
// webhook handler substitutes the arguments into MemQL text or a request, so
// the tool's arguments are no one call's; such a call is recorded as a
// comment line, and the transcript is not re-runnable.
//
// # What it reads changed with memql#5050
//
// It used to read v1:planner:task rows with category='toolInvocation', written
// by component/memql/taskstamp. Those are gone: a tool call is now a
// v1:work:observation with kind='tool_result', carrying the tool name and
// arguments in `data` and ordered by `data.seq`.
//
// That is not a rename, it is a change of SUBJECT. A task belonged to a Plan;
// an observation belongs to a RUN. So capture is triggered by a run reaching
// `succeeded` rather than a Plan, and the bundle records sourceRunId rather
// than sourcePlanId.
//
// The rendered automation is stored as the same v1:authoring:bundle +
// v1:authoring:construct the viewers already read, so those surfaces work
// unchanged. Status is recorded 'validated' with a transcript marker -- it is
// a faithful record, not a Gate-1-compiled artifact.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// toolCall is one recorded call: the tool name + its argument object as a JSON
// string (already valid MemQL object-literal syntax) + ordering key.
type toolCall struct {
	Name string
	Args string // raw JSON args object, rendered verbatim into the MemQL call
	Seq  int
}

// runCaptureTranscript records the literal MemQL of what a completed task ran.
// It loads the plan, reads its succeeded toolInvocation rows, renders them as a
// MemQL automation, and persists the bundle + construct (sourceRunId-linked).
// Owned writes run under the task owner's envelope. Best-effort: any failure
// logs and returns; it never affects the delivered result.
func (d *AuthoringCaptureDispatcher) runCaptureTranscript(ctx context.Context, runId, ownerUserId, goal string) error {
	ownerUserId = strings.TrimSpace(ownerUserId)
	goal = strings.TrimSpace(goal)
	if ownerUserId == "" {
		// A run with a present-and-empty owner is the DEPLOYMENT's own -- a
		// system run nobody asked for. Capturing it would write an authored
		// bundle owned by nobody, readable by nobody, and the surfaces would
		// show a capability with no author.
		d.logger.Info("authoring transcript: run has no owner; skipping", "runId", runId)
		return nil
	}

	if existing, err := d.existingBundleForRun(ctx, ownerUserId, runId); err != nil {
		d.logger.Warn("authoring transcript: idempotency lookup failed; proceeding", "runId", runId, "error", err)
	} else if existing != "" {
		d.logger.Info("authoring transcript: bundle already exists for run; skipping", "runId", runId, "bundleId", existing)
		return nil
	}

	calls, err := d.loadToolCalls(ctx, ownerUserId, runId)
	if err != nil {
		return fmt.Errorf("load tool calls: %w", err)
	}
	if len(calls) == 0 {
		// Nothing concrete ran (no recorded tool calls) -- nothing to transcribe.
		d.logger.Info("authoring transcript: no tool calls recorded for run; skipping", "runId", runId)
		return nil
	}

	autoName := transcriptAutomationName(runId)
	var callee toolCalleeFunc
	if speller, ok := d.engine.(ToolCallSpeller); ok {
		callee = speller.ToolCallee
	}
	source, everyCallWritten := renderTranscriptAutomation(autoName, goal, calls, callee)

	bundleId := id.NewShortId()
	title := goal
	if title == "" {
		title = "Transcript of a completed run"
	}
	if err := d.persistTranscriptBundle(ctx, ownerUserId, bundleId, runId, title, len(calls)); err != nil {
		return fmt.Errorf("persist transcript bundle: %w", err)
	}
	construct := memql.SandboxConstruct{Kind: "automation", Name: autoName, Source: source}
	if err := d.persistConstructs(ctx, ownerUserId, bundleId, []memql.SandboxConstruct{construct}); err != nil {
		return fmt.Errorf("persist transcript construct: %w", err)
	}

	// Gate 1 (re-runnability check, #1195): compile + bind the rendered
	// automation through the SAME sandbox the author path uses. A transcript is
	// a faithful RECORD regardless; this answers whether it is also genuinely
	// RE-RUNNABLE -- compiles + binds, and wrote every call as a statement --
	// versus a record whose literal calls (run-scoped ids, tool calls with no
	// statement form) don't form a runnable automation. Best-effort: a binary
	// without the authoring seams linked still stores the transcript as a
	// record, just without the Gate-1 verdict.
	var (
		report     memql.SandboxReport
		gate1Ran   bool
		reRunnable bool
	)
	if ae, ok := d.engine.(captureEngine); ok {
		report = ae.CompileBundle([]memql.SandboxConstruct{construct})
		gate1Ran = true
		reRunnable = report.OK && everyCallWritten
	}
	if err := d.recordTranscriptValidated(ctx, ownerUserId, bundleId, report, gate1Ran, reRunnable, len(calls)); err != nil {
		return fmt.Errorf("record transcript status: %w", err)
	}

	d.logger.Info("authoring transcript: captured", "runId", runId, "bundleId", bundleId,
		"calls", len(calls), "gate1Ran", gate1Ran, "reRunnable", reRunnable)
	return nil
}

// loadToolCalls reads the run's recorded tool calls, in the order they were
// made.
//
// The rows are v1:work:observation with kind='tool_result' (memql#5050); the
// tool name and its arguments ride in `data`, because an observation's own
// columns are deliberately generic. Read under the OWNER's actor through
// workObservationsForOwnerRun -- the cluster-owner variant of the same query
// exists for the sweeps, and using it here would let capture read a run that
// is not the caller's.
//
// Ordering is `data.seq`, not the row timestamp. Two calls in one turn can
// share a createdAt, and a transcript whose order is unstable reproduces a
// different automation every time it is captured.
func (d *AuthoringCaptureDispatcher) loadToolCalls(ctx context.Context, ownerUserId, runId string) ([]toolCall, error) {
	q := fmt.Sprintf(`query workObservationsForOwnerRun(runId:%s)`, langparser.QuoteString(runId))
	res, err := d.engine.Execute(ownerActorContext(ctx, ownerUserId), q)
	if err != nil {
		return nil, err
	}
	rows := memql.MaterializeRows(res)
	calls := make([]toolCall, 0, len(rows))
	for _, r := range rows {
		if getString(r, "kind") != "tool_result" {
			continue
		}
		data := mapField(r, "data")
		if data == nil {
			continue
		}
		// A FAILED call is not part of the reproducible path. The task rows
		// this replaced were filtered on status for the same reason; the
		// observation records it as data.isError.
		if isErr, ok := data["isError"].(bool); ok && isErr {
			continue
		}
		name := getString(data, "tool")
		if name == "" {
			continue
		}
		calls = append(calls, toolCall{
			Name: name,
			Args: observationArgsJSON(data),
			Seq:  intFromAny(data["seq"]),
		})
	}
	sort.SliceStable(calls, func(i, j int) bool { return calls[i].Seq < calls[j].Seq })
	return calls, nil
}

// observationArgsJSON returns the recorded argument object as the raw JSON the
// renderer writes verbatim into the MemQL call.
//
// A TRUNCATED argument set is dropped rather than rendered. data.args is cut
// at a byte bound when a call carried a large payload, and a cut JSON object
// does not parse -- rendering it would produce a bundle that cannot compile,
// which Gate 1 would then report as an authoring failure rather than as the
// missing evidence it actually is.
func observationArgsJSON(data map[string]any) string {
	if truncated, ok := data["argsTruncated"].(bool); ok && truncated {
		return "{}"
	}
	args, _ := data["args"].(string)
	if strings.TrimSpace(args) == "" {
		return "{}"
	}
	return args
}

// ToolCallSpeller is the engine seam that answers how a tool's call is written
// as a statement (MemQLEngine.ToolCallee). Optional: an engine without it
// writes every call as a comment line.
type ToolCallSpeller interface {
	ToolCallee(tool string) (kind, callee string, ok bool)
}

// toolCalleeFunc answers the kind and name of the construct call a tool's
// handler makes; ok is false when the tool has no such call.
type toolCalleeFunc func(tool string) (kind, callee string, ok bool)

// renderTranscriptAutomation builds the MemQL automation that reproduces the
// recorded calls: one statement per call, in order, each making the call the
// tool's handler made, with the exact args the tool ran with. Deterministic +
// verbatim -- this is what actually happened, expressed as MemQL. A call with
// no statement form (see the file comment) is a comment line naming the tool
// and its args; everyCallWritten reports whether there was none.
func renderTranscriptAutomation(name, goal string, calls []toolCall, callee toolCalleeFunc) (source string, everyCallWritten bool) {
	var b strings.Builder
	desc := goal
	if desc == "" {
		desc = fmt.Sprintf("Transcript of %d call(s) this run made.", len(calls))
	} else {
		desc = "Transcript of the calls this run made for: " + desc
	}
	fmt.Fprintf(&b, "@description(%q)\n", truncate(desc, 200))
	fmt.Fprintf(&b, "automation %s {\n", name)
	everyCallWritten = true
	for i, c := range calls {
		// Named args `name(k: v, ...)`. c.Args is the raw recorded JSON args
		// object; lower it to named args (nested values keep their JSON
		// braces). Unparseable / empty args render `name()`.
		args, written := transcriptArgs(c.Args)
		if written && callee != nil {
			if kind, target, ok := callee(c.Name); ok {
				fmt.Fprintf(&b, "  call%d := %s %s(%s)\n", i, kind, target, args)
				continue
			}
		}
		everyCallWritten = false
		if !written {
			// The recorded JSON, on one line: a comment ends at a newline.
			args = strings.Join(strings.Fields(c.Args), " ")
		}
		fmt.Fprintf(&b, "  // call%d: tool %s(%s) -- no statement writes this call\n", i, c.Name, args)
	}
	b.WriteString("}\n")
	return b.String(), everyCallWritten
}

// transcriptArgs writes a recorded JSON argument object as named arguments,
// `k: v, ...` in key order, each value a MemQL literal. ok is false when some
// part has no MemQL spelling: a key that is not a name (a map key is an
// unquoted name), or arguments that do not decode as an object. Empty or
// absent arguments write as nothing.
func transcriptArgs(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", true
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var args map[string]any
	if err := dec.Decode(&args); err != nil {
		return "", false
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v, ok := transcriptValue(args[k])
		if !ok || !transcriptName.MatchString(k) {
			return "", false
		}
		parts = append(parts, k+": "+v)
	}
	return strings.Join(parts, ", "), true
}

// transcriptValue writes one decoded JSON value as a MemQL literal.
func transcriptValue(v any) (string, bool) {
	switch x := v.(type) {
	case nil:
		return "nil", true
	case bool:
		return fmt.Sprintf("%t", x), true
	case string:
		return langparser.QuoteString(x), true
	case json.Number:
		return x.String(), true
	case []any:
		items := make([]string, 0, len(x))
		for _, item := range x {
			s, ok := transcriptValue(item)
			if !ok {
				return "", false
			}
			items = append(items, s)
		}
		return "[" + strings.Join(items, ", ") + "]", true
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		entries := make([]string, 0, len(keys))
		for _, k := range keys {
			s, ok := transcriptValue(x[k])
			if !ok || !transcriptName.MatchString(k) {
				return "", false
			}
			entries = append(entries, k+": "+s)
		}
		return "{" + strings.Join(entries, ", ") + "}", true
	}
	return "", false
}

// transcriptName is the shape of an argument name or a map key.
var transcriptName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// --- persistence (transcript-specific) ------------------------------------

func (d *AuthoringCaptureDispatcher) persistTranscriptBundle(ctx context.Context, ownerUserId, bundleId, runId, title string, callCount int) error {
	args := map[string]any{
		"bundleId":    bundleId,
		"title":       truncate(title, 120),
		"summary":     fmt.Sprintf("Verbatim transcript of the %d call(s) this run made.", callCount),
		"sourceRunId": runId,
	}
	q := fmt.Sprintf(`createAuthoringBundle(%s)`, encodeArgs(args))
	_, err := d.engine.Execute(ownerActorContext(ctx, ownerUserId), q)
	return err
}

// recordTranscriptValidated records the bundle's validation result. The
// transcript is always stored as a 'validated' RECORD -- a clean, non-draft
// status the viewers render well -- carrying a transcript marker. When the
// Gate-1 compiler is linked (gate1Ran), the report also carries the REAL
// compile/bind verdict plus a reRunnable flag (#1195): true when the rendered
// automation actually compiles + binds, false when it is a record whose literal
// calls don't form a runnable automation (diagnostics say why). The bundle is
// never auto-activated into the authored-construct runtime -- that stays a
// separate Gate-3 approval step -- so 'validated' here is a record status, not
// a live registration.
func (d *AuthoringCaptureDispatcher) recordTranscriptValidated(ctx context.Context, ownerUserId, bundleId string, report memql.SandboxReport, gate1Ran, reRunnable bool, callCount int) error {
	vr := map[string]any{
		"transcript": true,
		"callCount":  callCount,
		"reRunnable": reRunnable,
	}
	if gate1Ran {
		vr["ok"] = report.OK
		vr["gate1"] = "ran"
		if obj := structToObject(report); obj["diagnostics"] != nil {
			vr["diagnostics"] = obj["diagnostics"]
		}
	} else {
		// No Gate-1 seam in this binary: record the transcript without a
		// re-runnability verdict rather than asserting a compile that never ran.
		vr["ok"] = true
		vr["gate1"] = "unavailable"
	}
	args := map[string]any{
		"bundleId":         bundleId,
		"status":           "validated",
		"validationReport": vr,
	}
	q := fmt.Sprintf(`recordBundleValidation(%s)`, encodeArgs(args))
	_, err := d.engine.Execute(ownerActorContext(ctx, ownerUserId), q)
	return err
}

// --- helpers --------------------------------------------------------------

// transcriptAutomationName derives a valid camelCase automation name from the
// run id suffix so the construct is uniquely + deterministically named.
func transcriptAutomationName(runId string) string {
	short := runId
	if i := strings.LastIndex(runId, ":"); i >= 0 {
		short = runId[i+1:]
	}
	var b strings.Builder
	b.WriteString("reproduce")
	up := true
	for _, r := range short {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			if up {
				b.WriteRune(upper(r))
				up = false
			} else {
				b.WriteRune(r)
			}
		default:
			up = true // collapse separators into camelCase humps
		}
	}
	return b.String()
}

func upper(r rune) rune {
	if r >= 'a' && r <= 'z' {
		return r - 32
	}
	return r
}
