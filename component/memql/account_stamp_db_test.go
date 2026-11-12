package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// THE ACCOUNT STAMP, END TO END (design record
// docs/superpowers/specs/2026-11-12-account-row-ownership-design.md, section C
// and "Refinements made while planning"; the contract is account_stamp.go's
// header).
//
// Every case writes through Execute on the shared engine and reads the stored
// row back off the table, because the claim under test is what a row RECORDS,
// and a stamp that ran but was discarded by a later step would pass any test
// that only looked at the payload in flight.
//
// Real concepts from the embedded tree, chosen for the shape each one has:
// v1:notes:note is engine-managed with no parent, v1:work:goal -> run -> step is
// a `via` chain, v1:skills:skill is `shared`, v1:campaigns:audience and
// v1:rbac:role are `stated`, and v1:work:goal carries the list `accountIds`.
//
// The switch is process state on a shared engine, so each test turns it on and
// puts it back off when it ends. None of these runs in parallel: a parallel test
// in this package only starts once every serial one has finished.

// stampFixture is two accounts with a member each, and the helpers every case
// below writes and reads through.
type stampFixture struct {
	callAccountFixture
	t          *testing.T
	db         *bun.DB
	member     string // in acme only
	betaMember string // in beta only
	n          int
}

func newStampFixture(t *testing.T, name string) *stampFixture {
	t.Helper()
	base := newCallAccountFixture(t, name)
	_, db, _ := sharedReadMergeEngine(t)
	f := &stampFixture{callAccountFixture: base, t: t, db: db}
	f.member, f.betaMember = "stamp-member-"+f.suffix, "stamp-beta-"+f.suffix
	seedMembership(t, f.engine, f.gAcme, f.member, "active")
	seedMembership(t, f.engine, f.gBeta, f.betaMember, "active")
	f.on()
	t.Cleanup(f.off)
	return f
}

func (f *stampFixture) on()  { f.engine.SetAccountStampingEnabled(true) }
func (f *stampFixture) off() { f.engine.SetAccountStampingEnabled(false) }

// id is a fresh bare row id, unique to this fixture.
func (f *stampFixture) id(kind string) string {
	f.n++
	return fmt.Sprintf("stamp-%s-%s-%d", kind, f.suffix, f.n)
}

// person is the request path's caller: an actor and a token, no internal origin.
// Its raw writes are the raw surface the stamp's refusals are for.
func (f *stampFixture) person(user string, role auth.Role) context.Context {
	return rankActorCtx(user, role)
}

// trusted is server-side Go writing for somebody: a cluster owner with internal
// origin, optionally acting in a stated account.
func (f *stampFixture) trusted(account string) context.Context {
	ctx := auth.ContextWithInternalOrigin(rankActorCtx("stamp-operator-"+f.suffix, auth.RoleOwner))
	if account != "" {
		ctx = auth.ContextWithCallAccount(ctx, account)
	}
	return ctx
}

// insert is the raw `insert(...)` literal. An id that already has a row makes it
// an update: the executor read-merges onto the stored row.
func (f *stampFixture) insert(ctx context.Context, concept, id string, payload map[string]any) (string, error) {
	encoded, err := json.Marshal(payload)
	require.NoError(f.t, err)
	q := fmt.Sprintf(`insert(%s, id=%s, payload=%s)`,
		langparser.QuoteString(concept), langparser.QuoteString(id), string(encoded))
	res, err := f.engine.Execute(ctx, q)
	if err != nil {
		return "", err
	}
	require.NotNil(f.t, res)
	require.NotNil(f.t, res.Bundle)
	require.NotEmpty(f.t, res.Bundle.Nodes, "insert into %s wrote no node", concept)
	return res.Bundle.Nodes[0].Id, nil
}

func (f *stampFixture) mustInsert(ctx context.Context, concept, id string, payload map[string]any) string {
	f.t.Helper()
	stored, err := f.insert(ctx, concept, id, payload)
	require.NoError(f.t, err, "insert into %s", concept)
	return stored
}

// mutation runs a named mutation, the template surface.
func (f *stampFixture) mutation(ctx context.Context, call string) string {
	f.t.Helper()
	res, err := f.engine.Execute(ctx, call)
	require.NoError(f.t, err, "%s", call)
	require.NotEmpty(f.t, res.Bundle.Nodes, "%s wrote no node", call)
	return res.Bundle.Nodes[0].Id
}

// stored is the newest version of a row, read off the table.
func (f *stampFixture) stored(concept, id string) map[string]any {
	f.t.Helper()
	return latestPayload(f.t, context.Background(), f.db, concept, id)
}

// accountOf is the row's recorded account as a bare id, "" for none.
func (f *stampFixture) accountOf(concept, id string) string {
	f.t.Helper()
	return bareAccount(f.stored(concept, id)["accountId"])
}

func (f *stampFixture) createNote(ctx context.Context) string {
	f.t.Helper()
	return f.mutation(ctx, fmt.Sprintf(`mutation createNote(noteId: %s, body: "hello")`, langparser.QuoteString(f.id("note"))))
}

func (f *stampFixture) updateNote(ctx context.Context, id string, payload map[string]any) {
	f.t.Helper()
	encoded, err := json.Marshal(payload)
	require.NoError(f.t, err)
	f.mutation(ctx, fmt.Sprintf(`mutation updateNote(noteId: %s, payload: %s)`, langparser.QuoteString(BareShortId(id)), string(encoded)))
}

func goalPayload() map[string]any {
	return map[string]any{"statement": "stamp test goal", "origin": "user", "status": "open"}
}

func runPayload(goalId string) map[string]any {
	p := map[string]any{
		"automationName":      "stampTest",
		"templateFingerprint": "stamp-fingerprint",
		"mode":                "live",
		"status":              "running",
		"startedAt":           time.Now().UTC().Format(time.RFC3339),
	}
	if goalId != "" {
		p["goalId"] = BareShortId(goalId)
	}
	return p
}

func stepPayload(runId string) map[string]any {
	return map[string]any{
		"runId": BareShortId(runId), "key": "first", "seq": 0, "stepType": "function",
		"status": "running", "attempt": 1,
	}
}

