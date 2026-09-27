package memql

// tool_gate_rank_db_test.go -- a tool's @requiresRank judges the PERSON an
// agent acts for, read from the real principal table, when the actor on the
// context carries a stand-in role (memql#5438).
//
// Borrowed authority (auth.ContextWithUserActor) and work restored without a
// captured grant (auth.ContextWithPersistedOwner) both assert "writer" whatever
// the person holds. Judged as they stood, an agent acting for a READER cleared
// a writer floor and one acting for an ADMIN failed an admin floor. The unit
// test beside this one proves a person who resolves no role clears nothing;
// only a real principal table can prove the two directions, because only it
// can hold a reader and an admin.
//
// Postgres-gated like its neighbours; CI's db-tests lane sets
// MEMQL_REQUIRE_DB=1, so a skip there is a failure rather than a green.

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

func TestAToolFloorJudgesThePersonAnAgentActsForNotTheStandInRole(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	suffix := uniqueSuffix("toolfloor5438")
	reader := "reader-" + suffix
	admin := "admin-" + suffix
	seedPrincipal(t, eng, reader, auth.RoleReader)
	seedPrincipal(t, eng, admin, auth.RoleAdmin)

	writerFloor := &Tool{Name: "fileRequest", RequiresRank: "writer"}
	adminFloor := &Tool{Name: "approveRequest", RequiresRank: "admin"}

	// The two stand-in actors, each as an agent's tool loop sees it.
	standIns := map[string]func(t *testing.T, person string) context.Context{
		"borrowed authority": func(t *testing.T, person string) context.Context {
			return WithActingAgentRole(auth.ContextWithUserActor(context.Background(), person), "assistant")
		},
		"work with no captured grant": func(t *testing.T, person string) context.Context {
			ctx, err := auth.ContextWithPersistedOwner(context.Background(), person, nil, nil)
			if err != nil {
				t.Fatalf("restore %s: %v", person, err)
			}
			return WithActingAgentRole(ctx, "assistant")
		},
	}
	for name, as := range standIns {
		t.Run(name, func(t *testing.T) {
			// An agent acting for a READER must not clear a writer floor. The
			// refusal must name the reader's own role: a refusal for any other
			// reason -- the person not resolving at all -- would pass a weaker
			// assertion without the resolution having happened.
			forReader := as(t, reader)
			switch err := eng.ToolCallRefusal(forReader, writerFloor); {
			case err == nil:
				t.Error("an agent acting for a reader cleared a writer floor: its stand-in role was judged instead of the person")
			case !strings.Contains(err.Error(), `this caller holds "reader"`):
				t.Errorf("the refusal was %q; it should be the floor judging the reader's own role", err)
			}
			if eng.ToolListed(forReader, writerFloor) {
				t.Error("the writer-floored tool was listed for an agent acting for a reader")
			}

			// An agent acting for an ADMIN must clear an admin floor.
			forAdmin := as(t, admin)
			if err := eng.ToolCallRefusal(forAdmin, adminFloor); err != nil {
				t.Errorf("an agent acting for an admin was refused an admin floor: %v", err)
			}
			if !eng.ToolListed(forAdmin, adminFloor) {
				t.Error("the admin-floored tool was not listed for an agent acting for an admin")
			}

			// The person's role decides the FLOOR only. The call still runs
			// under the stand-in, which is what bounds what the work may write.
			if ac, _ := auth.AccessFromContext(forAdmin); ac == nil || ac.Role != auth.RoleWriter {
				t.Fatalf("the caller's own actor changed: %+v", ac)
			}
		})
	}
}
