package memql

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

// StructuredAIResult preserves the model attribution alongside structured output.
// Served distinguishes a fresh local/vendor call from a recorded journal answer.
type StructuredAIResult struct {
	Text       string
	Resolution airoute.Resolution
	Usage      common.ChatUsage
	Served     string
}

// CallAIStructured runs one structured call through routing and the work journal.
// Go callers declare a level; rules choose the provider on every invocation.
// There is no response cache before the journal: a work run must record its own
// call, and a replay must apply its journal policy before anything can answer.
//
// A person's override for the step this call serves (epic memql#5414, design
// D20) is applied HERE rather than by each caller, so a Go call site that
// builds its own request -- the file composer, a builtin's structured call --
// honours it exactly as a rendered prompt does: its level, model and effort on
// the request, its instructions and guidance after the caller's messages. The
// knobs are idempotent, so a request requestForPrompt already overrode is
// unchanged by the second pass; the messages are appended only here.
func (e *MemQLEngine) CallAIStructured(ctx context.Context, req airoute.ResolveRequest, messages []common.ChatMessage, schema common.StructuredSchema) (StructuredAIResult, error) {
	var out StructuredAIResult
	req, err := ApplyStepOverride(ctx, req)
	if err != nil {
		return out, err
	}
	messages = withStepOverrideMessages(ctx, messages)
	req.Modality = airoute.ModalityStructured
	req.Needs.Structured = true
	var input strings.Builder
	for _, message := range messages {
		input.WriteString(message.Content)
	}
	input.Write(schema.Schema)
	req.Needs.MinContextTokens = max(req.Needs.MinContextTokens, airoute.EstimateMinContextTokens(input.String(), 0))
	resolved, err := e.resolveAI(ctx, req)
	if err != nil {
		return out, err
	}
	provider, ok := resolved.Client.(common.ChatStructuredProvider)
	if !ok || provider == nil {
		return out, fmt.Errorf("the router resolved %q for a structured call and it does not serve one", resolved.Resolution.ProviderName)
	}
	out.Resolution = resolved.Resolution
	out.Served = "live"
	local := resolved.Resolution.Decision.Door == airoute.DoorLocal || strings.HasPrefix(resolved.Resolution.ProviderName, FleetReferencePrefix)
	if local {
		out.Served = "local"
	}
	journalReq := common.ModelRequest{Provider: resolved.Resolution.ProviderName, Model: resolved.Resolution.Model, Messages: messages, Schema: schema}
	if resolved.Entry != nil {
		journalReq.Settings = answerAffectingParams(resolved.Entry.Config.Params)
	}
	out.Text, err = e.modelSeam.serveText(ctx, journalReq, req.PromptName, func(ctx context.Context) (modelCallOutcome, error) {
		text, usage, callErr := callStructuredWithUsage(ctx, provider, messages, schema)
		out.Usage = usage
		if usage.Model != "" {
			out.Resolution.Model = usage.Model
		}
		return modelCallOutcome{Value: text, Usage: usage, Local: local}, callErr
	}, func(call JournaledCall) {
		out.Served = call.Served
		if call.Served == "journal" {
			out.Resolution.ProviderName, out.Resolution.Model = call.Provider, call.Model
			out.Usage = common.ChatUsage{InputTokens: int64(call.InputTokens), OutputTokens: int64(call.OutputTokens), Model: call.Model, Reported: call.InputTokens > 0 || call.OutputTokens > 0}
		}
	})
	return out, err
}
