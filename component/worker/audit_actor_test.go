package worker

// audit_actor_test.go -- worker audit rows were refused at insert.
//
// v1:identity:auditEvent.actorIdentityId is a RELATIONSHIP to
// v1:identity:identity, and insert-time canonicalization refuses a value whose
// embedded id names a different concept (canonicalizeIdValue ->
// id.ParseNodeId, which takes the FIRST version segment anywhere in the
// string). Every worker-path event put a free-form label there --
// "user:<ownerUserId>" for an app session, "worker:<registrationId>" for a
// connect or disconnect -- so once those ids became canonical,
// "user:v1:identity:user:X" parsed as v1:identity:user and the row was refused.
// The slog line still went out, so the loss was one WARN
// (audit_db_write_failed) per event and nothing else: the persisted trail held
// no app session and no worker connect at all.
//
// These tests drive the REAL emitters -- register, a cockpit-forwarded audit
// event, disconnect, a whole app session -- through the real IdentityAuditor,
// and assert on the identity.AuditEvent it hands the pipeline, which is exactly
// what EngineAuditSink writes. The ids are canonical, as they are in a cluster:
// a bare fixture id has no version segment and passes canonicalization, which
// is how this hid behind green tests.

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/core/id"
)

const (
	auditOwner    = "v1:identity:user:u-audit"
	auditIdentity = "v1:identity:identity:ident-audit"
)

// recordingAuditLogger is the identity side of the bridge: what it receives is
// what the DB sink would be asked to write. It also keeps ctx.Err() as it was
// when each event arrived, because EngineAuditSink writes on that context: an
// event handed over on a context that is already done is refused by the engine
// before any row exists, and all that is left is one audit_db_write_failed WARN.
type recordingAuditLogger struct {
	mu      sync.Mutex
	events  []identity.AuditEvent
	ctxErrs []error
}

func (r *recordingAuditLogger) Log(ctx context.Context, ev identity.AuditEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	r.ctxErrs = append(r.ctxErrs, ctx.Err())
}

func (r *recordingAuditLogger) all() []identity.AuditEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]identity.AuditEvent(nil), r.events...)
}

// assertWritableContext fails for every event that reached the sink on a
// context that was already done -- a row the DB write would have lost.
func (r *recordingAuditLogger) assertWritableContext(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, err := range r.ctxErrs {
		if err != nil {
			t.Errorf("%s: handed to the audit sink on a context that is already done (%v); the DB write "+
				"fails with that error before a row exists, so the event reaches the log and never the trail",
				r.events[i].Action, err)
		}
	}
}

// assertStorableActor fails when ActorIdentity is anything createAuditEvent
// cannot keep in a relationship field to v1:identity:identity. Stricter than
// the engine on purpose: a colon-bearing value with no version segment
// ("worker:25884ce0") passes canonicalization unchanged and stores a foreign
// key that points at nothing, which is the junk the pre-canonical rows hold.
func assertStorableActor(t *testing.T, ev identity.AuditEvent) {
	t.Helper()
	if ev.ActorIdentity == "" {
		return
	}
	concept, _, err := id.ParseNodeId(ev.ActorIdentity)
	if err != nil || concept != "v1:identity:identity" {
		t.Errorf("%s: actorIdentity %q is not a v1:identity:identity id (parses as concept %q, err %v). "+
			"auditEvent.actorIdentityId is a relationship to v1:identity:identity and the insert refuses "+
			"any other concept -- put a free-form actor in the label, not in the identity field",
			ev.Action, ev.ActorIdentity, concept, err)
	}
}

