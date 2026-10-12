package pipelinehop

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/campaigns"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/datasync"
	"github.com/znasllc-io/memql/component/emailrules"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/outbound"
)

type deliveryConsumerEngine struct{ *memql.MemQLEngine }

func (e deliveryConsumerEngine) Execute(ctx context.Context, query string) (any, error) {
	return e.MemQLEngine.Execute(ctx, query)
}

func TestDeliveryConsumersKeepTheirOperationalAuthority(t *testing.T) {
	ctx := t.Context()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	ping, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(ping); err != nil {
		dbtest.Unreachable(t, "delivery consumer authority", dbtest.DSN(), err)
	}
	_, err := memql.LoadUnifiedConcepts(nil)
	require.NoError(t, err)
	eng, err := memql.New(db)
	require.NoError(t, err)
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	require.NoError(t, eng.Init(memorynodes.DefaultRegistry()))
	adapter := deliveryConsumerEngine{eng}
	gateDB, err := database.NewGatePool(dbtest.DSN(), 2)
	require.NoError(t, err)
	t.Cleanup(func() { _ = gateDB.Close() })
	require.NoError(t, eng.RegisterIntegration(campaigns.NewWorker(adapter, nil, nil, eng.Logger, func() *sql.DB { return gateDB })))
	id := fmt.Sprintf("delivery-consumers-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, err := db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE strpos(id, ?) > 0 OR strpos(payload::text, ?) > 0`, id, id)
		require.NoError(t, err)
	})
	run := func(actor context.Context, kind, name string, args map[string]any) *memql.ExecuteResult {
		t.Helper()
		call, err := langparser.RenderCall(name, args)
		require.NoError(t, err)
		res, err := eng.Execute(actor, kind+" "+call)
		require.NoError(t, err)
		return res
	}

	// The receiver's server-only stage still works. Datasync reads the exact
	// signed body as its existing operator identity, with no new bypass.
	receiver := auth.ContextWithInternalOrigin(auth.ContextWithSystemActor(ctx, "inbound"))
	_, err = eng.Execute(receiver, fmt.Sprintf(`mutation stageInboundRequest(requestId: %s, source: "delivery-test", medium: "webhook", body: "private body", signatureVerified: true, receivedAt: "2026-10-05T12:00:00Z")`, langparser.QuoteString(id)))
	require.NoError(t, err)
	staged, found, err := datasync.NewStore(adapter).StagedInboundRequest(datasync.OperatorContext(ctx), id)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "private body", string(staged.Body))

	// A validated email rule acts for its author, who need not be a cluster
	// operator. Its bounded staging write retains that provenance without
	// granting the author permission to read the outbox.
	author := auth.ContextWithUserActor(ctx, id+"-author")
	seeder := auth.ContextWithToken(auth.ContextWithInternalOrigin(auth.ContextWithAccess(ctx,
		&auth.AccessContext{UserId: id + "-seeder", Role: auth.RoleOwner, Unranked: true, Synthetic: true})), &auth.TokenInfo{Subject: id + "-seeder"})
	for _, user := range []struct{ suffix, role, name string }{{"author", "writer", "Author"}, {"recipient", "reader", "Actual recipient"}} {
		_, err := eng.Execute(seeder, fmt.Sprintf(`insert("v1:identity:user", id=%s, payload={"primaryEmail":%s,"displayName":%s,"role":%s,"active":true})`,
			langparser.QuoteString(id+"-"+user.suffix), langparser.QuoteString(id+"-"+user.suffix+"@example.test"), langparser.QuoteString(user.name), langparser.QuoteString(user.role)))
		require.NoError(t, err)
	}
	account, group, template, rule := id+"-account", id+"-group", id+"-template", id+"-rule"
	run(seeder, "mutation", "createClientAccount", map[string]any{"accountId": account, "name": "Delivery test"})
	run(seeder, "mutation", "writeGroup", map[string]any{"groupId": group, "name": "Delivery test", "kind": "account", "accountId": account, "status": "active"})
	run(seeder, "mutation", "writeGroupMembership", map[string]any{"membershipId": id + "-membership", "groupId": group, "userId": id + "-author", "origin": "added", "status": "active", "accountId": account})
	content := map[string]any{"subject": "Notice for {{displayName}}", "textBody": "private body for {{displayName}}", "htmlBody": "<p>private body for {{displayName}}</p>"}
	templateArgs := map[string]any{"templateId": template, "accountId": account, "name": "Notice", "content": content, "action": "save"}
	saved := memql.MaterializeRows(run(author, "builtin", "campaignSaveTemplate", templateArgs))
	require.Len(t, saved, 1)
	templateArgs["expectedRevision"], templateArgs["action"] = saved[0]["revision"], "publish"
	run(author, "builtin", "campaignSaveTemplate", templateArgs)
	run(author, "mutation", "createEmailRule", map[string]any{"emailRuleId": rule, "name": "Delivery test", "triggerConcept": "v1:todos:todo", "eventKind": "created", "templateId": template, "recipientMode": "cluster_roles", "recipientRoles": []string{"reader"}, "accountId": account})
	run(author, "mutation", "setEmailRuleStatus", map[string]any{"emailRuleId": rule, "status": "active"})
	require.NoError(t, emailrules.NewStore(adapter).StageOperationalEmail(author, id, template, id+"-recipient@example.test", "Untrusted display name", id, rule))
	res, err := eng.Execute(author, `query outboundRequestsByStatus(status: "pending")`)
	require.NoError(t, err)
	require.Empty(t, res.Bundle.Nodes)

	// Use the actual drain actor helper and query, not an invented owner.
	worker := outbound.SystemActorContext(ctx)
	res, err = eng.Execute(worker, `query outboundRequestsByStatus(status: "pending")`)
	require.NoError(t, err)
	require.NotEmpty(t, res.Bundle.Nodes)
	var stagedEmail map[string]any
	for _, row := range memql.MaterializeRows(res) {
		if memql.BareShortId(fmt.Sprint(row["id"])) == id {
			stagedEmail = row
			break
		}
	}
	require.NotNil(t, stagedEmail, "the drain must see this rule's staged message")
	require.Equal(t, id+"-author", memql.BareShortId(fmt.Sprint(stagedEmail["requestedBy"])))
	require.Equal(t, id+"-recipient@example.test", stagedEmail["target"])
	require.Equal(t, "Notice for Actual recipient", stagedEmail["subject"])
	require.Equal(t, "private body for Actual recipient", stagedEmail["body"])
	require.Equal(t, "<p>private body for Actual recipient</p>", stagedEmail["htmlBody"])
	_, err = eng.Execute(auth.ContextWithInternalOrigin(worker), fmt.Sprintf(`mutation updateOutboundRequestStatus(requestId: %s, status: "sent")`, langparser.QuoteString(id)))
	require.NoError(t, err)
}
