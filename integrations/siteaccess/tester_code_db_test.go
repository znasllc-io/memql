package siteaccess

// tester_code_db_test.go -- the engine-owned calls-testers gate: offered on a calls
// deployable, never as a guest pass, and the verdict the calls service asks for a
// tester's typed code.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// callsSite writes a package site of this world's deployable with calls turned on.
func (w *world) callsSite(name string, over map[string]any) string {
	w.t.Helper()
	id := name + "-" + w.suffix
	fields := map[string]any{"calls": true}
	for key, value := range over {
		fields[key] = value
	}
	w.siteRow(id, fields)
	return id
}

// forgeGateCode writes a code of any gate straight through the mutation, for a row no
// handler would mint (an out-of-date organization copy).
func (w *world) forgeGateCode(site, codeID, account, gate string, expires time.Time) string {
	w.t.Helper()
	code, digest, hint, err := memql.MintAccessCode()
	require.NoError(w.t, err)
	args := map[string]any{
		"codeId": codeID, "siteId": site, "label": "Forged", "gate": gate,
		"codeHash": digest, "codeHint": hint, "issuedAt": stamp(time.Now()), "expiresAt": stamp(expires),
		"lastChangedBy": w.owner,
	}
	if account != "" {
		args["accountId"] = account
	}
	w.mutate(w.asOwner(), "createSiteAccessCode", args)
	return code
}

func (w *world) gatesOn(caller context.Context, site string) map[string]map[string]any {
	w.t.Helper()
	nodes, err := w.in.handleGates(caller, map[string]any{"siteId": site}, 0)
	require.NoError(w.t, err)
	out := map[string]map[string]any{}
	for _, node := range nodes {
		row := payloadOf(w.t, node)
		out[fmt.Sprint(row["gate"])] = row
	}
	return out
}

func TestGatesOfferCallsTestersOnCallsSites(t *testing.T) {
	w := newWorld(t, "callgates")
	operator := actorContext(w.operator, auth.RoleOwner)
	calls := w.callsSite("ac-calls", nil)
	unpackaged := w.callsSite("ac-calls-unpkg", map[string]any{"packageDeployableName": nil, "packageId": nil})

	got := w.gatesOn(operator, calls)
	require.Contains(t, got, memql.AccessGateCallsTesters, "a calls deployable did not offer the testers gate: %v", got)
	require.Equal(t, false, got[memql.AccessGateCallsTesters]["usesSubject"])
	require.Empty(t, got[memql.AccessGateCallsTesters]["constructs"], "the testers gate opens no DSL construct")
	require.Contains(t, got, "dashboard", "the deployable's own gates must still be offered")

	require.NotContains(t, w.gatesOn(operator, w.site), memql.AccessGateCallsTesters, "a deployable without calls offered the testers gate")

	// THE PACKAGE ONLY MATTERS FOR GATES DECLARED IN DSL: a calls deployable with no
	// package offers the engine's gate and nothing else, and one without calls nothing.
	only := w.gatesOn(operator, unpackaged)
	require.Len(t, only, 1, "a calls site with no package offered more than the testers gate: %v", only)
	require.Contains(t, only, memql.AccessGateCallsTesters)
	plain := "ac-plain-" + w.suffix
	w.siteRow(plain, map[string]any{"packageDeployableName": nil, "packageId": nil})
	require.Empty(t, w.gatesOn(operator, plain), "a site with no package and no calls offered a gate")
}

func TestCreateMintsCallsTestersCodeOnlyOnCallsSites(t *testing.T) {
	w := newWorld(t, "callmint")
	operator := actorContext(w.operator, auth.RoleOwner)
	calls := w.callsSite("ac-calls", nil)

	codeID, code := w.mint(operator, map[string]any{"siteId": calls, "gate": memql.AccessGateCallsTesters, "label": "Dana"})
	require.Regexp(t, codeFormat, code)
	require.Equal(t, memql.AccessGateCallsTesters, w.row(codeID)["gate"])

	_, err := w.in.handleCreate(operator, w.createArgs(map[string]any{"gate": memql.AccessGateCallsTesters}), 0)
	requireSentence(t, err)
	require.Zero(t, w.codesOn(w.site), "a testers code was minted on a deployable without calls")

	unpackaged := w.callsSite("ac-calls-unpkg", map[string]any{"packageDeployableName": nil, "packageId": nil})
	_, code = w.mint(operator, map[string]any{"siteId": unpackaged, "gate": memql.AccessGateCallsTesters, "label": "Sam"})
	require.Regexp(t, codeFormat, code, "a calls site with no package must take a testers code")
	_, err = w.in.handleCreate(operator, w.createArgs(map[string]any{"siteId": unpackaged, "gate": "dashboard"}), 0)
	requireSentence(t, err)
}

func TestGuestPassRefusedOnCallsTesters(t *testing.T) {
	w := newWorld(t, "callguest")
	operator := actorContext(w.operator, auth.RoleOwner)
	calls := w.callsSite("ac-calls", nil)

	_, err := w.in.handleGuestPassCreate(operator, w.guestArgs(map[string]any{"siteId": calls, "gate": memql.AccessGateCallsTesters}), 0)
	require.Equal(t, "Guests cannot open calls-testers. Add a code for each tester.", requireSentence(t, err))
	require.Zero(t, w.passesOn(calls))
}

