package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// Exercise the actual named queries and cursor across two engine instances:
// pagination and search cannot rely on a browser having loaded prior records
// or on the replica that served the previous page retaining any state.
func TestOSResourceBrowsePagesAndSearchAcrossReplicas(t *testing.T) {
	first, db, _ := sharedReadMergeEngine(t)
	second, err := New(db)
	require.NoError(t, err)
	require.NoError(t, bootPrivateEngine(second, memorynodes.DefaultRegistry()))
	f := shareDirFixture{t: t, eng: first, db: db, prefix: uniqueSuffix("os-browse")}
	owner := f.person("owner", true)
	stranger := f.person("stranger", true)
	ctx := shareDirActor(context.Background(), owner, auth.RoleOwner)
	hook := &osListEvidenceCapture{}
	db.AddQueryHook(hook)
	ctx = context.WithValue(ctx, osListEvidenceContextKey{}, true)
	base := time.Now().UTC().Add(-time.Hour)
	types := []struct {
		concept    string
		query      string
		timeField  string
		nameField  string
		heavyField string
	}{
		{"v1:pipelines:run", "pipelineRunsBrowse", "queuedAt", "title", "policySnapshot"},
		{"v1:pipelines:pipeline", "pipelinesBrowse", "connectedAt", "name", "timings"},
		{"v1:worker:registration", "myMachinesBrowse", "registeredAt", "displayName", ""},
	}
	for _, resource := range types {
		t.Run(resource.query, func(t *testing.T) {
			var want []string
			for i := 0; i < 25; i++ {
				id := f.id(resource.concept, fmt.Sprintf("item-%02d", i))
				name := fmt.Sprintf("Recent item %02d", i)
				if i < 12 {
					name = fmt.Sprintf("Earlier NEEDLE%%_ item %02d", i)
				}
				payload := map[string]any{
					"ownerUserId": owner, resource.nameField: name,
					// Deliberate timestamp ties prove the ID tiebreaker.
					resource.timeField: base.Add(time.Duration(i/2) * time.Minute).Format(time.RFC3339Nano),
					"repository":       "acme/example", "status": "completed", "conclusion": "success",
					"pipelineId": "pipeline", "headBranch": "main", "revokedAt": "",
				}
				if resource.heavyField != "" {
					payload[resource.heavyField] = map[string]any{"privateExecutionMetadata": strings.Repeat("x", 14*1024)}
				}
				f.insert(resource.concept, id, payload)
				want = append([]string{BareShortId(id)}, want...)
			}
			f.insert(resource.concept, f.id(resource.concept, "foreign"), map[string]any{
				"ownerUserId": stranger, resource.nameField: "Earlier NEEDLE%_ foreign",
				resource.timeField: base.Add(2 * time.Hour).Format(time.RFC3339Nano),
			})
			walk := func(call string, sizes []int) []string {
				t.Helper()
				var got []string
				cursor := ""
				for page, size := range sizes {
					eng := []*MemQLEngine{first, second}[page%2]
					result, err := eng.Execute(ContextWithCursor(ctx, cursor), call)
					require.NoError(t, err, call)
					rows := MaterializeRows(result.OutputPayload())
					require.Len(t, rows, size, call)
					wire, err := json.Marshal(result.OutputPayload())
					require.NoError(t, err)
					require.Less(t, len(wire), 24*1024, "list response must not contain execution-policy payloads")
					for _, node := range rows {
						got = append(got, BareShortId(fmt.Sprint(node["id"])))
						if resource.heavyField != "" {
							require.NotContains(t, node, resource.heavyField)
						}
					}
					cursor = result.GetMeta().Cursor
					if page < len(sizes)-1 {
						require.NotEmpty(t, cursor)
					} else {
						require.Empty(t, cursor)
					}
				}
				return got
			}
			require.Equal(t, want, walk("query "+resource.query+"()", []int{10, 10, 5}))
			// Every match is older than the first page. Search must find all
			// twelve, including literal wildcard characters, before paging.
			require.Equal(t, want[13:], walk("query "+resource.query+`(search: "needle%_")`, []int{10, 2}))
		})
	}
	hook.mu.Lock()
	queries := append([]string(nil), hook.queries...)
	hook.mu.Unlock()
	primary := 0
	for _, query := range queries {
		if strings.Contains(query, "AS latest_keys") && strings.Contains(query, `COLLATE "C"`) {
			primary++
			require.True(t, strings.HasSuffix(query, "LIMIT 10"), "the resource page must be bounded in SQL before materialization: %s", query)
		}
	}
	require.Positive(t, primary)
}

