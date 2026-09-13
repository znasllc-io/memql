package memql

// `@requiresCapability("<verb>", "<resource>")` -- THE SIBLING OF
// `@requiresRank` (epic memql#5166, decision D11).
//
// A RANK IS A FLOOR AND A CAPABILITY IS A GRANT, and the distinction is the
// whole reason this exists beside requires_rank.go rather than inside it.
// "developer and above" is a statement about the ladder and stays true however
// the cluster's roles are edited; "holds update on principal" is a statement
// about what a role was given, and a cluster may hold one without the other --
// developer ranks 300 above admin's 200 and holds strictly fewer principal
// verbs, so the two questions have opposite answers on the pair that matters
// most.
//
// WHAT IT REPLACES, and why the replacement is not a rename. The migrated
// constructs were gated by `requiresAdmin`, `requiresOwnerOrAdmin` and
// `requiresDeveloperOrAbove` -- specs comparing the actor's role STRING against
// one or three literals. Three problems, each of which this annotation removes:
//
//   - a slug comparison cannot see a custom role at all. `role == "admin"` is
//     false for a rank-250 role holding every principal verb, so every custom
//     role was refused every surface those specs gated, whatever it held.
//   - the names went stale silently. `requiresDeveloperOrAbove` reads as a
//     floor and IS a three-value set; its own comment says the name "predates
//     the ladder flip and reads a rung too high".
//   - a misspelled spec name is a missing conjunct nothing notices, while a
//     misspelled resource here refuses BOOT. That is the O2 argument
//     @requiresRank already made, applied to the other half of the model.
//
// IT GATES WHO MAY CALL, NEVER WHICH ROWS COME BACK. @rowAuthz keeps that job,
// exactly as it does beside @requiresRank.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
)

// CapabilityRequirement is one (verb, resource) pair a construct requires.
//
// The zero value means "no requirement", which is what nearly every construct
// carries -- so the emptiness test is `Verb == "" && Resource == ""` and a
// HALF-filled value is a load error rather than a weaker requirement. The
// parser stores @requiresCapability's arguments as a list, so a one-argument
// form is exactly what a typo produces, and reading it as half a requirement
// would gate on nothing while still looking like a gate.
type CapabilityRequirement struct {
	Verb     string
	Resource string
}

// declared reports whether a requirement was stated at all.
func (r CapabilityRequirement) declared() bool {
	return strings.TrimSpace(r.Verb) != "" || strings.TrimSpace(r.Resource) != ""
}

// complete reports whether both halves are present.
func (r CapabilityRequirement) complete() bool {
	return strings.TrimSpace(r.Verb) != "" && strings.TrimSpace(r.Resource) != ""
}

func (r CapabilityRequirement) String() string {
	return r.Verb + " on " + r.Resource
}

// refuseBelowRequiredCapability enforces a construct's `@requiresCapability`.
//
// FAILS CLOSED IN BOTH UNRESOLVABLE DIRECTIONS, exactly as its rank sibling
// does. A caller whose role holds nothing is refused; a requirement that is
// half-declared is refused for EVERYONE rather than treated as no requirement,
// which would be a gate that admits the whole cluster. The load-time check
// below is what makes the second case unreachable in a booted engine -- this is
// its runtime backstop, not a duplicate of it.
func (e *MemQLEngine) refuseBelowRequiredCapability(ctx context.Context, fn *Function, name string) error {
	if fn == nil {
		return nil
	}
	required := fn.RequiresCapability
	if !required.declared() {
		return nil
	}
	// INTERNAL ORIGIN passes, for the reason it passes the rank gate and the
	// @serverOnly gate: trusted server-side Go stamped for one call is not a
	// principal, and these rules govern principals. An automation that must
	// call a gated construct on a user's behalf stamps origin exactly as it
	// does for @serverOnly.
	if auth.OriginFromContext(ctx).IsInternal() {
		return nil
	}
	if !required.complete() {
		return fmt.Errorf(
			"%q declares @requiresCapability(%q, %q), which is half a requirement. "+
				"Refusing the call rather than treating an incomplete grant as no grant, "+
				"which would admit every caller (epic memql#5166)",
			name, required.Verb, required.Resource)
	}
	// THE ACTOR-SHAPED QUESTION (epic memql#5296): the subject is the verified
	// caller plus their memoised memberships, and CapableFor overlays the
	// actor's group and user grants on the role catalog's answer. A person
	// handed an app their role lacks is admitted here; a person barred from
	// one their role holds is refused here.
	subject, ok := e.subjectFor(ctx)
	if !ok {
		return fmt.Errorf(
			"%q requires the %s capability and this call carries no caller identity",
			name, required)
	}
	if auth.CapableFor(ctx, subject, required.Verb, required.Resource) {
		return nil
	}
	// The refusal names the requirement and the caller's own role, because the
	// person reading it is usually an operator wondering why a screen is empty.
	// It does NOT name who could do it: that is a directory disclosure on a
	// refusal path, and the rank sibling follows the same rule. Nor does it say
	// which level refused -- a deny is a decision about a person, and naming it
	// on the refusal path would tell them somebody singled them out.
	return fmt.Errorf(
		"%q requires the %s capability; the caller (role %q) does not hold it",
		name, required, string(subject.Role))
}

