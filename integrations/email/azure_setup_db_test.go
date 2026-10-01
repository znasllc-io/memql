package email

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

func TestAzureSetupSessionSurvivesReplicaHopAndRefusesAnotherActor(t *testing.T) {
	t.Setenv("MEMQL_MASTER_KEY", strings.Repeat("ac", 32))
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(context.Background()); err != nil {
		dbtest.Unreachable(t, "Azure setup", dbtest.DSN(), err)
		return
	}
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	var tokenCalls atomic.Int32
	protocol := newAzureProtocol("12345678-1234-1234-1234-123456789012")
	protocol.client.Transport = acsRoundTrip(func(r *http.Request) (*http.Response, error) {
		body, code := `{"device_code":"private-device-code","user_code":"VISIBLE","verification_uri":"https://microsoft.com/devicelogin","expires_in":900,"interval":5}`, 200
		if strings.HasSuffix(r.URL.Path, "/token") {
			if tokenCalls.Add(1) == 1 {
				body, code = `{"error":"authorization_pending"}`, 400
			} else {
				body = `{"token_type":"Bearer","access_token":"private-access-token","refresh_token":"private-refresh-token","expires_in":3600}`
			}
		} else if r.URL.Host == "management.azure.com" {
			if r.Header.Get("Authorization") != "Bearer private-access-token" {
				t.Error("lost session across replica hop")
			}
			body = `{"value":[{"subscriptionId":"12345678-1234-1234-1234-123456789012","displayName":"Selected subscription","state":"Enabled"}]}`
		}
		return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	instances := make([]*Integration, 2)
	var first *memql.MemQLEngine
	for n := range instances {
		e, err := memql.New(db)
		if err != nil {
			t.Fatal(err)
		}
		if err = e.Init(memorynodes.DefaultRegistry()); err != nil {
			t.Fatal(err)
		}
		if first == nil {
			first = e
		}
		setup := &azureSetup{store: &connectionStore{engine: NewConfigWriter(e), database: func() *sql.DB { return db.DB }}, protocol: protocol, now: func() time.Time { return now }, canManage: func(ctx context.Context, account string) bool {
			return e.OrganizationCapable(ctx, account, auth.VerbUpdate, auth.ResourceData)
		}}
		instances[n] = NewIntegration(NewLogSender(nil), nil)
		instances[n].azure = setup
	}
	suffix := fmt.Sprintf("azure-%d", time.Now().UnixNano())
	account, other := suffix+"-client", suffix+"-other"
	actor := func(user string) context.Context {
		return auth.ContextWithToken(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: user, Role: auth.RoleOwner}), &auth.TokenInfo{Subject: user})
	}
	owner := actor(suffix + "-owner")
	for _, id := range []string{account, other} {
		if _, err := first.Execute(owner, renderConfigCall("createClientAccount", map[string]string{"accountId": id, "name": id})); err != nil {
			t.Fatal(err)
		}
	}
	call := func(instance int, ctx context.Context, action, session, org string) (map[string]any, error) {
		rows, err := instances[instance].handleAzureSetup(ctx, map[string]any{"action": action, "sessionId": session, "accountId": org}, 0)
		if err != nil {
			return nil, err
		}
		if len(rows) != 1 {
			return nil, fmt.Errorf("no receipt")
		}
		var result map[string]any
		err = json.Unmarshal(rows[0].Payload, &result)
		encoded, _ := json.Marshal(result)
		if strings.Contains(string(encoded), "private-") {
			t.Error("secret crossed public result boundary")
		}
		return result, err
	}
	started, err := call(0, owner, "begin", "", account)
	if err != nil {
		t.Fatal(err)
	}
	session := started["sessionId"].(string)
	if _, err = call(1, actor(suffix+"-different-owner"), "poll", session, account); err == nil {
		t.Fatal("another owner reused session")
	}
	if _, err = call(1, owner, "poll", session, other); err == nil {
		t.Fatal("another organization reused session")
	}
	now = now.Add(6 * time.Second)
	var wg sync.WaitGroup
	for n := range instances {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if _, err := call(n, owner, "poll", session, account); err != nil {
				t.Error(err)
			}
		}(n)
	}
	wg.Wait()
	if tokenCalls.Load() != 1 {
		t.Fatal("replicas ignored shared polling interval")
	}
	now = now.Add(6 * time.Second)
	connected, err := call(1, owner, "poll", session, account)
	if err != nil || connected["status"] != "connected" {
		t.Fatal("replica could not finish sign-in", connected, err)
	}
	choices, err := call(0, owner, "subscriptions", session, account)
	if err != nil || len(choices["choices"].([]any)) != 1 {
		t.Fatal(choices, err)
	}
	now = now.Add(2 * time.Hour)
	expired, err := call(1, owner, "subscriptions", session, account)
	if err != nil || expired["status"] != "expired" {
		t.Fatal("expired session remained usable", expired, err)
	}
	var stored azureSession
	if _, _, err = instances[0].azure.store.read(context.Background(), "azure-session:"+session, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.AccessToken != "" || stored.RefreshToken != "" || stored.DeviceCode != "" {
		t.Fatal("expired session retained active credentials")
	}

	// A saved connection, sender attribution, and disconnect cross the same
	// hop. No sender-local cache can preserve a revoked client credential.
	cfg := acsFixture(t).cfg
	cfg.AccountID = account
	connection := azureConnection{Status: "ready", AccountID: account, Config: cfg}
	if err = instances[0].azure.store.change(owner, "organization:"+account, &connection, func(bool) error { return nil }); err != nil {
		t.Fatal(err)
	}
	identityID, err := instances[0].azure.saveSenderIdentity(owner, connection)
	if err != nil {
		t.Fatal(err)
	}
	if retry, err := instances[1].azure.saveSenderIdentity(owner, connection); err != nil || retry != identityID {
		t.Fatal("sender retry lost identity", err)
	}
	resolved, err := instances[1].azure.sender(owner, account)
	if err != nil || resolved == nil {
		t.Fatal("second replica cannot resolve client sender", err)
	}
	if err = CheckSender(owner, resolved, SendAs{AccountID: account, Address: cfg.Default, FromName: cfg.Senders[cfg.Default]}); err != nil {
		t.Fatal(err)
	}
	if _, err = instances[0].azure.connectionAction(owner, account, "disconnect", nil); err != nil {
		t.Fatal(err)
	}
	if _, err = instances[1].azure.sender(owner, account); !IsPermanent(err) {
		t.Fatal("second replica retained disconnected sender", err)
	}
	plan := azurePlanFixture()
	parameters := map[string]any{"subscriptionId": plan.SubscriptionID, "resourceGroup": plan.ResourceGroup, "domain": plan.Domain, "emailService": plan.EmailService, "communicationService": plan.CommunicationService, "dataLocation": plan.DataLocation}
	prepared, err := instances[0].azure.connectionAction(owner, account, "prepare", parameters)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = instances[1].azure.connectionAction(owner, account, "disconnect", nil); err != nil {
		t.Fatal(err)
	}
	parameters["domain"] = "changed.example"
	if _, err = instances[1].azure.connectionAction(owner, account, "prepare", parameters); err != nil {
		t.Fatal(err)
	}
	if _, err = instances[0].azure.resourceAction(owner, azureSession{AccountID: account}, "provision", map[string]any{"confirmed": true, "planId": prepared["planId"]}); err == nil || !strings.Contains(err.Error(), "plan changed") {
		t.Fatal("provisioned a different plan than the one reviewed", err)
	}
	// Both replicas observe a previously unused Azure path. Exactly one
	// organization may reserve it even before Azure exposes the resource.
	plan.ResourceGroup = suffix
	var claims atomic.Int32
	for n, organization := range []string{account, other} {
		wg.Add(1)
		go func(n int, organization string) {
			defer wg.Done()
			if err := instances[n].azure.claimResources(owner, organization, plan); err == nil {
				claims.Add(1)
			}
		}(n, organization)
	}
	wg.Wait()
	if claims.Load() != 1 {
		t.Fatal("replicas allowed conflicting Azure resource claims", claims.Load())
	}

}
