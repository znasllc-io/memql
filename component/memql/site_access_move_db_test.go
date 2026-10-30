package memql

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
)

// site_access_move_db_test.go -- a deployable's organization cannot change while
// it has live access codes (deployable access codes, review of Task 3, finding 2;
// design record decision D15).
//
// ===========================================================================
// WHAT WENT WRONG
// ===========================================================================
// A code COPIES its site's accountId when it is minted, and both things that
// decide an act on it read the copy: the row tier's account arm, and the
// organization gate (which asks the code's organization, not the site's, so an
// act needs no second read). Moving the site left the copy behind. The review
// moved a site from Acme to Beta and found both halves wrong at once -- a person
// of Acme could still regenerate the code on a site they could no longer read,
// and Beta, the site's organization now, saw an empty panel for codes it could not
// act on.
//
// The ruling is to refuse the move, because the alternative is a writer for a
// @serverWritten row that exists for nothing else. These tests pin the refusal
// from every side that matters: it holds for a tied site and for an untied one,
// the refusal says what to do, a code that was removed stops blocking, and only
// THIS site's codes block.

func TestASiteWithLiveAccessCodesCannotChangeOrganization(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	previousGrants, previousMembers := auth.InstalledGrantSource(), auth.InstalledMembershipSource()
	eng.InstallGrantResolution()
	t.Cleanup(func() { auth.SetGrantSource(previousGrants); auth.SetMembershipSource(previousMembers) })

	ids := newSiteAccessIDs("move")
	acme, beta := "acme-"+ids.account, "beta-"+ids.account
	owner, operator := ids.owner, "operator-"+ids.owner
	site := func(name string) string { return "v1:platform:site:" + ids.site + "-" + name }

	seedPrincipal(t, eng, owner, auth.RoleWriter)
	seedPrincipal(t, eng, operator, auth.RoleOwner)
	for _, account := range []string{acme, beta} {
		require.NoError(t, organizationInsert(t, eng, groupSeedCtx(), conceptAccountsAccount, account,
			map[string]any{"name": account, "status": "active", "domainStatus": "unverified"}))
		seedGroup(t, eng, "g-"+account, account, "account", account)
	}
	addSite := func(name, account string) string {
		t.Helper()
		id := site(name)
		payload := map[string]any{
			"hostname": ids.site + "-" + name + "." + siteHostnamePolicyDomain(), "kind": "spa", "status": "draft",
			"bundleRef": "blob://site-access/v1", "ownerUserId": "v1:identity:user:" + owner,
		}
		if account != "" {
			payload["accountId"] = account
		}
		require.NoError(t, organizationInsert(t, eng, groupSeedCtx(), conceptPlatformSite, id, payload))
		return id
	}

	issued := time.Now().UTC().Truncate(time.Second)
	internal := auth.ContextWithInternalOrigin(partGateActor(owner, auth.RoleWriter))
	mint := func(code, siteID, account string, expires time.Time) {
		t.Helper()
		args := map[string]any{
			"codeId": code, "siteId": siteID, "label": "Reviewers", "gate": "dashboard",
			"codeHash": siteAccessDigest(byte(len(code))), "codeHint": "FBZA",
			"issuedAt": issued.Format(time.RFC3339), "expiresAt": expires.Format(time.RFC3339),
			"lastChangedBy": owner,
		}
		if account != "" {
			args["accountId"] = account
		}
		_, err := eng.Execute(internal, siteAccessCall(t, "mutation", "createSiteAccessCode", args))
		require.NoError(t, err)
	}
	remove := func(code string) {
		t.Helper()
		_, err := eng.Execute(internal, siteAccessCall(t, "mutation", "removeSiteAccessCode", map[string]any{
			"codeId": code, "revokedAt": issued.Add(time.Minute).Format(time.RFC3339), "lastChangedBy": owner,
		}))
		require.NoError(t, err)
	}
	// The move is the page's: the operator picks an organization for the site.
	move := func(actor context.Context, siteID, account string) error {
		_, err := eng.Execute(actor, siteAccessCall(t, "mutation", "updateSiteAccount", map[string]any{"siteId": siteID, "accountId": account}))
		return err
	}
	accountOf := func(siteID string) string {
		t.Helper()
		account, _ := latestPayload(t, ctx, db, conceptPlatformSite, siteID)["accountId"].(string)
		return BareShortId(account)
	}
	operatorCtx := partGateActor(operator, auth.RoleOwner)
	refused := func(t *testing.T, err error, what string) {
		t.Helper()
		require.Error(t, err, "%s was allowed", what)
		require.Contains(t, err.Error(), "organization_transfer_refused", "%s was refused, but not by the transfer check: %v", what, err)
		require.Contains(t, err.Error(), "access codes", "the refusal does not say what is in the way: %v", err)
		require.Contains(t, err.Error(), "remove", "the refusal does not say what to do: %v", err)
	}

	t.Run("a tied site with a live code cannot move to another organization", func(t *testing.T) {
		tied := addSite("tied", acme)
		code := ids.code + "-tied"
		mint(code, tied, acme, issued.Add(time.Hour))

		refused(t, move(operatorCtx, tied, beta), "moving a site with a live code to another organization")
		require.Equal(t, BareShortId(acme), accountOf(tied), "the refused move changed the site's organization")

		// Moving to the organization it already has is no change at all.
		require.NoError(t, move(operatorCtx, tied, acme), "a write that changes nothing was refused")
		require.Equal(t, BareShortId(acme), accountOf(tied))

		// Untying is a change too, and a person meets this check FIRST: it runs
		// before the ownership rule that would stop them clearing an attributed
		// record anyway, so the codes sentence is what they read while a code is
		// live. (Once none is, that rule is what refuses them.)
		refused(t, move(operatorCtx, tied, ""), "clearing the organization of a site with a live code")
		require.Equal(t, BareShortId(acme), accountOf(tied))

		// A code that has been removed stops blocking, so a site that once had a
		// code is not pinned to its organization forever.
		remove(code)
		require.NoError(t, move(operatorCtx, tied, beta), "a removed code still blocks the move")
		require.Equal(t, BareShortId(beta), accountOf(tied))
	})

	t.Run("an untied site with a live code cannot be tied to an organization", func(t *testing.T) {
		untied := addSite("untied", "")
		code := ids.code + "-untied"
		mint(code, untied, "", issued.Add(time.Hour))

		refused(t, move(operatorCtx, untied, acme), "tying a site that has a live code")
		require.Empty(t, accountOf(untied), "the refused tie changed the site's organization")

		remove(code)
		require.NoError(t, move(operatorCtx, untied, acme), "a removed code still blocks the tie")
		require.Equal(t, BareShortId(acme), accountOf(untied))
	})

	t.Run("a code that has expired but not been removed still blocks", func(t *testing.T) {
		// Setting a new expiry revives an expired code, and the revived code must
		// still be judged by the organization that owns the site.
		lapsed := addSite("lapsed", acme)
		mint(ids.code+"-lapsed", lapsed, acme, issued.Add(-time.Hour))

		refused(t, move(operatorCtx, lapsed, beta), "moving a site whose only code has expired")
		require.Equal(t, BareShortId(acme), accountOf(lapsed))
	})

	t.Run("only this site's codes block it", func(t *testing.T) {
		busy, free := addSite("busy", acme), addSite("free", acme)
		mint(ids.code+"-busy", busy, acme, issued.Add(time.Hour))

		require.NoError(t, move(operatorCtx, free, beta), "another deployable's code blocked this one")
		require.Equal(t, BareShortId(beta), accountOf(free))
		refused(t, move(operatorCtx, busy, beta), "moving the deployable that has the code")
	})

	t.Run("a site that never had a code moves as it always did", func(t *testing.T) {
		plain := addSite("plain", acme)
		require.NoError(t, move(operatorCtx, plain, beta))
		require.Equal(t, BareShortId(beta), accountOf(plain))
	})

	t.Run("the cluster acting on its own is held to the same rule", func(t *testing.T) {
		// A maintenance or seed actor is not a person, and the check that skips
		// the dependent-record scan for it must not skip this one: the codes name
		// the organization whoever moves the site. The cluster is also the one
		// actor that can clear an organization, so both changes are asserted.
		synthetic := addSite("syn", acme)
		mint(ids.code+"-syn", synthetic, acme, issued.Add(time.Hour))

		refused(t, move(groupSeedCtx(), synthetic, beta), "the cluster moving a site with a live code")
		refused(t, move(groupSeedCtx(), synthetic, ""), "the cluster clearing the organization of a site with a live code")
		require.Equal(t, BareShortId(acme), accountOf(synthetic))

		remove(ids.code + "-syn")
		require.NoError(t, move(groupSeedCtx(), synthetic, beta), "a removed code still blocks the cluster")
		require.Equal(t, BareShortId(beta), accountOf(synthetic))
	})

	t.Run("clearing the organization is refused while a code is live and works once none is", func(t *testing.T) {
		// Clearing is the change most likely to be skipped, because the new organization is
		// empty. A person meets this check before the ownership rule that would stop them
		// clearing any attributed record, so they read the codes sentence while a code is
		// live. The cluster is the one actor that can clear at all, and it is held to the
		// same sentence, and can clear once no code is live.
		cleared := addSite("clear", acme)
		code := ids.code + "-clear"
		mint(code, cleared, acme, issued.Add(time.Hour))

		refused(t, move(operatorCtx, cleared, ""), "a person clearing the organization of a site with a live code")
		refused(t, move(groupSeedCtx(), cleared, ""), "the cluster clearing the organization of a site with a live code")
		require.Equal(t, BareShortId(acme), accountOf(cleared))

		remove(code)
		require.NoError(t, move(groupSeedCtx(), cleared, ""), "clearing the organization of a site with no live code stopped working")
		require.Empty(t, accountOf(cleared))
	})

	t.Run("a stale code's refusal says who can remove it", func(t *testing.T) {
		// A move racing a mint can leave a code whose copy names the organization the
		// site left. It opens nothing, and it still blocks the next move, and the
		// plain next step would be wrong: the shared gate judges the copy, so the people
		// of the site's own organization are refused the removal it asks of them.
		staleSite := addSite("stale", beta)
		code := ids.code + "-stale"
		mint(code, staleSite, acme, issued.Add(time.Hour))

		err := move(operatorCtx, staleSite, acme)
		refusedStale := func(err error, what string) {
			t.Helper()
			require.Error(t, err, "%s was allowed", what)
			require.Contains(t, err.Error(), "organization_transfer_refused", what)
			require.Contains(t, err.Error(), "access code", what)
			require.Contains(t, err.Error(), "cluster operator", "the refusal sends the person to an act the gate refuses them: %v", err)
		}
		refusedStale(err, "moving a site that has a stale code, even to the organization the copy names")
		require.Equal(t, BareShortId(beta), accountOf(staleSite))

		// A stale code among live ones is still the sentence the person needs.
		mint(ids.code+"-stale-live", staleSite, beta, issued.Add(time.Hour))
		refusedStale(move(operatorCtx, staleSite, acme), "moving a site with a stale code and a live one")

		// Once it is gone the live one is what is in the way, and that is the plain
		// sentence again: its organization's people can remove it themselves.
		remove(code)
		err = move(operatorCtx, staleSite, acme)
		refused(t, err, "moving a site whose stale code is gone and whose live one is not")
		require.NotContains(t, err.Error(), "cluster operator", "a live code does not need a cluster operator")

		remove(ids.code + "-stale-live")
		require.NoError(t, move(operatorCtx, staleSite, acme), "nothing is left in the way")
		require.Equal(t, BareShortId(acme), accountOf(staleSite))
	})

	t.Run("a null or a value that is not a string in the place of the organization is a change too", func(t *testing.T) {
		// The read-merge stores a delta key as given, so a null clears the organization as
		// surely as an empty string does. Reading a key that is not a string as 'no change'
		// let such a write skip this check, and the write was stopped only by the layers
		// after it (the schema's type check for the cluster, the ownership rule for a person).
		// The check must not depend on them, so the sentence is asserted, not just a refusal.
		// A raw write is the only way to send one: the mutation says `args.accountId ?? ""`.
		withCode := addSite("nullcode", acme)
		mint(ids.code+"-nullcode", withCode, acme, issued.Add(time.Hour))
		for name, value := range map[string]any{"a null": nil, "a number": 7} {
			err := organizationInsert(t, eng, groupSeedCtx(), conceptPlatformSite, withCode, map[string]any{"accountId": value})
			refused(t, err, "the cluster clearing the organization of a site with a live code with "+name)
			require.Equal(t, BareShortId(acme), accountOf(withCode))
		}

		// With no live code the check has nothing to say, and the write goes on to the layers
		// after it, which refuse it for its type. The refusal must not be this check's.
		noCode := addSite("nullfree", acme)
		err := organizationInsert(t, eng, groupSeedCtx(), conceptPlatformSite, noCode, map[string]any{"accountId": nil})
		require.Error(t, err, "a null in the place of the organization was accepted")
		require.NotContains(t, err.Error(), "organization_transfer_refused", "the codes check refused a site that has no live code")
		require.Equal(t, BareShortId(acme), accountOf(noCode))
	})
}
