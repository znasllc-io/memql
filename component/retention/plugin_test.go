package retention

import (
	"context"
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
