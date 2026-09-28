package conformance

// worker_audit_db_test.go -- the LIVE half of the worker audit actor fix.
//
// Every worker-path audit event -- worker_registered / worker_disconnected, the
// cockpit's forwarded events, app_session_started / ended, and the dispatch
// gate's denials -- was refused by createAuditEvent once registration and user
// ids became canonical. worker.IdentityAuditor copied a free-form actor label
// ("user:<id>", "worker:<id>", "agent:<id>") into auditEvent.actorIdentityId,
// a relationship to v1:identity:identity, and the insert canonicalizes that
// field: "user:v1:identity:user:X" parses as v1:identity:user and is refused.
// The only trace was one audit_db_write_failed WARN per event; the persisted
// trail had no app session and no machine connect at all. The dispatch denials
// had a second refusal waiting behind the first: targetType "agent" was in
// neither enum.
//
// component/worker and integrations/agent/worker pin what the emitters hand the
// bridge, and test/dslconformance pins the enums and the field's shape
// statically. This is the database saying yes: the events the bridge produces
// now are written by the real EngineAuditSink through the real engine and read
// back, and the pre-fix shapes are still refused -- so the contract the fix
// rests on is pinned rather than assumed.
//
// Postgres-gated; MEMQL_REQUIRE_DB=1 turns the skip into a failure.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/worker"
)

// sinkThrough is an identity.AuditLogger that writes each event with the real
// DB sink and KEEPS the error, which SlogAuditLogger demotes to a WARN.
type sinkThrough struct {
	sink *identity.EngineAuditSink
	ctx  context.Context
	errs map[string]error
}

func (s *sinkThrough) Log(_ context.Context, ev identity.AuditEvent) {
	s.errs[ev.Action] = s.sink.WriteAuditEvent(s.ctx, ev)
}

func TestWorkerAuditEventsPersist(t *testing.T) {
	e := newEnv(t)
	if !e.HasDB {
		if os.Getenv("MEMQL_REQUIRE_DB") == "1" {
			t.Fatal("the worker audit write needs Postgres, and MEMQL_REQUIRE_DB=1 makes its absence a failure")
		}
		t.Skip("requires Postgres (set MEMQL_DATABASE_DSN); validated in the CI conformance job")
	}

	sfx := uniqueSuffix("wkaudit")
	owner := "v1:identity:user:owner-" + sfx
	credential := "v1:identity:identity:wktoken-" + sfx
	registration := "v1:worker:registration:reg-" + sfx
	session := "v1:worker:appSession:sess-" + sfx
	agent := "v1:agents:agent:agent-" + sfx

	through := &sinkThrough{sink: &identity.EngineAuditSink{Engine: e.Eng}, ctx: e.Ctx, errs: map[string]error{}}
	bridge := &worker.IdentityAuditor{AuditLogger: through}

	// The shapes the emitters produce (component/worker server.go, runner.go;
	// integrations/agent/worker dispatch.go), one per actor kind.
	cases := []struct {
		ev           worker.AuditEvent
		wantIdentity string
	}{
		{worker.AuditEvent{
			Action: "worker_registered", ActorIdentityId: credential, ActorLabel: "worker:" + registration,
			Target: registration, TargetType: "worker", OwnerUserId: owner,
			Detail: map[string]any{"name": "mbp"},
		}, credential},
		{worker.AuditEvent{
			Action: "app_session_started", ActorLabel: "engine",
			Target: session, TargetType: "appSession", OwnerUserId: owner,
			Detail: map[string]any{"workerId": registration, "workerIdentityId": credential},
		}, ""},
		{worker.AuditEvent{
			Action: "command_blocked", ActorLabel: "agent:" + agent,
			Target: agent, TargetType: "agent", OwnerUserId: owner,
			Detail: map[string]any{"tool": "workerHost", "action": "exec"},
		}, ""},
	}
	for _, c := range cases {
		bridge.Emit(e.Ctx, c.ev)
		if err := through.errs[c.ev.Action]; err != nil {
			t.Errorf("%s: createAuditEvent refused the event the bridge produces: %v", c.ev.Action, err)
			continue
		}
		row := e.auditRowFor(t, c.ev.Action, c.ev.Target)
		if row == nil {
			t.Errorf("%s: the write reported success and no v1:identity:auditEvent row targets %q", c.ev.Action, c.ev.Target)
			continue
		}
		if got, _ := row["actorIdentityId"].(string); got != c.wantIdentity {
			t.Errorf("%s: stored actorIdentityId = %q, want %q", c.ev.Action, got, c.wantIdentity)
		}
		if got, _ := row["targetType"].(string); got != c.ev.TargetType {
			t.Errorf("%s: stored targetType = %q, want %q", c.ev.Action, got, c.ev.TargetType)
		}
		detail, _ := row["detail"].(map[string]any)
		if detail["actor"] != c.ev.ActorLabel {
			t.Errorf("%s: stored detail.actor = %v, want the label %q", c.ev.Action, detail["actor"], c.ev.ActorLabel)
		}
	}

	// THE NEGATIVE CONTROL. A label in the identity field is refused, which is
	// the whole reason the actor had to split -- if this ever starts passing,
	// the relationship stopped being canonicalized and this test is guarding
	// nothing.
	for _, label := range []string{"user:" + owner, "worker:" + registration} {
		err := through.sink.WriteAuditEvent(e.Ctx, identity.AuditEvent{
			Category:      identity.AuditCategoryAuthorization,
			Action:        "worker_audit_label_probe",
			ActorUserId:   owner,
			ActorIdentity: label,
			TargetType:    "worker",
			TargetId:      "probe-" + sfx,
			Outcome:       identity.AuditOutcomeSuccess,
		})
		if err == nil || !strings.Contains(err.Error(), "is under concept") {
			t.Errorf("actorIdentityId %q was not refused as a foreign concept (err %v); the insert no longer "+
				"canonicalizes the relationship, so this test's premise is gone", label, err)
		}
	}
}

// auditRowFor reads the newest v1:identity:auditEvent with this action and
// target straight off the append-only table. The sink mints the row id itself,
// so the target -- unique per run -- is the handle.
func (e *Env) auditRowFor(t *testing.T, action, targetId string) map[string]any {
	t.Helper()
	var nodes []memoryNodes.MemoryNode
	err := e.DB.NewSelect().Model(&nodes).
		Where("concept = ?", "v1:identity:auditEvent").
		Where("payload->>'action' = ?", action).
		Where("payload->>'targetId' = ?", targetId).
		Order("createdAt DESC").
		Limit(1).
		Scan(e.Ctx)
	if err != nil {
		t.Fatalf("read-back of the %s audit row failed: %v", action, err)
	}
	if len(nodes) == 0 {
		return nil
	}
	var p map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &p); err != nil {
		t.Fatalf("decode the %s audit row: %v", action, err)
	}
	return p
}
