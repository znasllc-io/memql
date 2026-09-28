//go:build agent || planner

package worker

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/num"
)

// The delegation policy is read on the agent (the cockpit-app executor, the
// app door) AND on the planner, whose app door orders `app:*` by the owner's
// appOrder exactly as the agent's does -- one Route must not pick Claude Code
// in triage and Codex in the reply for the same owner.

// DelegationPolicyReader resolves a user's delegation preference.
// Narrow on purpose so its readers do not import the engine.
type DelegationPolicyReader interface {
	DelegationPolicy(ctx context.Context, ownerUserId string) (DelegationPolicy, error)
}

// DelegationPolicy is the subset of v1:worker:delegationPolicy the
// executor reads.
type DelegationPolicy struct {
	Found                  bool
	PreferSubscriptionApps bool
	EligibleKinds          []string
	AppOrder               []string
	MaxConcurrentSessions  int
	WorkspaceRoot          string
	CredentialLifetime     time.Duration
}

// AllowsKind reports whether this policy permits delegating a task of
// the given kind. An empty EligibleKinds list allows NOTHING rather
// than everything: opting into delegation should not silently opt
// every task kind in with it.
func (p DelegationPolicy) AllowsKind(kind string) bool {
	if !p.PreferSubscriptionApps {
		return false
	}
	for _, k := range p.EligibleKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// DelegationPolicy resolves the user's app-delegation preference
// (memql#4362).
//
// The read runs under the OWNER's actor rather than a system one.
// `delegationPolicyForUser` is caller-scoped by design -- it is a
// user's own preference, readable in the portal -- so the engine
// BORROWS the owner's authority the same way the campaign sender
// does, instead of out-ranking them with a system read that would
// also work for anyone else's row.
//
// An absent row means "never delegate": PreferSubscriptionApps stays
// false, so a user who has not opted in is never surprised by an
// agent running on their laptop.
func (s *EngineStore) DelegationPolicy(ctx context.Context, ownerUserId string) (DelegationPolicy, error) {
	if s == nil || s.Engine == nil || strings.TrimSpace(ownerUserId) == "" {
		return DelegationPolicy{}, nil
	}
	// No ownerUserId argument: the query scopes on actor.userId. The engine
	// BORROWS the owner's actor for the read -- the same borrowed-authority
	// pattern the campaign sender uses -- rather than out-ranking them with
	// a system read that would work for anyone else's row too.
	res, err := s.Engine.Execute(
		auth.ContextWithUserActor(ctx, ownerUserId), "query delegationPolicyForUser()")
	if err != nil {
		return DelegationPolicy{}, fmt.Errorf("delegation policy lookup: %w", err)
	}
	// A shape() query lands on the Data axis, not on Bundle.Nodes --
	// reading the wrong one is how a query that works in psql returns
	// nothing here.
	rows := outputPayloadRows(res.OutputPayload())
	if len(rows) == 0 {
		return DelegationPolicy{}, nil
	}
	row := rows[0]
	policy := DelegationPolicy{
		Found:                  true,
		PreferSubscriptionApps: boolFrom(row["preferSubscriptionApps"]),
		EligibleKinds:          stringsFrom(row["eligibleKinds"]),
		AppOrder:               stringsFrom(row["appOrder"]),
		MaxConcurrentSessions:  intFrom(row["maxConcurrentSessions"]),
		WorkspaceRoot:          stringFrom(row["workspaceRoot"]),
	}
	// The seconds are bounded before they become a Duration, and that bound
	// is a DOMAIN rule rather than part of the narrowing (memql#4779):
	// time.Duration is int64 NANOSECONDS, so any seconds count above about
	// 9.2e9 overflows the multiply and lands somewhere arbitrary -- which a
	// saturating read makes reachable where a wrapping one merely made it
	// differently wrong. A credential that outlives the cluster is not a
	// lifetime anyone meant, so it is capped at the longest one that is.
	if secs := intFrom(row["credentialLifetimeSeconds"]); secs > 0 {
		if secs > maxCredentialLifetimeSeconds {
			secs = maxCredentialLifetimeSeconds
		}
		policy.CredentialLifetime = time.Duration(secs) * time.Second
	}
	// Zero reads as the DEFAULT rather than as "none": a zero here
	// would silently disable a feature the user turned on, which is
	// the opposite of what writing 0 into an unset field means.
	if policy.MaxConcurrentSessions <= 0 {
		policy.MaxConcurrentSessions = 1
	}
	return policy, nil
}

func boolFrom(v any) bool {
	b, _ := v.(bool)
	return b
}

func stringFrom(v any) string {
	s, _ := v.(string)
	return s
}

// intFrom reads a delegation-policy field.
//
// SATURATES out of range (memql#4779). Both readings are `> 0` guards, and
// `MaxConcurrentSessions` carries an explicit `<= 0 -> 1` normalization whose
// comment already says zero is the wrong answer here.
func intFrom(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return num.ClampInt64(n)
	case float64:
		return num.ClampFloat64(n)
	}
	return 0
}

func stringsFrom(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// maxCredentialLifetimeSeconds bounds the delegation policy's own field
// before it becomes a time.Duration.
//
// It is 8h because that is what the MINT side already enforces
// (appSessionCredentialMaxTTL in component/identity/http/node_bootstrap.go),
// so a policy asking for longer was never realizable -- this is the read side
// agreeing with the write side rather than a new rule.
const maxCredentialLifetimeSeconds = 8 * 60 * 60
