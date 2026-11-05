package email

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
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

// provisionHarness runs the real provision action against a fake Azure
// management API that records every create or update (any non-GET request).
type provisionHarness struct {
	t       *testing.T
	setup   *azureSetup
	session azureSession
	plan    azurePlan
	account string
	puts    map[string]int
	exists  map[string]bool
	callPut int
}

func newProvisionHarness(t *testing.T, existing func(p azurePlan) []string) *provisionHarness {
	t.Helper()
	t.Setenv("MEMQL_MASTER_KEY", strings.Repeat("ac", 32))
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(context.Background()); err != nil {
		dbtest.Unreachable(t, "email provision steps", dbtest.DSN(), err)
		return nil
	}
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	e, err := memql.New(db)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprintf("steps-%d", time.Now().UnixNano())
	account := suffix + "-client"
	owner := auth.ContextWithToken(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: suffix + "-owner", Role: auth.RoleOwner}), &auth.TokenInfo{Subject: suffix + "-owner"})
	if _, err = e.Execute(owner, renderConfigCall("createClientAccount", map[string]string{"accountId": account, "name": account})); err != nil {
		t.Fatal(err)
	}
	h := &provisionHarness{t: t, account: account, puts: map[string]int{}, exists: map[string]bool{}}
	h.plan = azurePlanFixture()
	h.plan.ClusterID = "cluster-" + suffix
	h.plan.CreateResourceGroup = true
	h.plan.ResourceGroupLocation = "eastus"
	groupPath := "/subscriptions/" + h.plan.SubscriptionID + "/resourceGroups/" + h.plan.ResourceGroup
	h.exists[groupPath] = false
	if existing != nil {
		for _, path := range existing(h.plan) {
			h.exists[path] = true
		}
	}
	protocol := newAzureProtocol("")
	protocol.client.Transport = acsRoundTrip(func(r *http.Request) (*http.Response, error) {
		path := r.URL.Path
		if strings.HasSuffix(path, "/providers/Microsoft.Communication") {
			return respond(200, map[string]any{"registrationState": "Registered"}), nil
		}
		if r.Method != http.MethodGet {
			h.puts[path]++
			h.callPut++
			h.exists[path] = true
		}
		if !h.exists[path] {
			return respond(404, map[string]any{}), nil
		}
		props := map[string]any{"provisioningState": "Succeeded", "dataLocation": "United States"}
		out := map[string]any{"id": path, "location": "eastus", "properties": props, "tags": map[string]string{"memql-organization": account, "memql-cluster": h.plan.ClusterID}}
		if strings.HasSuffix(path, "/domains/"+h.plan.Domain) {
			props["domainManagement"] = "CustomerManaged"
		}
		return respond(200, out), nil
	})
	h.setup = &azureSetup{store: &connectionStore{engine: installationTestStore{ConfigWriter: NewConfigWriter(e), prefix: suffix}, gateDB: replicaGatePool(t)}, protocol: protocol}
	h.session = azureSession{AccountID: account, ClusterID: h.plan.ClusterID, AccessToken: "token"}
	ctx := emailStateContext(context.Background())
	var c azureConnection
	if err = h.setup.store.change(ctx, "organization:"+account, &c, func(bool) error {
		c = azureConnection{Status: "planned", AccountID: account, Plan: h.plan}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return h
}

func respond(code int, body any) *http.Response {
	raw, _ := json.Marshal(body)
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(string(raw)))}
}

// call runs one provision call as the page does, from the saved plan.
func (h *provisionHarness) call() (map[string]any, int) {
	h.t.Helper()
	ctx := emailStateContext(context.Background())
	var c azureConnection
	if _, _, err := h.setup.store.read(ctx, "organization:"+h.account, &c); err != nil {
		h.t.Fatal(err)
	}
	h.callPut = 0
	out, err := h.setup.resourceAction(ctx, h.session, "provision", map[string]any{"confirmed": true, "planId": azurePlanID(c)})
	if err != nil {
		h.t.Fatal(err)
	}
	return out, h.callPut
}

func TestProvisionDoesOneCreatePerCall(t *testing.T) {
	h := newProvisionHarness(t, nil)
	// Resource group, email service, communication service, then the domain.
	for n := 1; n <= 4; n++ {
		out, creates := h.call()
		if creates != 1 {
			t.Fatalf("call %d issued %d creates, want exactly 1", n, creates)
		}
		want := "provisioning"
		if n == 4 {
			want = "dns"
		}
		if out["status"] != want {
			t.Fatalf("call %d answered %v, want %s", n, out["status"], want)
		}
		if out["planId"] == "" || out["plan"] == nil && n < 4 && out["planId"] == nil {
			t.Fatalf("call %d lost the plan id: %v", n, out)
		}
	}
	// A further call creates nothing and still answers the finished state.
	if out, creates := h.call(); creates != 0 || out["status"] != "dns" {
		t.Fatalf("settled call: %d creates, %v", creates, out["status"])
	}
	for path, n := range h.puts {
		if n != 1 {
			t.Fatalf("%s written %d times", path, n)
		}
	}
	var c azureConnection
	if _, _, err := h.setup.store.read(emailStateContext(context.Background()), "organization:"+h.account, &c); err != nil || c.Status != "dns" {
		t.Fatalf("saved plan status %q %v", c.Status, err)
	}
}

func TestProvisionIsIdempotentAcrossCalls(t *testing.T) {
	// The group and the email service already exist (a call that stopped
	// after its step, or a previous attempt). Nothing is created twice.
	h := newProvisionHarness(t, func(p azurePlan) []string {
		return []string{"/subscriptions/" + p.SubscriptionID + "/resourceGroups/" + p.ResourceGroup, p.emailPath()}
	})
	if out, creates := h.call(); creates != 1 || out["status"] != "provisioning" || h.puts[h.plan.communicationPath()] != 1 {
		t.Fatalf("first call: %d creates, %v, %v", creates, out["status"], h.puts)
	}
	// Repeating after that partial step creates only what is still missing.
	if out, creates := h.call(); creates != 1 || out["status"] != "dns" || h.puts[h.plan.communicationPath()] != 1 || h.puts[h.plan.emailPath()] != 0 {
		t.Fatalf("second call: %d creates, %v, %v", creates, out["status"], h.puts)
	}
	for n := 0; n < 3; n++ {
		if _, creates := h.call(); creates != 0 {
			t.Fatalf("repeat created %d", creates)
		}
	}
}