// A real server-authorized mutation publishes a versioned event; a separate
// engine sees the new latest row and re-evaluates the server-side facets. This
// guards the write -> invalidate -> reread path the browser's session cache
// depends on, without relying on request-local or originating-engine state.
func TestOSBrowseMutationEventsAndReplicaRead(t *testing.T) {
	writer, db, _ := readMergeTestEngine(t)
	reader, err := New(db)
	require.NoError(t, err)
	require.NoError(t, bootPrivateEngine(reader, memorynodes.DefaultRegistry()))
	reader.Logger = writer.Logger
	bus := events.NewBus()
	t.Cleanup(bus.Close)
	writer.SetEventBus(bus)
	reader.SetEventBus(bus)
	lifecycle, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	reader.StartCacheInvalidationSubscriber(lifecycle)
	f := shareDirFixture{t: t, eng: writer, db: db, prefix: uniqueSuffix("browse-mutation")}
	owner := f.person("owner", true)
	actor := shareDirActor(context.Background(), owner, auth.RoleOwner)
	internal := auth.ContextWithInternalOrigin(actor)
	updates := make(chan map[string]any, 4)
	t.Cleanup(bus.Subscribe("graph.node.updated.v1:pipelines:run", func(ev events.Event) { updates <- ev.Payload }))
	args := map[string]any{
		"runId": f.id("v1:pipelines:run", "run"), "pipelineId": "test-pipeline",
		"repository": "example/mutation", "sha": "abc123", "mode": "full", "event": "manual",
		"runKey": f.prefix, "attempt": 1, "trigger": "manual", "title": "Mutation search needle",
		"status": "queued", "queuedAt": time.Now().UTC().Format(time.RFC3339Nano),
		"policySnapshot": map[string]any{"workflow": strings.Repeat("x", 14*1024)},
	}
	stored := runMutation(t, internal, writer, "createPipelineRun", args)
	t.Cleanup(func() {
		_, _ = db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).Where("id = ?", stored).Exec(context.Background())
	})
	read := func(call string) []map[string]any {
		t.Helper()
		result, err := reader.Execute(actor, call)
		require.NoError(t, err)
		return MaterializeRows(result.OutputPayload())
	}
	require.Len(t, read(`query pipelineRunsBrowse(outcome:"queued",search:"NEEDLE")`), 1)
	runMutation(t, internal, writer, "updatePipelineRun", map[string]any{"runId": stored, "status": "completed", "conclusion": "success"})
	select {
	case event := <-updates:
		requireVersionTime(t, storedVersionTime(t, actor, db, "v1:pipelines:run", stored), event["createdAt"], "update event version must match the persisted row")
	case <-time.After(5 * time.Second):
		t.Fatal("updatePipelineRun published no update event")
	}
	require.Eventually(t, func() bool { return len(read(`query pipelineRunsBrowse(outcome:"queued",search:"NEEDLE")`)) == 0 }, time.Second, 10*time.Millisecond)
	rows := read(`query pipelineRunsBrowse(outcome:"passed",search:"NEEDLE")`)
	require.Len(t, rows, 1)
	require.Equal(t, "completed", rows[0]["status"])
	require.Equal(t, "success", rows[0]["conclusion"])
	require.NotContains(t, rows[0], "policySnapshot")
	detail := read(fmt.Sprintf(`query pipelineRunForOwner(runId:%s)`, langparser.QuoteString(stored)))
	require.Len(t, detail, 1)
	require.Contains(t, detail[0], "policySnapshot", "detail still has the full resource")
}

func TestOSMachineBrowseScopesRevokedAndPresencePayloads(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	f := shareDirFixture{t: t, eng: eng, db: db, prefix: uniqueSuffix("os-presence")}
	owner := f.person("owner", true)
	ctx := shareDirActor(context.Background(), owner, auth.RoleOwner)
	for i, revoked := range []string{"", time.Now().UTC().Format(time.RFC3339Nano)} {
		f.insert("v1:worker:registration", f.id("v1:worker:registration", fmt.Sprint(i)), map[string]any{
			"ownerUserId": owner, "name": "Machine", "registeredAt": time.Now().UTC().Format(time.RFC3339Nano),
			"revokedAt": revoked, "platformInfo": map[string]any{"hostname": "Special-HOST"},
			"hardware": map[string]any{"large": "equipment"}, "apps": []string{"large app inventory"},
		})
	}
	for _, tc := range []struct {
		call string
		size int
	}{
		{`query myMachinesBrowse(search: "SPECIAL-host")`, 1},
		{`query myMachinesBrowse(search: "SPECIAL-host", includeRevoked: true)`, 2},
		{`query myMachinePresence()`, 2},
	} {
		result, err := eng.Execute(ctx, tc.call)
		require.NoError(t, err)
		rows := MaterializeRows(result.OutputPayload())
		require.Len(t, rows, tc.size)
		if tc.call == `query myMachinePresence()` {
			for _, row := range rows {
				require.NotContains(t, row, "hardware")
				require.NotContains(t, row, "apps")
			}
		}
	}
}
