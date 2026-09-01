package memql

// SERVER-SIDE SURFACE AUTHORIZATION (epic memql#4832, task memql#4836 --
// decisions D6 and O2).
//
// MemQL OS declares a role requirement on an app, a SECTION and a widget
// (clients/os/src/system/registry.ts), and that requirement's own header
// says what it is worth: "Presentation gating only [...] hiding an app
// here is UX, not a security boundary." Accurate, and it means "a normal
// user cannot reach Accounts" has been, until now, a claim about a
// launcher.
//
// `@requiresRank("<role>")` is the enforced counterpart. BOTH ARE
// PERMANENT AND NEITHER STANDS IN FOR THE OTHER: hiding an action a
// caller cannot take beats letting them click it and reading a refusal,
// and that is a different job from refusing it.
//
// WHY IT IS DECLARED ON A CONSTRUCT (the crux of memql#4836). The
// tempting shape is "send the app id and let the engine check the
// manifest" -- which trusts the client to report which surface it is,
// precisely the thing being gated. An app id from a browser is a claim,
// not a fact. A surface IS a set of constructs, so the honest
// server-side statement is a requirement on the constructs the surface
// calls, and the manifest's `roles` field becomes the presentation
// MIRROR of it: one fact, two readers, the shape the front door and the
// domain derivations already use.
//
// WHY AN ANNOTATION AND NOT A SPEC CONJUNCT (O2, settled here). The spec
// form works today with no engine change -- `requiresOwnerOrAdmin` is the
// precedent, and `requiresDeveloperOrAbove` already exists. It is
// rejected for one reason: dslgate's AdminGateRe recognises gates BY
// NAME, and a gate it does not know is not a gate -- the composition
// rule never runs on a filter that uses one. That is not hypothetical.
// `forgeDeveloper` and `forgeApprover` were live authorization conjuncts
// in production filters the composition rule had never once run on; they
// were correctly written, which was luck rather than a checked property.
// An annotation is checkable at LOAD, which makes a missing or misspelled
// requirement a boot failure rather than a discovery in production.
//
// IT GATES WHO MAY CALL, NEVER WHICH ROWS COME BACK. @rowAuthz keeps that
// job. A floor that also narrowed rows would be a second answer to a
// question the tier already answers.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
)

// refuseBelowRequiredRank enforces a construct's `@requiresRank` floor.
//
// FAILS CLOSED IN BOTH UNRESOLVABLE DIRECTIONS. A caller whose role does
// not resolve ranks 0 and is refused; a floor whose slug does not resolve
// is refused for EVERYONE rather than treated as rank 0, which would be a
// floor that admits the whole cluster. The load-time check below is what
// makes the second case unreachable in a booted engine -- this is its
// runtime backstop, not a duplicate of it.
func (e *MemQLEngine) refuseBelowRequiredRank(ctx context.Context, fn *Function, name string) error {
	if fn == nil {
		return nil
	}
	required := strings.TrimSpace(fn.RequiresRank)
	if required == "" {
		return nil
	}
	// INTERNAL ORIGIN passes, for the reason it passes the write guard:
	// trusted server-side Go stamped for one call is not a principal, and
	// the rank rules govern principals (D4). An automation that must call
	// a floored construct on a user's behalf stamps origin exactly as it
	// does for @serverOnly.
	if auth.OriginFromContext(ctx).IsInternal() {
		return nil
	}
	ladder := e.rankLadder(ctx)
	floor := ladder.rankOf(required)
	if floor == 0 {
		return fmt.Errorf(
			"%q declares @requiresRank(%q), which names no role in this cluster's ladder. "+
				"Refusing the call rather than treating an unresolvable floor as rank 0, "+
				"which would admit every caller (epic memql#4832)",
			name, required)
	}
	ac, _ := auth.AccessFromContext(ctx)
	if ac == nil {
		return fmt.Errorf(
			"%q requires the %q role or above and this call carries no caller identity",
			name, required)
	}
	if ladder.rankOf(string(ac.Role)) >= floor {
		return nil
	}
	// The refusal names the requirement and the caller's own role, because
	// the person reading it is usually an operator wondering why a screen
	// is empty -- and "insufficient permissions" sends them to the wrong
	// place. It does NOT name who could do it: that is a directory
	// disclosure on a refusal path.
	return fmt.Errorf(
		"%q requires the %q role or above; this caller holds %q",
		name, required, string(ac.Role))
}

// validateRequiresRankSlugs is the LOAD-time half: every declared floor
// must name a role the ladder knows.
//
// THIS IS THE REASON O2 CHOSE THE ANNOTATION. A misspelled spec name is a
// missing conjunct nothing notices; a misspelled slug here refuses boot.
// The check runs against the seeded catalog when one is readable and
// against the engine's compiled base ladder otherwise, so a first boot --
// where the catalog is being seeded by the very startup this validates --
// still accepts the five base slugs rather than refusing to start.
func (e *MemQLEngine) validateRequiresRankSlugs(ctx context.Context, fns *FunctionRegistry) []error {
	if fns == nil {
		return nil
	}
	ladder := e.rankLadder(ctx)
	var problems []error
	names := make([]string, 0)
	byName := map[string]*Function{}
	fns.Range(func(name string, fn *Function) bool {
		if fn != nil && strings.TrimSpace(fn.RequiresRank) != "" {
			names = append(names, name)
			byName[name] = fn
		}
		return true
	})
	// Sorted so a boot failure names the same construct every time; map
	// order would make one broken declaration look like a flaky build.
	sort.Strings(names)
	for _, name := range names {
		slug := strings.TrimSpace(byName[name].RequiresRank)
		if ladder.rankOf(slug) > 0 {
			continue
		}
		problems = append(problems, fmt.Errorf(
			"%s declares @requiresRank(%q), which names no role in dsl/rbac. "+
				"A floor that does not resolve ranks 0 and would admit every caller, "+
				"so this refuses to load. Known roles: %s",
			name, slug, strings.Join(ladder.knownSlugs(), ", ")))
	}
	return problems
}

// knownSlugs renders the ladder for a diagnostic, lowest rung first, so
// the message that refuses a typo also shows what to write instead.
func (l roleLadder) knownSlugs() []string {
	out := make([]string, 0, len(l.ranks))
	for slug := range l.ranks {
		out = append(out, slug)
	}
	if len(out) == 0 {
		// No catalog readable: name the compiled base ladder, which
		// rankOf falls back to, rather than an empty list that reads as
		// "no roles exist".
		return []string{"reader", "writer", "admin", "developer", "owner"}
	}
	sort.Slice(out, func(i, j int) bool { return l.ranks[out[i]] < l.ranks[out[j]] })
	return out
}

// refusePlanBelowRequiredRank enforces every floor a plan collected.
//
// Sorted, so a query expanding two floored constructs names the same one
// in its refusal every time; map order would make one broken permission
// look like a flaky failure.
func (e *MemQLEngine) refusePlanBelowRequiredRank(ctx context.Context, plan *QueryPlan) error {
	if plan == nil || len(plan.RequiredRanks) == 0 {
		return nil
	}
	names := make([]string, 0, len(plan.RequiredRanks))
	for name := range plan.RequiredRanks {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := e.refuseBelowRequiredRank(ctx, &Function{RequiresRank: plan.RequiredRanks[name]}, name); err != nil {
			return err
		}
	}
	return nil
}
