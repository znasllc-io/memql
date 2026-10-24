package memql

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/core/common"
)

// ProviderAdmissionGuard reserves an actual inference attempt durably before
// dispatch. Unlike logical answer receipts it also covers auxiliary embeddings,
// failed attempts and SDK retries. Implementations must serialize peer writes,
// revalidate the owned run, and retain cumulative usage when windows renew.
type ProviderAdmissionGuard interface {
	AdmitProvider(context.Context, common.RunContext, ProviderAdmission) error
}

type ProviderAdmission struct {
	EstimatedCost    float64
	EstimatedTokens  int
	HardCallLimit    int
	HardCostLimit    float64
	HardCostExplicit bool
}

type workProviderGuardKey struct{}

func hasProviderWork(ctx context.Context) bool {
	rc, ok := common.RunFromContext(ctx)
	return ok && rc.IsRun() && rc.GoalId != ""
}

func (s *modelSeam) withProviderAdmission(ctx context.Context) context.Context {
	if rc, ok := common.RunFromContext(ctx); !ok || !rc.IsRun() || rc.GoalId == "" {
		return ctx
	}
	if s != nil {
		if guard, ok := s.ceilings.(ProviderAdmissionGuard); ok {
			return context.WithValue(ctx, workProviderGuardKey{}, guard)
		}
	}
	return ctx
}

func governedProviderWork(ctx context.Context) bool {
	rc, ok := common.RunFromContext(ctx)
	_, wired := ctx.Value(workProviderGuardKey{}).(ProviderAdmissionGuard)
	return ok && rc.IsRun() && rc.GoalId != "" && rc.OwnerUserId != "" && wired
}

func (g *llmGuard) admitWorkProvider(ctx context.Context, cost float64, tokens ...int) (bool, error) {
	if err := context.Cause(ctx); err != nil {
		return false, err
	}
	rc, ok := common.RunFromContext(ctx)
	if !ok || !rc.IsRun() || rc.GoalId == "" {
		return false, nil
	}
	guard, wired := ctx.Value(workProviderGuardKey{}).(ProviderAdmissionGuard)
	if !wired {
		return false, &RunCeilingError{RunId: rc.RunId, StepKey: rc.StepKey, Breach: RunCeilingBreach{Ceiling: "unevaluated", Reason: "owned work provider admission is not wired on this serving node"}}
	}
	if rc.OwnerUserId == "" {
		return false, fmt.Errorf("provider admission requires the work owner")
	}
	policy := ProviderAdmission{EstimatedCost: cost}
	if len(tokens) > 0 {
		policy.EstimatedTokens = tokens[0]
	}
	if g.scopeEnabled {
		policy.HardCostLimit = g.scopeMaxCostUSD
		policy.HardCostExplicit = g.scopeCostExplicit
		if g.scopeCallsExplicit {
			policy.HardCallLimit = g.scopeMaxCalls
		}
	}
	return true, guard.AdmitProvider(ctx, rc, policy)
}
