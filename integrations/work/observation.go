package work

// observation.go -- recording what happened INSIDE a step (memql#5050).
//
// `v1:work:observation` has existed since epic A2 with `tool_result` leading
// its kind enum, and nothing wrote one: `ToolLoopObservationSink` in
// component/memql/inner_loop.go is an interface with no production
// implementation, so `recordObservation` has always no-opped. This is the
// implementation, and its first caller is the agent's tool loop, whose calls
// used to land as `v1:planner:task` rows with `category='toolInvocation'`.
//
// It lives here for the reason every other write in this package does: the
// mutation is @serverOnly, and `auth.OriginFromContext` answers OriginClient
// for any context nobody stamped -- so an unstamped write is REFUSED with one
// warning nothing above it hears. The stamp is allowlisted per PACKAGE, and
// integrations/agent is not on that list.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	agentintegration "github.com/znasllc-io/memql/integrations/agent"
)

// observationKindToolResult is the enum value a recorded tool call takes.
const observationKindToolResult = "tool_result"

// maxObservationArgsBytes bounds what a single call's arguments may add to the
// graph.
//
// A tool's arguments are model output: a `writeFile` call can carry an entire
// document, and there is one of these rows per tool call per run. Truncation
// is recorded IN THE ROW (`data.argsTruncated`) rather than done silently,
// because the transcript reader reproduces calls from this and a silently
// shortened argument reproduces a DIFFERENT call.
const maxObservationArgsBytes = 8 << 10

// RecordToolInvocation writes one tool_result observation.
//
// Satisfies agent.ToolInvocationRecorder. The interface is declared on the
// agent side and implemented here, so the dependency points work -> agent and
// there is no cycle.
func (i *Integration) RecordToolInvocation(ctx context.Context, rec agentintegration.ToolInvocation) error {
	runId := strings.TrimSpace(rec.RunId)
	if runId == "" {
		// Refused rather than written under a blank run. An observation whose
		// runId is empty belongs to no run, is readable through no query
		// (every observation read is scoped by run), and is invisible to the
		// retention sweep that folds a run's detail before deleting it -- so
		// it would accumulate forever, unreachable.
		return fmt.Errorf("work: a tool invocation with no run id cannot be recorded")
	}

	data := map[string]any{
		"tool":    rec.ToolName,
		"seq":     rec.Seq,
		"isError": rec.IsError,
	}
	if args, truncated := encodeObservationArgs(rec.Args); args != "" {
		data["args"] = args
		if truncated {
			data["argsTruncated"] = true
		}
	}
	if rec.IsError && rec.Error != "" {
		data["error"] = rec.Error
	}

	return i.store().writeInternal(
		ownerActor(ctx, rec.OwnerUserId),
		"mutation "+call("createWorkObservation", map[string]any{
			"observationId": newRowId("v1:work:observation"),
			"runId":         runId,
			"stepKey":       rec.StepKey,
			"kind":          observationKindToolResult,
			"content":       observationContent(rec),
			"data":          data,
		}),
	)
}

// observationContent renders the call as the text the row is embedded from.
//
// `content` is "the embedding source, so observations are recall-able" -- so
// this is deliberately a sentence about what happened rather than a JSON dump:
// an embedding of `{"tool":"workbenchHost","action":"exec"}` retrieves on
// punctuation, and the thing worth recalling is that a tool ran and whether it
// worked.
func observationContent(rec agentintegration.ToolInvocation) string {
	var b strings.Builder
	b.WriteString("Called tool ")
	b.WriteString(rec.ToolName)
	if rec.IsError {
		b.WriteString(" and it failed")
		if rec.Error != "" {
			b.WriteString(": ")
			b.WriteString(truncateForContent(rec.Error))
		}
	} else {
		b.WriteString(" successfully")
	}
	b.WriteString(".")
	return b.String()
}

// encodeObservationArgs renders the call's arguments and reports whether it
// had to cut them.
func encodeObservationArgs(args map[string]any) (string, bool) {
	if len(args) == 0 {
		return "", false
	}
	raw, err := json.Marshal(args)
	if err != nil {
		// Unencodable arguments are not worth failing a tool call over, and
		// saying so is better than an empty field that reads as "no
		// arguments".
		return "<arguments could not be encoded>", true
	}
	if len(raw) <= maxObservationArgsBytes {
		return string(raw), false
	}
	return string(raw[:maxObservationArgsBytes]), true
}

// truncateForContent bounds the error text that reaches the embedding source.
func truncateForContent(s string) string {
	const max = 300
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
