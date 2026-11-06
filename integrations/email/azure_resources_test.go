package email

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/emailconfig"
)

func azurePlanFixture() azurePlan {
	return azurePlan{SubscriptionID: "12345678-1234-1234-1234-123456789012", ResourceGroup: "email", EmailService: "client-email", CommunicationService: "client-delivery", Domain: "client.example", DataLocation: "United States"}
}

func TestAzureProvisioningDoesNotOverwriteUnrelatedResources(t *testing.T) {
	p := azurePlanFixture()
	a := newAzureProtocol("")
	writes := 0
	a.client.Transport = acsRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			writes++
		}
		body, _ := json.Marshal(map[string]any{"id": p.emailPath(), "tags": map[string]string{"memql-organization": "another-client"}, "properties": map[string]string{"dataLocation": "United States", "provisioningState": "Succeeded"}})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})
	if _, err := a.ensureService(context.Background(), "token", p.emailPath(), "client", "United States", p.ClusterID, nil); err == nil {
		t.Fatal("unrelated service accepted")
	}
	if writes != 0 {
		t.Fatal("changed unrelated Azure resource")
	}
}

func TestAzureFinishPreservesDomainLinksAndUsesVerifiedSender(t *testing.T) {
	p := azurePlanFixture()
	p.ResourceGroup = "Client Email"
	domainID, _ := url.PathUnescape(p.domainPath())
	a := newAzureProtocol("")
	linked := false
	a.client.Transport = acsRoundTrip(func(r *http.Request) (*http.Response, error) {
		props := map[string]any{"dataLocation": "United States", "provisioningState": "Succeeded"}
		resource := map[string]any{"id": r.URL.Path, "tags": map[string]string{"memql-organization": "client"}, "properties": props}
		switch {
		case strings.HasSuffix(r.URL.Path, "/listKeys"):
			resource = map[string]any{"primaryKey": base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))}
		case strings.Contains(r.URL.Path, "/senderUsernames/"):
			props["username"] = "news"
			props["displayName"] = "Client"
		case strings.HasSuffix(r.URL.Path, "/domains/client.example"):
			props["domainManagement"] = "CustomerManaged"
			states := map[string]any{}
			for _, kind := range []string{"Domain", "SPF", "DKIM", "DKIM2"} {
				states[kind] = map[string]string{"status": "Verified"}
			}
			props["verificationStates"] = states
		case strings.Contains(r.URL.Path, "/communicationServices/"):
			props["hostName"] = "client.communication.azure.com"
			links := []string{"/existing/domain"}
			if linked {
				links = append(links, domainID)
			}
			props["linkedDomains"] = links
			if r.Method == http.MethodPatch {
				var body struct {
					Properties struct {
						LinkedDomains []string `json:"linkedDomains"`
					} `json:"properties"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if len(body.Properties.LinkedDomains) != 2 || body.Properties.LinkedDomains[0] != "/existing/domain" || body.Properties.LinkedDomains[1] != domainID {
					t.Fatal("lost existing domain links")
				}
				linked = true
			}
		}
		body, _ := json.Marshal(resource)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})
	cfg, added, err := a.finish(context.Background(), "token", "client", p, ACSConfig{}, map[string]any{"username": "news", "displayName": "Client", "replyTo": "help@client.example"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !linked || cfg.Default != "news@client.example" || cfg.AccountID != "client" || added.ReplyTo != "help@client.example" {
		t.Fatal("lost client sender", cfg.Default, added.ReplyTo)
	}
}

func TestAzureDomainCannotBecomeReadyWithMissingVerification(t *testing.T) {
	var domain azureResource
	if err := json.Unmarshal([]byte(`{"properties":{"provisioningState":"Succeeded","verificationStates":{"Domain":{"status":"Verified"},"SPF":{"status":"Verified"},"DKIM":{"status":"Verified"}}}}`), &domain); err != nil {
		t.Fatal(err)
	}
	if azureDomainVerified(domain) {
		t.Fatal("missing second signing key was accepted")
	}
	if azureDomainSummary(domain)["status"] != "dns" {
		t.Fatal("incomplete domain claimed ready")
	}
}

// azureDomainFake answers finish for a verified customer domain whose sender
// usernames are `registered` (username -> display name). A PUT registers one,
// as Azure does, so a test can read back what was registered. Its resources
// belong to `account` in `cluster`.
func azureDomainFake(t *testing.T, account, cluster string, registered map[string]string) *azureProtocol {
	t.Helper()
	domainID, _ := url.PathUnescape(azurePlanFixture().domainPath())
	a := newAzureProtocol("")
	a.client.Transport = acsRoundTrip(func(r *http.Request) (*http.Response, error) {
		props := map[string]any{"dataLocation": "United States", "provisioningState": "Succeeded"}
		var out any = map[string]any{"id": r.URL.Path, "tags": map[string]string{"memql-organization": account, "memql-cluster": cluster}, "properties": props}
		code := http.StatusOK
		switch path := r.URL.Path; {
		case strings.HasSuffix(path, "/listKeys"):
			out = map[string]any{"primaryKey": base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))}
		case strings.HasSuffix(path, "/senderUsernames"):
			rows := []any{}
			for username, name := range registered {
				rows = append(rows, map[string]any{"properties": map[string]string{"username": username, "displayName": name}})
			}
			out = map[string]any{"value": rows}
		case strings.Contains(path, "/senderUsernames/"):
			username := path[strings.LastIndex(path, "/")+1:]
			if r.Method == http.MethodPut {
				var body struct {
					Properties struct {
						DisplayName string `json:"displayName"`
					} `json:"properties"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				registered[username] = body.Properties.DisplayName
			}
			name, ok := registered[username]
			if !ok {
				code, out = http.StatusNotFound, map[string]any{}
			}
			props["username"], props["displayName"] = username, name
		case strings.HasSuffix(path, "/domains/client.example"):
			props["domainManagement"] = "CustomerManaged"
			states := map[string]any{}
			for _, kind := range []string{"Domain", "SPF", "DKIM", "DKIM2"} {
				states[kind] = map[string]string{"status": "Verified"}
			}
			props["verificationStates"] = states
		case strings.Contains(path, "/communicationServices/"):
			props["hostName"] = "client.communication.azure.com"
			props["linkedDomains"] = []string{domainID}
		}
		body, _ := json.Marshal(out)
		return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})
	return a
}

