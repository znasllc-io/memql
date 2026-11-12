package memql

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// THE CALL'S ACCOUNT, RESOLVED (design record
// docs/superpowers/specs/2026-11-12-account-row-ownership-design.md, section C).
//
// CallAccount answers "which account does this call act in". The design has a
// row record that answer when neither its parent nor the write itself names an
// account, and has an update carry a recorded account forward with nothing to
// change it afterwards. So a wrong answer here is meant to last, and each test
// below holds one way of getting it wrong.
//
// The first two tests need no database, and say so by not asking for one: what
// an entry point stated, who is plainly an operator, and who is plainly nobody
// are decided from the context alone. The rest resolve a person against real
// membership rows, real group rows and the real principal table, because every
// claim about "the account they belong to" is a claim about those reads.
// Postgres-gated like their neighbours; CI's db-tests lane sets
// MEMQL_REQUIRE_DB=1, so a skip there is a failure rather than a green.

// TestTheCallAccountKeyAgreesWithBareShortId holds component/auth's own spelling
// of "the bare id" to BareShortId's.
//
// auth cannot import this package, so auth.ContextWithCallAccount restates the
// rule (auth's bareAccountId says why it does not reach for core/id either).
// Two statements of one rule drift, and this drift would be quiet: the key
// would hold one spelling of an account, every reader here would compare it
// against the other, and a row would record an account that matches nothing.
func TestTheCallAccountKeyAgreesWithBareShortId(t *testing.T) {
	for _, spelling := range []string{
		"acme",
		"self",
		"  acme ",
		"v1:accounts:account:acme",
		"v1:accounts:account:self",
		" v1:accounts:account:acme\n",
		// The identity file's own `account` concept, and any other prefix.
		"v1:identity:account:acme",
		"v2:accounts:account:acme",
		"v12:accounts:account:acct-0f3a",
		// A short id that carries colons of its own keeps them.
		"v1:accounts:account:a:b",
		// Segments ahead of the version segment are not part of the short id.
		"legacy:v1:accounts:account:acme",
		// Not decomposable, so returned as it came by both.
		"v1:accounts:account",
		"v1:accounts:account:",
		"v1:accounts",
		"v1",
		"v:accounts:account:acme",
		"vx:accounts:account:acme",
		"acme:beta",
		"acct_0f3a-9",
	} {
		want := BareShortId(spelling)
		got, ok := auth.CallAccountFromContext(auth.ContextWithCallAccount(context.Background(), spelling))
		require.True(t, ok, "%q stated no account", spelling)
		require.Equal(t, want, got, "auth stores %q as %q, and BareShortId answers %q", spelling, got, want)
	}
}

