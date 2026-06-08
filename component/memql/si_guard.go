// si_guard.go
//
// Global LLM circuit breaker (memql#825). The single point every LLM
// HTTP call leaves the process is the provider SDKs' *http.Client. We
// install ONE guarding http.RoundTripper on the clients every provider
// (OpenAI + Anthropic, stream + non-stream, chat / tools / structured /
// vision) is built from, so a runaway loop is caught HERE -- cheaply,
// before a real vendor request -- no matter which caller (planner,
// agent tool loop, cognition, ...) initiated it.
//
// Primary guard: an identical-request loop breaker. Two LLM calls with
// the byte-identical request (method + URL + body) are fingerprinted to
// the same key; if the SAME fingerprint fires more than N times within
// a sliding window, the breaker trips for a cooldown and returns a
// synthetic 429 rate_limit_error response WITHOUT calling the vendor.
// Because distinct requests fingerprint differently, legitimate traffic
// is never throttled -- only a wild loop spamming the exact same call.
//
// Returning a real 429 (not a transport error) means both vendor SDKs
// surface their normal rate-limit error type, so this composes cleanly
// with the planner's provider-rate-limit backoff (memql#821).
//
// Secondary guard (memql#834): a HARD, process-wide rate ceiling on
// chat/messages calls. Independent of request content -- it counts
// EVERY guarded LLM call leaving the process within a rolling window,
// regardless of which caller fired it or what the body says. When the
// count in the window reaches the ceiling, further calls get the same
// synthetic 429 WITHOUT touching the vendor. This is the seatbelt the
// per-fingerprint loop breaker can't be: a runaway that VARIES its
// request body (growing context, timestamps) fingerprints differently
// every call and sails past the loop breaker -- but it cannot outrun
// the global call counter. With a generous default (20 calls / 10s) it
// is invisible to real traffic and lethal to a spend loop.
//
// Tertiary guard -- the KILL-SWITCH (memql#1145). Both guards above
// SELF-HEAL: the loop breaker resets its fingerprint state after the
// cooldown, and the rate ceiling drains its window and re-admits. That
// is correct for a transient spike, but it means a genuinely STUCK loop
// (varying body, paced just under the rate ceiling) trickles spend
// FOREVER -- you see `429 ... rate ceiling ... blocked` repeat
// indefinitely while money keeps leaving. The kill-switch is the piece a
// rate limiter cannot be: a CUMULATIVE, LATCHING total-call / total-cost
// breaker. It counts every admitted guarded call (and an upper-bound $
// estimate of it) cumulatively for the life of the process and, once a
// configured cap is crossed, latches OPEN PERMANENTLY -- every further
// guarded call is hard-stopped with a terminal 402 and makes NO vendor
// request. It does not drain. Because it lives at this one shared
// chokepoint and is path-agnostic (it counts calls, not callers), a
// brand-new runaway path nobody anticipated is bounded automatically.
// Process scope here; per-(space, plan-lineage) scopes ride the same
// mechanism in memql#1144.
package memql

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// laneCtxKey tags a context as belonging to the background (batch)
// execution lane (memql#897). The background SI executor stamps it via
// ContextWithBackgroundLane before issuing model calls; because the
// vendor SDKs thread the call context all the way to the HTTP request,
// guardedTransport.RoundTrip can read it off req.Context() and count the
// call against the background rate bucket instead of the interactive one.
type laneCtxKey struct{}

// ContextWithBackgroundLane marks ctx as the background execution lane so
// downstream LLM HTTP calls are rate-limited against the background bucket
// rather than the interactive one. No-op-safe on a nil ctx.
func ContextWithBackgroundLane(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, laneCtxKey{}, true)
}

// backgroundLaneFromContext reports whether ctx was tagged as the
// background execution lane. Defaults to false (interactive) for any
// context that wasn't stamped -- so chat, voice, suggest, and every legacy
// path keep counting against the interactive ceiling unchanged.
func backgroundLaneFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(laneCtxKey{}).(bool)
	return v
}

// budgetScopeCtxKey tags a context with the budget scopes a downstream LLM
// call should be charged against (memql#1144). Like the background lane, the
// vendor SDKs thread the call context to the HTTP request, where
// guardedTransport reads the scopes off req.Context() and charges the call's
// cumulative per-scope budget. A scope is an opaque id; callers stamp e.g.
// "space:<id>" + "plan:<id>" so a single conversation or plan-lineage gets
// its own cumulative latch independent of every other.
type budgetScopeCtxKey struct{}

