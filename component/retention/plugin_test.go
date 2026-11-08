package retention

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/memql"
)

type retentionAuthorityRegistry struct {
	t      *testing.T
	actor  *auth.AccessContext
	want   *auth.AccessContext
	calls  int
	result error
}

func (r *retentionAuthorityRegistry) Execute(ctx context.Context, step *automations.Step, _ *automations.StepContext) (*automations.StepResult, error) {
	r.t.Helper()
	if step.Function == nil || step.Function.Kind != "builtin" || step.Function.Name != "conceptRetentionSweep" {
		r.t.Fatalf("declared retention did not reach its production builtin: %+v", step)
	}
	r.calls++
	r.actor, _ = auth.AccessFromContext(ctx)
	if r.want != nil && r.actor != r.want {
		r.t.Fatalf("inherited caller was replaced: got %+v, want %+v", r.actor, r.want)
	}
	// Reach the real owner guard with the executor's context. There is no DB:
	// its unavailable answer is the positive witness that authorization passed,
	// and neither the scheduled nor manual fixture can retire any records.
	_, r.result = (&Integration{}).sweep(ctx, step.Function.Args, 0)
	return &automations.StepResult{StepId: step.ID, Status: "completed"}, nil
}

func TestDeclaredRetentionScheduledAuthority(t *testing.T) {
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	automation, err := automations.NewLoader(automations.LoaderOptions{}).LoadByName("retireDeclaredConceptVersions")
	if err != nil {
		t.Fatal(err)
	}
	if automation.Name != "retireDeclaredConceptVersions" || automation.Schedule != "0 40 * * * *" {
		t.Fatalf("expected the engine's hourly declared-retention automation: %+v", automation)
	}

	for _, tc := range []struct {
		name      string
		context   context.Context
		trigger   string
		wantOwner bool
	}{
		{"core schedule", context.Background(), "schedule", true},
		{"manual owner", auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "owner", Role: auth.RoleOwner}), "manual", true},
		{"manual reader", auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "reader", Role: auth.RoleReader}), "manual", false},
		{"manual writer", auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "writer", Role: auth.RoleWriter}), "manual", false},
		{"manual admin", auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "admin", Role: auth.RoleAdmin}), "manual", false},
		{"manual developer", auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "developer", Role: auth.RoleDeveloper}), "manual", false},
		{"manual anonymous", auth.ContextWithAnonymousActor(context.Background()), "manual", false},
		{"manual connector", auth.ContextWithAccess(context.Background(), auth.ConnectorActor("shopify")), "manual", false},
		{"authored schedule", automations.AuthorContext(context.Background(), "author"), "schedule", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inherited, _ := auth.AccessFromContext(tc.context)
			registry := &retentionAuthorityRegistry{t: t, want: inherited}
			executor := automations.NewExecutor(automations.ExecutorOptions{StepRegistry: registry})
			t.Cleanup(executor.Close)
			execution, err := executor.Execute(tc.context, automation, tc.trigger)
			if err != nil || execution.Status != "completed" || registry.calls != 1 {
				t.Fatalf("production automation did not reach one guard: execution=%+v calls=%d err=%v", execution, registry.calls, err)
			}
			wantError := "concept retention requires a cluster owner"
			if tc.wantOwner {
				wantError = "concept retention database unavailable"
			}
			if registry.result == nil || registry.result.Error() != wantError {
				t.Fatalf("retention authority: got %v, want %q", registry.result, wantError)
			}
			if inherited == nil && (registry.actor == nil || registry.actor.UserId != "system:maintenance:retireDeclaredConceptVersions" || !registry.actor.IsClusterOwner() || !registry.actor.Synthetic || !registry.actor.Unranked) {
				t.Fatalf("scheduled sweep lacks its named maintenance principal: %+v", registry.actor)
			}
		})
	}
}

func TestDeclaredRetentionDirectCallsKeepOwnerFloor(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		for _, tc := range []struct {
			name    string
			context context.Context
		}{
			{"absent", context.Background()},
			{"unlisted system automation", auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "system:automation:retireDeclaredConceptVersions", Role: auth.RoleReader, Unranked: true, Synthetic: true})},
			{"claimed maintenance name", auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "system:maintenance:retireDeclaredConceptVersions", Role: auth.RoleReader})},
		} {
			_, err := (&Integration{}).sweep(tc.context, map[string]any{"dryRun": dryRun}, 0)
			if err == nil || err.Error() != "concept retention requires a cluster owner" {
				t.Fatalf("%s dryRun=%v passed the owner floor: %v", tc.name, dryRun, err)
			}
		}
	}
}

// compactionAuthorityRegistry answers the reclaim automation's one step with the
// real compaction handler, as retentionAuthorityRegistry does for the sweep.
type compactionAuthorityRegistry struct {
	t      *testing.T
	actor  *auth.AccessContext
	want   *auth.AccessContext
	args   map[string]any
	calls  int
	result error
}

