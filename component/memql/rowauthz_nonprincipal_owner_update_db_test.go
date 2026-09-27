package memql

// A SYSTEM ACTOR'S WRITE NEVER CHANGES WHO OWNS AN EXISTING ROW (memql#5437).
//
// The db-backed half of the memql#4817 rule (rowauthz_nonprincipal_owner.go),
// over a fleet-shaped fixture: an owned concept whose every mutation re-stamps
// `ownerUserId: actor.userId`, written by the actor a shipped automation runs
// as -- synthetic, a reader, at internal origin (component/automations'
// contextWithSystemActor and its trusted step origin, rebuilt here because this
// package cannot import that one).
//
// The defect it guards was measured before it was fixed: the fleet's newly
// loaded sweeps call exactly such mutations on customers' instance and
// subscription rows, and the undo blanked the stamp after the read-merge had
// written it over the stored owner -- `v1:identity:user:<id>` became "" with no
// error, and the customer could no longer read their own row.
//
// Engine-mutating (a fixture domain mounted into the global tree), so it boots
// its own engine once and runs every case against it.

import (
	"context"
	"fmt"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

const (
	ownerKeepDomain = "ownkeep5437"
	ownerKeepGizmo  = "v1:ownkeep5437:gizmo"
	ownerKeepCog    = "v1:ownkeep5437:cog"
)

const ownerKeepConcepts = `use identity.concepts.{ user }

/// A fleet-shaped owned row: a required owner, re-stamped from the actor on every write.
@rowAuthz(owner="ownerUserId")
concept gizmo {
  ownerUserId string!
  status      string

  @relationship(type="parent", field="ownerUserId", target=user, direction="outgoing")
}

/// The same tier with an OPTIONAL owner, so a row can be stored with no owner key at all.
@rowAuthz(owner="ownerUserId")
concept cog {
  ownerUserId string
  status      string

  @relationship(type="parent", field="ownerUserId", target=user, direction="outgoing")
}
`

const ownerKeepMutations = `@actor
mutation gizmo createGizmoOwnerKeep {
  args {
    gizmoId string!
  }
  insert {
    id: args.gizmoId
    ownerUserId: actor.userId
    status: "running"
  }
}

@actor
mutation gizmo suspendGizmoOwnerKeep {
  args {
    gizmoId string!
  }
  update {
    id: args.gizmoId
    ownerUserId: actor.userId
    status: "suspending"
  }
}

mutation cog createCogWithoutOwner {
  args {
    cogId string!
  }
  insert {
    id: args.cogId
    status: "running"
  }
}

@actor
mutation cog suspendCogOwnerKeep {
  args {
    cogId string!
  }
  update {
    id: args.cogId
    ownerUserId: actor.userId
    status: "suspending"
  }
}
`

// ownerKeepAutomationActor is the context a shipped automation's step runs
// under: its own synthetic principal, a reader, stamped internal origin because
// its source came from the loaded tree.
func ownerKeepAutomationActor(name string) context.Context {
	actorId := "system:automation:" + name
	claims := map[string]any{"sub": actorId, "email": actorId, "role": "system"}
	ctx := auth.ContextWithClaims(context.Background(), claims)
	ctx = auth.ContextWithToken(ctx, auth.BuildTokenInfo(claims))
	ctx = auth.ContextWithAccess(ctx, &auth.AccessContext{
		UserId: actorId, Role: auth.RoleReader, Unranked: true, Synthetic: true,
	})
	return auth.ContextWithInternalOrigin(ctx)
}

func TestASystemActorsWriteNeverChangesWhoOwnsAnExistingRow(t *testing.T) {
	before := memorynodes.All()
	memqldsl.RegisterTree(ownerKeepDomain, withLanguageLine(fstest.MapFS{
		"concepts.memql":  {Data: []byte(ownerKeepConcepts)},
		"mutations.memql": {Data: []byte(ownerKeepMutations)},
	}))
	t.Cleanup(func() {
		memqldsl.UnregisterTree(ownerKeepDomain)
		memorynodes.ReplaceAll(before)
	})
	eng, db, ctx := readMergeTestEngine(t)
	sweep := ownerKeepAutomationActor("trialExpirySuspend")
	// Ids unique per RUN, not per process: `-count=N` reruns in one process,
	// and a second create landing on the first run's row is an update -- with
	// that run's owner -- which is a different case from the one named.
	unique := func(label string) string {
		return fmt.Sprintf("%s-%d", uniqueSuffix(label), time.Now().UnixNano())
	}

	// The fleet's case, and the defect: a customer's row, suspended by a sweep.
	t.Run("an update keeps the stored owner", func(t *testing.T) {
		id := "g-" + unique("keep")
		owner := "owner-5437-" + unique("keep")
		stored := runMutation(t, auth.ContextWithUserActor(ctx, owner), eng, "createGizmoOwnerKeep", map[string]any{"gizmoId": id})
		was := latestPayload(t, ctx, db, ownerKeepGizmo, stored)
		require.NotEmpty(t, was["ownerUserId"], "the owner's create must stamp them")

		runMutation(t, sweep, eng, "suspendGizmoOwnerKeep", map[string]any{"gizmoId": id})

		now := latestPayload(t, ctx, db, ownerKeepGizmo, stored)
		require.Equal(t, "suspending", now["status"], "the automation's write must land")
		require.Equal(t, was["ownerUserId"], now["ownerUserId"],
			"a system actor's update changed who owns the row: its own stamp was undone by blanking it "+
				"rather than by restoring the stored owner, which hands a customer's row to the cluster")
	})

	// Inserts keep the memql#4817 rule: the caller owns it or nobody does.
	t.Run("a create is cluster-owned", func(t *testing.T) {
		id := "g-" + unique("create")
		stored := runMutation(t, sweep, eng, "createGizmoOwnerKeep", map[string]any{"gizmoId": id})
		p := latestPayload(t, ctx, db, ownerKeepGizmo, stored)
		owner, present := p["ownerUserId"]
		require.True(t, present, "a system actor's create must say 'nobody' (present and empty), not leave the owner unsaid")
		require.Equal(t, "", owner, "a synthetic id must never own a row it creates")
	})

	t.Run("a present-and-empty owner stays empty", func(t *testing.T) {
		id := "g-" + unique("empty")
		stored := runMutation(t, sweep, eng, "createGizmoOwnerKeep", map[string]any{"gizmoId": id})
		runMutation(t, sweep, eng, "suspendGizmoOwnerKeep", map[string]any{"gizmoId": id})
		p := latestPayload(t, ctx, db, ownerKeepGizmo, stored)
		owner, present := p["ownerUserId"]
		require.True(t, present, "a cluster-owned row must stay a statement of cluster ownership")
		require.Equal(t, "", owner)
	})

	t.Run("an absent owner stays absent", func(t *testing.T) {
		id := "c-" + unique("absent")
		stored := runMutation(t, auth.ContextWithUserActor(ctx, "owner-5437-absent"), eng, "createCogWithoutOwner", map[string]any{"cogId": id})
		_, had := latestPayload(t, ctx, db, ownerKeepCog, stored)["ownerUserId"]
		require.False(t, had, "fixture: the row must be stored with no owner key")

		runMutation(t, sweep, eng, "suspendCogOwnerKeep", map[string]any{"cogId": id})

		p := latestPayload(t, ctx, db, ownerKeepCog, stored)
		require.Equal(t, "suspending", p["status"])
		_, has := p["ownerUserId"]
		require.False(t, has, "a row stored with no owner key must keep none: an absent owner and a present-and-empty one are read differently")
	})

	// The row memql#4817 exists to repair: owned by the synthetic id itself.
	// Restoring that would keep it forever, so it is still blanked.
	t.Run("a row the actor itself owns is still repaired", func(t *testing.T) {
		id := "g-" + unique("legacy")
		legacy := auth.ContextWithUserActor(ctx, "system:automation:trialExpirySuspend")
		stored := runMutation(t, legacy, eng, "createGizmoOwnerKeep", map[string]any{"gizmoId": id})
		require.NotEmpty(t, latestPayload(t, ctx, db, ownerKeepGizmo, stored)["ownerUserId"],
			"fixture: the row must be stored owned by the synthetic id")

		runMutation(t, sweep, eng, "suspendGizmoOwnerKeep", map[string]any{"gizmoId": id})

		p := latestPayload(t, ctx, db, ownerKeepGizmo, stored)
		require.Equal(t, "", p["ownerUserId"], "a row owned by the actor's own synthetic id must be blanked, as on a create")
	})

	// Borrowed authority is a real person (Unranked, NOT Synthetic): the rule
	// does not apply, and the rows it writes are that person's.
	t.Run("borrowed authority is unchanged", func(t *testing.T) {
		id := "g-" + unique("borrowed")
		person := "owner-5437-" + unique("borrowed")
		borrowed := auth.ContextWithUserActor(ctx, person)
		stored := runMutation(t, borrowed, eng, "createGizmoOwnerKeep", map[string]any{"gizmoId": id})
		was := latestPayload(t, ctx, db, ownerKeepGizmo, stored)
		require.NotEqual(t, "", was["ownerUserId"], "borrowed authority's create must be owned by the person")

		runMutation(t, borrowed, eng, "suspendGizmoOwnerKeep", map[string]any{"gizmoId": id})
		require.Equal(t, was["ownerUserId"], latestPayload(t, ctx, db, ownerKeepGizmo, stored)["ownerUserId"])
	})
}
