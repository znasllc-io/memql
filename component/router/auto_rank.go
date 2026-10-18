package router

// auto_rank.go -- AUTO (design record 2026-10-18-routes, "Auto").
//
// Auto is no route at all: the rules decide every call, exactly as before, and
// the locked prompt-specific rules keep their chains. What Auto adds is the
// ORDER inside the three general shipped chains -- localFirst, fastLocalFirst
// and federationStrongest -- which it ranks, per call, by four signals:
//
//   - SPEED: the median latency each kind of source served this level with
//     lately, from v1:router:call;
//   - RELIABILITY: the share of those calls that succeeded;
//   - COST: zero for a machine of the person's own and for a subscription
//     app, the whole of the scale for a metered vendor;
//   - QUALITY: a fixed, level-aware rank -- a strong vendor model above an
//     app above a local model at reasoning, the three level at quick tasks.
//
// A signal nobody has measured is NEUTRAL (0.5), never a guess. Each is
// normalized across the candidates and weighed by the cluster's Auto balance
// (Speed, Cost, Quality; Reliability weighs their mean, so no setting trades
// it away). Ties keep the chain's own order, which is local first -- so a
// cluster nobody measured yet routes exactly as it did before Auto existed.
//
// A PERSON'S MONTHLY BUDGET bends the order: from 80% of it vendors go after
// every free source whatever their score, and at 100% they are left out of the
// person's calls, the way a run's noPaidProviders ceiling leaves them out.
//
// AND THE COST CEILING STILL HOLDS. The guard asks the ceiling before a vendor
// hop only when a local door came first (walkChain), because a chain that
// STARTS at a vendor is a decision an author made. A chain Auto reordered is
// not, so every vendor hop in it asks the ceiling whatever comes before it.

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
)

// autoRankedPolicies are the general shipped chains Auto orders. Every other
// policy -- a locked rule's, an owner's own -- is walked as written.
var autoRankedPolicies = map[string]bool{
	"localFirst":          true,
	"fastLocalFirst":      true,
	"federationStrongest": true,
}

// autoSignalWindow is how far back the signals look, and autoSignalTTL how
// long one fold is reused: a minute keeps the read off every call, and an
// hour of decisions is enough to rank sources without remembering a machine
// that has since been replaced.
const (
	autoSignalWindow = 2 * time.Hour
	autoSignalTTL    = time.Minute
	autoBudgetTTL    = time.Minute
)

// autoSignal is what the ledger says about one door at one level.
type autoSignal struct {
	MedianMs float64
	Success  float64
	Samples  int
}

type autoSignalKey struct {
	Door  string
	Level airoute.Level
}

// autoState is the router's cache of the two reads Auto needs. Its zero value
// is ready to use.
type autoState struct {
	mu        sync.Mutex
	signals   map[autoSignalKey]autoSignal
	signalsAt time.Time
	budgets   map[string]autoBudgetEntry
}

type autoBudgetEntry struct {
	state string
	at    time.Time
}

type autoRankedKey struct{}

// autoRankedContext marks a walk over a chain Auto reordered, so walkChain
// asks the cost ceiling before EVERY vendor hop in it.
func autoRankedContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, autoRankedKey{}, true)
}

func isAutoRanked(ctx context.Context) bool {
	v, _ := ctx.Value(autoRankedKey{}).(bool)
	return v
}

// rankedEntry is one candidate with its score, for the decision record.
type rankedEntry struct {
	Entry       string
	Score       float64
	Speed       float64
	Reliability float64
	Cost        float64
	Quality     float64
}

// autoRankChain orders a general chain for this call, and returns the note the
// decision record keeps. A chain it does not rank is returned unchanged with
// no note.
func (r *Router) autoRankChain(ctx context.Context, req ResolveRequest, level airoute.Level, policyName string, chain []string, policies *memql.PolicyRegistry) ([]string, string) {
	if !autoRankedPolicies[policyName] || len(chain) < 2 {
		return chain, ""
	}
	balance, _ := policies.AutoBalanceSetting()
	budget := r.autoBudgetState(ctx, req.UserId)
	ranked, scored := rankAutoEntries(chain, level, r.autoSignals(ctx), balance, budget)
	return ranked, autoRankNote(scored, balance, budget, len(ranked) < len(chain))
}