func (r *compactionAuthorityRegistry) Execute(ctx context.Context, step *automations.Step, _ *automations.StepContext) (*automations.StepResult, error) {
	r.t.Helper()
	if step.Function == nil || step.Function.Kind != "builtin" || step.Function.Name != "closedChunkCompaction" {
		r.t.Fatalf("the reclaim automation did not reach its production builtin: %+v", step)
	}
	r.calls++
	r.args = step.Function.Args
	r.actor, _ = auth.AccessFromContext(ctx)
	if r.want != nil && r.actor != r.want {
		r.t.Fatalf("inherited caller was replaced: got %+v, want %+v", r.actor, r.want)
	}
	// No maintenance session: its unavailable answer is the positive witness that
	// authorization passed, and no fixture can rewrite anything.
	_, r.result = (&Integration{}).compact(ctx, step.Function.Args, 0)
	return &automations.StepResult{StepId: step.ID, Status: "completed"}, nil
}

// TestReclaimClosedChunkSpaceScheduledAuthority: the nightly automation is the
// engine's, at 04:20 UTC, after the 03:40 work-journal retention; its schedule
// reaches the handler as the maintenance principal (a cluster owner by grant),
// a person reaches it only as a cluster owner, and nobody else at all -- a dry
// run included, since even measuring spends the installation's I/O.
func TestReclaimClosedChunkSpaceScheduledAuthority(t *testing.T) {
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	automation, err := automations.NewLoader(automations.LoaderOptions{}).LoadByName("reclaimClosedChunkSpace")
	if err != nil {
		t.Fatal(err)
	}
	if automation.Schedule != "0 20 4 * * *" {
		t.Fatalf("expected the nightly 04:20 UTC compaction: %+v", automation)
	}
	for _, tc := range []struct {
		name      string
		context   context.Context
		trigger   string
		wantOwner bool
	}{
		{"core schedule", context.Background(), "schedule", true},
		{"manual owner", auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "owner", Role: auth.RoleOwner}), "manual", true},
		{"manual admin", auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "admin", Role: auth.RoleAdmin}), "manual", false},
		{"manual developer", auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "developer", Role: auth.RoleDeveloper}), "manual", false},
		{"manual reader", auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "reader", Role: auth.RoleReader}), "manual", false},
		{"manual anonymous", auth.ContextWithAnonymousActor(context.Background()), "manual", false},
		{"authored schedule", automations.AuthorContext(context.Background(), "author"), "schedule", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inherited, _ := auth.AccessFromContext(tc.context)
			registry := &compactionAuthorityRegistry{t: t, want: inherited}
			executor := automations.NewExecutor(automations.ExecutorOptions{StepRegistry: registry})
			t.Cleanup(executor.Close)
			execution, err := executor.Execute(tc.context, automation, tc.trigger)
			if err != nil || execution.Status != "completed" || registry.calls != 1 {
				t.Fatalf("production automation did not reach one guard: execution=%+v calls=%d err=%v", execution, registry.calls, err)
			}
			wantError := "chunk compaction requires a cluster owner"
			if tc.wantOwner {
				wantError = "chunk compaction database unavailable"
			}
			if registry.result == nil || registry.result.Error() != wantError {
				t.Fatalf("compaction authority: got %v, want %q", registry.result, wantError)
			}
			if inherited == nil && (registry.actor == nil || registry.actor.UserId != "system:maintenance:reclaimClosedChunkSpace" || !registry.actor.IsClusterOwner() || !registry.actor.Synthetic) {
				t.Fatalf("scheduled compaction lacks its named maintenance principal: %+v", registry.actor)
			}
			// The policy is the automation's to state, in the open: a real pass with
			// the thresholds the docs describe.
			opts := compactOptionsFrom(registry.args)
			if opts.DryRun || opts.MinReclaimBytes != 256<<20 || opts.MinReclaimPercent != 30 || opts.MeasureChunks != 4 {
				t.Fatalf("the nightly pass's policy moved: %+v from %v", opts, registry.args)
			}
		})
	}
}

// TestCompactionDirectCallsKeepOwnerFloor: a caller that only claims the
// principal's name, or an automation the list does not name, is refused before
// anything opens, dry run or not.
func TestCompactionDirectCallsKeepOwnerFloor(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		for _, tc := range []struct {
			name    string
			context context.Context
		}{
			{"absent", context.Background()},
			{"unlisted system automation", auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "system:automation:reclaimClosedChunkSpace", Role: auth.RoleReader, Unranked: true, Synthetic: true})},
			{"claimed maintenance name", auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "system:maintenance:reclaimClosedChunkSpace", Role: auth.RoleReader})},
		} {
			opened := false
			i := &Integration{context: memql.PluginContext{OpenMaintenanceSession: func(context.Context) (*sql.Conn, func(), error) {
				opened = true
				return nil, nil, errors.New("must not be reached")
			}}}
			_, err := i.compact(tc.context, map[string]any{"dryRun": dryRun}, 0)
			if err == nil || err.Error() != "chunk compaction requires a cluster owner" || opened {
				t.Fatalf("%s dryRun=%v passed the owner floor (opened=%v): %v", tc.name, dryRun, opened, err)
			}
		}
	}
}
