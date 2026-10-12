package memql

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// digestLookupSQL is how the keys query spells the digest lookup; a scan that
// carries it enumerated its ids from the digest lookup index.
const digestLookupSQL = "(payload->>'emailDigest')"

func lookupDigest(s string) string { return fmt.Sprintf("%x", md5.Sum([]byte(s))) }

// collapsedScans is capturedScans with no lookup index registered, so the read
// takes the concept-wide collapse it took before lookups existed: the
// reference a lookup read must agree with.
func collapsedScans(t *testing.T, eng *MemQLEngine, hook *latestPlanCapture, ctx context.Context, query string) (*ExecuteResult, []string) {
	t.Helper()
	saved := payloadLookups
	payloadLookups = nil
	defer func() { payloadLookups = saved }()
	return capturedScans(t, eng, hook, ctx, query)
}

// capturedScans executes query with the latest-keys capture on and returns
// the result and the concept scans it issued (the scan and its latest-version
// recheck).
func capturedScans(t *testing.T, eng *MemQLEngine, hook *latestPlanCapture, ctx context.Context, query string) (*ExecuteResult, []string) {
	t.Helper()
	hook.mu.Lock()
	hook.queries = nil
	hook.mu.Unlock()
	result, err := eng.Execute(context.WithValue(ContextWithFreshRead(ctx), latestPlanCaptureKey{}, true), query)
	require.NoError(t, err)
	hook.mu.Lock()
	defer hook.mu.Unlock()
	return result, append([]string(nil), hook.queries...)
}

// A LOOKUP ANSWERS FOR THE LATEST VERSION ONLY. The index holds every version,
// so it finds ids by values they no longer carry; the read must still decide on
// each id's latest version (or the latest one at asOf), exactly as the
// concept-wide collapse did.
func TestLatestScanLookupMatchesOnlyTheLatestVersion(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	hook := &latestPlanCapture{}
	db.AddQueryHook(hook)
	sfx := uniqueSuffix("lookup-latest")
	marker := "lookup-latest:" + sfx
	audienceX, audienceY := "v1:campaigns:audience:x-"+sfx, "v1:campaigns:audience:y-"+sfx
	moved := "v1:campaigns:recipient:moved-" + sfx       // its digest changed
	switched := "v1:campaigns:recipient:switched-" + sfx // its audience changed
	stays := "v1:campaigns:recipient:stays-" + sfx
	lacking := "v1:campaigns:recipient:lacking-" + sfx // written before digests existed
	digest := func(s string) string { return lookupDigest(s + sfx) }
	before := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	after := before.Add(time.Hour)
	var nodes []memorynodes.MemoryNode
	for _, v := range []struct {
		id               string
		at               time.Time
		audience, digest string
	}{
		{moved, before, audienceX, digest("old")}, {moved, after, audienceX, digest("new")},
		{switched, before, audienceX, digest("switched")}, {switched, after, audienceY, digest("switched")},
		{stays, before, audienceX, digest("stays")},
		{lacking, before, audienceX, ""},
	} {
		fields := map[string]any{"ownerUserId": "v1:identity:user:" + sfx, "audienceId": v.audience,
			"email": v.id + "@example.test", "subscriptionStatus": "subscribed", "source": "signup"}
		if v.digest != "" {
			fields["emailDigest"] = v.digest
		}
		payload, err := json.Marshal(fields)
		require.NoError(t, err)
		nodes = append(nodes, memorynodes.MemoryNode{ID: v.id, Concept: "v1:campaigns:recipient", CreatedAt: v.at, CreatedBy: marker, Payload: payload})
	}
	_, err := db.NewInsert().Model(&nodes).Exec(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).Where(`"createdBy" = ?`, marker).Exec(context.Background())
	})

	reader := clusterOwnerCtx("u-lookup-latest")
	query := func(audience, dig string) string {
		return fmt.Sprintf(`concept==v1:campaigns:recipient && payload.audienceId==%q && payload.emailDigest==%q`, audience, dig)
	}
	read := func(audience, dig string) []string {
		t.Helper()
		result, scans := capturedScans(t, eng, hook, reader, query(audience, dig))
		require.NotEmpty(t, scans)
		require.Contains(t, scans[0], digestLookupSQL, "the read must enumerate its ids through the digest lookup")
		collapsed, _ := collapsedScans(t, eng, hook, reader, query(audience, dig))
		require.Equal(t, pageIDs(t, collapsed), pageIDs(t, result), "the lookup must return exactly what the concept-wide collapse returns")
		return pageIDs(t, result)
	}
	require.Empty(t, read(audienceX, digest("old")), "a digest the row no longer carries must not find it")
	require.Equal(t, []string{moved}, read(audienceX, digest("new")))
	require.Empty(t, read(audienceX, digest("switched")), "an audience the row left must not find it")
	require.Equal(t, []string{switched}, read(audienceY, digest("switched")))
	require.Equal(t, []string{stays}, read(audienceX, digest("stays")))

	// A row LACKING the field answers the unset equality, and the lookup --
	// which finds versions carrying a value -- must not be what answers it.
	result, scans := capturedScans(t, eng, hook, reader, query(audienceX, ""))
	require.NotEmpty(t, scans)
	require.NotContains(t, scans[0], digestLookupSQL, "an unset equality must not narrow to a lookup")
	require.Equal(t, []string{lacking}, pageIDs(t, result))

	// An equality under || is not required: narrowing by it would lose the rows
	// the other branch admits.
	result, scans = capturedScans(t, eng, hook, reader, fmt.Sprintf(
		`concept==v1:campaigns:recipient && (payload.emailDigest==%q || payload.audienceId==%q)`, digest("new"), audienceY))
	require.NotEmpty(t, scans)
	require.NotContains(t, scans[0], digestLookupSQL, "an equality under || must not narrow to a lookup")
	require.ElementsMatch(t, []string{moved, switched}, pageIDs(t, result))

	// asOf decides on the latest version AT THAT TIME, through the same path.
	asOf := func(audience, dig string) []memorynodes.MemoryNode {
		t.Helper()
		plan, err := eng.parseWithFunctions(query(audience, dig), nil, nil, false)
		require.NoError(t, err)
		filter, ok := eng.tryCompileCombinedFilter(reader, plan.Root, "")
		require.True(t, ok)
		require.Contains(t, eng.latestScanKeys(plan.Root, &before).String(), digestLookupSQL)
		cutoff := before.Add(time.Minute)
		nodes, err := eng.executeCombinedFilterQuery(reader, plan.Root, filter, &cutoff, 100, nil)
		require.NoError(t, err)
		return nodes
	}
	historical := asOf(audienceX, digest("old"))
	require.Len(t, historical, 1)
	require.Equal(t, moved, historical[0].ID)
	require.True(t, historical[0].CreatedAt.Equal(before), "asOf must answer with the version current at that time")
	require.Empty(t, asOf(audienceX, digest("new")), "a value written after asOf must not match at asOf")
	require.Len(t, asOf(audienceX, digest("switched")), 1, "at asOf the row was still in its first audience")
}