// ContextWithBudgetScope stamps ctx with one or more budget scope ids so
// downstream LLM calls count against each scope's cumulative budget
// (memql#1144). Empty ids are dropped. Scopes COMPOSE: stamping again merges
// the new ids with any already on the context (deduped), so an agent turn
// nested inside a plan charges both the space scope and the plan scope.
// No-op-safe on a nil ctx; returns ctx unchanged when no non-empty id is given.
func ContextWithBudgetScope(ctx context.Context, scopeIds ...string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	merged := append([]string{}, budgetScopesFromContext(ctx)...)
	for _, s := range scopeIds {
		if s == "" {
			continue
		}
		dup := false
		for _, e := range merged {
			if e == s {
				dup = true
				break
			}
		}
		if !dup {
			merged = append(merged, s)
		}
	}
	if len(merged) == 0 {
		return ctx
	}
	return context.WithValue(ctx, budgetScopeCtxKey{}, merged)
}

// budgetScopesFromContext returns the budget scopes stamped on ctx, or nil.
func budgetScopesFromContext(ctx context.Context) []string {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(budgetScopeCtxKey{}).([]string)
	return v
}

// BudgetScopeId formats a "<kind>:<id>" budget scope id for
// ContextWithBudgetScope, returning "" (which the stamp drops) when id is
// empty -- so callers can stamp optional ids (a turn with no Plan, say)
// unconditionally without building the string defensively.
func BudgetScopeId(kind, id string) string {
	if id == "" {
		return ""
	}
	return kind + ":" + id
}

const (
	defaultLoopGuardMaxRepeat      = 8
	defaultLoopGuardWindowSeconds  = 60
	defaultLoopGuardCooldownSecond = 30

	// Global rate-ceiling defaults (memql#834). Generous for real
	// traffic, lethal to a runaway: 20 guarded LLM calls in any rolling
	// 10s window. A legitimate burst (a few concurrent users, a planner
	// fanning out a handful of tasks) stays well under 2 calls/sec; a
	// spend loop blows straight through it and is capped.
	defaultRateMaxCalls      = 20
	defaultRateWindowSeconds = 10

	// Background-lane rate-ceiling defaults (memql#897). The background /
	// batch execution lane (planner-dispatched plan/task turns) gets its
	// OWN rate bucket, independent of the interactive ceiling above, so a
	// burst of task executions counts against THIS budget and can never
	// consume the interactive budget that protects live chat + voice. A
	// touch more generous than interactive since background work is the
	// fan-out path (memql#899) and nobody is waiting on its first token.
	defaultBgRateMaxCalls      = 40
	defaultBgRateWindowSeconds = 10

	// Kill-switch cost-estimate defaults (memql#1145). At the
	// RoundTripper we only ever see the REQUEST, never the response usage,
	// so the cumulative $ tally is a deliberately CONSERVATIVE upper bound:
	// input tokens approximated from the request byte size (~4 bytes/token),
	// output tokens from the body's max_tokens, both priced at an Opus-tier
	// per-million rate. Over-estimating is the safe direction for a kill-
	// switch -- it latches sooner, never later. Both rates are env-tunable
	// (MEMQL_LLM_COST_INPUT_PER_MILLION / _OUTPUT_PER_MILLION) for operators
	// who want the tally tracked against their actual model mix.
	defaultCostInputPerMillion  = 15.0
	defaultCostOutputPerMillion = 75.0
	// Fallback output-token estimate when a request omits max_tokens.
	defaultEstOutputTokens = 4096

	// Per-scope budget defaults (memql#1144). Unlike the process-scope caps
	// (which default to 0/unlimited because a since-boot latch has no safe
	// universal value for a long-lived node), the per-scope latch is ON by
	// default: a scope is ONE conversation/space or plan-lineage, a
	// well-defined unit of work, so a runaway WITHIN a scope is unambiguous
	// and latching it kills just that loop without touching other
	// conversations or needing a restart. The caps sit well ABOVE the
	// per-turn iteration cap (120) so a single legitimately-deep turn never
	// trips them; a loop that spans turns/plan-cycles does. Conservative +
	// env-tunable; raise per deployment.
	defaultScopeMaxCalls   = 600
	defaultScopeMaxCostUSD = 20.0
	// Scopes idle longer than this are pruned so the map can't grow
	// unbounded across many short-lived conversations. A runaway is never
	// idle, so it stays latched; a finished conversation's scope ages out.
	scopeIdleTTLSeconds = 3600
)

