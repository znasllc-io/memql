package automations_test

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/memql"
)

// Execute the actual shipped cloud entrypoints, including their actions and
// pure choice logic. Only effects are replaced: no cloud credentials or shell
// process can be reached. Event annotations become callable in this test host.
func TestCloudInstallAlwaysEstablishesEmail(t *testing.T) {
	for _, entry := range []string{"installInstance", "bringUpInstance"} {
		for _, existing := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/existing=%v", entry, existing), func(t *testing.T) {
				var configured map[string]any
				calls := []string{}
				facts := map[string]any{"issuerHash": strings.Repeat("a", 64), "serviceSuffix": strings.Repeat("a", 48), "location": "northeurope", "geography": "Europe", "installed": map[string]any{}}
				if existing {
					facts["installed"] = map[string]any{"resourceGroup": "retained-group", "identityName": "retained-identity", "emailService": "retained-email", "communicationService": "retained-send", "location": "westus2", "dataLocation": "United States", "replyTo": "team@example.com"}
				}
				opts := workflowhost.Options{
					Load: func(name string) (*automations.Automation, error) {
						source, ok := memql.DSLConstructSource(nil, "automation", name)
						if !ok {
							return nil, fmt.Errorf("missing %s", name)
						}
						// Change invocation metadata only. Keep the shipped body.
						source = strings.ReplaceAll(source, `@trigger(event="instance.installRequested")`, "@template")
						source = strings.ReplaceAll(source, `@trigger(event="instance.bringUpRequested")`, "@template")
						source = strings.ReplaceAll(source, `@trigger(event="instance.provisionRequested")`, "@template")
						return automations.NewLoader(automations.LoaderOptions{}).CompileSource(source, "installed:"+name)
					},
					Operations: map[string]workflowhost.Operation{"shell.script": func(_ context.Context, args map[string]any) (any, error) {
						name, _ := args["script"].(string)
						calls = append(calls, name)
						switch name {
						case "deploy.inspectAzureEmail":
							return facts, nil
						case "deploy.configureAzureEmail":
							configured = maps.Clone(args)
						}
						return map[string]any{"passed": true}, nil
					}},
				}
				// No email options: exactly the pre-existing install contract.
				args := map[string]any{"instanceId": "instance", "subscriptionId": "subscription", "resourceGroup": "cluster-rg", "clusterName": "cluster", "registryName": "registry", "keyVaultName": "vault", "overlayPath": "overlay", "revision": "main", "repoUrl": "https://example.com/repo", "repoTransport": "https", "namespace": "memql", "esoClientId": "eso-client", "tenantId": "tenant"}
				if _, err := workflowhost.Run(context.Background(), entry, args, opts); err != nil {
					t.Fatal(err)
				}
				if configured == nil {
					t.Fatal("fresh cloud installation silently omitted email")
				}
				for _, key := range []string{"emailResourceGroup", "emailIdentityName", "emailService", "communicationService", "emailLocation", "emailDataLocation"} {
					if configured[key] == nil || configured[key] == "" {
						t.Errorf("missing %s: %v", key, configured)
					}
				}
				if existing {
					if configured["emailResourceGroup"] != "retained-group" || configured["emailIdentityName"] != "retained-identity" || configured["emailDataLocation"] != "United States" || configured["replyTo"] != "team@example.com" {
						t.Fatalf("installed choices replaced: %v", configured)
					}
				} else if configured["emailResourceGroup"] != "memql-email-"+strings.Repeat("a", 64) || configured["emailDataLocation"] != "Europe" || configured["emailLocation"] != "northeurope" {
					t.Fatalf("incorrect physical/residency defaults: %v", configured)
				}
				sequence := strings.Join(calls, " ")
				if strings.Index(sequence, "deploy.configureAzureEmail") > strings.Index(sequence, "argocd.sync") {
					t.Fatal("engine started before email authorization")
				}
			})
		}
	}
}