// A connection that is sending: news@ is its default and its Reply-To is the default's.
func sendingConfig() ACSConfig {
	return ACSConfig{AccountID: "client", Endpoint: "https://client.communication.azure.com", AccessKey: base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))), Senders: map[string]string{"news@client.example": "Client"}, Default: "news@client.example", ReplyTo: "help@client.example"}
}

func TestTheFirstSenderIsTheDefault(t *testing.T) {
	// Azure already has a username someone added outside MemQL. The first sender
	// MemQL adds is still the one unaddressed mail goes out as.
	registered := map[string]string{"billing": "Client Billing"}
	a := azureDomainFake(t, "client", "", registered)
	cfg, added, err := a.finish(context.Background(), "token", "client", azurePlanFixture(), ACSConfig{}, map[string]any{"username": "news", "displayName": "Client", "replyTo": "help@client.example"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Default != "news@client.example" || cfg.ReplyTo != "help@client.example" {
		t.Fatalf("the first sender is not the default: %q, replies to %q", cfg.Default, cfg.ReplyTo)
	}
	if registered["news"] != "Client" || cfg.Senders["news@client.example"] != "Client" || cfg.Senders["billing@client.example"] != "Client Billing" {
		t.Fatalf("lost a sender Azure has: %v", cfg.Senders)
	}
	if added != (azureSender{Address: "news@client.example", DisplayName: "Client", ReplyTo: "help@client.example"}) {
		t.Fatalf("recorded the wrong sender: %+v", added)
	}
	// A default Azure no longer has cannot stay the default: the sender being
	// added takes its place, as a first one would.
	gone := sendingConfig()
	gone.Default, gone.Senders = "gone@client.example", map[string]string{"gone@client.example": "Gone"}
	if cfg, _, err = a.finish(context.Background(), "token", "client", azurePlanFixture(), gone, map[string]any{"username": "news", "displayName": "Client"}, false); err != nil || cfg.Default != "news@client.example" || cfg.ReplyTo != "" {
		t.Fatalf("kept a default Azure does not have: %q, replies to %q, %v", cfg.Default, cfg.ReplyTo, err)
	}
}

func TestAddingASenderKeepsTheDefault(t *testing.T) {
	registered := map[string]string{"news": "Client"}
	a := azureDomainFake(t, "client", "", registered)
	cfg, added, err := a.finish(context.Background(), "token", "client", azurePlanFixture(), sendingConfig(), map[string]any{"username": "hello", "displayName": "Client Hello", "replyTo": "replies@client.example"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if registered["hello"] != "Client Hello" {
		t.Fatal("the new sender was not registered with Azure")
	}
	if cfg.Default != "news@client.example" {
		t.Fatalf("adding a sender moved the default to %q", cfg.Default)
	}
	// The connection's Reply-To is the default's, so the new sender's does not replace it.
	if cfg.ReplyTo != "help@client.example" {
		t.Fatalf("adding a sender replaced the default's Reply-To with %q", cfg.ReplyTo)
	}
	if len(cfg.Senders) != 2 || cfg.Senders["hello@client.example"] != "Client Hello" || cfg.Senders["news@client.example"] != "Client" {
		t.Fatalf("senders: %v", cfg.Senders)
	}
	// What MemQL records is the sender just added, with its own Reply-To.
	if added != (azureSender{Address: "hello@client.example", DisplayName: "Client Hello", ReplyTo: "replies@client.example"}) {
		t.Fatalf("recorded the wrong sender: %+v", added)
	}
	// Adding the default again keeps it, and its new Reply-To is the connection's.
	if cfg, _, err = a.finish(context.Background(), "token", "client", azurePlanFixture(), cfg, map[string]any{"username": "news", "displayName": "Client", "replyTo": "desk@client.example"}, false); err != nil || cfg.Default != "news@client.example" || cfg.ReplyTo != "desk@client.example" {
		t.Fatalf("re-adding the default: %q, replies to %q, %v", cfg.Default, cfg.ReplyTo, err)
	}
	// Azure's display name is the one every send must use (M5): a different one is refused.
	if _, _, err = a.finish(context.Background(), "token", "client", azurePlanFixture(), cfg, map[string]any{"username": "hello", "displayName": "Someone Else"}, false); err == nil {
		t.Fatal("accepted a display name Azure does not have")
	}
}

func TestMakeDefaultMovesTheDefault(t *testing.T) {
	// The domain's package names the default sender with the connection's
	// Reply-To, so the two must move together.
	exported := func(c azureConnection) emailconfig.Sender { return *publicEmailDomain(c, "Client").Sender }

	// Asked for while adding: the new sender is the default, with its Reply-To.
	registered := map[string]string{"news": "Client"}
	a := azureDomainFake(t, "client", "", registered)
	cfg, _, err := a.finish(context.Background(), "token", "client", azurePlanFixture(), sendingConfig(), map[string]any{"username": "hello", "displayName": "Client Hello", "replyTo": "replies@client.example"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Default != "hello@client.example" || cfg.ReplyTo != "replies@client.example" {
		t.Fatalf("make default left the default at %q, replies to %q", cfg.Default, cfg.ReplyTo)
	}
	if got := exported(azureConnection{Status: "ready", AccountID: "client", Plan: azurePlanFixture(), Config: cfg, ReplyTo: cfg.ReplyTo}); got != (emailconfig.Sender{Username: "hello", DisplayName: "Client Hello", ReplyTo: "replies@client.example"}) {
		t.Fatalf("the package pairs the default with another Reply-To: %+v", got)
	}

	// Make default on a sender the domain already has moves the default and
	// takes that sender's own Reply-To with it; nothing else changes, and it
	// needs no Azure call.
	ready := sendingConfig()
	ready.Senders["hello@client.example"] = "Client Hello"
	connection := azureConnection{Status: "ready", AccountID: "client", Plan: azurePlanFixture(), Config: ready, ReplyTo: ready.ReplyTo, SenderIdentityID: azureSenderIdentityID("client", "news@client.example")}
	if err := connection.makeDefault(" Hello@Client.example ", "replies@client.example", "hello-record"); err != nil {
		t.Fatal(err)
	}
	if connection.Config.Default != "hello@client.example" || connection.SenderIdentityID != "hello-record" {
		t.Fatalf("make default did not move the default: %q", connection.Config.Default)
	}
	if connection.ReplyTo != "replies@client.example" || connection.Config.ReplyTo != "replies@client.example" {
		t.Fatalf("make default left the old default's Reply-To: %q / %q", connection.ReplyTo, connection.Config.ReplyTo)
	}
	if len(connection.Config.Senders) != 2 || connection.Config.Senders["hello@client.example"] != "Client Hello" || connection.Config.Senders["news@client.example"] != "Client" || connection.Config.AccessKey != ready.AccessKey || connection.Status != "ready" {
		t.Fatalf("make default changed more than the default: %+v", connection)
	}
	if got := exported(connection); got != (emailconfig.Sender{Username: "hello", DisplayName: "Client Hello", ReplyTo: "replies@client.example"}) {
		t.Fatalf("the package pairs the default with another Reply-To: %+v", got)
	}
	// A sender with no Reply-To of its own takes none: replies go to it.
	if err := connection.makeDefault("news@client.example", "", "news-record"); err != nil || connection.ReplyTo != "" || connection.Config.ReplyTo != "" || exported(connection).ReplyTo != "" {
		t.Fatalf("kept another sender's Reply-To: %q, %v", connection.ReplyTo, err)
	}
	// Only to a sender the domain has registered, and only on a sending domain.
	if err := connection.makeDefault("someone@client.example", "x@client.example", "x"); err == nil || connection.Config.Default != "news@client.example" || connection.ReplyTo != "" {
		t.Fatal("made a sender Azure does not have the default")
	}
	connection.Status = "dns"
	if err := connection.makeDefault("hello@client.example", "replies@client.example", "hello-record"); err == nil || connection.Config.Default != "news@client.example" {
		t.Fatal("chose a default on a domain that is not sending")
	}
}

func TestAzureProvisioningRefusesSameOrganizationInAnotherCluster(t *testing.T) {
	p := azurePlanFixture()
	a := newAzureProtocol("")
	a.client.Transport = acsRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			t.Fatal("changed another cluster's resource")
		}
		body, _ := json.Marshal(map[string]any{"id": p.emailPath(), "tags": map[string]string{"memql-organization": "self", "memql-cluster": "other-cluster"}, "properties": map[string]string{"dataLocation": "United States", "provisioningState": "Succeeded"}})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})
	if _, err := a.ensureService(context.Background(), "token", p.emailPath(), "self", "United States", "this-cluster", nil); err == nil {
		t.Fatal("borrowed another cluster's service")
	}
}
