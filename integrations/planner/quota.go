package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
)

// TierEnterprise is the billing tier that always resolves to unlimited
// task concurrency, regardless of any stored maxConcurrentTasks value.
const TierEnterprise = "enterprise"

// EntitlementResolver reads a paying account's task-concurrency entitlement
// (epic memql#902 / foundation child #903) and resolves it to an effective
// cap. The data model lives in dsl/identity (v1:identity:accountEntitlement,
// global scope); this resolver is the seam the admission controller (#904)
// consumes to decide whether a Plan may transition into a running state.
//
// DEFAULT = UNLIMITED. An account with no entitlement row, an enterprise
// tier, or a non-positive maxConcurrentTasks is uncapped -- so the gate is a
// no-op until billing writes a finite cap, and existing / unconfigured
// accounts behave exactly as today (no regression).
type EntitlementResolver struct {
	engine Engine
	logger *slog.Logger
}

// NewEntitlementResolver builds a resolver over the planner integration's
// engine adapter. A nil logger falls back to slog.Default().
func NewEntitlementResolver(engine Engine, logger *slog.Logger) *EntitlementResolver {
	if logger == nil {
		logger = slog.Default()
	}
	return &EntitlementResolver{engine: engine, logger: logger}
}

// Entitlement is an account's resolved task-concurrency cap.
type Entitlement struct {
	// AccountID is the paying account this entitlement was resolved for
	// (v1: a v1:identity:user.id).
	AccountID string
	// Tier is the billing-tier label, carried for the Tasks-UX upgrade /
	// limit messaging (#909). Empty / "enterprise" when no finite cap.
	Tier string
	// MaxConcurrentTasks is the stored cap number. Only meaningful when
	// Unlimited is false; 0 when Unlimited is true.
	MaxConcurrentTasks int
	// Unlimited is true when the account has no finite cap (no row,
	// enterprise tier, or a stored cap <= 0). When true, the admission
	// controller bypasses slot accounting entirely.
	Unlimited bool
}

// Resolve returns the account's effective concurrency entitlement.
//
// It never fails closed: on a missing account id, an unconfigured engine, or
// any read / parse error it returns Unlimited=true. The cap is an optimization
// over the default-uncapped behavior, not a safety gate, so a transient lookup
// problem must not wedge dispatch -- the worst case of erring open is that an
// account briefly exceeds its cap, which self-corrects on the next admission.
func (r *EntitlementResolver) Resolve(ctx context.Context, accountId string) Entitlement {
	accountId = strings.TrimSpace(accountId)
	unlimited := Entitlement{AccountID: accountId, Tier: TierEnterprise, Unlimited: true}

	if accountId == "" || r.engine == nil {
		return unlimited
	}

	q := fmt.Sprintf(`queryAccountEntitlement({accountId:%q})`, accountId)
	res, err := r.engine.Execute(ctx, q)
	if err != nil {
		r.logger.Warn("entitlement resolve: query failed; defaulting to unlimited",
			"account_id", accountId,
			"error", err,
		)
		return unlimited
	}

	row, ok := latestEntitlementRow(res)
	if !ok {
		// No configured cap -> unlimited (the no-regression default).
		return unlimited
	}

	ent := Entitlement{
		AccountID:          accountId,
		Tier:               row.tier,
		MaxConcurrentTasks: row.maxConcurrentTasks,
	}
	// Enterprise is always unlimited; a non-positive stored cap is the
	// unlimited sentinel for every other tier.
	if row.tier == TierEnterprise || row.maxConcurrentTasks <= 0 {
		ent.Unlimited = true
		ent.MaxConcurrentTasks = 0
	}
	return ent
}

// entitlementRow is the parsed subset of accountEntitlementFull the resolver
// needs.
type entitlementRow struct {
	id                 string
	tier               string
	maxConcurrentTasks int
	createdAt          string
}

// latestEntitlementRow picks the newest time-series version from a
// queryAccountEntitlement result. The deterministic per-account id means there
// is normally one current row; if a reader sees more than one version, the
// latest createdAt wins (RFC3339 timestamps compare lexically).
func latestEntitlementRow(res any) (entitlementRow, bool) {
	rows := entitlementRowsFromExecuteResult(res)
	if len(rows) == 0 {
		return entitlementRow{}, false
	}
	best := rows[0]
	for _, row := range rows[1:] {
		if row.createdAt > best.createdAt {
			best = row
		}
	}
	return best, true
}

// entitlementRowsFromExecuteResult unpacks queryAccountEntitlement's shape()
// result into entitlementRow values. Mirrors planRowsFromExecuteResult: a
// single-row match returns the lone map unwrapped, a multi-row match returns
// []any, and an empty match returns []any{}.
func entitlementRowsFromExecuteResult(res any) []entitlementRow {
	resultMap, ok := res.(map[string]any)
	if !ok {
		return nil
	}
	var raw []map[string]any
	switch data := resultMap["data"].(type) {
	case map[string]any:
		raw = append(raw, data)
	case []any:
		for _, item := range data {
			if m, ok := item.(map[string]any); ok {
				raw = append(raw, m)
			}
		}
	case []map[string]any:
		raw = data
	}
	out := make([]entitlementRow, 0, len(raw))
	for _, m := range raw {
		row := entitlementRow{}
		if v, ok := m["id"].(string); ok {
			row.id = v
		}
		if v, ok := m["tier"].(string); ok {
			row.tier = v
		}
		if v, ok := m["createdAt"].(string); ok {
			row.createdAt = v
		}
		row.maxConcurrentTasks = entInt(m["maxConcurrentTasks"])
		out = append(out, row)
	}
	return out
}

// entInt coerces a JSON-decoded numeric field to int. shape() values arrive as
// float64 (encoding/json), but be defensive about int / int64 / json.Number /
// string forms too so the resolver is robust across engine result shapes.
func entInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return clampInt64ToInt(n, 0)
	case float64:
		return clampFloatToInt(n, 0)
	case float32:
		return clampFloatToInt(float64(n), 0)
	case json.Number:
		i, _ := n.Int64()
		return clampInt64ToInt(i, 0)
	case string:
		i, _ := strconv.Atoi(strings.TrimSpace(n))
		return i
	}
	return 0
}
