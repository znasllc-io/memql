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
// COST IS DELIBERATELY NOT SYNTHESISED. MemQL knows the app's
// per-token prices for its own providers, not for a run inside
// somebody's subscription -- so totalCost carries what the app
// REPORTED (Claude Code's --output-format json gives total_cost_usd)
// and nothing when it reported nothing. Pricing a subscription run
// off MemQL's own table would produce a confident number for money
// nobody was charged.
func (w *LedgerWriter) RecordAppSession(ctx context.Context, result workerservice.RunResult, spec workerservice.RunSpec) error {
	if w == nil || w.Engine == nil {
		return nil
	}
	args := map[string]any{
		"callId":            id.NewShortId(),
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
		"pricingConfigured":  result.Usage.Known,
		"timeToFirstTokenMs": 0,
		"totalDurationMs":    1,
		"tokensPerSec":       0.0,
		"streaming":          true,
		"outcome":            ledgerOutcome(result.Status),
		"errorCategory":      "",
		"errorMessage":       truncateForLedger(result.ErrorMessage),
		"fallbackFromModel":  "",
		"billing":            result.Billing,
		"executionSurface":   planner.BackendCockpitApp + ":" + spec.App,
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
