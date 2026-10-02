package email

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/znasllc-io/memql/core/emailconfig"
)

func publicAzure(c azureClusterConnection) emailconfig.Azure {
	p := c.Plan
	tenant := c.Session.Tenant
	if !azureUUID.MatchString(tenant) {
		tenant = c.TenantID
	}
	return emailconfig.Azure{TenantID: tenant, SubscriptionID: p.SubscriptionID, ResourceGroup: p.ResourceGroup, CreateResourceGroup: p.CreateResourceGroup, ResourceGroupLocation: p.ResourceGroupLocation, DataLocation: p.DataLocation}
}

func publicEmailDomain(c azureConnection, organization string) emailconfig.Domain {
	d := emailconfig.Domain{Organization: organization, Domain: c.Plan.Domain, EmailService: c.Plan.EmailService, CommunicationService: c.Plan.CommunicationService, Sender: c.RequestedSender, DNS: c.RequestedDNS}
	if c.Config.Default != "" {
		username, _, _ := strings.Cut(c.Config.Default, "@")
		d.Sender = &emailconfig.Sender{Username: username, DisplayName: c.Config.Senders[c.Config.Default], ReplyTo: c.ReplyTo}
	}
	return d
}

// Export an allowlisted projection, with DNS read from Azure now. Sealed
// credentials and cluster-local IDs never become package fields.
func (a *azureSetup) exportPackage(ctx context.Context, account, organization string) (map[string]any, error) {
	return a.withCluster(ctx, account, func(session azureSession) (map[string]any, error) {
		var c azureConnection
		found, _, err := a.store.read(ctx, "organization:"+account, &c)
		if err != nil {
			return nil, err
		}
		if !found || c.Status == "disconnected" {
			return nil, fmt.Errorf("configure a sending domain before exporting it")
		}
		if c.Plan.ClusterID != session.ClusterID {
			return nil, fmt.Errorf("review this domain under the current cluster connection before exporting")
		}
		// GET only: exporting never provisions or links a resource.
		for _, path := range []string{c.Plan.emailPath(), c.Plan.communicationPath()} {
			var r azureResource
			if err := a.protocol.arm(ctx, http.MethodGet, azureVersion(path), session.AccessToken, nil, &r); err != nil {
				return nil, err
			}
			if r.Tags["memql-organization"] != account || r.Tags["memql-cluster"] != session.ClusterID {
				return nil, fmt.Errorf("Azure resource ownership does not match this organization")
			}
		}
		resource, err := a.protocol.domain(ctx, session.AccessToken, c.Plan, false)
		if err != nil {
			return nil, err
		}
		var cluster azureClusterConnection
		if _, _, err := a.store.read(ctx, azureClusterKey, &cluster); err != nil {
			return nil, err
		}
		d := publicEmailDomain(c, organization)
		d.DNS = nil
		for _, purpose := range []string{"Domain", "SPF", "DKIM", "DKIM2"} {
			if r, ok := resource.Properties.VerificationRecords[purpose]; ok {
				d.DNS = append(d.DNS, emailconfig.DNSRecord{Purpose: purpose, Name: r.Name, Type: r.Type, Value: r.Value, TTL: r.TTL})
			}
		}
		config := emailconfig.Campaigns{Azure: publicAzure(cluster), Domains: []emailconfig.Domain{d}}
		if err := config.Validate(); err != nil {
			return nil, err
		}
		return map[string]any{"status": "exported", "configuration": config}, nil
	})
}