const (
	conceptNote     = "v1:notes:note"
	conceptGoal     = "v1:work:goal"
	conceptRun      = "v1:work:run"
	conceptStep     = "v1:work:step"
	conceptSkill    = "v1:skills:skill"
	conceptAudience = "v1:campaigns:audience"
	conceptRole     = "v1:rbac:role"
)

// TestAccountStampSources is each source of section C, one at a time.
func TestAccountStampSources(t *testing.T) {
	f := newStampFixture(t, "stamp-sources")

	t.Run("a via child takes its parent's account, over the call's", func(t *testing.T) {
		goal := f.mustInsert(f.trusted(f.acme), conceptGoal, f.id("goal"), goalPayload())
		require.Equal(t, f.acme, f.accountOf(conceptGoal, goal))
		run := f.mustInsert(f.trusted(f.beta), conceptRun, f.id("run"), runPayload(goal))
		require.Equal(t, f.acme, f.accountOf(conceptRun, run), "the run follows its goal, not the call")
		step := f.mustInsert(f.trusted(f.beta), conceptStep, f.id("step"), stepPayload(run))
		require.Equal(t, f.acme, f.accountOf(conceptStep, step), "the step follows its run, two links down")
	})

	t.Run("a child whose parent exists without an account waits for it", func(t *testing.T) {
		goal := f.mustInsert(groupSeedCtx(), conceptGoal, f.id("goal"), goalPayload())
		require.Empty(t, f.accountOf(conceptGoal, goal), "fixture: a system write states no account")
		run := f.mustInsert(f.trusted(f.beta), conceptRun, f.id("run"), runPayload(goal))
		require.Empty(t, f.accountOf(conceptRun, run),
			"the run took the call's account while its goal had none, and would disagree with the goal once the goal is assigned")
	})

	t.Run("a child naming a parent that does not exist takes the call's account", func(t *testing.T) {
		run := f.mustInsert(f.trusted(f.beta), conceptRun, f.id("run"), runPayload("no-such-goal-"+f.suffix))
		require.Equal(t, f.beta, f.accountOf(conceptRun, run))
		// And a run that names no goal at all.
		run = f.mustInsert(f.trusted(f.beta), conceptRun, f.id("run"), runPayload(""))
		require.Equal(t, f.beta, f.accountOf(conceptRun, run))
	})

	t.Run("a person in one account writes in it", func(t *testing.T) {
		note := f.createNote(f.person(f.member, auth.RoleWriter))
		require.Equal(t, f.acme, f.accountOf(conceptNote, note))
		// The raw surface agrees with the template.
		raw := f.mustInsert(f.person(f.betaMember, auth.RoleWriter), conceptNote, f.id("note"), map[string]any{"body": "raw"})
		require.Equal(t, f.beta, f.accountOf(conceptNote, raw))
	})

	t.Run("a cluster operator writes in the primary account", func(t *testing.T) {
		for _, role := range []auth.Role{auth.RoleOwner, auth.RoleDeveloper, auth.RoleAdmin} {
			note := f.createNote(f.person("stamp-op-"+string(role)+"-"+f.suffix, role))
			require.Equal(t, "self", f.accountOf(conceptNote, note), "%s", role)
		}
	})

	t.Run("an account stated for the call is the one written", func(t *testing.T) {
		stated := auth.ContextWithCallAccount(f.person(f.member, auth.RoleWriter), f.beta)
		note := f.createNote(stated)
		require.Equal(t, f.beta, f.accountOf(conceptNote, note), "the stated account wins over the person's own")
	})

	t.Run("the cluster acting for itself writes no account", func(t *testing.T) {
		note := f.mustInsert(groupSeedCtx(), conceptNote, f.id("note"), map[string]any{"body": "seed", "ownerUserId": f.member})
		_, has := f.stored(conceptNote, note)["accountId"]
		require.False(t, has, "a system write with nothing to go on records no account")
	})
}

// TestAccountStampUpdates: an update carries the stored account forward, and the
// call's account never fills a row that has none (only its parent may).
func TestAccountStampUpdates(t *testing.T) {
	f := newStampFixture(t, "stamp-updates")
	member := f.person(f.member, auth.RoleWriter)

	t.Run("an update that does not name the account keeps it", func(t *testing.T) {
		note := f.createNote(member)
		f.updateNote(member, note, map[string]any{"body": "edited"})
		require.Equal(t, f.acme, f.accountOf(conceptNote, note))
		f.mustInsert(f.trusted(f.beta), conceptNote, BareShortId(note), map[string]any{"body": "edited by the server for beta"})
		require.Equal(t, f.acme, f.accountOf(conceptNote, note), "an update in another account's call moved the row")
		require.Equal(t, "edited by the server for beta", f.stored(conceptNote, note)["body"], "fixture: the update landed")
	})

	t.Run("an unassigned row is not given the editor's account", func(t *testing.T) {
		f.off()
		note := f.createNote(member)
		f.on()
		require.Empty(t, f.accountOf(conceptNote, note), "fixture: written with the switch off")
		f.updateNote(member, note, map[string]any{"body": "edited"})
		require.Empty(t, f.accountOf(conceptNote, note), "the template update handed an old row to its editor")
		f.mustInsert(member, conceptNote, BareShortId(note), map[string]any{"body": "raw edit"})
		require.Empty(t, f.accountOf(conceptNote, note), "the raw update handed an old row to its editor")
		f.mustInsert(f.trusted(f.beta), conceptNote, BareShortId(note), map[string]any{"body": "server edit"})
		require.Empty(t, f.accountOf(conceptNote, note), "a stated call account filled an old row on update")
	})

	t.Run("a parent fills an unassigned child on update", func(t *testing.T) {
		goal := f.mustInsert(f.trusted(f.acme), conceptGoal, f.id("goal"), goalPayload())
		f.off()
		run := f.mustInsert(f.trusted(f.acme), conceptRun, f.id("run"), runPayload(goal))
		f.on()
		require.Empty(t, f.accountOf(conceptRun, run), "fixture: written with the switch off")
		f.mustInsert(f.trusted(f.beta), conceptRun, BareShortId(run), map[string]any{"status": "waiting"})
		require.Equal(t, f.acme, f.accountOf(conceptRun, run), "the run's goal decides, on update as on create")
	})
}

