package email

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/znasllc-io/memql/core/id"
)

const azureClusterKey = "azure-cluster"

// The cluster owns Azure management access. Organization transports retain
// their own resources, verified domains and sending keys. This sealed record
// never crosses the client boundary; every replica reads the same version.
type azureClusterConnection struct {
	TenantID string       `json:"tenantId,omitempty"`
	ID       string       `json:"id"`
	Revision string       `json:"revision"`
	Status   string       `json:"status"`
	Plan     azurePlan    `json:"plan"`
	Session  azureSession `json:"session"`
}

func azureClusterSummary(c azureClusterConnection) map[string]any {
	status := c.Status
	if status == "" {
		status = "unconfigured"
	}
	return map[string]any{"status": status, "subscriptionId": c.Plan.SubscriptionID,
		"resourceGroup": c.Plan.ResourceGroup, "createResourceGroup": c.Plan.CreateResourceGroup,
		"resourceGroupLocation": c.Plan.ResourceGroupLocation, "dataLocation": c.Plan.DataLocation, "tenantId": publicAzure(c).TenantID}
}

func (a *azureSetup) clusterAction(ctx context.Context, action string, options map[string]any) (map[string]any, error) {
	var cluster azureClusterConnection
	if action == "disconnectCluster" {
		if confirmed, _ := options["confirmed"].(bool); !confirmed {
			return nil, fmt.Errorf("confirm disconnecting Azure for all organizations in this cluster")
		}
		if err := a.store.change(ctx, azureClusterKey, &cluster, func(bool) error {
			cluster.Status = "disconnected"
			cluster.Revision = id.NewShortId()
			cluster.Session = azureSession{}
			return nil
		}); err != nil {
			return nil, err
		}
	} else if _, _, err := a.store.read(ctx, azureClusterKey, &cluster); err != nil {
		return nil, err
	}
	result := azureClusterSummary(cluster)
	if action == "clusterStatus" && cluster.Plan.SubscriptionID == "" && a.packageDefaults != nil {
		defaults, err := a.packageDefaults()
		if err != nil {
			return nil, err
		}
		if defaults != nil {
			for key, value := range publicScopeMap(defaults.Azure) {
				result[key] = value
			}
		}
	}
	clientID := a.protocol.clientID
	if a.applicationID != nil {
		var err error
		clientID, err = a.applicationID(ctx)
		if err != nil {
			return nil, err
		}
	}
	result["applicationReady"] = azureUUID.MatchString(clientID)
	return result, nil
}

