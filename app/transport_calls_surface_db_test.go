package app

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/server"
)

// transport_calls_surface_db_test.go -- the calls surface's one read of its own, against a
// real engine and a real Postgres: the site, read under the stamped owner. The handler's
// tests fake this read, so what the site's tier really admits, and that siteById really
// projects the calls switch, are said here and nowhere else before the edge's end-to-end
// test. Postgres-gated: a skip is what an unreachable database produces, and
// MEMQL_REQUIRE_DB=1 turns it into a failure.

func TestCallsSiteReadAnswersWhatTheTierAdmits(t *testing.T) {
	eng, db := workTemplateDBEngineAndDB(t)
	world := newAccessWorld(t, &accessSurfaceRig{eng: eng, db: db})
	surface := &callsSurface{engine: &callsEngineAdapter{engine: eng}}
	ctx := context.Background()

	on := "cs-on-" + world.suffix
	world.insert("v1:platform:site", on, map[string]any{
		"hostname": on + ".example.test", "kind": "spa", "status": "draft", "bundleRef": "blob://sites/" + on + "/v1/",
		"ownerUserId": "v1:identity:user:" + world.owner, "accountId": world.account,
		"packageId": "v1:platform:package:" + world.pkg, "packageDeployableName": "meetings", "calls": true,
	})

	t.Run("the owner reads the site and its switch", func(t *testing.T) {
		site, err := surface.SiteAsOwner(ctx, on, world.owner)
		require.NoError(t, err)
		require.Equal(t, &server.CallsSite{ID: on, OwnerUserID: world.owner, DeployableName: "meetings", Calls: true}, site,
			"ids bare and the switch projected: siteById's shape has to carry calls, or every site reads as off")
	})

	// THE ACCOUNT ARM ADMITS A MEMBER OF THE SITE'S ORGANIZATION, so the row coming back
	// proves nothing about the stamp: it comes back naming its real owner, and the handler's
	// comparison of the two is what refuses a forged owner who is a member.
	t.Run("a member of the site's organization reads the row, naming its real owner", func(t *testing.T) {
		site, err := surface.SiteAsOwner(ctx, on, world.member)
		require.NoError(t, err)
		require.NotNil(t, site, "the account arm no longer admits a member; the comparison is then belt and braces")
		require.Equal(t, world.owner, site.OwnerUserID)
	})

	t.Run("a stranger reads nothing", func(t *testing.T) {
		site, err := surface.SiteAsOwner(ctx, on, world.stranger)
		require.NoError(t, err)
		require.Nil(t, site)
	})

	t.Run("a site written before the switch existed is off", func(t *testing.T) {
		site, err := surface.SiteAsOwner(ctx, world.site, world.owner)
		require.NoError(t, err)
		require.NotNil(t, site)
		require.False(t, site.Calls)
	})
}
