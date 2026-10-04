package email

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/emailconfig"
)

func TestPublicEmailPackageExcludesCredentialsAndVerification(t *testing.T) {
	c := azureConnection{Status: "ready", AccountID: "private-account-id", SenderIdentityID: "private-sender-id", Plan: azurePlan{ClusterID: "private-cluster-id", Domain: "example.com", EmailService: "example-email", CommunicationService: "example-delivery"}, Config: ACSConfig{AccessKey: "private-access-key", Endpoint: "https://private-endpoint", Default: "news@example.com", Senders: map[string]string{"news@example.com": "Example"}}}
	cluster := azureClusterConnection{ID: "private-cluster-id", Session: azureSession{AccessToken: "private-access-token", RefreshToken: "private-refresh-token", DeviceCode: "private-device-code"}}
	raw, err := json.Marshal(emailconfig.Campaigns{Azure: publicAzure(cluster), Domains: []emailconfig.Domain{publicEmailDomain(c, "example.com")}})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"private-", "ready", "verified", "accessKey", "accessToken", "senderIdentityId", "clusterId"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("export included %s", forbidden)
		}
	}
	if !strings.Contains(string(raw), `"username":"news"`) || !strings.Contains(string(raw), `"displayName":"Example"`) {
		t.Fatal("public sender was lost")
	}
}

func TestInstalledDefaultsOnlyPrefillMatchingOrganization(t *testing.T) {
	a := &azureSetup{packageDefaults: func() (*emailconfig.Campaigns, error) {
		return &emailconfig.Campaigns{Azure: emailconfig.Azure{ResourceGroup: "mail"}, Domains: []emailconfig.Domain{{Organization: "example.com", Domain: "news.example.com", Sender: &emailconfig.Sender{Username: "news", DisplayName: "Client"}}}}, nil
	}}
	output := map[string]any{"status": "unconfigured"}
	if err := a.organizationDefaults(output, map[string]any{"name": "Other", "domain": "other.example"}); err != nil {
		t.Fatal(err)
	}
	if output["plan"] != nil {
		t.Fatal("client defaults leaked into another organization")
	}
	if err := a.organizationDefaults(output, map[string]any{"name": "Client", "domain": "example.com"}); err != nil {
		t.Fatal(err)
	}
	if output["status"] != "unconfigured" || output["requestedSender"] == nil || output["records"] != nil {
		t.Fatal("defaults claimed authorization or DNS verification")
	}
}
