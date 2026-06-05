package app

// engine_authored_test.go -- unit coverage for the authored-runtime bootstrap
// adapters (epic memql#954, issue #961 increment 5 / #959 live glue). The full
// wireAuthoredRuntime path needs a live engine + DB, so these cover the
// stand-alone adapters: the audit-sink mapping and the deps assembly.

import (
	"context"
	"log/slog"
	"testing"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/memql"
)

// recordingAuditLogger captures identity audit events.
type recordingAuditLogger struct{ events []identity.AuditEvent }

func (r *recordingAuditLogger) Log(_ context.Context, ev identity.AuditEvent) {
	r.events = append(r.events, ev)
}

// TestAuthoredAuditSink_MapsToIdentityAudit: the sink adapter maps an
// AuthoredAuditEvent onto an identity AuditEvent with the bundle as target and
// the owner as actor, under the configuration category.
func TestAuthoredAuditSink_MapsToIdentityAudit(t *testing.T) {
	rec := &recordingAuditLogger{}
	sink := authoredAuditSink{logger: rec}

	sink.EmitAuthoredAudit(context.Background(), memql.AuthoredAuditEvent{
		Action:      memql.AuditActionBundleActivated,
		OwnerUserId: "user-a",
		BundleId:    "v1:authoring:bundle:b1",
		Detail:      map[string]any{"version": 1},
	})

	if len(rec.events) != 1 {
		t.Fatalf("expected one audit event, got %d", len(rec.events))
	}
	ev := rec.events[0]
	if ev.Action != memql.AuditActionBundleActivated {
		t.Errorf("action not carried: %q", ev.Action)
	}
	if ev.ActorUserId != "user-a" || ev.TargetId != "v1:authoring:bundle:b1" {
		t.Errorf("actor/target not mapped: %+v", ev)
	}
	if ev.Category != identity.AuditCategoryConfiguration || ev.Outcome != identity.AuditOutcomeSuccess {
		t.Errorf("category/outcome not set: %+v", ev)
	}
}

// TestAuthoredAuditSink_NilLoggerSafe: a nil underlying logger is a no-op.
func TestAuthoredAuditSink_NilLoggerSafe(t *testing.T) {
	sink := authoredAuditSink{logger: nil}
	sink.EmitAuthoredAudit(context.Background(), memql.AuthoredAuditEvent{Action: "x"}) // must not panic
}

// TestAuthoredRuntimeDeps_MinimalApp: with only the registry constructed (no
// scheduler / engine), the deps carry the registry + logger and leave the
// optional hooks nil -- the graceful-degradation contract.
func TestAuthoredRuntimeDeps_MinimalApp(t *testing.T) {
	a := &App{Logger: slog.Default(), authoredRuntime: memql.NewAuthoredRuntimeRegistry()}
	deps := a.AuthoredRuntimeDeps()
	if deps.Registry == nil {
		t.Fatal("deps must carry the registry")
	}
	if deps.RegisterHook != nil || deps.PromoteCatalog != nil || deps.Audit != nil {
		t.Error("optional hooks should be nil when scheduler/engine are absent")
	}
}
