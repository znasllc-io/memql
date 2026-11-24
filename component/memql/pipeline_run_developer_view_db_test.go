package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// DEVELOPERS SEE EVERY PIPELINE RUN, VIEW ONLY (design record
// docs/superpowers/specs/2026-11-24-pipelines-developer-view-design.md).
//
// The run is tied to no account and owned by somebody else -- memQL's own
// pipeline is that shape, and the account arm never reached it. Reads run on a
// second replica that holds nothing from the seed: the floor resolves the
// caller's rank per request, and no node keeps any of it.
func TestDevelopersReadEveryPipelineRunAndWriteNone(t *testing.T) {
	first, db, _ := sharedReadMergeEngine(t)
	replica, err := New(db)
	require.NoError(t, err)
	require.NoError(t, bootPrivateEngine(replica, memorynodes.DefaultRegistry()))
	f := shareDirFixture{t: t, eng: first, db: db, prefix: uniqueSuffix("pipeline-view")}

	const concept = "v1:pipelines:run"
	runID := f.id(concept, "run")
	payload := map[string]any{
		"ownerUserId": f.person("owner", true), "accountId": "", "pipelineId": "pl", "repository": "acme/" + f.prefix,
		"sha": "abc123", "mode": "full", "event": "push", "runKey": f.prefix, "attempt": 1, "trigger": "webhook",
		"status": "completed", "conclusion": "failure", "title": "Untied " + f.prefix,
		"queuedAt": time.Now().UTC().Format(time.RFC3339Nano),
	}
	f.insert(concept, runID, payload)
	raw, err := json.Marshal(payload)
	require.NoError(t, err)

	as := func(role auth.Role) context.Context {
		return shareDirActor(context.Background(), f.prefix+"-"+string(role), role)
	}
	rows := func(ctx context.Context, call string) (int, error) {
		res, err := replica.Execute(ctx, call)
		if err != nil {
			return 0, err
		}
		return len(MaterializeRows(res.OutputPayload())), nil
	}
	id := langparser.QuoteString(BareShortId(runID))
	view := fmt.Sprintf(`query pipelineRunView(runId: %s)`, id)

	for call, want := range map[string]int{
		view: 1,
		fmt.Sprintf(`query pipelineRunsBrowseAll(search: %s)`, langparser.QuoteString(f.prefix)): 1,
		// Owner-only, and every act resolves through it: re-run and cancel
		// by a non-owner are refused because this answers nothing.
		fmt.Sprintf(`query pipelineRunForOwner(runId: %s)`, id): 0,
	} {
		got, err := rows(as(auth.RoleDeveloper), call)
		require.NoError(t, err, call)
		require.Equal(t, want, got, call)
	}
	_, err = rows(as(auth.RoleAdmin), view)
	require.Error(t, err, "an admin ranks below the developer floor")

	dev := contextWithRankScopeMemo(as(auth.RoleDeveloper), replica)
	admin := contextWithRankScopeMemo(as(auth.RoleAdmin), replica)
	require.Equal(t, rowAuthzAdmit, rowAuthzAdmits(dev, concept, runID, raw))
	require.NotEqual(t, rowAuthzAdmit, rowAuthzAdmitsWrite(dev, concept, runID, raw), "the floor never widens a write")
	require.NotEqual(t, rowAuthzAdmit, rowAuthzAdmits(admin, concept, runID, raw))

	// The live feed follows the read.
	stream := replica.SubscriptionRankContext(context.Background())
	require.Equal(t, SubscriptionAdmit, AdmitSubscriptionRow(stream, &auth.AccessContext{UserId: f.prefix + "-dev", Role: auth.RoleDeveloper}, concept, runID, raw))
	require.Equal(t, SubscriptionDeny, AdmitSubscriptionRow(stream, &auth.AccessContext{UserId: f.prefix + "-admin", Role: auth.RoleAdmin}, concept, runID, raw))
}