// TestWorkerStreamAuditNamesTheCredentialItWasAdmittedOn: connect, a
// cockpit-forwarded event and disconnect are all the MACHINE acting, on the
// worker token the stream was admitted with. That token is a real
// v1:identity:identity row, so it is what actorIdentityId names; the
// registration is the target, and the human-readable label rides in detail.
func TestWorkerStreamAuditNamesTheCredentialItWasAdmittedOn(t *testing.T) {
	now := time.Date(2026, 9, 28, 9, 44, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	rec := &recordingAuditLogger{}
	srv := newServer(slog.New(slog.NewTextHandler(io.Discard, nil)), &fakeRegistrationStore{},
		NewRegistry(nil, clock), &IdentityAuditor{AuditLogger: rec}, clock, testNodeId)

	session, err := srv.admitRegistration(context.Background(), newFakeCockpitStream(),
		&WorkerIdentity{IdentityId: auditIdentity, OwnerUserId: auditOwner, Active: true},
		registerMsg([]string{CapabilityHeadless}, ""), "10.0.0.1:1234")
	if err != nil {
		t.Fatalf("admitRegistration: %v", err)
	}
	regId := session.worker.RegistrationId
	session.handleAuditEvent(context.Background(), &memqlv1.AuditEvent{Action: "tool_consent_granted", DetailJson: []byte(`{}`)})
	session.close(nil)

	// worker_disconnected is emitted from close(), whose first act is to
	// cancel the session's context. Its row must still be written.
	rec.assertWritableContext(t)
	events := rec.all()
	want := []string{"worker_registered", "tool_consent_granted", "worker_disconnected"}
	if len(events) != len(want) {
		t.Fatalf("audit events = %d, want %d (%v)", len(events), len(want), want)
	}
	for i, ev := range events {
		if ev.Action != want[i] {
			t.Errorf("event %d action = %q, want %q", i, ev.Action, want[i])
		}
		assertStorableActor(t, ev)
		if ev.ActorIdentity != auditIdentity {
			t.Errorf("%s: actorIdentity = %q, want the worker token the stream was admitted on (%q)",
				ev.Action, ev.ActorIdentity, auditIdentity)
		}
		if ev.ActorUserId != auditOwner {
			t.Errorf("%s: actorUserId = %q, want the machine's owner %q", ev.Action, ev.ActorUserId, auditOwner)
		}
		if ev.TargetType != "worker" || ev.TargetId != regId {
			t.Errorf("%s: target = %s/%q, want worker/%q", ev.Action, ev.TargetType, ev.TargetId, regId)
		}
		if got := ev.Detail["actor"]; got != "worker:"+regId {
			t.Errorf("%s: detail.actor = %v, want the label %q -- the readable actor moved out of the "+
				"identity field, it must not be dropped", ev.Action, got, "worker:"+regId)
		}
	}
}

// TestAppSessionAuditTargetsTheSession: an app session is started by the ENGINE
// on the owner's behalf. Its back-channel credential is a service-account JWT
// with no identity row (appSession.credentialRef says so), so there is no
// credential to name and actorIdentityId stays empty. The target is the SESSION
// -- targetType appSession with the registration id in targetId named the wrong
// row -- and the machine moves to detail.
func TestAppSessionAuditTargetsTheSession(t *testing.T) {
	runner, session, _, _ := newRunnerFixture(t, SubscriptionPresent)
	rec := &recordingAuditLogger{}
	runner.Auditor = &IdentityAuditor{AuditLogger: rec}
	session.worker.OwnerUserId = auditOwner
	session.worker.IdentityId = auditIdentity
	spec := runSpec()
	spec.OwnerUserId = auditOwner

	go func() {
		waitForSession(t, session, spec.SessionId)
		session.handleAppSessionEnd(&memqlv1.AppSessionEnd{SessionId: spec.SessionId})
	}()
	if _, err := runner.Run(context.Background(), session.worker, spec, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	rec.assertWritableContext(t)
	events := rec.all()
	want := []string{"app_session_started", "app_session_ended"}
	if len(events) != len(want) {
		t.Fatalf("audit events = %d, want %d (%v)", len(events), len(want), want)
	}
	for i, ev := range events {
		if ev.Action != want[i] {
			t.Errorf("event %d action = %q, want %q", i, ev.Action, want[i])
		}
		assertStorableActor(t, ev)
		if ev.ActorIdentity != "" {
			t.Errorf("%s: actorIdentity = %q, want empty -- the engine opened this session and presented "+
				"no v1:identity:identity credential to do it", ev.Action, ev.ActorIdentity)
		}
		if ev.ActorUserId != auditOwner {
			t.Errorf("%s: actorUserId = %q, want the owner %q", ev.Action, ev.ActorUserId, auditOwner)
		}
		if ev.TargetType != "appSession" || ev.TargetId != spec.SessionId {
			t.Errorf("%s: target = %s/%q, want appSession/%q", ev.Action, ev.TargetType, ev.TargetId, spec.SessionId)
		}
		if ev.Detail["workerId"] != session.worker.RegistrationId || ev.Detail["workerIdentityId"] != auditIdentity {
			t.Errorf("%s: detail must name the machine (workerId %v, workerIdentityId %v)",
				ev.Action, ev.Detail["workerId"], ev.Detail["workerIdentityId"])
		}
		if got := ev.Detail["actor"]; got != "engine" {
			t.Errorf("%s: detail.actor = %v, want \"engine\"", ev.Action, got)
		}
	}
}

// TestAppSessionEndedAuditSurvivesACancelledRun: cancellation is one of the
// ways a session ends, and the ended event is the only record in the trail that
// it did. finishRow already writes the terminal row on a detached context for
// exactly this reason; the audit must not go out on the dead one.
func TestAppSessionEndedAuditSurvivesACancelledRun(t *testing.T) {
	runner, session, _, _ := newRunnerFixture(t, SubscriptionPresent)
	rec := &recordingAuditLogger{}
	runner.Auditor = &IdentityAuditor{AuditLogger: rec}
	spec := runSpec()
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		waitForSession(t, session, spec.SessionId)
		cancel()
	}()
	result, err := runner.Run(ctx, session.worker, spec, nil)
	if err == nil || result.Status != AppSessionStatusCancelled {
		t.Fatalf("Run = %q, %v; want a cancelled run", result.Status, err)
	}

	events := rec.all()
	if len(events) != 2 || events[1].Action != "app_session_ended" {
		t.Fatalf("audit events = %d, want app_session_started then app_session_ended", len(events))
	}
	if got := events[1].Detail["status"]; got != AppSessionStatusCancelled {
		t.Errorf("app_session_ended detail.status = %v, want %q", got, AppSessionStatusCancelled)
	}
	rec.assertWritableContext(t)
}

// TestIdentityAuditorKeepsTheLabelOutOfTheIdentityField pins the bridge
// itself: the identity id goes to the identity field untouched, the label to
// detail.actor, and the caller's own detail map is never written into.
func TestIdentityAuditorKeepsTheLabelOutOfTheIdentityField(t *testing.T) {
	rec := &recordingAuditLogger{}
	aud := &IdentityAuditor{AuditLogger: rec}
	callerDetail := map[string]any{"name": "mbp"}

	aud.Emit(context.Background(), AuditEvent{
		Action:          "worker_registered",
		ActorIdentityId: auditIdentity,
		ActorLabel:      "worker:reg-1",
		Target:          "reg-1",
		TargetType:      "worker",
		OwnerUserId:     auditOwner,
		Detail:          callerDetail,
	})
	// A label with no detail at all still lands; an event with no label
	// passes its detail through as it came.
	aud.Emit(context.Background(), AuditEvent{Action: "app_session_started", ActorLabel: "engine", OwnerUserId: auditOwner})
	aud.Emit(context.Background(), AuditEvent{Action: "unlabelled", Detail: map[string]any{"k": "v"}})

	events := rec.all()
	if len(events) != 3 {
		t.Fatalf("events = %d, want 3", len(events))
	}
	if got := events[0]; got.ActorIdentity != auditIdentity || got.Detail["actor"] != "worker:reg-1" ||
		got.Detail["name"] != "mbp" || got.ActorUserId != auditOwner || got.TargetId != "reg-1" {
		t.Errorf("mapped event = %+v, want the identity id in ActorIdentity, the label in detail.actor, "+
			"and the caller's detail kept", got)
	}
	if _, wrote := callerDetail["actor"]; wrote {
		t.Error("the bridge wrote detail.actor into the CALLER's map; it must stamp a copy")
	}
	if got := events[1]; got.ActorIdentity != "" || got.Detail["actor"] != "engine" {
		t.Errorf("label-only event = %+v, want no identity and detail.actor=engine", got)
	}
	if got := events[2]; got.Detail["k"] != "v" || got.Detail["actor"] != nil {
		t.Errorf("unlabelled event detail = %v, want it passed through with no actor key", got.Detail)
	}
}
