package automations_test

// site_access_trigger_db_test.go -- no automation triggers on an access code or a session
// (design D14; re-review of Task 3).
//
// An authored automation can name any concept in its @trigger, an email rule generates one,
// and the engine's write path hands the trigger the stored row whole (flattened into the
// event and retained again under "payload"), because the event bus has no authorization hook
// of any kind. The automation's run journal then writes that event into a run row that its
// author, and a cluster owner, can read. The re-review did it: an authored automation on
// node.created of the code concept fired on a mint, and the journal row held the digest and
// the label.
//
// The engine now publishes no graph event for a write to either concept
// (withholdGraphWriteEvent, component/memql), so a trigger on them has nothing to fire on.
// This drives that against a real engine, a real bus and the real authored scheduler: five
// automations are activated -- created and updated on each of the two concepts, and a control
// on an ordinary concept -- and every kind of write the two concepts take is made. Only the
// control may fire. Its firing is also what bounds the wait: it is written last, so by the
// time it has fired, anything the earlier writes were going to trigger has had its turn.

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
)

func (r *runRecorder) firedBy(automation string) []recordedRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []recordedRun
	for _, run := range r.fired {
		if run.automation == automation {
			out = append(out, run)
		}
	}
	return out
}

func TestNoAutomationTriggersOnAnAccessCodeOrSession(t *testing.T) {
	loadConceptsForAuthored(t)
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	if err := db.PingContext(context.Background()); err != nil {
		dbtest.Unreachable(t, "access code trigger test", dbtest.DSN(), err)
	}
	t.Cleanup(func() { _ = db.Close() })
	eng, err := memql.New(db)
	if err != nil {
		t.Fatalf("memql.New: %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(concept.DefaultRegistry()); err != nil {
		t.Fatalf("engine Init: %v", err)
	}
	bus := events.NewBus()
	eng.SetEventBus(bus)
	rec := &runRecorder{}
	s := newAuthoredSchedulerForTest(t, bus, rec.run)
	defer s.Stop()

	nonce := time.Now().UnixNano()
	author := fmt.Sprintf("v1:identity:user:zz-author-%d", nonce)
	watch := func(name, event, conceptName string) string {
		name = fmt.Sprintf("%s%d", name, nonce)
		src := fmt.Sprintf(`
@trigger(event=%q, concept=%q)
automation %s {
  run := logic sandboxNoopLogic(event: event)
}`, event, conceptName, name)
		if err := s.Activate(authoredAutomationConstruct(author, name, src)); err != nil {
			t.Fatalf("Activate %s: %v", name, err)
		}
		return name
	}
	codeCreated := watch("zzCodeCreated", "node.created", "v1:platform:siteAccessCode")
	codeUpdated := watch("zzCodeUpdated", "node.updated", "v1:platform:siteAccessCode")
	sessionCreated := watch("zzSessionCreated", "node.created", "v1:platform:siteAccessSession")
	sessionUpdated := watch("zzSessionUpdated", "node.updated", "v1:platform:siteAccessSession")
	control := watch("zzGroupCreated", "node.created", "v1:identity:group")

	owner := fmt.Sprintf("zz-owner-%d", nonce)
	ownerCtx := auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: owner, Role: auth.RoleWriter}))
	ownerCtx = auth.ContextWithToken(ownerCtx, &auth.TokenInfo{Subject: owner})
	issued := time.Now().UTC().Truncate(time.Second)
	stamp := func(d time.Duration) string { return issued.Add(d).Format(time.RFC3339) }
	digest := strings.Repeat("ab", 32)
	code, session, site := fmt.Sprintf("zzcode%d", nonce), fmt.Sprintf("zzsession%d", nonce), fmt.Sprintf("zzsite%d", nonce)
	call := func(name string, args map[string]any) {
		t.Helper()
		parts := make([]string, 0, len(args))
		for k, v := range args {
			parts = append(parts, fmt.Sprintf("%s: %s", k, mutationLiteral(v)))
		}
		if _, err := eng.Execute(ownerCtx, "mutation "+name+"("+strings.Join(parts, ", ")+")"); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	call("createSiteAccessCode", map[string]any{"codeId": code, "siteId": site, "label": "a label", "gate": "dashboard",
		"codeHash": digest, "codeHint": "FBZA", "issuedAt": stamp(0), "expiresAt": stamp(time.Hour), "lastChangedBy": owner})
	call("rotateSiteAccessCode", map[string]any{"codeId": code, "codeHash": strings.Repeat("cd", 32), "codeHint": "ZZZZ", "generation": 2,
		"issuedAt": stamp(time.Minute), "expiresAt": stamp(2 * time.Hour), "lastChangedBy": owner})
	call("createSiteAccessSession", map[string]any{"sessionId": session, "siteId": site, "codeId": code, "generation": 2,
		"tokenHash": strings.Repeat("ef", 32), "issuedAt": stamp(2 * time.Minute)})
	call("endSiteAccessSession", map[string]any{"sessionId": session, "endedAt": stamp(3 * time.Minute)})
	call("removeSiteAccessCode", map[string]any{"codeId": code, "revokedAt": stamp(4 * time.Minute), "lastChangedBy": owner})

	// The control, last: an ordinary concept's write fires its automation.
	seeder := auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "account-db-seeder", Role: auth.RoleOwner, Unranked: true, Synthetic: true,
	}))
	seeder = auth.ContextWithToken(seeder, &auth.TokenInfo{Subject: "account-db-seeder"})
	if _, err := eng.Execute(seeder, fmt.Sprintf(`mutation writeGroup(groupId: "zzgroup%d", name: "Triggers", kind: "account", accountId: "zzacct%d", status: "active")`, nonce, nonce)); err != nil {
		t.Fatalf("control write: %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && len(rec.firedBy(control)) == 0 {
		time.Sleep(25 * time.Millisecond)
	}
	if len(rec.firedBy(control)) == 0 {
		t.Fatal("the control automation never fired, so this test cannot tell a withheld event from a scheduler that fires nothing")
	}
	time.Sleep(500 * time.Millisecond)

	for _, name := range []string{codeCreated, codeUpdated, sessionCreated, sessionUpdated} {
		if runs := rec.firedBy(name); len(runs) != 0 {
			t.Errorf("%s fired %d time(s), first on %s: an automation triggered on a write to an access code or session, and its run journal would hold the row", name, len(runs), runs[0].eventTopic)
		}
	}
}

// mutationLiteral renders a value as the MemQL literal a mutation call takes.
func mutationLiteral(v any) string {
	switch x := v.(type) {
	case string:
		return fmt.Sprintf("%q", x)
	default:
		return fmt.Sprint(x)
	}
}
