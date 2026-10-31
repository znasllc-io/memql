package memql

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

// deployer_fleet_test.go -- the model call's identity, on the seam and nowhere
// else (docs/superpowers/specs/2026-10-31-deployer-fleet-design.md).

const fleetTestPerson = "v1:identity:user:alice"

// fleetFacts answers the two reads the seam makes.
type fleetFacts struct {
	packages []map[string]any
	role     auth.Role
	readErr  error
	roleErr  error
	asked    []string
}

func (f *fleetFacts) read(_ context.Context, statement string) ([]map[string]any, error) {
	f.asked = append(f.asked, statement)
	if len(statement) > 0 && statementIs(statement, "packagesShippingDslDomain") {
		return f.packages, f.readErr
	}
	if f.roleErr != nil {
		return nil, f.roleErr
	}
	if f.role == "" {
		return nil, nil
	}
	return []map[string]any{{"role": string(f.role)}}, nil
}

func statementIs(statement, name string) bool {
	return len(statement) >= len("query ")+len(name) && statement[len("query "):len("query ")+len(name)] == name
}

func flaggedContext(domain string) context.Context {
	ctx := auth.ContextWithSystemActor(context.Background(), "automation-under-test")
	return ContextWithDeployerFleet(ctx, true, domain)
}

func onePackage(deployer string) []map[string]any {
	return []map[string]any{{"id": "pkg", "dslDeployedBy": deployer}}
}

func TestAWorkflowThatDeclaredNothingIsUntouched(t *testing.T) {
	facts := &fleetFacts{}
	ctx := auth.ContextWithSystemActor(context.Background(), "plain")
	got, err := contextForDeployerFleet(ctx, facts.read)
	require.NoError(t, err)
	require.Equal(t, ctx, got)
	require.Empty(t, facts.asked, "a workflow that declared nothing read a package")
}

// D10: a person already acting wins.
func TestAPersonAlreadyActingIsUntouched(t *testing.T) {
	facts := &fleetFacts{}
	ctx := ContextWithDeployerFleet(auth.ContextWithUserActor(context.Background(), "v1:identity:user:bob"), true, "shop")
	got, err := contextForDeployerFleet(ctx, facts.read)
	require.NoError(t, err)
	require.Equal(t, ctx, got)
	require.Empty(t, facts.asked)
}

// A goal's run belongs to its owner; a blank-owner run is the deployment's.
func TestARunWithAnOwnerIsUntouched(t *testing.T) {
	facts := &fleetFacts{}
	ctx := common.ContextWithRun(flaggedContext("shop"), common.RunContext{RunId: "r", OwnerUserId: "v1:identity:user:bob"})
	got, err := contextForDeployerFleet(ctx, facts.read)
	require.NoError(t, err)
	require.Equal(t, ctx, got)
	require.Empty(t, facts.asked)
}

func TestAFlaggedCallBorrowsTheDeployerForThatCall(t *testing.T) {
	facts := &fleetFacts{packages: onePackage(fleetTestPerson), role: auth.RoleDeveloper}
	ctx := common.ContextWithRun(flaggedContext("shop"), common.RunContext{RunId: "run-1", StepKey: "s"})

	got, err := contextForDeployerFleet(ctx, facts.read)
	require.NoError(t, err)

	// The call acts for the person...
	require.Equal(t, fleetTestPerson, actingUserFromContext(got))
	require.False(t, auth.ActsForNoPerson(got, actingUserFromContext(got)))

	// ...with the assertion a hop to another node requires (D9)...
	authority, ok := auth.ForwardedAuthorityFromContext(got)
	require.True(t, ok, "a hop would be refused with no_forwarded_authority")
	require.Equal(t, fleetTestPerson, authority.Subject)
	require.Equal(t, auth.RoleWriter, authority.Role)

	// ...on the Local route, keeping the run it is part of (D8).
	run, ok := common.RunFromContext(got)
	require.True(t, ok)
	require.Equal(t, "run-1", run.RunId)
	require.Equal(t, "s", run.StepKey)
	require.Equal(t, RouteSourcePrefix+RouteIdLocal, run.Routing.Source)
	require.Equal(t, fleetTestPerson, run.Routing.By)
	require.Empty(t, run.OwnerUserId, "the run stays the deployment's")

	// And the caller's own context is not changed (D1).
	require.NotEqual(t, fleetTestPerson, actingUserFromContext(ctx))
	_, stamped := auth.ForwardedAuthorityFromContext(ctx)
	require.False(t, stamped)
	before, _ := common.RunFromContext(ctx)
	require.True(t, before.Routing.IsZero())
}