// rankAutoEntries is the decision, as a function of values.
func rankAutoEntries(chain []string, level airoute.Level, signals map[autoSignalKey]autoSignal, balance memql.AutoBalance, budgetState string) ([]string, []rankedEntry) {
	wSpeed := float64(balance.Speed) / 100
	wCost := float64(balance.Cost) / 100
	wQuality := float64(balance.Quality) / 100
	if budgetState == "near" {
		wCost = 1
	}
	wReliability := (wSpeed + wCost + wQuality) / 3

	// The fastest median among the candidates sets the scale: a source twice
	// as slow scores half on speed. Unmeasured is neutral.
	fastest := math.Inf(1)
	for _, e := range chain {
		if s, ok := signals[autoSignalKey{Door: doorFor(e), Level: level}]; ok && s.Samples > 0 && s.MedianMs > 0 {
			fastest = math.Min(fastest, s.MedianMs)
		}
	}
	scored := make([]rankedEntry, 0, len(chain))
	for _, e := range chain {
		door := doorFor(e)
		if budgetState == "over" && door == DoorFederation {
			continue
		}
		speed, reliability := 0.5, 0.5
		if s, ok := signals[autoSignalKey{Door: door, Level: level}]; ok && s.Samples > 0 {
			if s.MedianMs > 0 && !math.IsInf(fastest, 1) {
				speed = fastest / s.MedianMs
			}
			reliability = s.Success
		}
		cost := 0.0
		if door == DoorFederation {
			cost = 1
		}
		quality := autoQuality(e, level)
		score := wSpeed*speed + wCost*(1-cost) + wReliability*reliability + wQuality*quality
		scored = append(scored, rankedEntry{Entry: e, Score: score, Speed: speed, Reliability: reliability, Cost: cost, Quality: quality})
	}
	sort.SliceStable(scored, func(i, j int) bool {
		// NEAR THE BUDGET, every free source goes first whatever the scores:
		// the person is close to the limit they set, and a vendor that scores
		// higher is still the one that spends it.
		if budgetState == "near" {
			fi, fj := scored[i].Cost > 0, scored[j].Cost > 0
			if fi != fj {
				return !fi
			}
		}
		return scored[i].Score > scored[j].Score+1e-9
	})
	out := make([]string, 0, len(scored))
	for _, s := range scored {
		out = append(out, s.Entry)
	}
	return out, scored
}

// autoQuality is the fixed, level-aware quality rank. At quick tasks every
// kind of source is nearly as good as another, so the chain's own local-first
// order decides; the higher the level, the more a strong vendor model and an
// app pull ahead of a local one.
func autoQuality(entry string, level airoute.Level) float64 {
	door := doorFor(entry)
	switch level {
	case airoute.LevelFast:
		if entry == "federation:strongest" {
			return 0.7
		}
		return 0.6
	case airoute.LevelReasoning:
		switch {
		case entry == "federation:strongest":
			return 0.9
		case door == DoorApp:
			return 0.8
		case entry == memql.FleetFastest:
			return 0.4
		}
		return 0.6
	}
	// strong, and anything the router did not name a level for.
	switch {
	case entry == "federation:strongest":
		return 0.8
	case door == DoorApp, entry == memql.FleetStrongest:
		return 0.7
	case entry == memql.FleetFastest:
		return 0.5
	}
	return 0.6
}

// autoRankNote is the one considered entry that says what Auto did.
func autoRankNote(scored []rankedEntry, balance memql.AutoBalance, budgetState string, droppedVendors bool) string {
	parts := make([]string, 0, len(scored))
	for _, s := range scored {
		parts = append(parts, fmt.Sprintf("%s %.2f (speed %.2f, reliability %.2f, cost %.2f, quality %.2f)",
			s.Entry, s.Score, s.Speed, s.Reliability, s.Cost, s.Quality))
	}
	note := fmt.Sprintf("auto ranked %s; balance speed %d, cost %d, quality %d", strings.Join(parts, ", "),
		balance.Speed, balance.Cost, balance.Quality)
	switch budgetState {
	case "near":
		note += "; monthly budget at 80% or more, so vendors come after every free source"
	case "over":
		note += "; monthly budget reached, so vendors are left out"
	}
	if droppedVendors && budgetState != "over" {
		note += "; vendors left out"
	}
	return note
}