// llmGuard is the process-global circuit breaker shared by every
// provider HTTP client. Safe for concurrent use.
type llmGuard struct {
	enabled bool

	maxRepeat int
	window    time.Duration
	cooldown  time.Duration

	// Global rate ceiling (memql#834). Independent of the per-fingerprint
	// loop breaker above: counts EVERY admitted guarded call across the
	// whole process within rateWindow. rateEnabled gates it separately
	// so an operator can tune the loop breaker and the rate ceiling on
	// their own switches.
	rateEnabled bool
	rateMax     int
	rateWindow  time.Duration

	mu      sync.Mutex
	hits    map[string][]time.Time // fingerprint -> recent call times (within window)
	tripped map[string]time.Time   // fingerprint -> breaker-open-until

	// rateHits is the global sliding window of admitted guarded-call
	// timestamps (memql#834). Blocked calls are NOT recorded, so a
	// hammering loop can't extend its own window -- the window drains
	// naturally and admits real traffic again once old calls age out.
	rateHits []time.Time
	// lastRateAlert throttles the loud ERROR log to once per window so a
	// sustained runaway doesn't drown the logs while still being loud.
	lastRateAlert time.Time

	// Background-lane rate ceiling (memql#897). A SEPARATE bucket from the
	// interactive one above, selected per-call by a context flag the
	// background executor stamps (see ContextWithBackgroundLane). Keeping
	// the two buckets independent is the isolation that matters: a burst of
	// planner-dispatched task executions fills bgRateHits and can trip the
	// background ceiling without ever touching rateHits, so live chat +
	// voice never get a synthetic 429 because batch work was busy.
	bgRateEnabled   bool
	bgRateMax       int
	bgRateWindow    time.Duration
	bgRateHits      []time.Time
	bgLastRateAlert time.Time

	// Cumulative, LATCHING kill-switch (memql#1145). Unlike the loop
	// breaker and rate ceiling above -- both of which self-heal -- these
	// counters accumulate for the whole process lifetime and never drain.
	// killSwitchEnabled gates the whole mechanism. A cap of 0 means
	// "unlimited" for that dimension. costIn/costOutPerMillion price the
	// per-call estimate. totalCalls / totalCostUSD are the running tallies;
	// once a cap is crossed, latched flips true PERMANENTLY (until
	// resetLatch / process restart), latchReason records why, and
	// latchAlerted ensures the loud alert fires exactly once.
	killSwitchEnabled bool
	maxTotalCalls     int     // 0 = unlimited
	maxTotalCostUSD   float64 // 0 = unlimited
	costInPerMillion  float64
	costOutPerMillion float64

	totalCalls   int64
	totalCostUSD float64
	latched      bool
	latchReason  string
	latchAlerted bool

	// Per-scope cumulative latch (memql#1144). Same latching semantics as
	// the process kill-switch above, but keyed per scope id (a conversation/
	// space or plan-lineage) read off the request context. scopeEnabled gates
	// it; a cap of 0 means unlimited for that dimension. scopes holds the
	// live per-scope tallies; idle ones are pruned past scopeIdleTTL.
	scopeEnabled    bool
	scopeMaxCalls   int
	scopeMaxCostUSD float64
	scopeIdleTTL    time.Duration
	scopes          map[string]*scopeBudget

	// now is injectable for tests; defaults to time.Now.
	now func() time.Time

	logger *slog.Logger
}

// scopeBudget is one scope's cumulative tally + latch state (memql#1144).
type scopeBudget struct {
	calls    int64
	costUSD  float64
	latched  bool
	reason   string
	lastSeen time.Time
}

// loggerOrDefault guards against a nil logger in tests that build the
// struct literal directly.
func (g *llmGuard) log() *slog.Logger {
	if g.logger == nil {
		return slog.Default()
	}
	return g.logger
}

// sharedLLMGuard is the one breaker instance every provider references.
var sharedLLMGuard = newLLMGuardFromEnv()