// TestAccountStampRawSurface: the refusals, which only the raw surface meets.
func TestAccountStampRawSurface(t *testing.T) {
	f := newStampFixture(t, "stamp-raw")
	member := f.person(f.member, auth.RoleWriter)

	t.Run("a stored account cannot be changed", func(t *testing.T) {
		note := f.createNote(member)
		_, err := f.insert(member, conceptNote, BareShortId(note), map[string]any{"body": "moved", "accountId": f.beta})
		require.ErrorContains(t, err, codeAccountChangeRefused)
		require.Equal(t, f.acme, f.accountOf(conceptNote, note))
		// Restating the stored account, in either spelling, is not a change.
		f.mustInsert(member, conceptNote, BareShortId(note), map[string]any{"body": "same", "accountId": conceptAccountsAccount + ":" + f.acme})
		require.Equal(t, f.acme, f.accountOf(conceptNote, note))
	})

	t.Run("an explicit account the caller is not in is refused", func(t *testing.T) {
		_, err := f.insert(member, conceptNote, f.id("note"), map[string]any{"body": "x", "accountId": f.beta})
		require.ErrorContains(t, err, "organization_forbidden")
	})

	t.Run("an explicit account that differs from the parent's is refused", func(t *testing.T) {
		goal := f.mustInsert(f.trusted(f.beta), conceptGoal, f.id("goal"), goalPayload())
		p := runPayload(goal)
		p["accountId"] = f.acme
		_, err := f.insert(member, conceptRun, f.id("run"), p)
		require.ErrorContains(t, err, codeAccountMismatch)
		// Naming the parent's own account does not get round the membership
		// check: the member is not in beta.
		p["accountId"] = f.beta
		_, err = f.insert(member, conceptRun, f.id("run"), p)
		require.ErrorContains(t, err, "organization_forbidden")
	})

	// A raw write names its parent with nothing but an id, and the stamp reads
	// that parent asking nobody's permission. So a person's raw child takes the
	// parent's account only when they may write to that account; otherwise it
	// is recorded under their own, and is not refused.
	t.Run("a person's raw child does not take a parent's account they may not write to", func(t *testing.T) {
		theirs := f.mustInsert(f.trusted(f.beta), conceptGoal, f.id("goal"), goalPayload())
		run := f.mustInsert(member, conceptRun, f.id("run"), runPayload(theirs))
		require.Equal(t, f.acme, f.accountOf(conceptRun, run), "a parent id filed the row under an account its writer is not in")

		own := f.mustInsert(f.trusted(f.acme), conceptGoal, f.id("goal"), goalPayload())
		run = f.mustInsert(member, conceptRun, f.id("run"), runPayload(own))
		require.Equal(t, f.acme, f.accountOf(conceptRun, run), "a parent in the writer's own account is followed")

		operator := f.person("stamp-raw-operator-"+f.suffix, auth.RoleOwner)
		run = f.mustInsert(operator, conceptRun, f.id("run"), runPayload(theirs))
		require.Equal(t, f.beta, f.accountOf(conceptRun, run), "a cluster operator's raw child follows its parent")
	})

	// A template that accepts a payload passes its caller's `accountId` through,
	// so a person's write through one is held exactly as the raw literal is.
	t.Run("a person naming an account through a template is held to their own accounts", func(t *testing.T) {
		note := f.createNote(member)
		_, err := f.engine.Execute(member, fmt.Sprintf(`mutation updateNote(noteId: %s, payload: {"accountId": %s})`,
			langparser.QuoteString(BareShortId(note)), langparser.QuoteString(f.beta)))
		require.ErrorContains(t, err, codeAccountChangeRefused)
		require.Equal(t, f.acme, f.accountOf(conceptNote, note))

		f.off()
		unassigned := f.createNote(member)
		f.on()
		_, err = f.engine.Execute(member, fmt.Sprintf(`mutation updateNote(noteId: %s, payload: {"accountId": %s})`,
			langparser.QuoteString(BareShortId(unassigned)), langparser.QuoteString(f.beta)))
		require.ErrorContains(t, err, "organization_forbidden")
		require.Empty(t, f.accountOf(conceptNote, unassigned))

		f.updateNote(member, unassigned, map[string]any{"accountId": f.acme})
		require.Equal(t, f.acme, f.accountOf(conceptNote, unassigned), "their own account, named through the template")
	})

	t.Run("an explicit account the caller is in is accepted", func(t *testing.T) {
		note := f.mustInsert(member, conceptNote, f.id("note"), map[string]any{"body": "x", "accountId": f.acme})
		require.Equal(t, f.acme, f.accountOf(conceptNote, note))
		// A template that names an account is the author's decision, and so is
		// trusted Go: neither meets the refusals.
		note = f.mustInsert(f.trusted(""), conceptNote, f.id("note"), map[string]any{"body": "x", "accountId": f.beta, "ownerUserId": f.member})
		require.Equal(t, f.beta, f.accountOf(conceptNote, note))
	})
}

