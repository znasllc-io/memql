package packages

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/emailconfig"
	"gopkg.in/yaml.v3"
)

const campaignManifest = `formatVersion: 1
name: client-package
deployables:
  - name: web
    path: web
    kind: static
    build: {command: npm run build, output: out}
    deployment: {slug: quiet-cedar, domains: [www.example.com]}
campaigns:
  azure:
    subscriptionId: 12345678-1234-1234-1234-123456789012
    resourceGroup: email-resources
    dataLocation: United States
  domains:
    - organization: example.com
      domain: example.com
      emailService: example-email
      communicationService: example-delivery
      sender: {username: news, displayName: Example}
      dns:
        - {purpose: Domain, name: example.com, type: TXT, value: proof, ttl: 3600}
`

func campaignActor() context.Context {
	return auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "operator", Role: auth.RoleOwner})
}

func TestCampaignPackageRoundTripAndRefusals(t *testing.T) {
	m, err := ParseManifest([]byte(campaignManifest))
	if err != nil {
		t.Fatal(err)
	}
	rep := &Report{OK: true, Manifest: m}
	before := PlanFingerprint(rep)
	raw, err := yaml.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	again, err := ParseManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(m)
	b, _ := json.Marshal(again)
	if string(a) != string(b) {
		t.Fatal("round trip lost package fields")
	}
	m.Campaigns.Domains[0].Sender.Username = "hello"
	if before == PlanFingerprint(rep) {
		t.Fatal("campaign change bypassed review")
	}
	for _, bad := range []string{
		strings.Replace(campaignManifest, "resourceGroup:", "accessToken: leaked\n    resourceGroup:", 1),
		strings.Replace(campaignManifest, "sender: {username: news, displayName: Example}", "sender: {username: news, displayName: Example, verified: true}", 1),
		strings.Replace(campaignManifest, "ttl: 3600", "ttl: 3600, status: Verified", 1),
		strings.Replace(campaignManifest, "domain: example.com", "domain: https://example.com", 1),
		strings.Replace(campaignManifest, "username: news", "username: bad@address", 1),
	} {
		if _, err := ParseManifest([]byte(bad)); err == nil {
			t.Fatal("accepted invalid configuration")
		}
	}
}

func TestCampaignPackageExportPreservesAppsAndImportBindsExplicitly(t *testing.T) {
	m, err := ParseManifest([]byte(campaignManifest))
	if err != nil {
		t.Fatal(err)
	}
	cfg := *m.Campaigns
	cfg.Domains = append([]emailconfig.Domain(nil), cfg.Domains...)
	// The account display name can differ from the package's portable label.
	cfg.Domains[0].Organization = "Example LLC"
	cfg.Domains[0].DNS[0].Value = "fresh-proof"
	raw, _ := json.Marshal(cfg)
	var config map[string]any
	_ = json.Unmarshal(raw, &config)
	e := &recordingEngine{rows: map[string][]map[string]any{"builtin emailAzureSetup": {{"status": "exported", "configuration": config}}}}
	i := NewIntegration(e, nil)
	reply, err := i.handleCampaigns(campaignActor(), map[string]any{"action": "export", "manifest": campaignManifest, "accountId": "client"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	output := replyPayload(t, reply)["yaml"].(string)
	saved, err := ParseManifest([]byte(output))
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(m.Deployables)
	after, _ := json.Marshal(saved.Deployables)
	if string(before) != string(after) || len(saved.Campaigns.Domains) != 1 || saved.Campaigns.Domains[0].Organization != "example.com" || saved.Campaigns.Domains[0].DNS[0].Value != "fresh-proof" {
		t.Fatal("export lost apps, the portable binding, or fresh DNS")
	}
	calls := len(e.queries)
	for _, args := range []map[string]any{
		{"action": "import", "manifest": output, "accountId": "client", "organization": "example.com"},
		{"action": "import", "manifest": output, "accountId": "", "organization": "example.com", "confirmed": true},
		{"action": "import", "manifest": output, "accountId": "client", "organization": "missing", "confirmed": true},
	} {
		if _, err := i.handleCampaigns(campaignActor(), args, 0); err == nil {
			t.Fatal("import accepted missing review or binding")
		}
	}
	if len(e.queries) != calls {
		t.Fatal("refused import reached email")
	}
	_, err = i.handleCampaigns(campaignActor(), map[string]any{"action": "import", "manifest": output, "accountId": "destination", "organization": "example.com", "confirmed": true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	q := e.queries[len(e.queries)-1]
	if !strings.Contains(q, `accountId: "destination"`) || !strings.Contains(q, `action: "importPackage"`) {
		t.Fatal("lost explicit destination")
	}
	for _, ctx := range []context.Context{context.Background(), auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "admin", Role: auth.RoleAdmin}), auth.ContextWithAccess(context.Background(), &auth.AccessContext{Role: auth.RoleOwner, Synthetic: true})} {
		if _, err := i.handleCampaigns(ctx, map[string]any{"action": "inspect", "manifest": output}, 0); err == nil {
			t.Fatal("unauthorized package access")
		}
	}
}

func TestCampaignPackageExportRefusesAmbiguousBinding(t *testing.T) {
	m, err := ParseManifest([]byte(campaignManifest))
	if err != nil {
		t.Fatal(err)
	}
	cfg := *m.Campaigns
	cfg.Domains = append([]emailconfig.Domain(nil), cfg.Domains...)
	cfg.Domains[0].Organization = "Example LLC"
	m.Campaigns.Domains = append(m.Campaigns.Domains, emailconfig.Domain{
		Organization: "Example LLC", Domain: "other.example.com", EmailService: "other-email", CommunicationService: "other-delivery",
	})
	source, err := yaml.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	e := &recordingEngine{rows: map[string][]map[string]any{"builtin emailAzureSetup": {{"status": "exported", "configuration": cfg}}}}
	i := NewIntegration(e, nil)
	_, err = i.handleCampaigns(campaignActor(), map[string]any{"action": "export", "manifest": string(source), "accountId": "client"}, 0)
	if err == nil || !strings.Contains(err.Error(), "different organization and domain bindings") {
		t.Fatalf("ambiguous binding was not refused: %v", err)
	}
}

func TestInstalledPackageIsOptionalAndReloaded(t *testing.T) {
	path := filepath.Join(t.TempDir(), ManifestName)
	if m, err := readInstalledManifest(path); m != nil || err != nil {
		t.Fatal(m, err)
	}
	if err := os.WriteFile(path, []byte(campaignManifest), 0600); err != nil {
		t.Fatal(err)
	}
	first, err := readInstalledManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(campaignManifest, "example.com", "client.example", -1)), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := readInstalledManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if first.Campaigns.Domains[0].Domain == second.Campaigns.Domains[0].Domain {
		t.Fatal("package defaults were cached")
	}
}