func newLLMGuardFromEnv() *llmGuard {
	g := &llmGuard{
		enabled:   envBoolDefault("MEMQL_LLM_LOOP_GUARD_ENABLED", true),
		maxRepeat: envIntDefault("MEMQL_LLM_LOOP_MAX_REPEAT", defaultLoopGuardMaxRepeat),
		window:    time.Duration(envIntDefault("MEMQL_LLM_LOOP_WINDOW_SECONDS", defaultLoopGuardWindowSeconds)) * time.Second,
		cooldown:  time.Duration(envIntDefault("MEMQL_LLM_LOOP_COOLDOWN_SECONDS", defaultLoopGuardCooldownSecond)) * time.Second,

		rateEnabled: envBoolDefault("MEMQL_LLM_RATE_GUARD_ENABLED", true),
		rateMax:     envIntDefault("MEMQL_LLM_MAX_CALLS_PER_WINDOW", defaultRateMaxCalls),
		rateWindow:  time.Duration(envIntDefault("MEMQL_LLM_RATE_WINDOW_SECONDS", defaultRateWindowSeconds)) * time.Second,

		bgRateEnabled: envBoolDefault("MEMQL_LLM_BG_RATE_GUARD_ENABLED", true),
		bgRateMax:     envIntDefault("MEMQL_LLM_BG_MAX_CALLS_PER_WINDOW", defaultBgRateMaxCalls),
		bgRateWindow:  time.Duration(envIntDefault("MEMQL_LLM_BG_RATE_WINDOW_SECONDS", defaultBgRateWindowSeconds)) * time.Second,

		// Kill-switch (memql#1145). The MECHANISM is on by default, but the
		// process-scope caps default to 0 (unlimited) -- a since-boot
		// cumulative latch has no single value that is safe for both a tiny
		// local dev run and a long-lived multi-tenant production node, so
		// the process backstop is opt-in (local repro / paranoid prod set
		// it; e.g. MEMQL_LLM_MAX_TOTAL_CALLS=20). The conservative
		// ON-by-default protection is the per-(space, plan-lineage) latch in
		// memql#1144, where "one conversation/plan" is a well-defined unit of
		// work and a runaway within one scope is unambiguous.
		killSwitchEnabled: envBoolDefault("MEMQL_LLM_KILL_SWITCH_ENABLED", true),
		maxTotalCalls:     envIntDefault("MEMQL_LLM_MAX_TOTAL_CALLS", 0),
		maxTotalCostUSD:   envFloatDefault("MEMQL_LLM_MAX_TOTAL_COST_USD", 0),
		costInPerMillion:  envFloatDefault("MEMQL_LLM_COST_INPUT_PER_MILLION", defaultCostInputPerMillion),
		costOutPerMillion: envFloatDefault("MEMQL_LLM_COST_OUTPUT_PER_MILLION", defaultCostOutputPerMillion),

		// Per-scope latch (memql#1144) -- ON by default with conservative caps
		// (see the const comments). The scope is a conversation/space or
		// plan-lineage stamped on the request context.
		scopeEnabled:    envBoolDefault("MEMQL_LLM_SCOPE_GUARD_ENABLED", true),
		scopeMaxCalls:   envIntDefault("MEMQL_LLM_SCOPE_MAX_CALLS", defaultScopeMaxCalls),
		scopeMaxCostUSD: envFloatDefault("MEMQL_LLM_SCOPE_MAX_COST_USD", defaultScopeMaxCostUSD),
		scopeIdleTTL:    time.Duration(envIntDefault("MEMQL_LLM_SCOPE_IDLE_TTL_SECONDS", scopeIdleTTLSeconds)) * time.Second,
		scopes:          map[string]*scopeBudget{},

		hits:    map[string][]time.Time{},
		tripped: map[string]time.Time{},
		now:     time.Now,
		logger:  slog.Default().With("component", "llmGuard"),
	}
	if g.maxRepeat < 1 {
		g.maxRepeat = defaultLoopGuardMaxRepeat
	}
	if g.window <= 0 {
		g.window = defaultLoopGuardWindowSeconds * time.Second
	}
	if g.rateMax < 1 {
		g.rateMax = defaultRateMaxCalls
	}
	if g.rateWindow <= 0 {
		g.rateWindow = defaultRateWindowSeconds * time.Second
	}
	if g.bgRateMax < 1 {
		g.bgRateMax = defaultBgRateMaxCalls
	}
	if g.bgRateWindow <= 0 {
		g.bgRateWindow = defaultBgRateWindowSeconds * time.Second
	}
	if g.scopeIdleTTL <= 0 {
		g.scopeIdleTTL = scopeIdleTTLSeconds * time.Second
	}
	return g
}

