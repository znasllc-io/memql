// Package router is the memQL AI Router: the single entry point every AI
// call flows through. It resolves a ResolveRequest to a concrete provider,
// wraps the provider with an observer that records one v1:router:call row
// per call (tokens, cost, latency, outcome), and hands the wrapped provider
// back to the caller.
//
// Phase 1 scope: observability and attribution for existing OpenAI and
// Anthropic providers. Policy evaluation, fallback chains, BYOK, and
// budgets land in later phases. Selection today is strictly "explicit
// provider > default provider"; policies become a third input in Phase 2.
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
)

// ResolveRequest carries the attribution and selection inputs for one
// router resolution. Every field is optional except that SOMETHING must
// pick a provider or a policy.
//
// Selection precedence (first non-empty wins):
//  1. ExplicitProvider  (caller pinned a specific registry entry)
//  2. PolicyName        (caller picked a policy -- primary + fallbacks)
//  3. DefaultProvider   (caller's pre-policy default)
//  4. Registry default  (engine-wide default provider)
type ResolveRequest struct {
	// RequestId correlates the router call to an upstream request --
	// cognition turn, suggest envelope, prompt invocation. The router
	// stamps a UUID when the caller leaves this empty.
	RequestId string

	// AgentId, UserId, PromptName are attribution fields written
	// straight to the v1:router:call row. Leave empty when a given
	// dimension doesn't apply (e.g. AgentId is empty for a suggest
	// call). (Epic 3 3.2 #1899: the CoPresent space-attribution field
	// was dropped -- the tenant scope is Partition; per-space AI-cost
	// attribution is a CoPresent-pack concern.)
	AgentId    string
	UserId     string
	PromptName string

	// Partition is the tenant scope. The mutation call is executed
	// under this partition; rows land in the tenant's usage ledger.
	// Leave empty to use the engine's current-partition default.
	Partition string

	// ExplicitProvider names a specific provider registry entry
	// (e.g. "stream54Mini", "streamClaudeSonnet"). Takes precedence
	// over PolicyName and DefaultProvider when set.
	ExplicitProvider string

	// PolicyName names a routing policy from policies/v1/<name>.memql.
	// When set (and ExplicitProvider is empty), the router uses the
	// policy's primary provider and tries fallbacks on pre-flight
	// error.
	PolicyName string

	// DefaultProvider is what the caller would have picked absent any
	// agent-level override. Used when neither ExplicitProvider nor
	// PolicyName is set.
	DefaultProvider string
}

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
	Chain        []string      // full provider chain (primary + fallbacks), for logging
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
}
