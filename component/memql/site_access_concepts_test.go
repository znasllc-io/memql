package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// THE SHAPE OF THE TWO CONCEPTS, as the loaded tree declares it (deployable access
// codes design record, 2026-10-29, section 4). Declarations only: the behaviour that
// follows from each is proved against a real database in
// site_access_contract_db_test.go, and these are the pins that fail first if one is
// edited away.

// A SESSION EXPIRES AND A CODE DOES NOT.
//
// A session digest has no value once the session has ended, and no session outlives
// 24 hours, so thirty days cannot touch a live one. The policy therefore has to make
// the ROW expire -- keepLatest=false, because the default keeps the newest version of
// every record forever -- and has to need no archive, because a digest is not
// evidence worth a verified copy in object storage (archive="none"). The code's row
// is the opposite: it is the lasting record of who could get in and when, and it
// carries no policy at all.
func TestSiteAccessSessionsExpireAndCodesDoNot(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}

	session, err := memorynodes.Get("v1:platform:siteAccessSession")
	if err != nil || session == nil {
		t.Fatalf("v1:platform:siteAccessSession is not in the loaded tree: %v", err)
	}
	if session.Retention == nil {
		t.Fatal("siteAccessSession declares no @retention, so its rows are kept forever")
	}
	if got, want := *session.Retention, (memorynodes.RetentionPolicy{TTL: 30 * 24 * time.Hour, KeepLatest: false, Archive: "none"}); got != want {
		t.Errorf("siteAccessSession retention = %+v, want %+v: keepLatest=true would keep the newest version of every session forever, and archive=required would need an object store before anything is deleted", got, want)
	}

	code, err := memorynodes.Get("v1:platform:siteAccessCode")
	if err != nil || code == nil {
		t.Fatalf("v1:platform:siteAccessCode is not in the loaded tree: %v", err)
	}
	if code.Retention != nil {
		t.Errorf("siteAccessCode carries a retention policy %+v: its row is the lasting record of who could get in, and the retention sweep would delete it", *code.Retention)
	}
}

// THE SWEEP IS ALLOWED TO RETIRE A SESSION ROW. The retention sweep deletes a row only after
// it passes the same per-row gate a read does (PluginContext.AdmitSourceRow), and refuses the
// whole batch when one row fails it -- so a tier that did not admit the sweeper would leave
// every session in the graph for good and log an error every hour. The sweep runs under a
// cluster owner, whom the tier's clusterOwner arm admits for rows owned by somebody else;
// a stranger gets no such pass, so the admission is the tier's doing and not a gate that
// admits everything.
func TestTheRetentionSweepAdmitsSessionRows(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	payload, err := json.Marshal(map[string]any{
		"siteId": "v1:platform:site:s", "codeId": "v1:platform:siteAccessCode:c", "generation": 1,
		"ownerUserId": "v1:identity:user:the-site-owner", "tokenHash": siteAccessDigest(0x55), "issuedAt": "2026-09-01T00:00:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	row := memorynodes.MemoryNode{ID: "v1:platform:siteAccessSession:s", Concept: "v1:platform:siteAccessSession", Payload: payload}

	sweep := auth.ContextWithAccess(context.Background(), auth.MaintenanceActor("retireDeclaredConceptVersions"))
	if !AdmitSourceRow(sweep, row) {
		t.Error("the retention sweep's principal is refused a session row, so the sweep would preserve every session and fail every hour")
	}
	stranger := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "a-stranger", Role: auth.RoleWriter})
	if AdmitSourceRow(stranger, row) {
		t.Error("a stranger is admitted to another owner's session row: the admission above proves nothing")
	}
}

// NOTHING WRITES THESE ROWS BUT SERVER CODE. @serverOnly on the mutations bars only the
// named call; @serverWritten is what bars the raw insert and the second insert at an
// existing id (the executeWrite chokepoint), and the live proof that it does is
// TestSiteAccessRowsCannotBeWrittenAroundTheBuiltins. This is the declaration that proof
// stands on.
func TestSiteAccessConceptsAreServerWritten(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	for _, name := range []string{"v1:platform:siteAccessCode", "v1:platform:siteAccessSession"} {
		c, err := memorynodes.Get(name)
		if err != nil || c == nil {
			t.Fatalf("%s is not in the loaded tree: %v", name, err)
		}
		if !c.ServerWritten {
			t.Errorf("%s is not @serverWritten: its owner could write a row with a digest and an expiry they chose, around every builtin", name)
		}
	}
}

// THE AUDIT LOG CAN NAME A CODE. The spec has each operator act write a
// v1:identity:auditEvent with targetType siteAccessCode, and targetType is a CLOSED
// enum on the mutation: a value outside it is refused before any row is written, and
// the failure is one WARN per act with nothing in the trail. The writers live in
// integrations/siteaccess, which the identity enum gate does not walk, so this pins the
// value where the writer will be refused if it goes missing.
// TestCreateAuditEvent_EveryDeclaredTargetTypeIsWritable exercises the write itself.
func TestSiteAccessCodeIsAnAuditTargetType(t *testing.T) {
	fns := loadedSiteAccessTree(t)
	fn, err := fns.Get("createAuditEvent")
	if err != nil || fn == nil || fn.ArgsSchema == nil {
		t.Fatalf("createAuditEvent is not in the loaded tree: %v", err)
	}
	for _, field := range fn.ArgsSchema.Fields {
		if field == nil || field.Name != "targetType" {
			continue
		}
		for _, v := range field.Enum {
			if fmt.Sprint(v) == "siteAccessCode" {
				return
			}
		}
		t.Fatalf("createAuditEvent.targetType does not accept siteAccessCode: %v", field.Enum)
	}
	t.Fatal("createAuditEvent declares no targetType")
}