// TestCallAccountFromTheContextAlone is everything CallAccount decides without
// reading a row, on an engine that has no database to read one from.
func TestCallAccountFromTheContextAlone(t *testing.T) {
	engine := newQuietEngine(t)

	owner := rankActorCtx("call-account-owner", auth.RoleOwner)
	member := rankActorCtx("call-account-member", auth.RoleWriter)
	system := auth.ContextWithSystemActor(context.Background(), "edge")
	connector := auth.ContextWithConnectorActor(context.Background(), "shopify")
	automation := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "system:automation:indexTodoOnCreate", Role: auth.RoleReader, Unranked: true, Synthetic: true,
	})
	maintenance := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "system:maintenance:sweep", Role: auth.RoleOwner, Unranked: true, Synthetic: true,
	})
	anonymous := auth.ContextWithAnonymousActor(context.Background())

	t.Run("an account an entry point stated is the answer, whoever the actor is", func(t *testing.T) {
		for name, ctx := range map[string]context.Context{
			"nobody":        context.Background(),
			"an operator":   owner,
			"a person":      member,
			"the cluster":   system,
			"an automation": automation,
			"a connector":   connector,
			"anonymous":     anonymous,
			"a borrowed person, as a shopper form runs": auth.ContextWithUserActor(context.Background(), "call-account-site-owner"),
		} {
			stated := auth.ContextWithCallAccount(ctx, "v1:accounts:account:acme")
			require.Equal(t, "acme", engine.CallAccount(stated), "%s: the stated account was not the answer", name)
		}
		// And a nil engine still reads the key: nothing about it needs one.
		var none *MemQLEngine
		require.Equal(t, "acme", none.CallAccount(auth.ContextWithCallAccount(context.Background(), "acme")))
	})

	t.Run("a cluster operator acts in the primary account", func(t *testing.T) {
		for _, role := range []auth.Role{auth.RoleOwner, auth.RoleDeveloper, auth.RoleAdmin} {
			require.Equal(t, "self", engine.CallAccount(rankActorCtx("call-account-operator", role)),
				"%s is a cluster operator and acts in the primary account", role)
		}
	})

	t.Run("the cluster acting for itself acts in no account", func(t *testing.T) {
		for name, ctx := range map[string]context.Context{
			"no actor at all":           context.Background(),
			"a nil context":             nil,
			"the system actor":          system,
			"an automation's own actor": automation,
			// RoleOwner, like the seed materializer: the role buys the
			// cluster-owner escape and is not a person's rung, so it must not
			// read as "an operator, therefore the primary account".
			"a maintenance sweep": maintenance,
			// The same without the unranked flag. Every synthetic actor in the
			// tree carries both today, and it is the SYNTHETIC one that says
			// the cluster is acting: with only the role to go on, this is an
			// owner and would answer the primary account.
			"the cluster under a ranked owner role": auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "system:maintenance", Role: auth.RoleOwner, Synthetic: true}),
			"a connector":                           connector,
			"the anonymous actor":                   anonymous,
			"an actor with no id":                   auth.ContextWithAccess(context.Background(), &auth.AccessContext{Role: auth.RoleOwner}),
			// A malformed half of either named actor is still not a person,
			// whatever role sits beside it.
			"the anonymous flag on another role": auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "x", Role: auth.RoleOwner, IsAnonymous: true}),
			"a connector name on another role":   auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "x", Role: auth.RoleOwner, ConnectorName: "shopify"}),
		} {
			require.Empty(t, engine.CallAccount(ctx), "%s acts in no account", name)
		}
	})

	t.Run("with no database a person is not guessed into the primary account", func(t *testing.T) {
		// "In no group" is an answer only a read can give, and an engine that
		// cannot make the read states nothing rather than the primary account.
		require.Empty(t, engine.CallAccount(member))
		require.Empty(t, engine.CallAccount(auth.ContextWithUserActor(context.Background(), "call-account-member")))
	})
}

// callAccountFixture is two accounts, the account-kind group of each, and the
// ids every test below derives its own people from.
type callAccountFixture struct {
	engine     *MemQLEngine
	acme, beta string
	gAcme      string
	gBeta      string
	suffix     string
}

func newCallAccountFixture(t *testing.T, name string) callAccountFixture {
	t.Helper()
	engine, _, _ := sharedReadMergeEngine(t)
	suffix := uniqueSuffix(name)
	f := callAccountFixture{engine: engine, suffix: suffix, acme: "acme-" + suffix, beta: "beta-" + suffix}
	f.gAcme, f.gBeta = "g-"+f.acme, "g-"+f.beta
	seedGroup(t, engine, f.gAcme, f.acme, "account", f.acme)
	seedGroup(t, engine, f.gBeta, f.beta, "account", f.beta)
	return f
}

// betweenMembershipWrites puts two writes on different instants. A row version's
// time is the engine's clock at the write, and "joined first" below is a
// statement about the order of those times.
func betweenMembershipWrites() { time.Sleep(15 * time.Millisecond) }