// admit records one call against the fingerprint and reports whether
// the breaker is OPEN for it (i.e. the call must be rejected). When it
// returns true, the caller returns a synthetic 429 and makes no vendor
// request.
func (g *llmGuard) admit(fingerprint string) (open bool, repeatCount int) {
	if g == nil || !g.enabled {
		return false, 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()

	// Already tripped + still in cooldown -> reject without recording
	// (so a hammering loop doesn't keep extending its own window).
	if until, ok := g.tripped[fingerprint]; ok {
		if now.Before(until) {
			return true, len(g.hits[fingerprint])
		}
		// Cooldown elapsed -- reset this fingerprint and let it through.
		delete(g.tripped, fingerprint)
		delete(g.hits, fingerprint)
	}

	// Prune entries older than the window, then record this call.
	cutoff := now.Add(-g.window)
	kept := g.hits[fingerprint][:0]
	for _, t := range g.hits[fingerprint] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	kept = append(kept, now)
	g.hits[fingerprint] = kept

	if len(kept) > g.maxRepeat {
		// Trip the breaker for this exact request.
		g.tripped[fingerprint] = now.Add(g.cooldown)
		g.log().Error("LLM circuit breaker TRIPPED: identical request repeated past the loop threshold",
			"fingerprint", fingerprint,
			"repeats_in_window", len(kept),
			"max_repeat", g.maxRepeat,
			"window", g.window.String(),
			"cooldown", g.cooldown.String(),
		)
		return true, len(kept)
	}
	return false, len(kept)
}

// admitRate enforces the global, content-independent rate ceiling
// (memql#834). It prunes the sliding window, and if the count of
// admitted guarded calls in the last rateWindow has reached rateMax it
// returns open=true (REJECT) WITHOUT recording this call -- so a
// runaway can't keep its own window full and starve real traffic
// forever; the window drains and admits again once old calls age out.
// Otherwise it records the call and returns open=false (ADMIT).
//
// This is deliberately separate from admit(): admit fingerprints on the
// request body and only catches byte-identical loops; admitRate counts
// ALL guarded calls regardless of content, which is the only thing that
// stops a loop that varies its request body.
//
// background selects which lane's bucket the call counts against
// (memql#897). The interactive and background buckets are fully
// independent -- distinct windows, distinct ceilings, distinct alert
// throttles -- so saturating one cannot trip the other. The background
// executor stamps the lane on the request context; everything else
// (chat, voice, suggest) counts against the interactive bucket.
func (g *llmGuard) admitRate(background bool) (open bool, count int) {
	if g == nil {
		return false, 0
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()

	// Select the lane's bucket. Pointers so the single window-prune +
	// record body works against either bucket without duplication.
	enabled, max, window := g.rateEnabled, g.rateMax, g.rateWindow
	hits, lastAlert := &g.rateHits, &g.lastRateAlert
	lane := "interactive"
	if background {
		enabled, max, window = g.bgRateEnabled, g.bgRateMax, g.bgRateWindow
		hits, lastAlert = &g.bgRateHits, &g.bgLastRateAlert
		lane = "background"
	}
	if !enabled {
		return false, 0
	}

	cutoff := now.Add(-window)
	kept := (*hits)[:0]
	for _, t := range *hits {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	*hits = kept

	if len(kept) >= max {
		// Throttle the alert to once per window so a sustained runaway
		// stays loud without flooding the logs.
		if lastAlert.IsZero() || now.Sub(*lastAlert) >= window {
			*lastAlert = now
			g.log().Error("LLM rate ceiling TRIPPED: per-lane call rate exceeded; blocking to prevent a runaway spend loop",
				"lane", lane,
				"calls_in_window", len(kept),
				"max_calls", max,
				"window", window.String(),
			)
		}
		return true, len(kept)
	}
	*hits = append(*hits, now)
	return false, len(*hits)
}

// killed is the cheap read checked FIRST on every guarded call: once the
// kill-switch has latched, no further work (fingerprinting, rate
// accounting, vendor request) should happen -- return the terminal reason
// straight away. Self-gates on killSwitchEnabled.
func (g *llmGuard) killed() (reason string, dead bool) {
	if g == nil || !g.killSwitchEnabled {
		return "", false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.latchReason, g.latched
}

// recordAndMaybeLatch is the cumulative kill-switch accounting, called
// LAST -- only for a call that has already cleared the loop breaker and the
// rate ceiling and is therefore about to reach the vendor. It charges the
// call against BOTH the process-wide budget and every per-scope budget named
// by scopeIds (memql#1144). If admitting the call would push ANY of those
// cumulative tallies past its cap, it LATCHES that breaker open permanently
// and BLOCKS this call WITHOUT charging anything (so a blocked call never
// pollutes a tally). Otherwise it charges the call against the process budget
// and every scope and admits it.
//
// Counting here (post loop+rate, pre-vendor) keeps the tallies honest: calls
// the cheaper guards already blocked never reached the vendor, so they must
// not count against any cumulative budget.
func (g *llmGuard) recordAndMaybeLatch(scopeIds []string, body []byte) (reason string, blocked bool) {
	if g == nil || (!g.killSwitchEnabled && !g.scopeEnabled) {
		return "", false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	cost := estimateRequestCostUSD(body, g.costInPerMillion, g.costOutPerMillion)

	// --- process-wide kill-switch (check, no charge yet) ---
	if g.killSwitchEnabled {
		if g.latched { // tripped by another goroutine between killed() and here
			return g.latchReason, true
		}
		if g.maxTotalCalls > 0 && g.totalCalls >= int64(g.maxTotalCalls) {
			return g.tripLocked(fmt.Sprintf(
				"cumulative LLM call ceiling reached: %d calls admitted this process (MEMQL_LLM_MAX_TOTAL_CALLS=%d)",
				g.totalCalls, g.maxTotalCalls)), true
		}
		if g.maxTotalCostUSD > 0 && g.totalCostUSD >= g.maxTotalCostUSD {
			return g.tripLocked(fmt.Sprintf(
				"cumulative LLM cost ceiling reached: est $%.2f spent this process (MEMQL_LLM_MAX_TOTAL_COST_USD=$%.2f)",
				g.totalCostUSD, g.maxTotalCostUSD)), true
		}
	}

	// --- per-scope budgets (check, no charge yet) ---
	var charge []*scopeBudget
	if g.scopeEnabled {
		for _, sid := range scopeIds {
			if sid == "" {
				continue
			}
			sb := g.scopes[sid]
			if sb == nil {
				sb = &scopeBudget{lastSeen: now}
				g.scopes[sid] = sb
			}
			if sb.latched {
				return sb.reason, true
			}
			if g.scopeMaxCalls > 0 && sb.calls >= int64(g.scopeMaxCalls) {
				return g.tripScopeLocked(sid, sb, fmt.Sprintf(
					"per-scope LLM call ceiling reached for scope %q: %d cumulative calls (MEMQL_LLM_SCOPE_MAX_CALLS=%d)",
					sid, sb.calls, g.scopeMaxCalls)), true
			}
			if g.scopeMaxCostUSD > 0 && sb.costUSD >= g.scopeMaxCostUSD {
				return g.tripScopeLocked(sid, sb, fmt.Sprintf(
					"per-scope LLM cost ceiling reached for scope %q: est $%.2f cumulative (MEMQL_LLM_SCOPE_MAX_COST_USD=$%.2f)",
					sid, sb.costUSD, g.scopeMaxCostUSD)), true
			}
			charge = append(charge, sb)
		}
	}

	// Admit: charge the process budget and every scope its cost.
	if g.killSwitchEnabled {
		g.totalCalls++
		g.totalCostUSD += cost
	}
	for _, sb := range charge {
		sb.calls++
		sb.costUSD += cost
		sb.lastSeen = now
	}
	g.pruneIdleScopesLocked(now)
	return "", false
}

// tripLocked latches the breaker open permanently and logs ONE loud alert.
// The caller must hold g.mu. Returns the latch reason for convenience.
func (g *llmGuard) tripLocked(reason string) string {
	g.latched = true
	g.latchReason = reason
	if !g.latchAlerted {
		g.latchAlerted = true
		g.log().Error("LLM KILL-SWITCH LATCHED -- cumulative LLM budget exhausted; HARD-STOPPING all further LLM calls process-wide to stop a runaway spend loop. This does NOT drain; it stays latched until the process restarts (or the guard is reset).",
			"reason", reason,
			"total_calls", g.totalCalls,
			"est_total_cost_usd", g.totalCostUSD,
		)
	}
	return reason
}

// tripScopeLocked latches ONE scope's breaker open permanently and logs the
// alert (once -- after latching, the scope short-circuits on its next call).
// The caller must hold g.mu. Returns the reason for convenience.
func (g *llmGuard) tripScopeLocked(scopeId string, sb *scopeBudget, reason string) string {
	sb.latched = true
	sb.reason = reason
	g.log().Error("LLM PER-SCOPE KILL-SWITCH LATCHED -- one conversation/plan-lineage exhausted its cumulative LLM budget; HARD-STOPPING further LLM calls for THIS scope only. Other conversations are unaffected; this does NOT drain.",
		"scope", scopeId,
		"scope_calls", sb.calls,
		"est_scope_cost_usd", sb.costUSD,
		"reason", reason,
	)
	return reason
}

// pruneIdleScopesLocked drops scope budgets untouched for longer than
// scopeIdleTTL so the map can't grow unbounded across many short-lived
// conversations. Only sweeps when the map is non-trivial to keep the hot path
// cheap. A runaway scope is never idle, so it is never pruned. Caller holds
// g.mu.
func (g *llmGuard) pruneIdleScopesLocked(now time.Time) {
	if len(g.scopes) <= 256 {
		return
	}
	cutoff := now.Add(-g.scopeIdleTTL)
	for id, sb := range g.scopes {
		if sb.lastSeen.Before(cutoff) {
			delete(g.scopes, id)
		}
	}
}

// resetLatch clears the latch, the cumulative tallies, and every per-scope
// budget. Used by tests and available as the seam for a future operator/admin
// reset lever.
func (g *llmGuard) resetLatch() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.latched = false
	g.latchReason = ""
	g.latchAlerted = false
	g.totalCalls = 0
	g.totalCostUSD = 0
	g.scopes = map[string]*scopeBudget{}
}

// estimateRequestCostUSD returns a deliberately CONSERVATIVE (upper-bound)
// dollar estimate for one chat/messages request, derived only from the
// request bytes we can see at the RoundTripper (the response usage is never
// visible here). Input tokens are approximated from the request size
// (~4 bytes/token); output tokens from the body's max_tokens (a high
// fallback when absent). Over-estimating is the safe direction for a kill-
// switch: it latches sooner.
func estimateRequestCostUSD(body []byte, inPerMillion, outPerMillion float64) float64 {
	inputTokens := float64(len(body)) / 4.0
	outputTokens := float64(parseMaxOutputTokens(body))
	return inputTokens/1_000_000.0*inPerMillion + outputTokens/1_000_000.0*outPerMillion
}

// parseMaxOutputTokens pulls the output-token cap from a chat/messages
// request body (Anthropic `max_tokens`, OpenAI `max_tokens` /
// `max_completion_tokens`). Falls back to defaultEstOutputTokens when the
// body omits it or can't be parsed. Bodies are small JSON payloads, so a
// best-effort unmarshal is cheap relative to the LLM call it gates.
func parseMaxOutputTokens(body []byte) int {
	var p struct {
		MaxTokens           *int `json:"max_tokens"`
		MaxCompletionTokens *int `json:"max_completion_tokens"`
	}
	if err := json.Unmarshal(body, &p); err == nil {
		if p.MaxTokens != nil && *p.MaxTokens > 0 {
			return *p.MaxTokens
		}
		if p.MaxCompletionTokens != nil && *p.MaxCompletionTokens > 0 {
			return *p.MaxCompletionTokens
		}
	}
	return defaultEstOutputTokens
}

// fingerprintRequest derives the loop-breaker key from a request:
// method + URL path + a hash of the (buffered) body. Returns ("", body,
// false) when the request is not a chat/messages call we want to guard
// -- callers then skip the guard entirely.
func fingerprintRequest(method, urlPath string, body []byte) (fingerprint string, guarded bool) {
	if !isGuardedLLMPath(urlPath) {
		return "", false
	}
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte{0})
	h.Write([]byte(urlPath))
	h.Write([]byte{0})
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil)), true
}

// isGuardedLLMPath limits the breaker to chat-completion / messages
// endpoints. Embeddings, TTS, audio, moderation, etc. legitimately
// repeat identical inputs and must NOT be loop-guarded.
func isGuardedLLMPath(urlPath string) bool {
	p := strings.ToLower(urlPath)
	return strings.Contains(p, "/chat/completions") || // OpenAI-wire vendors
		strings.HasSuffix(p, "/messages") || // Anthropic
		strings.Contains(p, "/messages?") ||
		strings.Contains(p, "/v1/messages")
}

// guardedTransport is the http.RoundTripper that fronts every provider
// HTTP client. It fingerprints guarded requests and short-circuits with
// a synthetic 429 when the breaker is open.
type guardedTransport struct {
	base  http.RoundTripper
	guard *llmGuard
}

func (t *guardedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	g := t.guard
	if g == nil || (!g.enabled && !g.rateEnabled && !g.killSwitchEnabled && !g.scopeEnabled) {
		return base.RoundTrip(req)
	}

	// Read + restore the body so we can fingerprint it. Chat request
	// bodies are small JSON payloads (the request we send, not the SSE
	// response), so buffering once is cheap.
	var bodyBytes []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			// Can't read the body -> can't fingerprint; fail open so a
			// transient read error never blocks legitimate work.
			return base.RoundTrip(req)
		}
		bodyBytes = b
		req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(bodyBytes)), nil
		}
		req.ContentLength = int64(len(bodyBytes))
	}

	fp, guarded := fingerprintRequest(req.Method, req.URL.Path, bodyBytes)
	if guarded {
		// 0. KILL-SWITCH first (memql#1145). If the cumulative breaker has
		// already latched, hard-stop immediately with a terminal 402 -- no
		// fingerprinting, no rate accounting, no vendor request. This is the
		// process-wide budget being exhausted; it does not drain.
		if reason, dead := g.killed(); dead {
			return killSwitchResponse(req, reason), nil
		}
		// 1. Per-fingerprint loop breaker (catches byte-identical loops
		// cheaply). admit() self-gates on g.enabled.
		if open, repeats := g.admit(fp); open {
			return loopBreakerResponse(req, repeats), nil
		}
		// 2. The per-lane, content-independent rate ceiling -- the backstop
		// that catches loops varying their request body. The lane is read
		// off the request context (memql#897): background-executor calls
		// count against the background bucket, everything else against the
		// interactive bucket. admitRate self-gates on the lane's enable flag.
		background := backgroundLaneFromContext(req.Context())
		if open, calls := g.admitRate(background); open {
			return rateCeilingResponse(req, calls), nil
		}
		// 3. Cumulative kill-switch ACCOUNTING last -- this call has cleared
		// the cheaper guards and is about to reach the vendor, so it counts
		// against the cumulative budgets: the process-wide one AND every
		// per-scope budget (space / plan-lineage) stamped on the request
		// context (memql#1144). If it pushes any tally past a cap, that
		// breaker latches and THIS call is hard-stopped too, keeping the
		// number of vendor calls <= the cap for every scope it belongs to.
		scopes := budgetScopesFromContext(req.Context())
		if reason, blocked := g.recordAndMaybeLatch(scopes, bodyBytes); blocked {
			return killSwitchResponse(req, reason), nil
		}
	}
	return base.RoundTrip(req)
}