// refusePlanBelowRequiredCapability enforces every requirement a plan
// collected.
//
// BOTH HALVES, NOT ONE. A requirement enforced only on the direct call is
// bypassed by a query that EXPANDS the gated construct, which is the hole
// refusePlanBelowRequiredRank exists to close for ranks. Sorted, so a query
// expanding two gated constructs names the same one in its refusal every time.
func (e *MemQLEngine) refusePlanBelowRequiredCapability(ctx context.Context, plan *QueryPlan) error {
	if plan == nil || len(plan.RequiredCapabilities) == 0 {
		return nil
	}
	names := make([]string, 0, len(plan.RequiredCapabilities))
	for name := range plan.RequiredCapabilities {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fn := &Function{RequiresCapability: plan.RequiredCapabilities[name]}
		if err := e.refuseBelowRequiredCapability(ctx, fn, name); err != nil {
			return err
		}
	}
	return nil
}

// validateRequiresCapabilitySlugs is the LOAD-time half: every declared
// requirement must name one of the five verbs and a resource this cluster's
// catalog knows.
//
// THIS IS THE REASON D11 CHOSE THE ANNOTATION over a spec conjunct, and it is
// the same argument O2 made for @requiresRank. A misspelled spec name is a
// missing conjunct nothing notices; a misspelled resource here refuses boot,
// with the list in the message so the author does not have to grep the seeds
// for it.
//
// THE RESOURCE VOCABULARY IS OPEN BY DESIGN, which is why the known set is read
// from the CATALOG when one is readable and from the engine's own constants
// otherwise. v1:rbac:capability documents resourceType as "an open string (not
// a closed enum) so product layers can introduce their own resource kinds
// without an engine change" -- so a product bundle that seeds a `campaign`
// resource and gates a construct on it must load, and it does, as soon as its
// seeds are readable. On a FIRST boot, where the catalog is being seeded by the
// very startup this validates, the core constants answer -- which is exactly
// the shape validateRequiresRankSlugs already uses for the base ladder.
func (e *MemQLEngine) validateRequiresCapabilitySlugs(ctx context.Context, fns *FunctionRegistry) []error {
	if fns == nil {
		return nil
	}
	verbs := knownVerbs()
	resources := e.knownResources(ctx)

	var problems []error
	names := make([]string, 0)
	byName := map[string]*Function{}
	fns.Range(func(name string, fn *Function) bool {
		if fn != nil && fn.RequiresCapability.declared() {
			names = append(names, name)
			byName[name] = fn
		}
		return true
	})
	// Sorted so a boot failure names the same construct every time; map order
	// would make one broken declaration look like a flaky build.
	sort.Strings(names)
	for _, name := range names {
		required := byName[name].RequiresCapability
		if !required.complete() {
			problems = append(problems, fmt.Errorf(
				"%s declares @requiresCapability(%q, %q), which is half a requirement. "+
					"It takes exactly two arguments -- a verb and a resource -- and an "+
					"incomplete one would gate on nothing while still reading like a gate. "+
					"Verbs: %s. Resources: %s",
				name, required.Verb, required.Resource,
				strings.Join(verbs, ", "), strings.Join(resources, ", ")))
			continue
		}
		if !vocabularyHas(verbs, strings.TrimSpace(required.Verb)) {
			problems = append(problems, fmt.Errorf(
				"%s declares @requiresCapability(%q, ...), which is not one of the five verbs. "+
					"The verb set is CLOSED and uniform across every resource. Verbs: %s",
				name, required.Verb, strings.Join(verbs, ", ")))
			continue
		}
		if !vocabularyHas(resources, strings.TrimSpace(required.Resource)) {
			problems = append(problems, fmt.Errorf(
				"%s declares @requiresCapability(..., %q), which names no resource any role in "+
					"this cluster holds a grant on. A requirement no role can satisfy refuses "+
					"every caller, so this refuses to load rather than gating a surface into "+
					"silence. Resources: %s",
				name, required.Resource, strings.Join(resources, ", ")))
		}
	}
	return problems
}

// knownVerbs is the closed five, sorted for a stable diagnostic.
func knownVerbs() []string {
	out := []string{
		auth.VerbCreate, auth.VerbDelete, auth.VerbExecute, auth.VerbRead, auth.VerbUpdate,
	}
	sort.Strings(out)
	return out
}

// knownResources reads the resource kinds any role holds a grant on.
//
// From the CATALOG when one is installed, so a product bundle's own resource
// kind is accepted the moment its seeds are readable; from the engine's core
// constants otherwise, which is what answers on a first boot.
func (e *MemQLEngine) knownResources(_ context.Context) []string {
	seen := map[string]bool{}
	if cat := auth.InstalledCapabilityCatalog(); cat != nil {
		if lister, ok := cat.(interface{ Slugs() []string }); ok {
			for _, slug := range lister.Slugs() {
				for _, vr := range cat.Grants(slug) {
					if r := strings.TrimSpace(vr.Resource); r != "" {
						seen[r] = true
					}
				}
			}
		}
	}
	// The core vocabulary is ALWAYS included, catalog or not. It is what the
	// engine's own constructs name, and a first boot -- where the catalog is
	// being seeded by this very startup -- would otherwise refuse every one of
	// them.
	for _, r := range []string{
		auth.ResourceAdmission, auth.ResourceAgent, auth.ResourceConstruct, auth.ResourceData,
		auth.ResourceDeployment, auth.ResourceGroup, auth.ResourcePrincipal, auth.ResourceRole,
	} {
		seen[r] = true
	}
	out := make([]string, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// vocabularyHas is a membership test over a sorted diagnostic list. Named for
// what it tests rather than `contains`, which this package already uses for a
// different question in the MCP read path.
func vocabularyHas(vocabulary []string, value string) bool {
	for _, s := range vocabulary {
		if s == value {
			return true
		}
	}
	return false
}