// TestCallAccountOfAPerson is the person's half of the table in the design
// record: the account they belong to, and the primary account for a person in
// none.
func TestCallAccountOfAPerson(t *testing.T) {
	f := newCallAccountFixture(t, "callacct-person")
	engine := f.engine

	t.Run("one membership", func(t *testing.T) {
		member := "member-" + f.suffix
		seedMembership(t, engine, f.gAcme, member, "active")
		require.Equal(t, f.acme, engine.CallAccount(rankActorCtx(member, auth.RoleWriter)))
		require.Equal(t, f.acme, engine.CallAccount(rankActorCtx(member, auth.RoleReader)))
		// The canonical spelling of the same person is the same person.
		require.Equal(t, f.acme, engine.CallAccount(rankActorCtx(conceptIdentityUser+":"+member, auth.RoleWriter)))
		// And the answer is the same on a request that has already resolved its
		// account scope, which is the state every write finds the context in.
		warm := contextWithAccountScopeMemo(rankActorCtx(member, auth.RoleWriter), engine)
		require.NotNil(t, engine.accountScopeFor(warm))
		require.Equal(t, f.acme, engine.CallAccount(warm))
	})

	t.Run("no group at all is the primary account", func(t *testing.T) {
		require.Equal(t, "self", engine.CallAccount(rankActorCtx("loner-"+f.suffix, auth.RoleWriter)))
	})

	t.Run("two groups of one account are one account", func(t *testing.T) {
		member := "twice-" + f.suffix
		custom := "team-" + f.acme
		seedGroup(t, engine, custom, "Acme design team", "custom", f.acme)
		seedMembership(t, engine, f.gAcme, member, "active")
		seedMembership(t, engine, custom, member, "active")
		require.Equal(t, f.acme, engine.CallAccount(rankActorCtx(member, auth.RoleWriter)))
	})

	t.Run("a group tied to no account is not an account", func(t *testing.T) {
		member := "untied-" + f.suffix
		untied := "untied-group-" + f.suffix
		seedGroup(t, engine, untied, "Reading club", "custom", "")
		seedMembership(t, engine, untied, member, "active")
		require.Equal(t, "self", engine.CallAccount(rankActorCtx(member, auth.RoleWriter)),
			"a group that grants no account places nobody in one")
	})

	t.Run("a removed membership places nobody", func(t *testing.T) {
		member := "left-" + f.suffix
		seedMembership(t, engine, f.gAcme, member, "active")
		require.Equal(t, f.acme, engine.CallAccount(rankActorCtx(member, auth.RoleWriter)))
		seedMembership(t, engine, f.gAcme, member, "removed")
		require.Equal(t, "self", engine.CallAccount(rankActorCtx(member, auth.RoleWriter)),
			"the membership's first version still says active; only the newest version counts")
	})

	t.Run("an archived group places nobody", func(t *testing.T) {
		member := "archived-" + f.suffix
		gone := "g-gone-" + f.suffix
		seedGroup(t, engine, gone, "Gone", "account", "gone-"+f.suffix)
		seedMembership(t, engine, gone, member, "active")
		require.Equal(t, "gone-"+f.suffix, engine.CallAccount(rankActorCtx(member, auth.RoleWriter)))
		q := fmt.Sprintf(`mutation writeGroup(groupId: %s, name: "Gone", kind: "account", accountId: %s, status: "archived", archivedAt: %s)`,
			langparser.QuoteString(gone), langparser.QuoteString("gone-"+f.suffix), langparser.QuoteString(time.Now().UTC().Format(time.RFC3339)))
		_, err := engine.Execute(groupSeedCtx(), q)
		require.NoError(t, err)
		require.Equal(t, "self", engine.CallAccount(rankActorCtx(member, auth.RoleWriter)),
			"a group is archived without its memberships being rewritten, so the group's own status decides")
	})

	t.Run("half of a named actor is not a person in no group", func(t *testing.T) {
		// With a database to read, an actor that fell through to the membership
		// branch would be found in no group and answered the primary account.
		// The role that only a named non-person carries stops it before that,
		// even when the other half of that actor is missing.
		for name, role := range map[string]auth.Role{"the anonymous role": auth.RoleAnonymous, "the connector role": auth.RoleConnector} {
			ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "halfnamed-" + f.suffix, Role: role})
			require.Empty(t, engine.CallAccount(ctx), "%s without its flag", name)
		}
	})
}

