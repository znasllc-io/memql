//go:build agent

package worker

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/planner"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/core/costestimate"
	"github.com/znasllc-io/memql/core/id"
)

// cockpitapp_ledger.go writes the v1:router:call row for a delegated
// app session (memql#4362). The delegation probe used to live here and
// now sits in component/worker, untagged: this file is //go:build agent,
// and a probe the PLANNER node cannot compile answers "no machine" on
// every planner in a real cluster.
//
// Why an app session writes a LEDGER row at all: the ledger is where
// "what did this cost" is answered, and a delegated run that skipped
// it would make a plan's spend look smaller than the work it did --
// the exact opposite of what a cost surface is for. The row carries
// billing=subscription and executionSurface=cockpit-app:<appId>, so a
// reader can separate what MemQL paid for from what ran somewhere it
// does not pay, without joining to the session row.

// LedgerWriter records an app session's reported usage on the AI
// ledger.
type LedgerWriter struct {
	Engine *memqlengine.MemQLEngine
}

// RecordAppSession writes one v1:router:call row for a finished
// session.
//
// The app's cost report is preserved separately from the official API-equivalent
// estimate. Neither establishes actual subscription billing. Aggregate usage has
// no per-request cache/context split; the calculator retains that uncertainty.
func (w *LedgerWriter) RecordAppSession(ctx context.Context, result workerservice.RunResult, spec workerservice.RunSpec) error {
	if w == nil || w.Engine == nil {
		return nil
	}
	args := map[string]any{
		"callId":            id.NewShortId(),
		"servedModel":       result.Model,
		"servedEffort":      result.Effort,
		"apiEquivalent":     costestimate.EstimateTokens(appVendor(spec.App), result.Model, costestimate.Usage{Input: result.Usage.InputTokens, Output: result.Usage.OutputTokens, Known: result.Usage.Known, Aggregate: true}, time.Now()),
		"actualBilling":     map[string]any{"status": "unavailable", "reason": "App-reported cost is not a billing receipt."},
		"requestId":         result.SessionId,
		"agentId":           "",
		"userId":            spec.OwnerUserId,
		"promptName":        "",
		"policyName":        "",
		"vendor":            appVendor(spec.App),
		"model":             spec.App,
		"providerName":      "cockpit-app",
		"inputTokens":       int(result.Usage.InputTokens),
		"outputTokens":      int(result.Usage.OutputTokens),
		"cachedInputTokens": 0,
		// The app reported these; MemQL did not estimate them. Saying
		// otherwise would put a heuristic-quality number in a column
		// readers treat as measured.
		"tokensEstimated":    false,
		"inputCost":          0.0,
		"outputCost":         0.0,
		"cachedInputCost":    0.0,
		"totalCost":          result.Usage.CostUSD,
		"pricingConfigured":  false,
		"timeToFirstTokenMs": 0,
		"totalDurationMs":    1,
		"tokensPerSec":       0.0,
		"streaming":          true,
		"outcome":            ledgerOutcome(result.Status),
		"errorCategory":      "",
		"errorMessage":       truncateForLedger(result.ErrorMessage),
		"fallbackFromModel":  "",
		// NO DECISION FIELDS, and their absence is the record rather than an
		// omission (epic memql#5127, D10). A decision says which rule matched
		// and which policy chain picked which door -- and this row is a
		// session an APP ran on the user's own machine, where the app chose
		// its own model and MemQL's router was never consulted. Writing a
		// level or a rule here would attribute a decision nobody made. The
		// row still says what served, through providerName, model and
		// executionSurface.
		"billing":          result.Billing,
		"executionSurface": planner.BackendCockpitApp + ":" + spec.App,
	}
	call, err := langparser.RenderCall("recordRouterCall", args)
	if err != nil {
		return fmt.Errorf("cockpit-app ledger: render recordRouterCall: %w", err)
	}
	// The ledger insert needs an actor. Borrow the OWNER's, the same
	// way the campaign sender does, rather than stamping a system
	// principal: the row is about their spend, so it should be
	// attributable to them.
	writeCtx, cancel := context.WithTimeout(
		auth.ContextWithUserActor(context.WithoutCancel(ctx), spec.OwnerUserId), 10*time.Second)
	defer cancel()
	if _, err := w.Engine.Execute(writeCtx, call); err != nil {
		return fmt.Errorf("cockpit-app ledger: record call: %w", err)
	}
	return nil
}

// appVendor names the vendor behind an app id. Recorded so a cost
// reader can group by vendor across metered and subscription spend.
func appVendor(appId string) string {
	switch appId {
	case workerservice.AppIdClaudeCode:
		return "anthropic"
	case workerservice.AppIdCodex:
		return "openai"
	}
	return "unknown"
}

func ledgerOutcome(status string) string {
	switch status {
	case workerservice.AppSessionStatusEnded:
		return "ok"
	case workerservice.AppSessionStatusCancelled:
		return "cancelled"
	}
	return "error"
}

func truncateForLedger(msg string) string {
	msg = strings.TrimSpace(msg)
	if len(msg) > 500 {
		return msg[:500]
	}
	return msg
}