func (a *azureSetup) saveCluster(ctx context.Context, session azureSession, options map[string]any) (map[string]any, error) {
	// Reuse the provider's validation of subscription, group, region and data
	// location. Domain/service values here are validation placeholders only.
	input := map[string]any{}
	for key, value := range options {
		input[key] = value
	}
	input["domain"], input["emailService"], input["communicationService"] = "setup.invalid", "setup", "setup"
	plan, err := readAzurePlan(input)
	if err != nil {
		return nil, err
	}
	plan.Domain, plan.EmailService, plan.CommunicationService = "", "", ""
	var subscription struct {
		State string `json:"state"`
	}
	if err := a.protocol.arm(ctx, http.MethodGet, "/subscriptions/"+plan.SubscriptionID+"?api-version=2022-12-01", session.AccessToken, nil, &subscription); err != nil {
		return nil, err
	}
	if subscription.State != "Enabled" {
		return nil, fmt.Errorf("this Azure subscription is not enabled for resource changes; check its subscription status in Azure")
	}
	if !plan.CreateResourceGroup {
		if err := a.protocol.arm(ctx, http.MethodGet, "/subscriptions/"+plan.SubscriptionID+"/resourceGroups/"+url.PathEscape(plan.ResourceGroup)+"?api-version=2021-04-01", session.AccessToken, nil, nil); err != nil {
			return nil, err
		}
	}
	if session.RefreshToken == "" {
		return nil, fmt.Errorf("Microsoft did not grant reusable sign-in; sign in again to connect the cluster")
	}
	var cluster azureClusterConnection
	err = a.store.change(ctx, azureClusterKey, &cluster, func(found bool) error {
		if cluster.Session.ID == session.ID && session.ID != "" && cluster.Status == "connected" && cluster.Plan == plan {
			return nil
		}
		if cluster.Revision != session.ClusterRevision {
			return fmt.Errorf("the cluster's Azure configuration changed; start Microsoft sign-in again")
		}
		if found && cluster.Plan != plan && cluster.Status != "disconnected" {
			return fmt.Errorf("disconnect the cluster's Azure connection before changing its subscription or resource settings")
		}
		if cluster.ID == "" || cluster.Plan != plan {
			cluster.ID = id.NewShortId()
		}
		cluster.Status, cluster.Plan = "connected", plan
		cluster.Revision = id.NewShortId()
		cluster.Session = session
		cluster.Session.DeviceCode, cluster.Session.UserCode = "", ""
		cluster.Session.ExpiresAt = time.Time{}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return azureClusterSummary(cluster), nil
}

// Serialize token rotation and provider changes across replicas. The callback
// receives only the saved cluster credential, never a caller's session id.
func (a *azureSetup) withCluster(ctx context.Context, account string, run func(azureSession) (map[string]any, error)) (map[string]any, error) {
	var cluster azureClusterConnection
	var output map[string]any
	var operationErr error
	err := a.store.change(ctx, azureClusterKey, &cluster, func(found bool) error {
		if !found || cluster.Status != "connected" {
			return fmt.Errorf("connect Microsoft Azure for this cluster before configuring an organization's email domain")
		}
		protocol := &azureProtocol{clientID: cluster.Session.ClientID, client: a.protocol.client}
		if !a.clock().Add(time.Minute).Before(cluster.Session.TokenExpiresAt) {
			reply, err := protocol.token(ctx, cluster.Session.Tenant, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {cluster.Session.RefreshToken}, "scope": {azureManagementScope + " offline_access"}})
			if err != nil {
				return err
			}
			if reply.Error != "" {
				cluster.Status = "reauthorize"
				cluster.Session = azureSession{}
				operationErr = fmt.Errorf("Microsoft requires sign-in again for the cluster's Azure connection")
				return nil
			}
			cluster.Session.AccessToken = reply.AccessToken
			if reply.RefreshToken != "" {
				cluster.Session.RefreshToken = reply.RefreshToken
			}
			cluster.Session.TokenExpiresAt = a.clock().Add(time.Duration(reply.ExpiresIn) * time.Second)
		}
		session := cluster.Session
		session.AccountID, session.ClusterID = account, cluster.ID
		// Persist a rotated refresh token even when the subsequent provider
		// operation fails; otherwise another replica might reuse the old token.
		output, operationErr = run(session)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return output, operationErr
}

func (a *azureSetup) organizationPlan(ctx context.Context, options map[string]any) (azurePlan, error) {
	var cluster azureClusterConnection
	if found, _, err := a.store.read(ctx, azureClusterKey, &cluster); err != nil {
		return azurePlan{}, err
	} else if !found || cluster.Status != "connected" {
		return azurePlan{}, fmt.Errorf("connect Microsoft Azure for this cluster first")
	}
	input := map[string]any{}
	for key, value := range options {
		input[key] = value
	}
	for key, value := range map[string]any{"subscriptionId": cluster.Plan.SubscriptionID, "resourceGroup": cluster.Plan.ResourceGroup,
		"createResourceGroup": cluster.Plan.CreateResourceGroup, "resourceGroupLocation": cluster.Plan.ResourceGroupLocation, "dataLocation": cluster.Plan.DataLocation} {
		if requested, exists := input[key]; exists {
			matches := false
			switch expected := value.(type) {
			case string:
				actual, ok := requested.(string)
				matches = ok && actual == expected
			case bool:
				actual, ok := requested.(bool)
				matches = ok && actual == expected
			}
			if !matches {
				return azurePlan{}, fmt.Errorf("email resources must use the cluster's saved Azure settings")
			}
		}
		input[key] = value
	}
	plan, err := readAzurePlan(input)
	plan.ClusterID = cluster.ID
	return plan, err
}