// loopBreakerResponse builds a synthetic HTTP 429 the vendor SDKs parse
// as a rate-limit error. Shaped like an Anthropic error envelope; the
// OpenAI SDK keys on the 429 status code, so the body shape is
// tolerated by both.
func loopBreakerResponse(req *http.Request, repeats int) *http.Response {
	msg := fmt.Sprintf(
		"memql LLM circuit breaker: the identical request was repeated %d times within the loop window and was blocked to prevent a runaway spend loop. This is a local guard, not a provider limit; the request will be admitted again after the cooldown.",
		repeats,
	)
	body := fmt.Sprintf(`{"type":"error","error":{"type":"rate_limit_error","message":%q}}`, msg)
	return &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Status:     "429 Too Many Requests",
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"Retry-After":  []string{"30"},
		},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

// rateCeilingResponse builds the synthetic HTTP 429 returned when the
// process-wide rate ceiling (memql#834) is hit. Same envelope shape as
// loopBreakerResponse so both vendor SDKs surface their normal
// rate-limit error type and the planner's 429 backoff (memql#821)
// composes cleanly.
func rateCeilingResponse(req *http.Request, calls int) *http.Response {
	msg := fmt.Sprintf(
		"memql LLM rate ceiling: %d chat/messages calls left the process within the rolling rate window, at or above the configured ceiling, so this call was blocked to prevent a runaway spend loop. This is a local process-wide guard, not a provider limit; calls are admitted again as the window drains.",
		calls,
	)
	body := fmt.Sprintf(`{"type":"error","error":{"type":"rate_limit_error","message":%q}}`, msg)
	return &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Status:     "429 Too Many Requests",
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"Retry-After":  []string{"5"},
		},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