// Import saves a draft for one explicitly selected organization. It cannot
// connect Azure, create resources, change DNS, transfer resource ownership, or
// activate a sender. Existing ready connections must match rather than being
// overwritten. The cluster and organization locks serialize this across nodes.
func (a *azureSetup) importPackage(ctx context.Context, account string, options map[string]any) (map[string]any, error) {
	config, err := emailconfig.Decode([]byte(argString(options, "configuration")))
	if err != nil {
		return nil, err
	}
	if len(config.Domains) != 1 {
		return nil, fmt.Errorf("import one campaign organization at a time")
	}
	d, scope := config.Domains[0], config.Azure
	raw, _ := json.Marshal(scope)
	var input map[string]any
	_ = json.Unmarshal(raw, &input)
	input["domain"], input["emailService"], input["communicationService"] = d.Domain, d.EmailService, d.CommunicationService
	plan, err := readAzurePlan(input)
	if err != nil {
		return nil, err
	}
	var cluster azureClusterConnection
	var output map[string]any
	err = a.store.change(ctx, azureClusterKey, &cluster, func(found bool) error {
		selected := plan
		selected.Domain, selected.EmailService, selected.CommunicationService = "", "", ""
		if found && (cluster.Status == "connected" || cluster.Status == "reauthorize") && !emailconfig.SameResources(publicAzure(cluster), scope) {
			return fmt.Errorf("this package uses different Azure settings; disconnect the cluster explicitly before changing them")
		}
		if cluster.Status == "connected" || cluster.Status == "reauthorize" {
			plan.ClusterID = cluster.ID
			plan.ResourceGroup = cluster.Plan.ResourceGroup
			plan.CreateResourceGroup = cluster.Plan.CreateResourceGroup
			plan.ResourceGroupLocation = cluster.Plan.ResourceGroupLocation
		}
		var c azureConnection
		err := a.store.change(ctx, "organization:"+account, &c, func(found bool) error {
			if found && c.Status != "disconnected" && c.Status != "draft" {
				if c.Plan != plan {
					return fmt.Errorf("this package would replace the organization's sending domain; disconnect it explicitly first")
				}
				if d.Sender != nil && c.Config.Default != "" {
					current := publicEmailDomain(c, d.Organization).Sender
					if *current != *d.Sender {
						return fmt.Errorf("this package changes a working sender; review that change in sender settings")
					}
				}
				// Existing readiness and credentials are retained, never imported.
				output = azureConnectionSummary(c)
				return nil
			}
			c = azureConnection{AccountID: account, Status: "draft", Plan: plan, RequestedSender: d.Sender, RequestedDNS: d.DNS}
			output = azureConnectionSummary(c)
			return nil
		})
		if err != nil {
			return err
		}
		if cluster.Status != "connected" && cluster.Status != "reauthorize" {
			cluster.Plan = selected
			cluster.TenantID = scope.TenantID
		}
		return nil
	})
	return output, err
}

// SetPackageDefaults installs a public, shared configuration source. Loading a
// package only pre-fills setup; it never grants consent or activates delivery.
func (i *Integration) SetPackageDefaults(source func() (*emailconfig.Campaigns, error)) {
	if i.azure != nil {
		i.azure.packageDefaults = source
	}
}

func publicScopeMap(scope emailconfig.Azure) map[string]any {
	raw, _ := json.Marshal(scope)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func (a *azureSetup) organizationDefaults(output map[string]any, account map[string]any) error {
	if a.packageDefaults == nil {
		return nil
	}
	config, err := a.packageDefaults()
	if err != nil || config == nil {
		return err
	}
	name, _ := account["name"].(string)
	domain, _ := account["domain"].(string)
	var match *emailconfig.Domain
	for _, d := range config.Domains {
		if strings.EqualFold(d.Organization, name) || domain != "" && strings.EqualFold(d.Organization, domain) {
			if match != nil {
				return fmt.Errorf("the package matches more than one email configuration; import the intended organization explicitly")
			}
			copy := d
			match = &copy
		}
	}
	if match == nil {
		return nil
	}
	scope := publicScopeMap(config.Azure)
	scope["domain"], scope["emailService"], scope["communicationService"] = match.Domain, match.EmailService, match.CommunicationService
	output["plan"], output["requestedSender"] = scope, match.Sender
	return nil
}
