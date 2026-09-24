package compose

import (
	"context"
	"database/sql"
	"encoding/json"
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
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/steps"
	pure "github.com/znasllc-io/memql/component/compose"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	work "github.com/znasllc-io/memql/integrations/work"
)

func materializeDBEngine(t *testing.T) *memql.MemQLEngine {
	t.Helper()
	dsn := dbtest.DSN()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "materializer lifecycle", dsn, err)
		return nil
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	e, err := memql.New(db)
	if err != nil {
		t.Fatal(err)
	}
	e.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := e.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestMaterializeDB_SharedCompositionKeepsSourcesPrivate(t *testing.T) {
	e := materializeDBEngine(t)
	i := New(e, e.Logger)
	i.SetGoalOpener(work.New(e, e.Logger))
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	owner, member := "compose-owner-"+suffix, "compose-member-"+suffix
	account, group := "compose-account-"+suffix, "compose-group-"+suffix
	seed := auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: owner, Role: auth.RoleOwner}))
	seed = auth.ContextWithToken(seed, &auth.TokenInfo{Subject: owner})
	for _, user := range []string{owner, member} {
		payload, _ := json.Marshal(map[string]any{"displayName": user, "primaryEmail": user + "@example.test", "role": "writer", "active": true})
		if _, err := e.Execute(seed, fmt.Sprintf(`insert("v1:identity:user", id=%q, payload=%s)`, user, payload)); err != nil {
			t.Fatal(err)
		}
	}
	for _, query := range []string{
		fmt.Sprintf(`insert("v1:accounts:account", id=%q, payload={"name": "Shared composition", "status": "active", "domainStatus": "unverified"})`, account),
		"mutation " + call("writeGroup", map[string]any{"groupId": group, "name": "Shared composition", "kind": "account", "accountId": account, "status": "active"}),
		"mutation " + call("writeGroupMembership", map[string]any{"membershipId": group + "-" + member, "groupId": group, "userId": member, "accountId": account, "origin": "added", "status": "active"}),
	} {
		if _, err := e.Execute(seed, query); err != nil {
			t.Fatal(err)
		}
	}
	ctx := auth.ContextWithUserActor(context.Background(), owner)
	privateSource := "source-" + suffix
	secret := "private-source-marker-" + suffix
	if err := i.store().createComposition(ctx, map[string]any{"compositionId": privateSource, "name": "Private source", "statement": secret, "format": "txt"}); err != nil {
		t.Fatal(err)
	}
	a := materializeDraft()
	a.AccountIds = []string{account}
	a.Sources = []SourceRef{{Kind: KindQuery, Ref: call("compositionById", map[string]any{"compositionId": privateSource})}}
	accepted, err := i.materialize(ctx, owner, "", a)
	if err != nil {
		t.Fatal(err)
	}
	compositionId := stringOf(accepted["compositionId"])
	memberCtx := auth.ContextWithUserActor(context.Background(), member)
	readRaw := func(ctx context.Context, concept string) []map[string]any {
		t.Helper()
		rows, err := i.store().query(ctx, fmt.Sprintf("concept==%q && row.id==%q", concept, compositionId))
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	countConcept := func(rows []map[string]any, concept string) int {
		count := 0
		for _, row := range rows {
			if row["concept"] == concept {
				count++
			}
		}
		return count
	}
	shared := readRaw(memberCtx, "v1:compose:composition")
	if countConcept(shared, "v1:compose:composition") != 1 {
		t.Fatalf("account member should see the shared composition: %+v", shared)
	}
	encoded, _ := json.Marshal(shared)
	if strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), `"request"`) {
		t.Fatalf("raw shared composition exposes captured private input: %s", encoded)
	}
	if hidden := readRaw(memberCtx, "v1:compose:compositionInput"); len(hidden) != 0 {
		t.Fatalf("account member can browse private input: %+v", hidden)
	}
	if hidden, err := i.store().compositionInputById(memberCtx, compositionId); err != nil || hidden != nil {
		t.Fatalf("account member input query: %+v %v", hidden, err)
	}
	owned := readRaw(ctx, "v1:compose:compositionInput")
	encoded, _ = json.Marshal(owned)
	if countConcept(owned, "v1:compose:compositionInput") != 1 || !strings.Contains(string(encoded), secret) {
		t.Fatalf("owner cannot recover the saved source snapshot: %s", encoded)
	}
}