func TestTesterCodeVerdicts(t *testing.T) {
	w := newWorld(t, "testercode")
	operator := actorContext(w.operator, auth.RoleOwner)
	calls := w.callsSite("ac-calls", nil)
	other := w.callsSite("ac-calls-other", nil)
	unpackaged := w.callsSite("ac-calls-unpkg", map[string]any{"packageDeployableName": nil, "packageId": nil})
	store := NewGraphStore(liveEngine{w.eng})
	now := time.Now()
	verdict := func(site, typed string) TesterCodeVerdict {
		t.Helper()
		v, err := store.TesterCode(context.Background(), site, typed, now)
		require.NoError(t, err)
		return v
	}

	codeID, code := w.mint(operator, map[string]any{"siteId": calls, "gate": memql.AccessGateCallsTesters, "label": "Dana"})
	require.Equal(t, TesterCodeLive, verdict(calls, code))
	require.Equal(t, TesterCodeLive, verdict(calls, strings.ToLower(strings.ReplaceAll(code, "-", " – "))),
		"the access-code normalization must apply: lowercase, spaces and en dashes")
	require.Equal(t, TesterCodeUnknown, verdict(other, code), "a code opened another deployable")
	require.Equal(t, TesterCodeUnknown, verdict(calls, "NOT-A-CODE"))

	_, dashboard := w.mint(operator, map[string]any{"siteId": calls, "gate": "dashboard"})
	require.Equal(t, TesterCodeUnknown, verdict(calls, dashboard), "a code of another gate opened simulated results")

	w.moveExpiry(codeID, now.Add(-time.Minute))
	require.Equal(t, TesterCodeExpired, verdict(calls, code))
	w.moveExpiry(codeID, now.Add(time.Hour))
	require.Equal(t, TesterCodeLive, verdict(calls, code), "a revived code must open again")

	nodes, err := w.in.handleRegenerate(operator, map[string]any{"codeId": codeID}, 0)
	require.NoError(t, err)
	fresh := fmt.Sprint(onlyPayload(t, nodes)["code"])
	require.Equal(t, TesterCodeUnknown, verdict(calls, code), "the value a regenerate replaced still opened")
	require.Equal(t, TesterCodeLive, verdict(calls, fresh))

	w.removeCode(codeID)
	require.Equal(t, TesterCodeUnknown, verdict(calls, fresh), "a removed code still opened")

	moved := w.forgeGateCode(calls, "ac-moved-"+w.suffix, "ac-acct-elsewhere-"+w.suffix, memql.AccessGateCallsTesters, now.Add(time.Hour))
	require.Equal(t, TesterCodeUnknown, verdict(calls, moved), "a code whose organization copy is out of date opened")

	_, unpackagedCode := w.mint(operator, map[string]any{"siteId": unpackaged, "gate": memql.AccessGateCallsTesters, "label": "Sam"})
	require.Equal(t, TesterCodeLive, verdict(unpackaged, unpackagedCode), "a calls site with no package must open its testers code")
	require.Equal(t, TesterCodeUnavailable, verdict(w.site, unpackagedCode), "a site without calls cannot hold tester codes and must say so")
}

func TestCallsTestersCodesAreForClusterOwners(t *testing.T) {
	w := newWorld(t, "callowners")
	operator := actorContext(w.operator, auth.RoleOwner)
	developer := "ac-developer-" + w.suffix
	w.principal(developer, auth.RoleDeveloper)
	dev := actorContext(developer, auth.RoleDeveloper)
	calls := w.callsSite("ac-calls", nil)

	// THE GATES SAY WHAT THE CALLER MAY DO, so the panel offers only legal acts: the
	// testers gate is the engine's own and, for anybody but a cluster owner, not theirs to
	// issue codes for.
	devGates := w.gatesOn(dev, calls)
	require.Equal(t, true, devGates[memql.AccessGateCallsTesters]["engineOwned"])
	require.Equal(t, false, devGates[memql.AccessGateCallsTesters]["issuable"])
	require.Equal(t, false, devGates["dashboard"]["engineOwned"])
	require.Equal(t, true, devGates["dashboard"]["issuable"])
	require.Equal(t, true, w.gatesOn(operator, calls)[memql.AccessGateCallsTesters]["issuable"])

	_, err := w.in.handleCreate(dev, w.createArgs(map[string]any{"siteId": calls, "gate": memql.AccessGateCallsTesters}), 0)
	require.Equal(t, "Only the installation's owners can add tester codes, because they spend the simulated-payment budget the whole installation shares.", requireSentence(t, err))
	w.mint(dev, map[string]any{"siteId": calls, "gate": "dashboard"})

	codeID, _ := w.mint(operator, map[string]any{"siteId": calls, "gate": memql.AccessGateCallsTesters, "label": "Dana"})
	_, err = w.in.handleRegenerate(dev, map[string]any{"codeId": codeID}, 0)
	require.Equal(t, "Only the installation's owners can regenerate a tester code.", requireSentence(t, err))
	_, err = w.in.handleSetExpiry(dev, map[string]any{"codeId": codeID, "expiresAt": after(2 * time.Hour)}, 0)
	require.Equal(t, "Only the installation's owners can change when a tester code expires.", requireSentence(t, err))
	// Removing one takes access away, so anybody who may manage the deployable's codes may.
	_, err = w.in.handleRemove(dev, map[string]any{"codeId": codeID}, 0)
	require.NoError(t, err)
}
