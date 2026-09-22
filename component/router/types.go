// Package router is the MemQL AI Router: the single entry point every AI
// call flows through. It resolves a ResolveRequest to a concrete provider,
// wraps the provider with an observer that records one v1:router:call row
// per call (tokens, cost, latency, outcome), and hands the wrapped provider
// back to the caller.
//
// SELECTION IS RULE-DRIVEN (epic memql#5127). A call declares a LEVEL and
// carries the metadata a rule may branch on; the router matches the first rule
// in the registry's evaluation order, takes the POLICY that rule names, walks
// the policy's expanded chain, and either resolves an entry or degrades to the
// next level down -- or parks, when the rule says so. An explicit provider pin
// still wins over all of it, and nothing else does: there is no default-provider
// tier and no caller-supplied policy name any more, because both were ways for
// a call site to make a routing decision the rules could not see.
//
// Design notes
//
//   - The router is an in-process component, not a dedicated node. Every
//     node that calls AI embeds it. Observability rides the shared
//     Postgres/TimescaleDB store via a MemQL mutation; no extra network
//     hop on the AI call path.
//
//   - Token counts in Phase 1 are heuristic (char-count / 4). The
//     CallRecord carries tokensEstimated=true so downstream dashboards
//     can flag ballpark numbers. Phase 2 will thread real vendor usage
//     through the stream chunk types.
//
//   - Recording is fire-and-forget on a detached context. The router
//     never blocks an AI reply on the ledger write. A dropped write is
//     logged and the AI call continues.
package router

import (
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
)

// ResolveRequest is the shared vocabulary's request, aliased so that
// router.ResolveRequest keeps resolving for every existing caller.
//
// It LIVES in core/airoute rather than here because the seam's two halves are
// separate Go modules pointing one way: this package imports component/memql,
// and the call sites that build a request live inside component/memql.
// Declaring the request here would make every one of those an import cycle.
type ResolveRequest = airoute.ResolveRequest

// Resolution is the shared vocabulary's answer: the entry that was picked and
// the whole decision that led there.
type Resolution = airoute.Resolution

// Resolved describes the provider the router picked and is returned to
// the caller alongside the wrapped provider. Callers log this so the
// decision is visible in the call path, independent of the v1:router:call
// row the observer will write asynchronously.
type Resolved struct {
	ProviderName string        // registry name, e.g. "stream54Mini"
	Vendor       string        // "openai" | "anthropic"
	Model        string        // vendor-side model id, e.g. "gpt-5.4-mini"
	Pricing      memql.Pricing // per-million-token USD prices (may be zero-valued)
	Streaming    bool          // true when the wrapped path is a streaming interface
	PolicyName   string        // non-empty when selection came from a policy
	Chain        []string      // the concrete chain from the winner on, for the fallback walk

	// Decision is the whole resolution in the record's own words: the level
	// asked for, the level served, the rule and policy that decided, the door
	// the winner came through, and the entries considered on the way.
	//
	// It is filled on SUCCESS, not only on refusal (design D10). The pre-rules
	// router accumulated exactly this report and dropped it with the stack
	// frame the moment an entry won, so the only decisions that could ever be
	// read back were the ones that failed -- and a rule is falsifiable only if
	// the decisions it MADE can be read.
	Decision airoute.Decision

	// Entry is the registry record the chain walk picked, handed back so
	// component/memql's own callers keep the provider's configuration -- its
	// answer-affecting parameters and its declared modality -- without a
	// second lookup that could resolve differently.
	Entry *memql.ProviderConfigEntry

	// Client is the client to call, set ONLY for a SESSION winner (design D7).
	//
	// Every other door hands its client back through the modality-specific
	// resolve* path, which looks the winning entry up again and type-asserts
	// it. A session winner cannot: its client is NOT the entry's. The entry's
	// appProvider deliberately does not serve tool turns, and the session
	// client is built around the STEP rather than the turn. Nil means the
	// ordinary path applies.
	Client any
}

