package memql

// ai_attribution.go -- WHO CAUSED THIS MODEL CALL (memql#5581).
//
// # The defect
//
// requestForPrompt built a router request out of the PROMPT and nothing else:
// a level, a modality, a context floor and a pin. Every attribution field was
// left at its zero value, so the v1:router:call row written for a prompt
// invocation named no person, no upstream request and no footprint. The row
// existed and could not answer "who caused this" -- which is most of what a
// spend ledger is for.
//
// # Why the context and not a parameter
//
// The obvious fix is an argument, and it is the wrong one: `ai(...)` is a DSL
// expression evaluated inside an automation step, so the callers between the
// person and this seam are the shape evaluator and the step registry, neither
// of which knows what a request is. Threading a parameter through them would
// put attribution in the hands of every intermediate frame and would still
// miss the one place the answer exists.
//
// The answer is already on the context, in three layers that were all there
// before this file:
//
//   - auth.AccessContext -- the resolved caller. UserId names the person;
//     CallerKindFromContext names the KIND when there is no person, which is
//     what makes an empty userId an answer rather than a gap.
//   - common.RunContext -- the work run and step. component/automations stamps
//     one on EVERY non-sandboxed automation execution (executor.go's
//     withRunContext), which is exactly the path a DSL `ai(...)` runs on, so
//     the run and step that caused the call are on the context of every prompt
//     call an automation makes. It also carries the run's OwnerUserId, which
//     is a person's id on a context that may carry no access context at all.
//   - AICallAttribution -- the one thing neither of the above can know: the
//     upstream request id a transport assigned, the agent acting, and the
//     footprint the call is about. A caller that HAS those stamps them once;
//     one that does not is no worse off than it was.
//
// # What is deliberately not derived
//
// A REQUEST ID IS NEVER INVENTED HERE. When nothing on the context names one,
// the field is left empty and the router stamps a fresh id as it always has --
// which is honest (this row belongs to no upstream request anyone recorded)
// where a synthesised id would be a correlation key that correlates nothing.

import (
	"context"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

// AICallAttribution is what a CALLER knows about a model call and the engine
// cannot derive: the upstream request it belongs to, the agent acting, and the
// footprint it touches.
//
// It is stamped on the context once, at the entry point that knows these
// things, and read at the seam that builds the router request. Every field is
// optional; an absent one falls through to what the context can derive.
type AICallAttribution struct {
	// RequestId correlates this call to an upstream request -- a gRPC turn, a
	// suggest envelope, an inbound webhook.
	RequestId string
	// AgentId names the agent whose work this call serves.
	AgentId string
	// Touches is the call's footprint: the concept ids or knowledge-domain
	// ids the call is about. A rule matches it with startsWith semantics.
	Touches []string
}

type aiCallAttributionKey struct{}

// ContextWithAICallAttribution stamps what the caller knows onto ctx.
//
// A zero-valued attribution returns ctx UNCHANGED rather than stamping an
// empty one: an empty stamp would shadow a richer attribution an outer frame
// had already set, and "I know nothing" is not worth recording over "I know
// the request id".
func ContextWithAICallAttribution(ctx context.Context, a AICallAttribution) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(a.RequestId) == "" && strings.TrimSpace(a.AgentId) == "" && len(a.Touches) == 0 {
		return ctx
	}
	return context.WithValue(ctx, aiCallAttributionKey{}, a)
}

// AICallAttributionFromContext returns the caller-stamped attribution.
func AICallAttributionFromContext(ctx context.Context) (AICallAttribution, bool) {
	if ctx == nil {
		return AICallAttribution{}, false
	}
	a, ok := ctx.Value(aiCallAttributionKey{}).(AICallAttribution)
	return a, ok
}

// applyCallAttribution fills the attribution half of a router request from the
// context, leaving anything the caller already set alone.
//
// EVERY FIELD IS FILLED ONLY IF EMPTY, because a call site that named a value
// knows something this derivation does not -- the agent replier stamps the
// gRPC request id straight onto its own request, and rediscovering a different
// one here would rename the very thing the field exists to join on.
func applyCallAttribution(ctx context.Context, req airoute.ResolveRequest) airoute.ResolveRequest {
	if ctx == nil {
		ctx = context.Background()
	}
	stamped, _ := AICallAttributionFromContext(ctx)
	run, inRun := common.RunFromContext(ctx)

	if strings.TrimSpace(req.RequestId) == "" {
		req.RequestId = firstNonBlank(stamped.RequestId, runRequestId(run, inRun))
	}
	if strings.TrimSpace(req.AgentId) == "" {
		req.AgentId = strings.TrimSpace(stamped.AgentId)
	}
	if len(req.Touches) == 0 && len(stamped.Touches) > 0 {
		// Copied rather than aliased: the request travels onto a decision
		// record that outlives the caller's slice.
		req.Touches = append([]string(nil), stamped.Touches...)
	}

	// THE RUN AND STEP ARE THE SESSION DOOR'S HINGE (design D7) and nothing
	// filled them for a prompt call, so an app door resolved for a tool-needing
	// prompt turn had no step to hand over and was refused at resolution. The
	// run context is where a step is named, and it is on every automation
	// execution's context already.
	if inRun {
		if strings.TrimSpace(req.RunId) == "" {
			req.RunId = run.RunId
		}
		if strings.TrimSpace(req.StepId) == "" {
			req.StepId = run.StepKey
		}
	}

	if strings.TrimSpace(req.UserId) == "" {
		if access, ok := auth.AccessFromContext(ctx); ok && access != nil {
			req.UserId = strings.TrimSpace(access.UserId)
		}
	}
	if strings.TrimSpace(req.UserId) == "" && inRun {
		// A run's owner is a person, recorded on the run row, and it is
		// available on contexts that carry no access context at all -- an
		// automation executing on a replica that did not take the request.
		req.UserId = strings.TrimSpace(run.OwnerUserId)
	}
	if strings.TrimSpace(req.CallerKind) == "" {
		req.CallerKind = auth.CallerKindFromContext(ctx)
	}
	return req
}

// runRequestId is the correlation key for a call made inside a work run.
//
// `run:<runId>` names the run and `#<stepKey>` the step, because a run makes
// many model calls and a ledger row that named only the run could not be
// joined to the step that caused it. It is a real correlation key rather than
// a minted one: both halves are ids the journal already writes.
func runRequestId(run common.RunContext, inRun bool) string {
	if !inRun || !run.IsRun() {
		return ""
	}
	if step := strings.TrimSpace(run.StepKey); step != "" {
		return "run:" + strings.TrimSpace(run.RunId) + "#" + step
	}
	return "run:" + strings.TrimSpace(run.RunId)
}

func firstNonBlank(values ...string) string {
	for _, v := range values {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