// TestCallAccountOfAPersonInSeveralAccountsIsTheirOldestMembership: a person in
// several accounts acts in the one they joined first.
//
// "Joined" is the FIRST version of the membership row. A membership has no
// joined-at field and one id for the life of a (group, person) pair, so leaving
// and returning, or any later edit, writes a new version of the same row. A
// reader that took the newest version's time would move a person's work to a
// different account every time one of their memberships was touched.
func TestCallAccountOfAPersonInSeveralAccountsIsTheirOldestMembership(t *testing.T) {
	f := newCallAccountFixture(t, "callacct-several")
	engine := f.engine
	multi := "multi-" + f.suffix
	actor := rankActorCtx(multi, auth.RoleWriter)

	// Beta first, although `acme` sorts first: if the order of the ids decided
	// this, the answer would be acme.
	seedMembership(t, engine, f.gBeta, multi, "active")
	betweenMembershipWrites()
	seedMembership(t, engine, f.gAcme, multi, "active")
	require.Equal(t, f.beta, engine.CallAccount(actor), "beta was joined first")

	// They leave beta. One account is left, and it is the answer.
	betweenMembershipWrites()
	seedMembership(t, engine, f.gBeta, multi, "removed")
	require.Equal(t, f.acme, engine.CallAccount(actor))

	// They come back. The row's newest version is now the newest membership
	// write of all, and its first version is still the oldest.
	betweenMembershipWrites()
	seedMembership(t, engine, f.gBeta, multi, "active")
	require.Equal(t, f.beta, engine.CallAccount(actor),
		"a membership that was left and rejoined is as old as its first version, not its newest")

	// Borrowed authority for the same person resolves the same account.
	seedPrincipal(t, engine, multi, auth.RoleWriter)
	require.Equal(t, f.beta, engine.CallAccount(auth.ContextWithUserActor(context.Background(), multi)))
}

// TestCallAccountTieOnJoinTimeIsBrokenByAccountId: two memberships that began at
// the same instant are ordered by ACCOUNT id, so the answer does not depend on
// which row the database happened to return first.
//
// The rows are written directly, with one createdAt, because no pair of real
// writes can be made to share an instant. The groups are named so their ids, and
// therefore the membership ids, sort the other way round from the accounts: an
// implementation that broke the tie on either would answer beta.
func TestCallAccountTieOnJoinTimeIsBrokenByAccountId(t *testing.T) {
	engine, db, _ := sharedReadMergeEngine(t)
	suffix := uniqueSuffix("callacct-tie")
	acme, beta := "acme-"+suffix, "beta-"+suffix
	groupOfAcme, groupOfBeta := "zz-"+suffix, "aa-"+suffix
	seedGroup(t, engine, groupOfAcme, "Acme, sorting last", "account", acme)
	seedGroup(t, engine, groupOfBeta, "Beta, sorting first", "account", beta)

	twin := "twin-" + suffix
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	for group, account := range map[string]string{groupOfAcme: acme, groupOfBeta: beta} {
		seedRawRow(t, context.Background(), db, conceptIdentityGroupMembership,
			conceptIdentityGroupMembership+":"+group+"-"+twin, at, "call-account-tie",
			map[string]any{
				"groupId": conceptIdentityGroup + ":" + group, "userId": conceptIdentityUser + ":" + twin,
				"accountId": account, "ownerUserId": "", "origin": "added", "status": "active",
			})
	}
	require.Equal(t, acme, engine.CallAccount(rankActorCtx(twin, auth.RoleWriter)))
}

// callAccountCatalog is the seeded rungs plus roles a test adds: `scoped` maps a
// slug to the account it is scoped to, `global` lists slugs with no scope. Every
// added role sits at 250, above admin, so that rank alone would make its holder
// a cluster operator.
type callAccountCatalog struct {
	scoped map[string]string
	global map[string]bool
}

var callAccountSeededRanks = map[string]int{
	"owner": 400, "developer": 300, "admin": 200, "user": 100, "writer": 100, "viewer": 50, "reader": 50,
}

func (c callAccountCatalog) Rank(slug string) (int, bool) {
	if _, ok := c.scoped[slug]; ok || c.global[slug] {
		return 250, true
	}
	rank, ok := callAccountSeededRanks[slug]
	return rank, ok
}
func (c callAccountCatalog) Name(slug string) string         { return slug }
func (callAccountCatalog) Holds(string, string, string) bool { return false }
func (callAccountCatalog) Grants(string) []auth.VerbResource { return nil }
func (c callAccountCatalog) Scope(slug string) string        { return c.scoped[slug] }
func (c callAccountCatalog) Active(slug string) bool         { _, ok := c.Rank(slug); return ok }

