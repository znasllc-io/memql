package packages

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/emailconfig"
	"gopkg.in/yaml.v3"
)

// This capability shares the package decoder with Deployables and leaves its
// declarations intact. Email still owns authorization, credentials and Azure.
func (i *Integration) handleCampaigns(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil || ac.Synthetic || ac.UserId == "" || (ac.Role != auth.RoleOwner && ac.Role != auth.RoleDeveloper) {
		return nil, fmt.Errorf("campaign package configuration requires a signed-in owner or developer")
	}
	textArg := func(name string) string { v, _ := args[name].(string); return v }
	action, source := textArg("action"), textArg("manifest")
	if action != "inspect" && action != "export" && action != "import" && action != "installed" {
		return nil, fmt.Errorf("unknown campaign package action")
	}
	if len(source) > 2<<20 {
		return nil, fmt.Errorf("memql-package.yaml exceeds 2 MiB")
	}
	if action == "installed" {
		manifest, err := ReadInstalledManifest()
		if err != nil {
			return nil, err
		}
		return resultNode(map[string]any{"status": "installed", "manifest": manifest}), nil
	}
	var manifest *Manifest
	if strings.TrimSpace(source) == "" && action == "export" {
		manifest = &Manifest{FormatVersion: ManifestFormatVersion, Name: "campaigns", Deployables: []ManifestDeployable{}}
	} else {
		var err error
		manifest, err = ParseManifest([]byte(source))
		if err != nil {
			return nil, err
		}
	}
	if action == "inspect" {
		return resultNode(map[string]any{"status": "review", "manifest": manifest}), nil
	}
	account := memql.BareShortId(textArg("accountId"))
	if account == "" {
		return nil, fmt.Errorf("select the destination organization")
	}
	options := "{}"
	if action == "import" {
		if confirmed, _ := args["confirmed"].(bool); !confirmed {
			return nil, fmt.Errorf("review the package and organization before importing")
		}
		if manifest.Campaigns == nil {
			return nil, fmt.Errorf("this package has no campaign configuration")
		}
		var chosen *emailconfig.Domain
		for _, d := range manifest.Campaigns.Domains {
			if d.Organization == textArg("organization") {
				copy := d
				chosen = &copy
			}
		}
		if chosen == nil {
			return nil, fmt.Errorf("choose a campaign organization from this package")
		}
		config := emailconfig.Campaigns{Azure: manifest.Campaigns.Azure, Domains: []emailconfig.Domain{*chosen}}
		raw, err := json.Marshal(config)
		if err != nil {
			return nil, err
		}
		options = "{configuration: " + langparser.QuoteString(string(raw)) + "}"
	}
	query := "builtin emailAzureSetup(accountId: " + langparser.QuoteString(account) + ", action: " + langparser.QuoteString(action+"Package") + ", options: " + options + ")"
	response, err := i.engine.Execute(memql.ContextWithFreshRead(ctx), query)
	if err != nil {
		return nil, err
	}
	rows := memql.MaterializeRows(response)
	if len(rows) != 1 {
		return nil, fmt.Errorf("email setup did not confirm the package operation")
	}
	if action == "import" {
		return resultNode(map[string]any{"status": "imported", "connection": rows[0]}), nil
	}
	raw, err := json.Marshal(rows[0]["configuration"])
	if err != nil {
		return nil, err
	}
	config, err := emailconfig.Decode(raw)
	if err != nil {
		return nil, err
	}
	if len(config.Domains) != 1 {
		return nil, fmt.Errorf("email export did not return one organization")
	}
	if manifest.Campaigns == nil {
		manifest.Campaigns = config
	} else {
		if !emailconfig.SameResources(manifest.Campaigns.Azure, config.Azure) {
			return nil, fmt.Errorf("the loaded package uses different Azure settings; export to a separate package or review its cluster configuration first")
		}
		if config.Azure.TenantID == "" {
			config.Azure.TenantID = manifest.Campaigns.Azure.TenantID
		}
		manifest.Campaigns.Azure = config.Azure
		matched := -1
		for n, d := range manifest.Campaigns.Domains {
			if strings.EqualFold(d.Organization, config.Domains[0].Organization) || strings.EqualFold(d.Domain, config.Domains[0].Domain) {
				if matched >= 0 {
					return nil, fmt.Errorf("the loaded package matches different organization and domain bindings; review them before exporting")
				}
				matched = n
			}
		}
		if matched >= 0 {
			// The package's portable label may differ from the live account's
			// display name. Azure has already verified this domain's ownership.
			config.Domains[0].Organization = manifest.Campaigns.Domains[matched].Organization
			manifest.Campaigns.Domains[matched] = config.Domains[0]
		} else {
			manifest.Campaigns.Domains = append(manifest.Campaigns.Domains, config.Domains[0])
		}
	}
	if err := manifest.Campaigns.Validate(); err != nil {
		return nil, err
	}
	out, err := yaml.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	return resultNode(map[string]any{"status": "exported", "manifest": manifest, "yaml": string(out)}), nil
}