// autoSignals folds the recent decisions into per-door, per-level signals,
// reusing one fold for a minute.
func (r *Router) autoSignals(ctx context.Context) map[autoSignalKey]autoSignal {
	if r == nil || r.engine == nil {
		return nil
	}
	r.auto.mu.Lock()
	if r.auto.signals != nil && time.Since(r.auto.signalsAt) < autoSignalTTL {
		out := r.auto.signals
		r.auto.mu.Unlock()
		return out
	}
	r.auto.mu.Unlock()

	rows, err := memql.ReadRouterCallSignals(ctx, r.engine, time.Now().Add(-autoSignalWindow))
	if err != nil {
		// A read that fails ranks on the neutral signals: the chain's own
		// order decides, which is what the cluster did before Auto. Kept for
		// a minute too, so a database that is down is not asked per call.
		r.logger.Warn("router: auto could not read recent decisions; ranking on neutral signals", "error", err)
		r.storeSignals(map[autoSignalKey]autoSignal{})
		return nil
	}
	folded := foldAutoSignals(rows)
	r.storeSignals(folded)
	return folded
}

func (r *Router) storeSignals(s map[autoSignalKey]autoSignal) {
	r.auto.mu.Lock()
	r.auto.signals = s
	r.auto.signalsAt = time.Now()
	r.auto.mu.Unlock()
}

// foldAutoSignals turns decision rows into signals. Cancelled calls say
// nothing about a source; a fallback_used row is a failure of the source it
// names.
func foldAutoSignals(rows []map[string]any) map[autoSignalKey]autoSignal {
	type acc struct {
		latencies []float64
		ok, total int
	}
	accs := map[autoSignalKey]*acc{}
	for _, row := range rows {
		door, _ := row["door"].(string)
		if door == airoute.DoorSession {
			door = DoorApp
		}
		level, _ := row["servedLevel"].(string)
		if level == "" {
			level, _ = row["level"].(string)
		}
		outcome, _ := row["outcome"].(string)
		if door == "" || level == "" || outcome == "cancelled" {
			continue
		}
		key := autoSignalKey{Door: door, Level: airoute.Level(level)}
		a := accs[key]
		if a == nil {
			a = &acc{}
			accs[key] = a
		}
		a.total++
		if outcome == airoute.OutcomeOK {
			a.ok++
			if ms, ok := row["totalDurationMs"].(float64); ok && ms > 0 {
				a.latencies = append(a.latencies, ms)
			}
		}
	}
	out := make(map[autoSignalKey]autoSignal, len(accs))
	for k, a := range accs {
		s := autoSignal{Samples: a.total, Success: float64(a.ok) / float64(a.total)}
		if len(a.latencies) > 0 {
			sort.Float64s(a.latencies)
			s.MedianMs = a.latencies[len(a.latencies)/2]
		}
		out[k] = s
	}
	return out
}

// autoBudgetState is where a person's monthly budget stands, reused for a
// minute: "none", "under", "near" or "over". A call with no person has no
// budget.
func (r *Router) autoBudgetState(ctx context.Context, userId string) string {
	userId = strings.TrimSpace(userId)
	if r == nil || r.engine == nil || userId == "" {
		return "none"
	}
	r.auto.mu.Lock()
	if e, ok := r.auto.budgets[userId]; ok && time.Since(e.at) < autoBudgetTTL {
		r.auto.mu.Unlock()
		return e.state
	}
	r.auto.mu.Unlock()
	state, err := memql.ReadUserBudgetState(ctx, r.engine, userId, time.Now())
	if err != nil {
		r.logger.Warn("router: auto could not read a monthly budget; ranking as if none were set", "error", err)
		state = "none"
	}
	r.auto.mu.Lock()
	if r.auto.budgets == nil {
		r.auto.budgets = map[string]autoBudgetEntry{}
	}
	r.auto.budgets[userId] = autoBudgetEntry{state: state, at: time.Now()}
	r.auto.mu.Unlock()
	return state
}