// installCallAccountCatalog swaps the process's role catalog for one test and
// puts back whatever was installed, which may be none.
func installCallAccountCatalog(t *testing.T, c callAccountCatalog) {
	t.Helper()
	previous := auth.InstalledCapabilityCatalog()
	auth.SetCapabilityCatalog(c)
	t.Cleanup(func() { auth.SetCapabilityCatalog(previous) })
}

// TestCallAccountOfAScopedRoleIsItsAccount: a person holding a role scoped to one
// account acts in that account.
//
// The role's rank does not make them a cluster operator, however high it is: the
// control below is the same rank with no scope, and it answers the primary
// account. And the scope decides on its own. A person can hold a role scoped to
// acme and a membership in beta that is older, and their account scope reaches
// acme's rows only (resolveAccountScope), so a row stamped with beta would be
// one they could not have been admitted to through that scope.
func TestCallAccountOfAScopedRoleIsItsAccount(t *testing.T) {
	f := newCallAccountFixture(t, "callacct-scoped")
	engine := f.engine
	lead := auth.Role("acme-lead-" + f.suffix)
	global := auth.Role("global-lead-" + f.suffix)

	member, both, stray, borrowed := "lead-"+f.suffix, "lead-both-"+f.suffix, "lead-stray-"+f.suffix, "lead-borrowed-"+f.suffix
	// Everything is seeded BEFORE the catalog below is installed, so that no
	// seed write is judged by a catalog whose roles hold nothing.
	seedMembership(t, engine, f.gAcme, member, "active")
	seedMembership(t, engine, f.gBeta, both, "active")
	// The borrowed person is in beta first as well, so that an actor read as the
	// writer it stands in as would be answered beta.
	seedMembership(t, engine, f.gBeta, borrowed, "active")
	betweenMembershipWrites()
	seedMembership(t, engine, f.gAcme, both, "active")
	seedMembership(t, engine, f.gAcme, borrowed, "active")
	seedPrincipal(t, engine, borrowed, lead)

	installCallAccountCatalog(t, callAccountCatalog{
		scoped: map[string]string{string(lead): conceptAccountsAccount + ":" + f.acme},
		global: map[string]bool{string(global): true},
	})
	require.False(t, auth.IsClusterOperator(lead), "fixture: a scoped role must not be a cluster operator")
	require.True(t, auth.IsClusterOperator(global), "fixture: the same rank with no scope is one")

	require.Equal(t, f.acme, engine.CallAccount(rankActorCtx(member, lead)), "a member of the account their role is scoped to")
	require.Equal(t, f.acme, engine.CallAccount(rankActorCtx(both, lead)),
		"an older membership elsewhere does not outrank the account the role is scoped to")
	require.Equal(t, f.acme, engine.CallAccount(rankActorCtx(stray, lead)),
		"the role is the account's whether or not a membership row says so today")
	require.Equal(t, f.acme, engine.CallAccount(auth.ContextWithUserActor(context.Background(), borrowed)),
		"borrowed authority reads the person's own role, and it is scoped")

	// The control: the scope is what decided the four above.
	require.Equal(t, "self", engine.CallAccount(rankActorCtx(member, global)))
}