// THE LOOKUP NARROWS WHICH IDS ARE READ, NEVER WHO MAY READ THEM. Every row
// authz term rides the filter applied to each candidate's latest version, as it
// did over the concept-wide collapse. consentEventsBySubscriber is owned,
// cluster-owner and organization (account) scoped, and requires a digest.
func TestLatestScanLookupKeepsRowAuthzAndOrganizationScoping(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	previousGrants, previousMembers := auth.InstalledGrantSource(), auth.InstalledMembershipSource()
	eng.InstallGrantResolution()
	t.Cleanup(func() { auth.SetGrantSource(previousGrants); auth.SetMembershipSource(previousMembers) })
	hook := &latestPlanCapture{}
	db.AddQueryHook(hook)
	sfx := uniqueSuffix("lookup-authz")
	acme, beta := "acme-"+sfx, "beta-"+sfx
	member, builder, stranger, operator := "member-"+sfx, "builder-"+sfx, "stranger-"+sfx, "operator-"+sfx
	seedPrincipal(t, eng, member, auth.RoleWriter)
	seedPrincipal(t, eng, builder, auth.RoleWriter)
	seedPrincipal(t, eng, stranger, auth.RoleWriter)
	seedPrincipal(t, eng, operator, auth.RoleOwner)
	seedGroup(t, eng, "g-"+acme, "Acme", "account", acme)
	seedGroup(t, eng, "g-"+beta, "Beta", "account", beta)
	seedMembership(t, eng, "g-"+acme, member, "active")

	digest := lookupDigest("subscriber@" + sfx)
	seed := func(id, owner, account, dig string) {
		t.Helper()
		payload := map[string]any{"ownerUserId": owner, "emailDigest": dig, "kind": "grant", "source": "signup", "occurredAt": "2026-03-01T00:00:00Z"}
		if account != "" {
			payload["accountId"] = account
		}
		encoded, err := json.Marshal(payload)
		require.NoError(t, err)
		_, err = eng.Execute(groupSeedCtx(), fmt.Sprintf(`insert(%s, id=%s, payload=%s)`,
			langparser.QuoteString("v1:campaigns:consentEvent"), langparser.QuoteString(id), string(encoded)))
		require.NoError(t, err)
	}
	ownMember, peerAcme, outsideBeta, strangers := "ce-member-"+sfx, "ce-acme-"+sfx, "ce-beta-"+sfx, "ce-stranger-"+sfx
	seed(ownMember, member, acme, digest)
	seed(peerAcme, builder, acme, digest)
	seed(outsideBeta, builder, beta, digest)
	seed(strangers, stranger, "", digest)
	seed("ce-other-digest-"+sfx, member, acme, lookupDigest("someone-else@"+sfx))

	query := fmt.Sprintf(`query consentEventsBySubscriber(emailDigest: %s)`, langparser.QuoteString(digest))
	ids := func(result *ExecuteResult) []string {
		out := []string{}
		for _, n := range result.Bundle.GetNodes() {
			out = append(out, BareShortId(n.GetId()))
		}
		return out
	}
	read := func(ctx context.Context) []string {
		t.Helper()
		result, scans := capturedScans(t, eng, hook, ctx, query)
		require.NotEmpty(t, scans)
		require.Contains(t, scans[0], digestLookupSQL, "the read must enumerate its ids through the digest lookup")
		collapsed, scans := collapsedScans(t, eng, hook, ctx, query)
		require.NotEmpty(t, scans)
		require.NotContains(t, scans[0], digestLookupSQL)
		require.ElementsMatch(t, ids(collapsed), ids(result), "the lookup must return exactly what the concept-wide collapse returns")
		return ids(result)
	}
	require.ElementsMatch(t, []string{ownMember, peerAcme}, read(rankActorCtx(member, auth.RoleWriter)), "own row plus the organization's")
	require.Empty(t, read(rankActorCtx(builder, auth.RoleWriter)),
		"the organization boundary is mandatory: a row tied to an organization the reader is not in stays hidden, its owner included")
	require.ElementsMatch(t, []string{strangers}, read(rankActorCtx(stranger, auth.RoleWriter)), "own untied row only")
	require.ElementsMatch(t, []string{ownMember, peerAcme, outsideBeta, strangers}, read(clusterOwnerCtx(operator)), "a cluster owner reads every row")
}