// killSwitchResponse builds the TERMINAL synthetic response returned once
// the cumulative kill-switch (memql#1145) has latched. Unlike the 429s the
// loop breaker and rate ceiling return -- which both vendor SDKs treat as
// retryable rate limits and back off on -- this is a 402 Payment Required,
// which neither the Anthropic nor the OpenAI SDK retries. That is
// deliberate: the budget is exhausted, so the call must surface as a clear
// TERMINAL error to the agent/planner loop (ending the turn), not as a
// transient limit it will keep retrying against.
func killSwitchResponse(req *http.Request, reason string) *http.Response {
	msg := fmt.Sprintf(
		"memql LLM kill-switch: %s. This call was HARD-STOPPED locally to prevent a runaway spend loop; it is terminal, not a retryable provider rate limit. The process-wide LLM budget stays latched until the process restarts (or the guard is reset).",
		reason,
	)
	body := fmt.Sprintf(`{"type":"error","error":{"type":"budget_exceeded_error","message":%q}}`, msg)
	return &http.Response{
		StatusCode: http.StatusPaymentRequired,
		Status:     "402 Payment Required",
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
		},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

// guardedHTTPClient returns an *http.Client whose transport is fronted
// by the shared circuit breaker. base may be nil (uses
// http.DefaultTransport). Both vendor SDKs accept an *http.Client, so
// this one helper covers every provider construction.
func guardedHTTPClient(base *http.Client) *http.Client {
	var baseTransport http.RoundTripper
	c := &http.Client{}
	if base != nil {
		*c = *base
		baseTransport = base.Transport
	}
	c.Transport = &guardedTransport{base: baseTransport, guard: sharedLLMGuard}
	return c
}

// --- small env helpers (local to keep the guard self-contained) ------------

func envBoolDefault(key string, def bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

func envIntDefault(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	if n, err := strconv.Atoi(v); err == nil {
		return n
	}
	return def
}

func envFloatDefault(key string, def float64) float64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return f
	}
	return def
}
