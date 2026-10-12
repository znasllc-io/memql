package email

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/emailconfig"
	"github.com/znasllc-io/memql/core/id"
)

// Only node startup/maintenance constructs this scope, from validated installed
// configuration. Direct DSL calls receive refusing stubs, never this authority.
// A bounded pass leaves durable progress; another replica can resume it.
func (a *azureSetup) bootstrapInstallation(ctx context.Context) error {
	if a == nil || a.installation == nil {
		return nil
	}
	s := &installationScope{a: a}
	_, err := workflowhost.Run(emailStateContext(ctx), "emailInstallationWorkflow", nil, workflowhost.Options{Operations: s.operations()})
	if err != nil {
		return err
	}
	var c azureConnection
	if _, _, err = a.store.read(ctx, "organization:self", &c); err != nil {
		return err
	}
	if c.Plan.ManagedDomain && c.Status != "ready" && c.Status != "disconnected" {
		return fmt.Errorf("Azure is preparing the installation's email sender; setup will resume automatically")
	}
	return nil
}

type installationScope struct{ a *azureSetup }

func (s *installationScope) operations() map[string]workflowhost.Operation {
	return map[string]workflowhost.Operation{
		"emailInstallationAccount":   s.account,
		"emailInstallationConnect":   s.connect,
		"emailInstallationPrepare":   s.prepare,
		"emailInstallationProvision": s.provision,
		"emailInstallationSender":    s.sender,
	}
}

func (s *installationScope) connect(ctx context.Context, _ map[string]any) (any, error) {
	a, c := s.a, s.a.installation
	var cluster azureClusterConnection
	stopped := false
	err := a.store.change(ctx, azureClusterKey, &cluster, func(found bool) error {
		if found {
			// A user's later disconnect or delegated replacement wins over startup.
			if cluster.Status == "disconnected" || cluster.WorkloadClientID == "" {
				stopped = true
				return nil
			}
			if cluster.WorkloadClientID != c.ClientID || cluster.TenantID != c.TenantID || cluster.Plan != c.clusterPlan() {
				return fmt.Errorf("installed email identity differs from the saved cluster connection; review installation settings")
			}
			if cluster.Status != "connected" {
				return fmt.Errorf("the installed Azure email connection needs attention in Settings")
			}
			return nil
		}
		cluster = azureClusterConnection{ID: id.NewShortId(), Revision: id.NewShortId(), Status: "connected", TenantID: c.TenantID, WorkloadClientID: c.ClientID, Plan: c.clusterPlan()}
		return nil
	})
	return map[string]any{"stopped": stopped}, err
}
func (s *installationScope) prepare(ctx context.Context, _ map[string]any) (any, error) {
	a, c := s.a, s.a.installation
	var cluster azureClusterConnection
	if _, _, err := a.store.read(ctx, azureClusterKey, &cluster); err != nil {
		return nil, err
	}
	var connection azureConnection
	err := a.store.change(ctx, "organization:self", &connection, func(found bool) error {
		// An existing operator connection is an explicit choice; never replace it.
		if found {
			return nil
		}
		connection = azureConnection{AccountID: "self", Status: "planned", Plan: azurePlan{ClusterID: cluster.ID, SubscriptionID: c.SubscriptionID, ResourceGroup: c.ResourceGroup, DataLocation: c.DataLocation, EmailService: c.EmailService, CommunicationService: c.CommunicationService, Domain: "AzureManagedDomain", ManagedDomain: true}, RequestedSender: &emailconfig.Sender{Username: "donotreply", DisplayName: "DoNotReply", ReplyTo: c.ReplyTo}}
		return nil
	})
	stopped := connection.Status == "ready" || connection.Status == "disconnected" || !connection.Plan.ManagedDomain
	return map[string]any{"stopped": stopped}, err
}
func (s *installationScope) provision(ctx context.Context, _ map[string]any) (any, error) {
	return s.resource(ctx, "provision")
}
func (s *installationScope) sender(ctx context.Context, _ map[string]any) (any, error) {
	return s.resource(ctx, "sender")
}
func (s *installationScope) resource(ctx context.Context, action string) (any, error) {
	a := s.a
	return a.withCluster(ctx, "self", func(session azureSession) (map[string]any, error) {
		var c azureConnection
		if _, _, err := a.store.read(ctx, "organization:self", &c); err != nil {
			return nil, err
		}
		if c.Status == "ready" || c.Status == "disconnected" || !c.Plan.ManagedDomain {
			return azureConnectionSummary(c), nil
		}
		options := map[string]any{"confirmed": true, "planId": azurePlanID(c), "username": "donotreply", "displayName": "DoNotReply", "replyTo": a.installation.ReplyTo}
		return a.resourceAction(ctx, session, action, options)
	})
}

// Owner sign-in mail is needed before system.startup seeds self. Use the same
// ordinary create-only mutation under a cross-replica gate; never reset edits.
func (s *installationScope) account(ctx context.Context, _ map[string]any) (any, error) {
	db, err := s.a.store.gates()
	if err != nil {
		return nil, err
	}
	release, err := memql.AcquireWriteGate(ctx, db, "email-installation:self-account")
	if err != nil {
		return nil, err
	}
	defer release()
	result, err := s.a.store.engine.Execute(ctx, `query clientAccountById(accountId: "self")`)
	if err != nil {
		return nil, err
	}
	if len(memql.MaterializeRows(result)) != 0 {
		return nil, nil
	}
	_, err = s.a.store.engine.Execute(ctx, `mutation createClientAccount(accountId: "self", name: "My company")`)
	return nil, err
}