// TestAccountStampEmptyAccountIsNoAccount: `accountId: ""` is exactly no key
// (refinement R6). Ten mutations write the empty string where others write
// nothing, so a difference here would split one behaviour by spelling.
func TestAccountStampEmptyAccountIsNoAccount(t *testing.T) {
	f := newStampFixture(t, "stamp-empty")
	member := f.person(f.member, auth.RoleWriter)

	t.Run("on a create", func(t *testing.T) {
		withKey := f.mustInsert(member, conceptNote, f.id("note"), map[string]any{"body": "x", "accountId": ""})
		without := f.mustInsert(member, conceptNote, f.id("note"), map[string]any{"body": "x"})
		require.Equal(t, f.acme, f.accountOf(conceptNote, withKey))
		require.Equal(t, f.stored(conceptNote, without)["accountId"], f.stored(conceptNote, withKey)["accountId"])

		// And for a system write that has nothing to go on: both store nothing.
		withKey = f.mustInsert(groupSeedCtx(), conceptNote, f.id("note"), map[string]any{"body": "x", "accountId": "", "ownerUserId": f.member})
		without = f.mustInsert(groupSeedCtx(), conceptNote, f.id("note"), map[string]any{"body": "x", "ownerUserId": f.member})
		a, hasA := f.stored(conceptNote, withKey)["accountId"]
		b, hasB := f.stored(conceptNote, without)["accountId"]
		require.Equal(t, hasB, hasA, "one write stored the key and the other did not")
		require.Equal(t, b, a)
	})

	t.Run("on an update it keeps the stored account", func(t *testing.T) {
		note := f.createNote(member)
		f.updateNote(member, note, map[string]any{"body": "template", "accountId": ""})
		require.Equal(t, f.acme, f.accountOf(conceptNote, note), "a template update writing the empty string erased the account")
		_, err := f.insert(member, conceptNote, BareShortId(note), map[string]any{"body": "raw", "accountId": ""})
		require.NoError(t, err, "a raw update writing the empty string was read as a move")
		require.Equal(t, f.acme, f.accountOf(conceptNote, note))
		f.mustInsert(f.trusted(f.beta), conceptNote, BareShortId(note), map[string]any{"body": "server", "accountId": ""})
		require.Equal(t, f.acme, f.accountOf(conceptNote, note), "trusted Go writing the empty string erased the account")
	})

	t.Run("on a shared concept", func(t *testing.T) {
		withKey := f.mustInsert(f.trusted(f.acme), conceptSkill, f.id("skill"), map[string]any{"slug": f.id("slug"), "name": "x", "tier": "A", "accountId": ""})
		require.Equal(t, f.acme, f.accountOf(conceptSkill, withKey), "the empty string on a shared concept was read as a choice of no account")
	})
}

// TestAccountStampLeavesStatedConceptsAlone: a `stated` concept's writers state
// the account, and older rules decide what an absent value means. The same
// write with the switch on and off must store the same row, or fail the same
// way.
func TestAccountStampLeavesStatedConceptsAlone(t *testing.T) {
	f := newStampFixture(t, "stamp-stated")
	// The organization rules refuse a campaign naming an account that has no
	// row, and default an operator's to the primary one, so both exist here.
	seedAccountOwnedBy(t, f.engine, "stamp-account-owner-"+f.suffix, auth.RoleOwner, f.acme, "Acme")
	if _, err := f.engine.Execute(rankActorCtx("stamp-account-owner-"+f.suffix, auth.RoleOwner),
		`mutation createClientAccount(accountId: "self", name: "Primary")`); err != nil {
		t.Logf("the primary account was not created here (it may exist already): %v", err)
	}

	// compare writes the same payload twice, switch off then on, and returns the
	// row stored with it on, or nil when both writes were refused alike. The
	// payload is built per write from `which` so that ids and slugs can differ
	// and still be compared.
	compare := func(t *testing.T, concept string, ctx context.Context, payload func(which string) map[string]any) map[string]any {
		t.Helper()
		base := f.id("cmp")
		f.off()
		offId, offErr := f.insert(ctx, concept, base+"-off", payload(base+"-off"))
		f.on()
		onId, onErr := f.insert(ctx, concept, base+"-on", payload(base+"-on"))
		if offErr != nil || onErr != nil {
			require.Error(t, offErr, "the switch introduced a refusal: %v", onErr)
			require.Error(t, onErr, "the switch removed a refusal: %v", offErr)
			require.Equal(t, strings.ReplaceAll(offErr.Error(), base+"-off", base), strings.ReplaceAll(onErr.Error(), base+"-on", base))
			t.Logf("%s: refused alike with the switch off and on: %s", concept, firstLine(onErr.Error()))
			return nil
		}
		off, on := f.stored(concept, offId), f.stored(concept, onId)
		for _, p := range []map[string]any{off, on} {
			for k, v := range p {
				if s, ok := v.(string); ok {
					p[k] = strings.ReplaceAll(strings.ReplaceAll(s, base+"-off", base), base+"-on", base)
				}
			}
		}
		require.Equal(t, off, on, "%s: the switch changed what is stored", concept)
		return on
	}

	// An audience rather than a campaign: both are among the organization-owned
	// concepts, and an audience references no other row, so the organization
	// rules' refusals of a dangling reference (checked in map order, so not
	// even worded alike from one write to the next) do not decide the case.
	campaign := func(account string) func(string) map[string]any {
		return func(string) map[string]any {
			p := map[string]any{"ownerUserId": f.member, "name": "stamp audience"}
			if account != "" {
				p["accountId"] = account
			}
			return p
		}
	}
	t.Run("an organization-owned row with and without an account", func(t *testing.T) {
		written := 0
		for name, write := range map[string]struct {
			ctx     context.Context
			payload func(string) map[string]any
		}{
			"trusted Go, no account":           {f.trusted(f.acme), campaign("")},
			"trusted Go, the primary":          {f.trusted(f.acme), campaign("self")},
			"a person, no account":             {f.person(f.member, auth.RoleWriter), campaign("")},
			"a person, an account they are in": {f.person(f.member, auth.RoleWriter), campaign(f.acme)},
		} {
			if compare(t, conceptAudience, write.ctx, write.payload) != nil {
				written++
			} else {
				t.Logf("audience, %s: refused alike", name)
			}
		}
		require.NotZero(t, written, "no audience write succeeded, so nothing was compared")
	})

	t.Run("a custom role created with no account stays cluster-wide", func(t *testing.T) {
		// The seed materializer's kind of actor: the only one the custom-role
		// rank bound lets write a rank without a resolved catalog.
		seeder := auth.ContextWithToken(auth.ContextWithInternalOrigin(auth.ContextWithSystemActor(context.Background(), "stamp-test")),
			&auth.TokenInfo{Subject: "system:stamp-test"})
		seeder = auth.ContextWithCallAccount(seeder, f.acme)
		role := func(which string) map[string]any {
			return map[string]any{"slug": which, "name": "Custom", "rank": 150, "active": true}
		}
		stored := compare(t, conceptRole, seeder, role)
		require.NotNil(t, stored, "fixture: the role write was refused")
		_, has := stored["accountId"]
		require.False(t, has, "a custom role written with no account was given one")
		// And under a person acting in an account, whatever the outcome.
		compare(t, conceptRole, f.trusted(f.acme), role)
	})
}