// TestCallAccountOfBorrowedAuthorityIsThePersons: work the server does on a
// person's behalf acts in the account that person would.
//
// Borrowed authority and work restored for its owner both carry a STAND-IN role
// (writer, whatever the person holds), so the actor alone would send an
// operator's borrowed work to whichever client group they happen to sit in, and
// their own calls to the primary account. The person's role is read instead.
func TestCallAccountOfBorrowedAuthorityIsThePersons(t *testing.T) {
	f := newCallAccountFixture(t, "callacct-borrowed")
	engine := f.engine

	member, operator, seated := "member-"+f.suffix, "operator-"+f.suffix, "seated-"+f.suffix
	seedPrincipal(t, engine, member, auth.RoleWriter)
	seedMembership(t, engine, f.gAcme, member, "active")
	seedPrincipal(t, engine, operator, auth.RoleOwner)
	// An operator who also sits in a client's group. Staff need no membership to
	// reach a client's rows, and some have one anyway.
	seedPrincipal(t, engine, seated, auth.RoleAdmin)
	seedMembership(t, engine, f.gAcme, seated, "active")

	borrow := func(user string) context.Context { return auth.ContextWithUserActor(context.Background(), user) }
	restore := func(user string) context.Context {
		ctx, err := auth.ContextWithPersistedOwner(context.Background(), user, nil, nil)
		require.NoError(t, err)
		return ctx
	}
	for name, as := range map[string]func(string) context.Context{"borrowed authority": borrow, "a restored owner": restore} {
		require.Equal(t, f.acme, engine.CallAccount(as(member)), "%s for a member", name)
		require.Equal(t, "self", engine.CallAccount(as(operator)), "%s for an operator", name)
		require.Equal(t, "self", engine.CallAccount(as(seated)),
			"%s for an operator seated in a client's group: their own calls act in the primary account, and so does work done for them", name)
		// The direct call is the control for each.
		require.Equal(t, "self", engine.CallAccount(rankActorCtx(seated, auth.RoleAdmin)))
	}

	// A person the principal table does not resolve holds no role anybody can
	// read, so nothing says whether they are an operator. The stand-in is not
	// the fallback: it is the answer this read replaces.
	require.Empty(t, engine.CallAccount(borrow("nobody-"+f.suffix)))
	require.Empty(t, engine.CallAccount(restore("nobody-"+f.suffix)))

	// An account stated for the call still wins over all of it: a shopper form
	// borrows the site's owner and acts in the site's account.
	require.Equal(t, f.beta, engine.CallAccount(auth.ContextWithCallAccount(borrow(member), f.beta)))
}

// TestTheSiteReadOfAShopperFormAnswersTheSitesAccount is the DSL half of the
// shopper surface's account: the read its handler makes, `siteById` under the
// site owner's borrowed authority, has to hand back the site's account, because
// that row is the only place the handler learns it (app's shopperSiteReader,
// whose own projection is tested beside it with no engine).
//
// The owner here belongs to two accounts, beta first, and the site to acme,
// which is one of the two cases the entry point exists for (the other is an
// operator, who owns clients' sites and acts in the primary account). Left to
// the actor the call would act in beta, the owner's oldest membership, and what
// a shopper submitted on acme's storefront would be recorded there. An owner in
// neither of a site's accounts is not a case at all: the site reads as zero rows
// under them, and the surface answers nothing.
func TestTheSiteReadOfAShopperFormAnswersTheSitesAccount(t *testing.T) {
	f := newCallAccountFixture(t, "callacct-site")
	engine := f.engine

	merchant := "merchant-" + f.suffix
	seedPrincipal(t, engine, merchant, auth.RoleWriter)
	seedMembership(t, engine, f.gBeta, merchant, "active")
	betweenMembershipWrites()
	seedMembership(t, engine, f.gAcme, merchant, "active")
	site := "shop-" + f.suffix
	seedSiteFor(t, engine, site, merchant, f.acme)

	borrowed := auth.ContextWithUserActor(context.Background(), merchant)
	result, err := engine.Execute(borrowed, fmt.Sprintf(`query siteById(siteId: %s)`, langparser.QuoteString(site)))
	require.NoError(t, err)
	rows := MaterializeRows(result)
	require.Len(t, rows, 1, "the site's owner could not read their own site")
	account := stringFromAny(rows[0]["accountId"])
	require.Equal(t, f.acme, BareShortId(account),
		"siteById's projection no longer answers the site's account, so a shopper form has nowhere to learn it")

	require.Equal(t, f.beta, engine.CallAccount(borrowed), "control: the owner's own account is beta")
	require.Equal(t, f.acme, engine.CallAccount(auth.ContextWithCallAccount(borrowed, account)),
		"a call made for the site acts in the site's account")
}