// CallRecord is the payload for one v1:router:call row. Populated by
// observed providers as the call progresses; handed to recordCall at
// stream end (or immediately on pre-flight error).
type CallRecord struct {
	// Attribution + identity
	RequestId  string
	Partition  string
	AgentId    string
	UserId     string
	PromptName string
	PolicyName string // Phase 1: always empty

	// CallerKind is WHAT KIND of caller made this call (memql#5581), so that
	// an empty UserId is an ANSWER -- the cluster's own maintenance sweep, a
	// connector writing a mirror, an anonymous reader -- rather than a hole in
	// attribution that a reader has to treat as a possible defect. One of
	// component/auth's CallerKinds, and never empty on a row this router
	// writes: recordCall fills `unattributed` for a record that reached it
	// without one, because "nobody said" is itself one of the five answers.
	CallerKind string

	// CacheKind is set ONLY on a row for a call served from a CACHE, and
	// names which one: "exact" or "semantic" (memql#5581). EMPTY MEANS A
	// PROVIDER ANSWERED, which is what every row written before this was.
	//
	// It is how a reader tells a cache hit from a provider call, and it is why
	// the zeros on such a row are readable: on a cache row every token and
	// cost figure is a real zero -- nothing was spent -- where on a provider
	// row a zero cost means the provider declared no pricing.
	CacheKind string

	// Provider selection
	Vendor       string
	Model        string
	ProviderName string

	// Token counts
	InputTokens       int
	OutputTokens      int
	CachedInputTokens int
	TokensEstimated   bool

	// Cost breakdown (USD)
	InputCost         float64
	OutputCost        float64
	CachedInputCost   float64
	TotalCost         float64
	PricingConfigured bool

	// Latency
	StartedAt          time.Time
	TimeToFirstTokenMs int
	TotalDurationMs    int
	TokensPerSec       float64
	Streaming          bool

	// Outcome
	Outcome           string // "ok" | "error" | "cancelled" | "fallback_used"
	ErrorCategory     string
	ErrorMessage      string
	FallbackFromModel string // set on fallback_used rows: the model that failed

	// THE DECISION (design D10). What the call asked for, what served it, and
	// which rule decided -- carried on every row, success and refusal alike,
	// so a park is as legible as a hit.
	//
	// Nothing writes these to v1:router:call yet; the concept fields, the
	// mutation and the routerDecisionsRecent query are memql#5132's. They are
	// populated here because the resolution is the only moment that knows
	// them, and a field filled in later from a re-derivation would be a second
	// answer to a question the router already answered.
	Level string
	// RequestedLevel is what the CALL declared, before a rule overrode it.
	// Equal to Level when no rule raised or lowered it, which is how a rule's
	// effect is visible on the record without diffing the rule set.
	RequestedLevel string
	ServedLevel    string
	Degraded       bool
	Rule           string
	Policy         string
	Door           string
	// Considered is the door report, in the order the chain was walked.
	Considered []airoute.ConsideredEntry
	// Touches is the call's footprint, copied from the request.
	Touches []string
	// MinContextTokens is the floor the resolution was made against. It sits
	// beside the winning model's window, which is what makes an under-counted
	// estimate diagnosable rather than merely wrong.
	MinContextTokens int
	// MachineOwnerUserId names whose machine served a local call. Empty until
	// epic 4 (shared machines) fills it.
	MachineOwnerUserId string

	// Billing says who PAID (memql#4362): "metered" (MemQL's own
	// vendor spend, which is what the cost fields above are about),
	// "subscription" (the call ran inside an app the user already
	// pays for) or "unknown". Empty reads as metered on the row, so
	// every existing writer keeps its meaning without a migration.
	Billing string
	// ServedModel and ServedEffort are what the SURFACE REPORTED serving this
	// call with (design D9), as distinct from Model, which is what the CHAIN
	// resolved. For an app door those are different facts: the chain resolves
	// `app:claude-code` and the app itself reports `claude-opus-5`, and the
	// gap between them is the only way to see an app that rerouted or ignored
	// a model pin. Both are EMPTY when the surface said nothing, which the row
	// records as unknown -- never the requested value, never a guess.
	ServedModel  string
	ServedEffort string
	// ExecutionSurface names where the call ran: empty for MemQL's
	// own provider calls, "cockpit-app:<appId>" for one made by a
	// local app on a user's machine.
	ExecutionSurface string
}

// Billing values. They mirror v1:router:call.billing,
// v1:worker:appSession.billing and planner.Billing* so one vocabulary
// spans the ledger, the session row and the executor seam.
const (
	BillingMetered      = "metered"
	BillingSubscription = "subscription"
	// BillingLocal is a call served by a model on one of the user's own
	// machines (epic memql#4676). Real tokens, no bill.
	BillingLocal   = "local"
	BillingUnknown = "unknown"
)
