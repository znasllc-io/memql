package memql

// deployer_fleet.go -- a deployed workflow's model calls on its deployer's fleet
// (docs/superpowers/specs/2026-10-31-deployer-fleet-design.md).
//
// An automation in a deployed package runs as the cluster, and the cluster
// reaches only a machine lent to everyone. @usesDeployerFleet lends ONE thing:
// each `builtin ai` call in that workflow is planned, admitted, dispatched and
// counted as the person who staged the package's DSL. That is the rule
// memql#5662 wrote down -- an automation under a person's borrowed authority is
// that person's work -- and this file is the one place a package workflow can
// reach it.
//
// THE CALL BORROWS, NEVER THE RUN (D1). What a workflow reads and writes stays
// what the cluster may, so a bug in one cannot act as a person. The bound context
// lives for one call and the caller's is left as it was.
//
// NOTHING IN THE DSL NAMES THE PERSON (D2). The declaration is a flag, its domain
// comes from where the file was loaded, and the person comes off a row only a
// deploy or a rollback writes.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/common"
)

// deployerFleetActorName is the cluster actor the seam's own two reads run under.
const deployerFleetActorName = "deployer-fleet"

type deployerFleetKey struct{}

type deployerFleetDeclaration struct{ domain string }

// ContextWithDeployerFleet records what the automation now starting declared.
//
// CALLED FOR EVERY AUTOMATION, and an undeclared one CLEARS (D11): a flagged
// workflow calling an unflagged one must not lend it a person. A logic or a query
// the automation calls inherits, being part of the same run.
func ContextWithDeployerFleet(ctx context.Context, declared bool, domain string) context.Context {
	if !declared {
		if ctx.Value(deployerFleetKey{}) == nil {
			return ctx
		}
		return context.WithValue(ctx, deployerFleetKey{}, (*deployerFleetDeclaration)(nil))
	}
	return context.WithValue(ctx, deployerFleetKey{}, &deployerFleetDeclaration{domain: strings.TrimSpace(domain)})
}

// DeployerFleetDeclaration reports whether the automation running on ctx declared
// @usesDeployerFleet, and the domain it was loaded from.
func DeployerFleetDeclaration(ctx context.Context) (string, bool) {
	declared, _ := ctx.Value(deployerFleetKey{}).(*deployerFleetDeclaration)
	if declared == nil {
		return "", false
	}
	return declared.domain, true
}

// DeployerFleetRefusal is why a declared call has no person to act for. It is an
// ordinary `ai` error, so `on error continue` and a workflow's own failure path
// handle it as they do any shut door. Nothing falls back to acting as the cluster.
type DeployerFleetRefusal struct {
	Domain string
	Reason string
}

func (r *DeployerFleetRefusal) Error() string {
	return fmt.Sprintf("deployer_fleet_refused: domain %q: %s", r.Domain, r.Reason)
}

// DeclaredDomainForOrigin is the domain a construct loaded from origin belongs to,
// spelled the way a concept id spells it, which is how package.dslDomains holds it:
// the first segment of the namespace, a namespace.pin honoured (D5).
func DeclaredDomainForOrigin(origin string) string {
	return domainOfNamespace(declaredNamespaceForOrigin(origin))
}

// domainOfNamespace cuts at a colon, which a namespace extends with, and at a slash,
// which is how a nested directory with no pin of its own reads.
func domainOfNamespace(namespace string) string {
	namespace = strings.TrimSpace(namespace)
	if i := strings.IndexAny(namespace, ":/"); i >= 0 {
		return namespace[:i]
	}
	return namespace
}

// ContextForDeployerFleet is the context one `builtin ai` call runs on: the
// caller's own, unless the automation declared @usesDeployerFleet.
func (e *MemQLEngine) ContextForDeployerFleet(ctx context.Context) (context.Context, error) {
	return contextForDeployerFleet(ctx, func(readCtx context.Context, statement string) ([]map[string]any, error) {
		res, err := e.Execute(readCtx, statement)
		if err != nil {
			return nil, err
		}
		return MaterializeRows(res), nil
	}, e.localRouteJobs)
}

// localRouteJobs is the Local route as this cluster has it, its override applied.
func (e *MemQLEngine) localRouteJobs() RouteJobs {
	var override *RouteJobs
	if policies := e.Policies(); policies != nil {
		if jobs, ok := policies.RouteOverride(RouteIdLocal); ok {
			override = &jobs
		}
	}
	spec, _ := BuiltinRouteSpec(RouteIdLocal, override)
	return spec.Jobs
}

// deployerFleetRead runs one of the seam's reads as the cluster.
type deployerFleetRead func(ctx context.Context, statement string) ([]map[string]any, error)

func contextForDeployerFleet(ctx context.Context, read deployerFleetRead, localRoute func() RouteJobs) (context.Context, error) {
	domain, declared := DeployerFleetDeclaration(ctx)
	if !declared {
		return ctx, nil
	}
	// A RUN WITH AN OWNER IS THAT PERSON'S WORK, on whatever route they chose for it.
	if run, inRun := common.RunFromContext(ctx); inRun && strings.TrimSpace(run.OwnerUserId) != "" {
		return ctx, nil
	}
	// THE PROMISE IS THE FLEET, NOT THE ROUTE'S NAME (D8). An owner or a developer can
	// edit Local for the whole cluster, and a source other than a fleet's would take a
	// visitor's click to an app session or a vendor, so that refuses.
	if source := firstNonFleetSource(localRoute()); source != "" {
		return nil, &DeployerFleetRefusal{Domain: domain, Reason: fmt.Sprintf(
			"this cluster's Local route also lists %s, so this call could leave the fleet", source)}
	}
	// A PERSON ALREADY ACTING KEEPS THEIR OWN FLEET (D10): an authored automation's
	// author, or someone resuming the run. It stays on Local, since the run they are
	// resuming may be one a visitor started.
	if acting := actingUserFromContext(ctx); !auth.ActsForNoPerson(ctx, acting) {
		return onLocalRoute(ctx, acting), nil
	}
	person, err := deployerOfDomain(ctx, domain, read)
	if err != nil {
		return nil, err
	}
	bound, err := DeployerCallContext(ctx, person)
	if err != nil {
		return nil, &DeployerFleetRefusal{Domain: domain, Reason: err.Error()}
	}
	return bound, nil
}