// TestCallAccountAfterAHopIsTheOriginatingNodes: the node that handles a
// forwarded call answers what the originating node answered.
//
// The receiver is a second engine over the same database, handed a context built
// from nothing but the verified assertion: no request memo, no session, none of
// the originating node's state. Two halves. When the origin STATED an account,
// the receiver answers it and reads nothing, even though the actor would resolve
// elsewhere. When it stated none, the receiver resolves the actor the assertion
// carries, by the same rules over the same rows, and lands on the same account.
func TestCallAccountAfterAHopIsTheOriginatingNodes(t *testing.T) {
	f := newCallAccountFixture(t, "callacct-hop")
	origin := f.engine
	_, db, _ := sharedReadMergeEngine(t)
	// Not booted, on purpose: CallAccount reads rows and the role catalog, and
	// nothing a boot loads.
	receiver := newQuietEngineOn(t, db)

	member, operator := "member-"+f.suffix, "operator-"+f.suffix
	seedMembership(t, origin, f.gAcme, member, "active")

	hop := func(t *testing.T, originCtx context.Context) context.Context {
		t.Helper()
		ac, ok := auth.AccessFromContext(originCtx)
		require.True(t, ok)
		now := time.Now()
		built, err := auth.ForwardedAuthorityForUser(ac, auth.ForwardedClassUser, "", time.Time{}, now)
		require.NoError(t, err)
		// What a producer does: bind, then re-assert off the context at the
		// moment of the forward, which is where a stated account is read.
		bound := auth.ContextWithForwardedAuthority(originCtx, built)
		wire, ok := auth.ForwardedAuthorityFromContext(bound)
		require.True(t, ok)
		access, err := auth.VerifyForwardedAuthority(wire, now)
		require.NoError(t, err)
		return auth.BindForwardedContext(context.Background(), wire.Principal().Claims, access, wire)
	}

	t.Run("an account the origin stated is the receiver's answer", func(t *testing.T) {
		// A run a beta row triggered, forwarding under a member of acme and
		// under an operator: neither actor resolves to beta.
		for name, actor := range map[string]context.Context{
			"a member of another account": rankActorCtx(member, auth.RoleWriter),
			"an operator":                 rankActorCtx(operator, auth.RoleOwner),
		} {
			stated := auth.ContextWithCallAccount(actor, conceptAccountsAccount+":"+f.beta)
			require.Equal(t, f.beta, origin.CallAccount(stated), "%s: control, on the originating node", name)
			require.Equal(t, f.beta, receiver.CallAccount(hop(t, stated)),
				"%s: the receiving node worked the account out again instead of recording the one it was sent", name)
		}
	})

	t.Run("with none stated the receiver resolves the same account", func(t *testing.T) {
		for name, actor := range map[string]context.Context{
			"a member":          rankActorCtx(member, auth.RoleWriter),
			"an operator":       rankActorCtx(operator, auth.RoleOwner),
			"a person in none":  rankActorCtx("loner-"+f.suffix, auth.RoleWriter),
			"a developer":       rankActorCtx("dev-"+f.suffix, auth.RoleDeveloper),
			"a reader in acme":  rankActorCtx(member, auth.RoleReader),
			"an admin operator": rankActorCtx("admin-"+f.suffix, auth.RoleAdmin),
		} {
			want := origin.CallAccount(actor)
			require.NotEmpty(t, want, "%s: control, a person acts in some account", name)
			remote := hop(t, actor)
			_, stated := auth.CallAccountFromContext(remote)
			require.False(t, stated, "%s: the hop bound an account nobody stated", name)
			require.Equal(t, want, receiver.CallAccount(remote), "%s: the two nodes disagree about one call", name)
		}
	})
}