// What the router does with the choice, end to end through ApplyRunRouting.
func TestABorrowedCallResolvesToTheLocalRoute(t *testing.T) {
	facts := &fleetFacts{packages: onePackage(fleetTestPerson), role: auth.RoleOwner}
	ctx := common.ContextWithRun(flaggedContext("shop"), common.RunContext{RunId: "run-1"})
	got, err := contextForDeployerFleet(ctx, facts.read)
	require.NoError(t, err)
	require.Equal(t, RouteIdLocal, routedPersonRoute(t, got))
}

func TestACallOutsideARunStillGetsTheLocalRoute(t *testing.T) {
	facts := &fleetFacts{packages: onePackage(fleetTestPerson), role: auth.RoleOwner}
	got, err := contextForDeployerFleet(flaggedContext("shop"), facts.read)
	require.NoError(t, err)
	require.Equal(t, RouteIdLocal, routedPersonRoute(t, got))
}

// routedPersonRoute is the route the router would walk for a structured call made
// on ctx.
func routedPersonRoute(t *testing.T, ctx context.Context) string {
	t.Helper()
	req, err := ApplyRunRouting(ctx, airoute.ResolveRequest{Level: airoute.LevelFast, Modality: airoute.ModalityStructured})
	require.NoError(t, err)
	require.Equal(t, fleetTestPerson, req.PersonRouteOwner)
	return req.PersonRoute
}

func TestTheDeployerFleetRefusals(t *testing.T) {
	boom := errors.New("boom")
	for _, tc := range []struct {
		name   string
		domain string
		facts  fleetFacts
	}{
		{"a workflow loaded from no domain", "", fleetFacts{}},
		{"no package ships the domain", "shop", fleetFacts{}},
		{"two packages ship the domain (D6)", "shop", fleetFacts{packages: append(onePackage(fleetTestPerson), onePackage(fleetTestPerson)...), role: auth.RoleOwner}},
		{"the package read failed", "shop", fleetFacts{readErr: boom}},
		{"nobody is recorded as staging it", "shop", fleetFacts{packages: onePackage(""), role: auth.RoleOwner}},
		{"the recorded actor is no person", "shop", fleetFacts{packages: onePackage("system:automation:x"), role: auth.RoleOwner}},
		{"the person no longer exists (D7)", "shop", fleetFacts{packages: onePackage(fleetTestPerson)}},
		{"the role read failed (D7)", "shop", fleetFacts{packages: onePackage(fleetTestPerson), roleErr: boom}},
		{"the person may not author constructs (D7)", "shop", fleetFacts{packages: onePackage(fleetTestPerson), role: auth.RoleAdmin}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			facts := tc.facts
			got, err := contextForDeployerFleet(flaggedContext(tc.domain), facts.read)
			require.Nil(t, got)
			var refusal *DeployerFleetRefusal
			require.ErrorAs(t, err, &refusal)
			require.Equal(t, tc.domain, refusal.Domain)
			require.NotEmpty(t, refusal.Reason)
		})
	}
}

func TestDeclaredDomainForOrigin(t *testing.T) {
	require.Equal(t, "shop", domainOfNamespace("shop"))
	require.Equal(t, "shop", domainOfNamespace("shop:orders"))
	require.Equal(t, "shop", domainOfNamespace("shop/orders"))
	require.Equal(t, "", domainOfNamespace(""))
}