func firstNonFleetSource(jobs RouteJobs) string {
	for _, sources := range [][]string{jobs.Fast, jobs.Strong, jobs.Reasoning} {
		for _, source := range sources {
			if !strings.HasPrefix(strings.TrimSpace(source), FleetReferencePrefix) {
				return source
			}
		}
	}
	return ""
}

// DeployerCallContext is ctx for one model call made as person, on the Local route.
//
// THE LOCAL ROUTE (D8). Their machines first, then the ones lent to them, and it never
// falls through: no signed-in app session and no vendor is reached for work a visitor's
// click may have started. A refusal names the route.
func DeployerCallContext(ctx context.Context, person string) (context.Context, error) {
	bound, err := bindDeployerAuthority(ctx, person)
	if err != nil {
		return nil, err
	}
	return onLocalRoute(bound, person), nil
}

func onLocalRoute(ctx context.Context, person string) context.Context {
	choice := common.RouteChoice{Source: RouteSourcePrefix + RouteIdLocal, By: person}
	if run, inRun := common.RunFromContext(ctx); inRun {
		run.Routing = choice
		return common.ContextWithRun(ctx, run)
	}
	return ContextWithGoalRouteChoice(ctx, choice)
}

// deployerOfDomain finds the person whose fleet serves a domain's declared
// workflows, or says why there is none. Every answer but one person refuses.
func deployerOfDomain(ctx context.Context, domain string, read deployerFleetRead) (string, error) {
	refuse := func(reason string) (string, error) {
		return "", &DeployerFleetRefusal{Domain: domain, Reason: reason}
	}
	if domain == "" {
		return refuse("this workflow was not loaded from a domain, so no package can answer for it")
	}
	readCtx := deployerFleetReadContext(ctx)

	statement, err := languageParser.RenderCall("packagesShippingDslDomain", map[string]any{"domain": domain})
	if err != nil {
		return refuse(fmt.Sprintf("could not ask which package ships it: %v", err))
	}
	packages, err := read(readCtx, "query "+statement)
	if err != nil {
		return refuse(fmt.Sprintf("could not read which package ships it: %v", err))
	}
	switch len(packages) {
	case 0:
		return refuse("no active package ships this domain")
	case 1:
	default:
		// D6: a guess here would be a guess about whose machines to use.
		return refuse(fmt.Sprintf("%d active packages ship this domain, so none can answer for it", len(packages)))
	}
	person := strings.TrimSpace(mapString(packages[0], "dslDeployedBy"))
	if auth.NamesNoPerson(person) {
		return refuse("no person is recorded as having staged this package's DSL")
	}
	canonical := canonicalDeployerId + BareShortId(person)

	// D7: the gate an automatic deploy answers, asked again on every call. Read
	// fresh, because a person who just lost the right must not keep the fleet.
	statement, err = languageParser.RenderCall("userByIdSystem", map[string]any{"userId": canonical})
	if err != nil {
		return refuse(fmt.Sprintf("could not ask for the deployer's role: %v", err))
	}
	users, err := read(ContextWithFreshRead(readCtx), "query "+statement)
	if err != nil {
		return refuse(fmt.Sprintf("could not read the deployer's role: %v", err))
	}
	if len(users) == 0 {
		return refuse("the person who staged this package's DSL no longer exists")
	}
	if !auth.CanAuthor(auth.UserContext{Role: auth.Role(mapString(users[0], "role"))}) {
		return refuse("the person who staged this package's DSL may no longer author constructs")
	}
	return canonical, nil
}

const canonicalDeployerId = "v1:identity:user:"

// deployerFleetReadContext is the cluster acting, with internal origin: the package
// row belongs to its owner and the user row is @serverOnly. The caller's forwarded
// authority stays behind, as library review history and the fleet catalog leave
// theirs, because the service actor cannot honour one. The stamp dies at the read.
func deployerFleetReadContext(ctx context.Context) context.Context {
	blank := auth.ContextWithForwardedAuthority(ctx, auth.ForwardedAuthority{})
	return auth.ContextWithInternalOrigin(auth.ContextWithSystemActor(blank, deployerFleetActorName))
}

// bindDeployerAuthority is the person's assertion on ctx, as a verified forwarded authority
// (D9): integrations/pipelinesteps.ownerAuthority's form, at the same rank. A hop to
// the agent holding the machine's stream is refused without one (memql#3205).
func bindDeployerAuthority(ctx context.Context, person string) (context.Context, error) {
	now := time.Now()
	authority, err := auth.ForwardedAuthorityForUser(&auth.AccessContext{UserId: person, Role: auth.RoleWriter}, "", "", time.Time{}, now)
	if err != nil {
		return nil, fmt.Errorf("asserting the deployer's authority for the machine hop: %w", err)
	}
	verified, err := auth.VerifyForwardedAuthority(authority, now)
	if err != nil {
		return nil, fmt.Errorf("asserting the deployer's authority for the machine hop: %w", err)
	}
	return auth.BindForwardedContext(ctx, authority.Principal().Claims, verified, authority), nil
}