// TestAccountStampSharedConcept: a shared concept's rows with nothing to go on
// are the cluster's; a person's are their account's.
func TestAccountStampSharedConcept(t *testing.T) {
	f := newStampFixture(t, "stamp-shared")
	skill := func() map[string]any { return map[string]any{"slug": f.id("slug"), "name": "x", "tier": "A"} }

	seeded := f.mustInsert(groupSeedCtx(), conceptSkill, f.id("skill"), skill())
	_, has := f.stored(conceptSkill, seeded)["accountId"]
	require.False(t, has, "a seeded skill was placed in an account")

	authored := f.mustInsert(f.person(f.member, auth.RoleWriter), conceptSkill, f.id("skill"), skill())
	require.Equal(t, f.acme, f.accountOf(conceptSkill, authored))
}

// listOf is the row's stored accountIds, as strings.
func (f *stampFixture) listOf(concept, id string) []string {
	f.t.Helper()
	out := []string{}
	if list, ok := f.stored(concept, id)["accountIds"].([]any); ok {
		for _, v := range list {
			s, _ := v.(string)
			out = append(out, s)
		}
	}
	return out
}

// followTasksFor counts the follow tasks recorded for a row.
func (f *stampFixture) followTasksFor(id string) int {
	f.t.Helper()
	var n int
	require.NoError(f.t, f.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM account_follow_tasks WHERE parent_id = ?`, id).Scan(&n))
	return n
}

// TestAccountStampListHoldsOneAccount: the five concepts that still carry
// `accountIds` hold at most one account in it once recording is on (design
// record docs/superpowers/specs/2026-11-12-account-export-design.md, section I,
// "The account lists"). One entry files the row there; an empty list keeps the
// row's account and shares it with nobody.
func TestAccountStampListHoldsOneAccount(t *testing.T) {
	f := newStampFixture(t, "stamp-list")
	a, b, c := "a-"+f.suffix, "b-"+f.suffix, "c-"+f.suffix
	tagged := func(accounts ...string) map[string]any {
		p := goalPayload()
		p["accountIds"] = accounts
		return p
	}

	inA := f.mustInsert(f.trusted(c), conceptGoal, f.id("goal"), tagged(a))
	require.Equal(t, a, f.accountOf(conceptGoal, inA), "one entry files the row there")
	require.Equal(t, []string{a}, f.listOf(conceptGoal, inA))

	_, err := f.insert(f.trusted(a), conceptGoal, f.id("goal"), tagged(b, a))
	require.ErrorContains(t, err, "account_list_single", "a list of two accounts was stored")

	f.mustInsert(f.trusted(c), conceptGoal, BareShortId(inA), map[string]any{"accountIds": []string{c}})
	require.Equal(t, c, f.accountOf(conceptGoal, inA), "a re-tag files the row in the account it names")
	require.Equal(t, []string{c}, f.listOf(conceptGoal, inA))
	require.Equal(t, 1, f.followTasksFor(inA), "a re-tag records a follow task, so the row's runs follow it")

	f.mustInsert(f.trusted(a), conceptGoal, BareShortId(inA), map[string]any{"status": "active"})
	require.Equal(t, c, f.accountOf(conceptGoal, inA), "an update that leaves the list alone keeps the account")
	require.Equal(t, []string{c}, f.listOf(conceptGoal, inA))

	f.mustInsert(f.trusted(a), conceptGoal, BareShortId(inA), map[string]any{"accountIds": []string{}})
	require.Equal(t, c, f.accountOf(conceptGoal, inA), "an emptied list keeps the account: the row is private to its owner")
	require.Equal(t, []string{}, f.listOf(conceptGoal, inA))
	require.Equal(t, 1, f.followTasksFor(inA), "nothing changed account, and nothing more was recorded")

	t.Run("a stored list naming another account is trimmed on the next write", func(t *testing.T) {
		legacy := conceptGoal + ":" + f.id("goal")
		seedRawRow(t, context.Background(), f.db, conceptGoal, legacy, time.Now().Add(-time.Hour), "stamp-test",
			map[string]any{"statement": "legacy", "origin": "user", "status": "open", "accountId": a, "accountIds": []string{b, a}})
		f.mustInsert(f.trusted(a), conceptGoal, BareShortId(legacy), map[string]any{"status": "active"})
		require.Equal(t, a, f.accountOf(conceptGoal, legacy))
		require.Equal(t, []string{a}, f.listOf(conceptGoal, legacy))
	})
}

// TestAccountStampListIsHeldToThePersonsAccounts: the list is the caller's own
// input on every surface, so a person may name only an account they may write
// to (account_stamp.go, the list).
func TestAccountStampListIsHeldToThePersonsAccounts(t *testing.T) {
	f := newStampFixture(t, "stamp-list-person")
	member := f.person(f.member, auth.RoleWriter)
	tagged := func(accounts ...string) map[string]any {
		p := goalPayload()
		p["accountIds"] = accounts
		return p
	}

	t.Run("an account the person belongs to files the row there", func(t *testing.T) {
		goal := f.mustInsert(member, conceptGoal, f.id("goal"), tagged(f.acme))
		require.Equal(t, f.acme, f.accountOf(conceptGoal, goal))
	})

	t.Run("an account the person does not belong to is refused", func(t *testing.T) {
		_, err := f.insert(member, conceptGoal, f.id("goal"), tagged(f.beta))
		require.ErrorContains(t, err, "organization_forbidden")
	})

	t.Run("retagging cannot move a row to an account the person does not belong to", func(t *testing.T) {
		goal := f.mustInsert(member, conceptGoal, f.id("goal"), tagged(f.acme))
		_, err := f.insert(member, conceptGoal, BareShortId(goal), map[string]any{"accountIds": []string{f.beta}})
		require.ErrorContains(t, err, "organization_forbidden")
		require.Equal(t, f.acme, f.accountOf(conceptGoal, goal))
		require.Equal(t, []string{f.acme}, f.listOf(conceptGoal, goal))
	})

	t.Run("a cluster operator may name any account", func(t *testing.T) {
		operator := f.person("stamp-list-operator-"+f.suffix, auth.RoleOwner)
		goal := f.mustInsert(operator, conceptGoal, f.id("goal"), tagged(f.beta))
		require.Equal(t, f.beta, f.accountOf(conceptGoal, goal))
	})
}

// TestAccountStampListHeldOnWritesThatDoNotNameIt: where the stored list would
// decide a row's account, a person's right to write to that account is asked on
// every write they make, naming the list or not. Not naming it, the list then
// decides nothing and the other sources apply.
func TestAccountStampListHeldOnWritesThatDoNotNameIt(t *testing.T) {
	f := newStampFixture(t, "stamp-list-unnamed")
	member := f.person(f.member, auth.RoleWriter)
	legacy := conceptGoal + ":" + f.id("goal")
	seedRawRow(t, context.Background(), f.db, conceptGoal, legacy, time.Now().Add(-time.Hour), "stamp-test",
		map[string]any{"statement": "legacy", "origin": "user", "status": "open", "ownerUserId": f.member, "accountIds": []string{f.beta}})
	f.mustInsert(member, conceptGoal, BareShortId(legacy), map[string]any{"status": "active"})
	require.NotEqual(t, f.beta, f.accountOf(conceptGoal, legacy), "a person's write filed a row in an account they may not write to, through a stored list")

	t.Run("one the person may write to decides", func(t *testing.T) {
		own := conceptGoal + ":" + f.id("goal")
		seedRawRow(t, context.Background(), f.db, conceptGoal, own, time.Now().Add(-time.Hour), "stamp-test",
			map[string]any{"statement": "legacy", "origin": "user", "status": "open", "ownerUserId": f.member, "accountIds": []string{f.acme}})
		f.mustInsert(member, conceptGoal, BareShortId(own), map[string]any{"status": "active"})
		require.Equal(t, f.acme, f.accountOf(conceptGoal, own))
	})
}

// TestAccountStampRestatingTheListIsNotARetag: a write that restates the list it
// found (a whole-row re-insert does) changes nothing about who the row is for.
func TestAccountStampRestatingTheListIsNotARetag(t *testing.T) {
	f := newStampFixture(t, "stamp-list-restate")
	a, b := "a-"+f.suffix, "b-"+f.suffix
	row := conceptGoal + ":" + f.id("goal")
	seedRawRow(t, context.Background(), f.db, conceptGoal, row, time.Now().Add(-time.Hour), "stamp-test",
		map[string]any{"statement": "legacy", "origin": "user", "status": "open", "accountId": a, "accountIds": []string{b}})
	restated := goalPayload()
	restated["accountIds"] = []string{b}
	f.mustInsert(f.trusted(b), conceptGoal, BareShortId(row), restated)
	require.Equal(t, a, f.accountOf(conceptGoal, row), "restating the list it found moved the row")
	require.Zero(t, f.followTasksFor(row))
}

// TestAccountStampFirstStampOfALegacyRowAsksNothing: an engine-managed row
// getting its first account writes no change record and asks for no pass; its
// children were waiting on it, and the job's own passes assign them.
func TestAccountStampFirstStampOfALegacyRowAsksNothing(t *testing.T) {
	f := newStampFixture(t, "stamp-first-stamp")
	goal := f.mustInsert(f.trusted(f.acme), conceptGoal, f.id("goal"), goalPayload())
	run := conceptRun + ":" + f.id("run")
	legacy := runPayload(goal)
	seedRawRow(t, context.Background(), f.db, conceptRun, run, time.Now().Add(-time.Hour), "stamp-test", legacy)
	var requests [][]string
	accountFullPassRequested = func(concepts []string) { requests = append(requests, concepts) }
	t.Cleanup(func() { accountFullPassRequested = nil })

	f.mustInsert(f.trusted(f.beta), conceptRun, BareShortId(run), map[string]any{"status": "succeeded"})
	require.Equal(t, f.acme, f.accountOf(conceptRun, run), "fixture: the update stamped the run from its goal")
	require.Zero(t, f.followTasksFor(run))
	require.Empty(t, requests, "a first stamp asked the job for passes")
}

// TestAccountStampStatedFirstAccountAsksForItsFollowersPasses: a row of a kind
// that states its own account, given its first account through its own
// mutation, asks for full passes of the kinds below it, and its children then
// take its account. The job had left them alone while it had none.
func TestAccountStampStatedFirstAccountAsksForItsFollowersPasses(t *testing.T) {
	f := newStampFixture(t, "stamp-stated-first")
	ctx := context.Background()
	for _, account := range []string{f.acme} {
		seedRawRow(t, ctx, f.db, conceptAccountsAccount, conceptAccountsAccount+":"+account,
			time.Now().Add(-2*time.Hour), "stamp-test", map[string]any{"name": account, "status": "active"})
	}
	at := time.Now().Add(-time.Hour)
	canonical := conceptAccountsAccount + ":" + f.acme
	audience := conceptAudience + ":" + f.id("audience")
	seedRawRow(t, ctx, f.db, conceptAudience, audience, at, "stamp-test", map[string]any{"name": "a", "ownerUserId": f.member, "accountId": canonical})
	template := "v1:campaigns:template:" + f.id("template")
	seedRawRow(t, ctx, f.db, "v1:campaigns:template", template, at, "stamp-test", map[string]any{"name": "t", "ownerUserId": f.member, "accountId": canonical})
	campaign := "v1:campaigns:campaign:" + f.id("campaign")
	seedRawRow(t, ctx, f.db, "v1:campaigns:campaign", campaign, at, "stamp-test", map[string]any{
		"name": "c", "ownerUserId": f.member, "audienceId": audience, "templateId": template, "status": "draft",
	})
	job := "v1:campaigns:sendJob:" + f.id("job")
	seedRawRow(t, ctx, f.db, "v1:campaigns:sendJob", job, at, "stamp-test", map[string]any{
		"campaignId": campaign, "campaignOwnerUserId": f.member, "audienceId": audience, "templateId": template, "status": "scheduled",
	})
	_, order, err := assignmentConcepts()
	require.NoError(t, err)
	require.NoError(t, ensureAssignmentProgressRows(ctx, f.db, order))
	var requests [][]string
	accountFullPassRequested = func(concepts []string) { requests = append(requests, concepts) }
	t.Cleanup(func() { accountFullPassRequested = nil })

	f.mustInsert(f.trusted(""), "v1:campaigns:campaign", BareShortId(campaign), map[string]any{"accountId": canonical})
	require.Equal(t, f.acme, f.accountOf("v1:campaigns:campaign", campaign), "fixture: the campaign took its account")
	require.Len(t, requests, 1)
	require.Contains(t, requests[0], "v1:campaigns:sendJob")
	require.Zero(t, f.followTasksFor(campaign), "a first account writes no change record")

	runJobUntil(t, f.engine, f.db, "the send jobs' requested pass finished", func() bool {
		return !fullPassPending(t, f.engine, f.db, "v1:campaigns:sendJob")
	})
	require.Equal(t, f.acme, f.accountOf("v1:campaigns:sendJob", job))
}

// TestAccountStampListUnchangedWhileTheSwitchIsOff: before recording is on the
// list behaves as it did, a list of several accounts included.
func TestAccountStampListUnchangedWhileTheSwitchIsOff(t *testing.T) {
	f := newStampFixture(t, "stamp-list-off")
	f.off()
	p := goalPayload()
	p["accountIds"] = []string{f.beta, f.acme}
	goal := f.mustInsert(f.person(f.member, auth.RoleWriter), conceptGoal, f.id("goal"), p)
	require.Equal(t, []string{f.beta, f.acme}, f.listOf(conceptGoal, goal))
	_, has := f.stored(conceptGoal, goal)["accountId"]
	require.False(t, has)
	require.Zero(t, f.followTasksFor(goal))
}

// TestAccountStampStatedChangeRecordsAFollowTask: a `stated` kind's own
// mutation that changes the row's account records a follow task, as the move
// and a re-tag do.
func TestAccountStampStatedChangeRecordsAFollowTask(t *testing.T) {
	f := newStampFixture(t, "stamp-stated-follow")
	for _, account := range []string{f.acme, f.beta} {
		seedRawRow(t, context.Background(), f.db, conceptAccountsAccount, conceptAccountsAccount+":"+account,
			time.Now().Add(-2*time.Hour), "stamp-test", map[string]any{"name": account, "status": "active"})
	}
	audience := conceptAudience + ":" + f.id("audience")
	seedRawRow(t, context.Background(), f.db, conceptAudience, audience, time.Now().Add(-time.Hour), "stamp-test",
		map[string]any{"name": "a", "ownerUserId": f.member, "accountId": conceptAccountsAccount + ":" + f.acme})
	f.mustInsert(f.trusted(""), conceptAudience, BareShortId(audience), map[string]any{"accountId": conceptAccountsAccount + ":" + f.beta})
	require.Equal(t, f.beta, f.accountOf(conceptAudience, audience), "fixture: the audience changed account")
	require.Equal(t, 1, f.followTasksFor(audience))
}

// TestAccountStampReinsertKeepsTheAccount: a write that re-inserts a whole row
// under an id that already has one never loses or changes the account the row
// records. The read-merge carries it on this path; the stamp carries it as well,
// for writers that replace the row (account_stamp.go, source 3), and the pure
// half of that is TestAccountStampCarriesTheStoredAccountForward.
func TestAccountStampReinsertKeepsTheAccount(t *testing.T) {
	f := newStampFixture(t, "stamp-reinsert")
	member := f.person(f.member, auth.RoleWriter)

	t.Run("a template re-insert in another account's call", func(t *testing.T) {
		note := f.createNote(member)
		f.mutation(auth.ContextWithCallAccount(member, f.beta),
			fmt.Sprintf(`mutation createNote(noteId: %s, body: "again")`, langparser.QuoteString(BareShortId(note))))
		require.Equal(t, f.acme, f.accountOf(conceptNote, note))
		require.Equal(t, "again", f.stored(conceptNote, note)["body"], "fixture: the re-insert landed")
	})

	t.Run("a raw re-insert with no account in the payload", func(t *testing.T) {
		note := f.createNote(member)
		f.mustInsert(f.trusted(f.beta), conceptNote, BareShortId(note), map[string]any{"body": "whole row", "ownerUserId": f.member})
		require.Equal(t, f.acme, f.accountOf(conceptNote, note))
		f.mustInsert(member, conceptNote, BareShortId(note), map[string]any{"body": "whole row, raw"})
		require.Equal(t, f.acme, f.accountOf(conceptNote, note))
	})

	t.Run("a via child keeps the account it took from its parent", func(t *testing.T) {
		goal := f.mustInsert(f.trusted(f.acme), conceptGoal, f.id("goal"), goalPayload())
		run := f.mustInsert(f.trusted(f.acme), conceptRun, f.id("run"), runPayload(goal))
		require.Equal(t, f.acme, f.accountOf(conceptRun, run))
		// Re-inserted in beta's call, once restating the goal and once naming
		// another goal that has an account of its own.
		f.mustInsert(f.trusted(f.beta), conceptRun, BareShortId(run), runPayload(goal))
		require.Equal(t, f.acme, f.accountOf(conceptRun, run))
		other := f.mustInsert(f.trusted(f.beta), conceptGoal, f.id("goal"), goalPayload())
		f.mustInsert(f.trusted(f.beta), conceptRun, BareShortId(run), runPayload(other))
		require.Equal(t, f.acme, f.accountOf(conceptRun, run), "a recorded account changed because a later write named another parent")
	})

	t.Run("a re-insert of an unassigned row is not given the editor's account", func(t *testing.T) {
		f.off()
		note := f.createNote(member)
		f.on()
		f.mutation(member, fmt.Sprintf(`mutation createNote(noteId: %s, body: "again")`, langparser.QuoteString(BareShortId(note))))
		require.Empty(t, f.accountOf(conceptNote, note))
		f.mustInsert(f.trusted(f.beta), conceptNote, BareShortId(note), map[string]any{"body": "whole row", "ownerUserId": f.member})
		require.Empty(t, f.accountOf(conceptNote, note))
	})
}

// TestAccountStampSwitchOff: with the switch off a write is what the previous
// release wrote, so not one of the sources leaves a key behind.
func TestAccountStampSwitchOff(t *testing.T) {
	f := newStampFixture(t, "stamp-off")
	goal := f.mustInsert(f.trusted(f.acme), conceptGoal, f.id("goal"), goalPayload())
	require.Equal(t, f.acme, f.accountOf(conceptGoal, goal), "fixture: a parent with an account")
	f.off()

	written := map[string][2]string{}
	written["a person's note"] = [2]string{conceptNote, f.createNote(f.person(f.member, auth.RoleWriter))}
	written["an operator's note"] = [2]string{conceptNote, f.createNote(f.person("stamp-off-op-"+f.suffix, auth.RoleOwner))}
	written["a stated call"] = [2]string{conceptNote, f.createNote(auth.ContextWithCallAccount(f.person(f.member, auth.RoleWriter), f.beta))}
	written["a child of an assigned parent"] = [2]string{conceptRun, f.mustInsert(f.trusted(f.beta), conceptRun, f.id("run"), runPayload(goal))}
	tagged := goalPayload()
	tagged["accountIds"] = []string{f.acme}
	written["a tagged row"] = [2]string{conceptGoal, f.mustInsert(f.trusted(f.acme), conceptGoal, f.id("goal"), tagged)}
	written["a shared row"] = [2]string{conceptSkill, f.mustInsert(f.person(f.member, auth.RoleWriter), conceptSkill, f.id("skill"), map[string]any{"slug": f.id("slug"), "name": "x", "tier": "A"})}

	for name, row := range written {
		_, has := f.stored(row[0], row[1])["accountId"]
		require.False(t, has, "%s: the switch is off and the row records an account", name)
	}

	// AN ACCOUNT NAMED BEFORE THE SWITCH IS DROPPED, FOR EVERYONE. The write is
	// not refused, and the row records no account: a value stored now would be
	// believed once the switch is on, having met none of the checks, and the
	// previous release could not update a row that carries the key.
	member := f.person(f.member, auth.RoleWriter)
	raw, err := f.insert(member, conceptNote, f.id("note"), map[string]any{"body": "x", "accountId": f.beta})
	require.NoError(t, err)
	_, has := f.stored(conceptNote, raw)["accountId"]
	require.False(t, has, "a person's raw write planted an account while the switch was off")

	templated := f.createNote(member)
	f.updateNote(member, templated, map[string]any{"body": "edited", "accountId": f.beta})
	_, has = f.stored(conceptNote, templated)["accountId"]
	require.False(t, has, "a person's template write planted an account while the switch was off")
	require.Equal(t, "edited", f.stored(conceptNote, templated)["body"], "fixture: the rest of the update landed")

	server := f.mustInsert(f.trusted(""), conceptNote, f.id("note"), map[string]any{"body": "x", "accountId": f.beta, "ownerUserId": f.member})
	_, has = f.stored(conceptNote, server)["accountId"]
	require.False(t, has, "server code wrote the field before the switch")

	// A row another replica stamped keeps its account through a held node's
	// write, named or not.
	f.on()
	stamped := f.createNote(member)
	require.Equal(t, f.acme, f.accountOf(conceptNote, stamped))
	f.off()
	f.updateNote(member, stamped, map[string]any{"body": "edited while held", "accountId": f.beta})
	require.Equal(t, f.acme, f.accountOf(conceptNote, stamped), "a held node changed a stored account")
}

// TestAccountStampSpelling: the stamp writes the bare id, and a concept that
// declares a relationship on the field stores it canonically (refinement R7).
// Read straight off the table, because the wire bare-ifies either spelling.
func TestAccountStampSpelling(t *testing.T) {
	f := newStampFixture(t, "stamp-spelling")

	note := f.createNote(f.person(f.member, auth.RoleWriter))
	require.Equal(t, f.acme, f.stored(conceptNote, note)["accountId"], "a concept with no relationship on the field stores the bare id")

	// Every concept in the tree that declares a relationship on accountId today
	// is `stated`, so the engine-managed case is a fixture: the note concept
	// under another name, with the relationship an author would write. A real
	// one, when it appears, is covered by the canary, which compares bare ids.
	related := registerRelatedAccountNote(t)
	row := f.mustInsert(f.trusted(f.acme), related, f.id("related"), map[string]any{"body": "x", "ownerUserId": f.member})
	require.Equal(t, conceptAccountsAccount+":"+f.acme, f.stored(related, row)["accountId"],
		"a concept that declares a relationship on accountId stores the canonical id")

	// The two spellings an author can hand in end in the same stored value.
	row = f.mustInsert(f.trusted(""), related, f.id("related"), map[string]any{"body": "x", "ownerUserId": f.member, "accountId": f.acme})
	require.Equal(t, conceptAccountsAccount+":"+f.acme, f.stored(related, row)["accountId"])
	row = f.mustInsert(f.trusted(""), conceptNote, f.id("note"), map[string]any{"body": "x", "ownerUserId": f.member, "accountId": conceptAccountsAccount + ":" + f.acme})
	require.Equal(t, conceptAccountsAccount+":"+f.acme, f.stored(conceptNote, row)["accountId"],
		"an explicit value is stored as written on a concept with no relationship; only the stamp writes bare")
}

// registerRelatedAccountNote registers, for one test, an engine-managed concept
// that declares `@relationship(type="references", field="accountId",
// target=account)`, and removes it when the test ends.
func registerRelatedAccountNote(t *testing.T) string {
	t.Helper()
	note, err := memorynodes.Get(conceptNote)
	require.NoError(t, err)
	name := "v1:stamptest:relatedNote"
	fixture := &memorynodes.Concept{
		Name: name, SchemaId: name, Schemas: note.Schemas, NodeType: note.NodeType,
		Relationships: append(append([]memorynodes.RelationshipDefinition{}, note.Relationships...),
			memorynodes.RelationshipDefinition{Type: "references", Field: accountField, TargetConcept: conceptAccountsAccount, Direction: relationshipDirectionOutgoing}),
		BelongsTo: note.BelongsTo,
	}
	memorynodes.MergeAll(map[string]*memorynodes.Concept{name: fixture})
	t.Cleanup(func() { memorynodes.DefaultRegistry().(*memorynodes.MemoryRegistry).Remove(name) })
	return name
}