// The accepting and executing engines share only Postgres. This exercises the
// actual template, argument binding, owner tier, work journal and terminal run
// transition, which the in-memory pipeline tests cannot prove.
func TestMaterializeDB_AcceptThenAdoptOnAnotherEngine(t *testing.T) {
	accepting := materializeDBEngine(t)
	executing := materializeDBEngine(t)
	accept := New(accepting, accepting.Logger)
	accept.SetGoalOpener(work.New(accepting, accepting.Logger))
	worker := New(executing, executing.Logger)
	if err := executing.RegisterIntegration(worker); err != nil {
		t.Fatal(err)
	}
	upload := &materializeUploader{}
	worker.SetUploader(upload, "files")
	loader := automations.NewLoader(automations.LoaderOptions{Logger: executing.Logger})
	auto, err := loader.LoadByName("materializeFile")
	if err != nil || auto == nil {
		t.Fatalf("template: %v", err)
	}
	exec := automations.NewExecutor(automations.ExecutorOptions{Engine: executing, Logger: executing.Logger, StepRegistry: steps.NewRegistry()})
	defer exec.Close()
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("providerFailure=%v", fail), func(t *testing.T) {
			owner := fmt.Sprintf("materializer-db-%d", time.Now().UnixNano())
			ctx := auth.ContextWithUserActor(context.Background(), owner)
			worker.SetComposer(materializeComposerFunc(func(ctx context.Context, req ComposeRequest) (ComposeReply, error) {
				rc, ok := common.RunFromContext(ctx)
				if !ok || rc.GoalId == "" || rc.RunId == "" || rc.StepKey == "" {
					t.Fatalf("provider lost run scope: %+v", rc)
				}
				if fail {
					return ComposeReply{}, fmt.Errorf("provider unavailable")
				}
				return ComposeReply{Draft: pure.Draft{Body: "A persisted result"}}, nil
			}))
			accepted, err := accept.materialize(ctx, owner, "", materializeDraft())
			if err != nil {
				t.Fatal(err)
			}
			compositionId, goalId, runId := stringOf(accepted["compositionId"]), stringOf(accepted["goalId"]), stringOf(accepted["runId"])
			row, err := worker.store().compositionById(ctx, compositionId)
			if err != nil || row == nil {
				t.Fatalf("composition missing before dispatch: %v", err)
			}
			if row["status"] != "draft" || row["goalId"] != goalId || row["runId"] != runId {
				t.Fatalf("accept did not persist matching identities before returning: %v", row)
			}
			journal, err := automations.LoadRunJournal(ctx, executing, runId)
			if err != nil {
				t.Fatal(err)
			}
			if journal.AutomationName != "materializeFile" || journal.Status != "running" {
				t.Fatalf("materializer sent request to compiler: %+v", journal)
			}
			// Dispatch rebuilds context from the persisted journal. Its RunId is
			// bare while goal and owner references retain canonical identities.
			ctx, err = auth.ContextWithPersistedOwner(context.Background(), journal.OwnerUserId)
			if err != nil {
				t.Fatal(err)
			}
			ctx = common.ContextWithRun(ctx, common.RunContext{RunId: journal.RunId, GoalId: journal.GoalId, OwnerUserId: journal.OwnerUserId, Mode: journal.Mode})
			result, runErr := exec.ExecuteAdopted(ctx, auto, automations.RunAdoption{RunId: runId, Variables: journal.Variables, Journal: journal})
			journal, err = automations.LoadRunJournal(ctx, executing, runId)
			if err != nil {
				t.Fatal(err)
			}
			row, err = worker.store().compositionById(ctx, compositionId)
			if err != nil {
				t.Fatal(err)
			}
			if fail {
				if row["status"] != "failed" || journal.Status != "failed" {
					t.Fatalf("provider failure did not close composition and run: composition=%v journal=%+v result=%+v err=%v", row, journal, result, runErr)
				}
				return
			}
			if runErr != nil || row["status"] != "ready" || journal.Status != "succeeded" {
				t.Fatalf("actual template failed: composition=%v journal=%+v result=%+v err=%v", row, journal, result, runErr)
			}
			file, err := worker.store().libraryFileById(ctx, stringOf(row["outputFileId"]))
			if err != nil || file == nil {
				t.Fatalf("file missing: %v", err)
			}
			if file["producedByRunId"] != runId {
				t.Fatalf("file missing Nexus provenance: %v", file)
			}

			index, err := loader.LoadByName("indexFileOnCreate")
			if err != nil || index == nil {
				t.Fatalf("file promotion template: %v", err)
			}
			ownerCtx := auth.ContextWithUserActor(context.Background(), owner)
			promoted, promoteErr := exec.ExecuteWithEvent(ownerCtx, index, "file-created", &events.Event{Topic: "graph.node.created.v1:library:file", Payload: file})
			if promoteErr != nil || promoted.Status != "completed" {
				t.Fatalf("file index promotion failed: %+v %v", promoted, promoteErr)
			}
			indexed, err := worker.store().query(ownerCtx, "query "+call("libraryArtifactBySourceConceptRef", map[string]any{"sourceConceptRef": file["id"]}))
			if err != nil || len(indexed) != 1 || indexed[0]["producedByRunId"] != runId {
				t.Fatalf("Nexus cannot find producing run on file index: %+v %v", indexed, err)
			}
			// Source snapshots are internal. The ordinary owner query never sends
			// gathered source payloads to a browser watching composition state.
			if _, leaked := row["request"]; leaked {
				t.Fatal("public shape exposes internal request snapshot")
			}
			outsider := auth.ContextWithUserActor(context.Background(), owner+"-stranger")
			hidden, err := worker.store().compositionById(outsider, compositionId)
			if err != nil {
				t.Fatal(err)
			}
			if hidden != nil {
				t.Fatal("stranger can read the composition")
			}
			if !strings.Contains(string(upload.bytes), "A persisted result") {
				t.Fatal("file does not contain composed result")
			}
		})
	}
}
