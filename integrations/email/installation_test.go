package email

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
)

func installationFixture() *emailInstallation {
	return &emailInstallation{TenantID: "11111111-1111-1111-1111-111111111111", ClientID: "22222222-2222-2222-2222-222222222222", SubscriptionID: "12345678-1234-1234-1234-123456789012", ResourceGroup: "email", DataLocation: "United States", EmailService: "operator-email", CommunicationService: "operator-delivery"}
}
func TestEmailInstallationConfigurationRejectsSecretsAndPartialChoices(t *testing.T) {
	c := installationFixture()
	raw, _ := json.Marshal(c)
	if got, err := readEmailInstallation(string(raw)); err != nil || *got != *c {
		t.Fatalf("configuration %v %v", got, err)
	}
	for _, raw := range []string{`{}`, string(raw) + ` {}`, strings.Replace(string(raw), `"tenantId":`, `"clientSecret":"private", "tenantId":`, 1), strings.Replace(string(raw), `"email"`, `" "`, 1), strings.Repeat("x", 8193)} {
		if _, err := readEmailInstallation(raw); err == nil {
			t.Fatalf("accepted invalid installation configuration %s", raw)
		}
	}
	if got, err := readEmailInstallation(""); got != nil || err != nil {
		t.Fatal("absent installation changed existing setup")
	}
}
func TestEmailInstallationWorkloadAssertionRotatesAcrossReplicas(t *testing.T) {
	c := installationFixture()
	cluster := azureClusterConnection{ID: "installation", Status: "connected", TenantID: c.TenantID, WorkloadClientID: c.ClientID, Plan: c.clusterPlan()}
	assertions := []string{}
	protocol := newAzureProtocol("")
	protocol.client.Transport = acsRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "login.microsoftonline.com" || !strings.Contains(r.URL.Path, c.TenantID) {
			t.Fatal("credential sent outside selected tenant")
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("client_id") != c.ClientID || r.Form.Get("grant_type") != "client_credentials" || r.Form.Get("scope") != "https://management.azure.com/.default" || r.Form.Get("refresh_token") != "" || r.Form.Get("client_secret") != "" {
			t.Fatalf("unexpected token exchange %v", r.Form)
		}
		assertions = append(assertions, r.Form.Get("client_assertion"))
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"token_type":"Bearer","access_token":"short-lived-token","expires_in":3600}`))}, nil
	})
	for n, assertion := range []string{"replica.one.signature", "replica.two.signature", "rotated.one.signature"} {
		// Independent setup objects have no originating replica's local session.
		a := &azureSetup{installation: c, protocol: protocol, readAssertion: func(path string) ([]byte, error) {
			if path != azureAssertionPath {
				t.Fatal("caller-controlled token path")
			}
			return []byte(assertion), nil
		}}
		session, err := a.workloadSession(context.Background(), cluster)
		if err != nil || session.AccessToken != "short-lived-token" || session.RefreshToken != "" {
			t.Fatalf("exchange %d: %+v %v", n, session, err)
		}
	}
	if len(assertions) != 3 || assertions[0] == assertions[2] {
		t.Fatal("projected assertion cached across rotation")
	}
	wrong := cluster
	wrong.WorkloadClientID = "33333333-3333-3333-3333-333333333333"
	a := &azureSetup{installation: c, protocol: protocol, readAssertion: func(string) ([]byte, error) { t.Fatal("mismatched identity read assertion"); return nil, nil }}
	if _, err := a.workloadSession(context.Background(), wrong); err == nil {
		t.Fatal("accepted another cluster's identity")
	}
}
func TestEmailInstallationWorkflowSelectsOnlyReadySenderAndRefusesDirectCalls(t *testing.T) {
	for _, tc := range []struct {
		name, status          string
		stopCluster, stopPlan bool
		want                  string
	}{
		{"pending", "provisioning", false, false, "connect account prepare provision"},
		{"verified", "verified", false, false, "connect account prepare provision sender"},
		{"disconnected", "", true, false, "connect"},
		{"existing transport", "", false, true, "connect account prepare"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := []string{}
			operation := func(name string, result any) workflowhost.Operation {
				return func(context.Context, map[string]any) (any, error) { called = append(called, name); return result, nil }
			}
			ops := map[string]workflowhost.Operation{"emailInstallationAccount": operation("account", nil), "emailInstallationConnect": operation("connect", map[string]any{"stopped": tc.stopCluster}), "emailInstallationPrepare": operation("prepare", map[string]any{"stopped": tc.stopPlan}), "emailInstallationProvision": operation("provision", map[string]any{"status": tc.status}), "emailInstallationSender": operation("sender", nil)}
			if _, err := workflowhost.Run(context.Background(), "emailInstallationWorkflow", nil, workflowhost.Options{Operations: ops}); err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(called, " "); got != tc.want {
				t.Fatalf("workflow %s want %s", got, tc.want)
			}
		})
	}
	for _, c := range workflowhost.ScopedCapabilities((&installationScope{}).operations()) {
		if _, err := c.Handler(context.Background(), map[string]any{}, 0); err == nil {
			t.Fatal("wire caller obtained installation authority")
		}
	}
}

// Resource fixture models Azure's asynchronous named creates. It also fails
// immediately if a workload token attempts subscription-wide registration.
func installationProtocol(t *testing.T, created map[string]bool, clusterID func() string) *azureProtocol {
	t.Helper()
	p := newAzureProtocol("")
	p.client.Transport = acsRoundTrip(func(r *http.Request) (*http.Response, error) {
		var out any
		code := 200
		if r.URL.Host == "login.microsoftonline.com" {
			out = map[string]any{"token_type": "Bearer", "access_token": "ephemeral", "expires_in": 3600}
		} else {
			path := r.URL.Path
			if strings.HasSuffix(path, "/providers/Microsoft.Communication") || strings.HasSuffix(path, "/register") {
				t.Fatal("group-scoped identity attempted subscription registration")
			}
			props := map[string]any{"provisioningState": "Succeeded", "dataLocation": "United States"}
			resource := map[string]any{"id": path, "properties": props, "tags": map[string]string{"memql-cluster": clusterID(), "memql-organization": "self"}}
			out = resource
			switch {
			case strings.HasSuffix(path, "/listKeys"):
				out = map[string]any{"primaryKey": base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))}
			case strings.HasSuffix(path, "/senderUsernames"):
				out = map[string]any{"value": []any{map[string]any{"properties": map[string]any{"username": "DoNotReply", "displayName": "DoNotReply"}}}}
			case strings.Contains(path, "/senderUsernames/"):
				if r.Method != http.MethodGet {
					t.Fatal("managed-domain username was modified")
				}
				props["username"] = "DoNotReply"
				props["displayName"] = "DoNotReply"
			case strings.HasSuffix(path, "/domains/AzureManagedDomain"):
				if r.Method == http.MethodPut {
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body["properties"].(map[string]any)["domainManagement"] != "AzureManaged" {
						t.Fatal("bootstrap requires custom DNS")
					}
					created[path] = true
				}
				if !created[path] {
					code = 404
				} else {
					props["domainManagement"] = "AzureManaged"
					props["fromSenderDomain"] = "11111111-1111-1111-1111-111111111111.azurecomm.net"
				}
			case strings.Contains(path, "/emailServices/") || strings.Contains(path, "/communicationServices/"):
				if r.Method == http.MethodPut {
					created[path] = true
				}
				if !created[path] {
					code = 404
				} else {
					if strings.Contains(path, "/communicationServices/") {
						props["hostName"] = "installation.communication.azure.com"
						props["linkedDomains"] = []string{strings.Replace(path, "communicationServices/operator-delivery", "emailServices/operator-email/domains/AzureManagedDomain", 1)}
					}
				}
			}
		}
		raw, _ := json.Marshal(out)
		return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
	})
	return p
}

func TestEmailInstallationManagedDomainUsesProviderDefault(t *testing.T) {
	p := azurePlanFixture()
	p.EmailService = "operator-email"
	p.CommunicationService = "operator-delivery"
	p.Domain = "AzureManagedDomain"
	p.ManagedDomain = true
	p.ClusterID = "cluster"
	created := map[string]bool{p.emailPath(): true, p.communicationPath(): true, p.domainPath(): true}
	protocol := installationProtocol(t, created, func() string { return p.ClusterID })
	cfg, _, err := protocol.finish(context.Background(), "ephemeral", "self", p, map[string]any{"username": "donotreply", "displayName": "Unusable custom name"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Default != "donotreply@11111111-1111-1111-1111-111111111111.azurecomm.net" || cfg.Senders[cfg.Default] != "DoNotReply" {
		t.Fatalf("wrong actual sender: %+v", cfg)
	}
	if ok, err := protocol.ensureSubscription(context.Background(), "ephemeral", p.ClusterID, p, true); !ok || err != nil {
		t.Fatalf("group access: %v %v", ok, err)
	}
}