// TestCallAccountGuessesNothingWhenAReadFails: a read that failed is not "in no
// group".
//
// The membership reader answers no groups when its read fails, which is the
// tolerant answer for a gate: a row withheld. Here the same answer would be an
// assertion, that this person belongs to the primary account. So a failed read
// states no account at all.
func TestCallAccountGuessesNothingWhenAReadFails(t *testing.T) {
	f := newCallAccountFixture(t, "callacct-failed")
	engine := f.engine

	member, multi := "member-"+f.suffix, "multi-"+f.suffix
	seedMembership(t, engine, f.gAcme, member, "active")
	seedMembership(t, engine, f.gBeta, multi, "active")
	betweenMembershipWrites()
	seedMembership(t, engine, f.gAcme, multi, "active")
	seedPrincipal(t, engine, member, auth.RoleWriter)

	cancelled := func(ctx context.Context) context.Context {
		ctx, cancel := context.WithCancel(ctx)
		cancel()
		return ctx
	}

	// Controls: the reads succeed on a live context.
	require.Equal(t, f.acme, engine.CallAccount(rankActorCtx(member, auth.RoleWriter)))
	require.Equal(t, f.beta, engine.CallAccount(rankActorCtx(multi, auth.RoleWriter)))

	require.Empty(t, engine.CallAccount(cancelled(rankActorCtx(member, auth.RoleWriter))),
		"a membership read that failed was read as membership of nothing, and answered the primary account")
	require.Empty(t, engine.CallAccount(cancelled(rankActorCtx("loner-"+f.suffix, auth.RoleWriter))),
		"a person in no group is told apart from a read that could not say")
	require.Empty(t, engine.CallAccount(cancelled(auth.ContextWithUserActor(context.Background(), member))),
		"a role read that failed resolves nobody")

	// The failure is passed on to a caller that is listening for one.
	listening, failures := auth.ContextWithReadFailures(cancelled(rankActorCtx(member, auth.RoleWriter)))
	require.Empty(t, engine.CallAccount(listening))
	require.Error(t, failures.Err(), "a caller listening for read failures was not told of this one")

	// What needs no read is unaffected by the database being unreachable.
	require.Equal(t, "self", engine.CallAccount(cancelled(rankActorCtx("operator-"+f.suffix, auth.RoleOwner))))
	require.Equal(t, f.beta, engine.CallAccount(cancelled(auth.ContextWithCallAccount(rankActorCtx(member, auth.RoleWriter), f.beta))))

	// The second read, of how old each membership is, fails the same way. Its
	// context is cancelled only after the membership read has answered, through
	// the memo a request installs.
	warm := contextWithAccountScopeMemo(rankActorCtx(multi, auth.RoleWriter), engine)
	require.Len(t, engine.membershipsFor(warm, multi).accountIds, 2, "fixture: the memo holds both accounts")
	require.Equal(t, f.beta, engine.CallAccount(warm))
	require.Empty(t, engine.CallAccount(cancelled(warm)),
		"the read of the memberships' ages failed, and an account was chosen anyway")

	// An engine with no component, the shape this package's own tests build by
	// hand, holding the same answer in a memo of its own. Its logger is promoted
	// through the component it does not have (memql#2674), and the failure below
	// is the one path here that would say something to it.
	_, db, _ := sharedReadMergeEngine(t)
	bare := &MemQLEngine{db: db}
	bareWarm := contextWithAccountScopeMemo(rankActorCtx(multi, auth.RoleWriter), bare)
	require.Len(t, bare.membershipsFor(bareWarm, multi).accountIds, 2, "fixture: the bare engine's memo holds both accounts")

	// And a request can hold an answer the rows no longer back: both memberships
	// are removed after the memo counted them, so the second read finds nothing
	// to date. Neither account is chosen from a list that is no longer true, and
	// the caller who is listening hears that the answer could not be established.
	seedMembership(t, engine, f.gAcme, multi, "removed")
	seedMembership(t, engine, f.gBeta, multi, "removed")
	stale, heard := auth.ContextWithReadFailures(warm)
	require.Empty(t, engine.CallAccount(stale),
		"an account was chosen from memberships that were gone by the time they were dated")
	require.ErrorIs(t, heard.Err(), errNoDatedMembership)
	require.NotPanics(t, func() { require.Empty(t, bare.CallAccount(bareWarm)) },
		"reporting the failure reached for a logger the engine does not have")
	// A request that starts now reads them as they stand: in no group.
	require.Equal(t, "self", engine.CallAccount(rankActorCtx(multi, auth.RoleWriter)))
}
