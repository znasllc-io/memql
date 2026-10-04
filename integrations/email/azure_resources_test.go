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
	if _, err := a.ensureService(context.Background(), "token", p.emailPath(), "client", "United States", p.ClusterID); err == nil {
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
	cfg, reply, err := a.finish(context.Background(), "token", "client", p, map[string]any{"username": "news", "displayName": "Client", "replyTo": "help@client.example"})
	if err != nil {
		t.Fatal(err)
	}
	if !linked || cfg.Default != "news@client.example" || cfg.AccountID != "client" || reply != "help@client.example" {
		t.Fatal("lost client sender", cfg.Default, reply)
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
	if _, err := a.ensureService(context.Background(), "token", p.emailPath(), "self", "United States", "this-cluster"); err == nil {
		t.Fatal("borrowed another cluster's service")
	}
}
