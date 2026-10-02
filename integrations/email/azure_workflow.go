package email

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

func azurePlanID(connection azureConnection) string {
	data, _ := json.Marshal(connection.Plan)
	return emailStateID("plan:" + connection.AccountID + ":" + string(data))
}

func azureConnectionSummary(connection azureConnection) map[string]any {
	return map[string]any{"status": connection.Status, "plan": connection.Plan, "planId": azurePlanID(connection), "sender": connection.Config.Default, "senders": connection.Config.Senders, "replyTo": connection.ReplyTo, "senderIdentityId": connection.SenderIdentityID, "requestedSender": connection.RequestedSender}
}

// Organization status and disconnect require MemQL permission but no live
// Microsoft session. Disconnect drops the active credential; it never deletes
// Azure resources, changes DNS, or cancels billing on the user's behalf.
func (a *azureSetup) connectionAction(ctx context.Context, account, action string, options map[string]any) (map[string]any, error) {
	var connection azureConnection
	if action == "operations" {
		rows, err := (&storedACSOperations{connection: a.store}).recent(ctx, account)
		if err != nil {
			return nil, err
		}
		return map[string]any{"status": "ok", "operations": rows, "deliveryFeedback": "not-configured"}, nil
	}
	if action == "status" {
		found, _, err := a.store.read(ctx, "organization:"+account, &connection)
		if err != nil {
			return nil, err
		}
		ready := a.protocol != nil && a.protocol.clientID != ""
		if a.applicationID != nil {
			clientID, appErr := a.applicationID(ctx)
			if appErr != nil {
				return nil, appErr
			}
			ready = clientID != ""
		}
		summary := azureConnectionSummary(connection)
		if !found {
			summary = map[string]any{"status": "unconfigured"}
		}
		summary["applicationReady"] = ready
		return summary, nil
	}
	err := a.store.change(ctx, "organization:"+account, &connection, func(found bool) error {
		if action == "disconnect" {
			connection.Status = "disconnected"
			connection.AccountID = account
			connection.Config = ACSConfig{}
			return nil
		}
		plan, err := a.organizationPlan(ctx, options)
		if err != nil {
			return err
		}
		if found && connection.Status != "disconnected" && connection.Status != "draft" && connection.Plan != plan {
			return fmt.Errorf("disconnect the existing email connection before choosing different resources")
		}
		if found && connection.Plan == plan && connection.Status != "disconnected" && connection.Status != "draft" {
			return nil
		}
		connection = azureConnection{Status: "planned", AccountID: account, Plan: plan, RequestedSender: connection.RequestedSender, RequestedDNS: connection.RequestedDNS}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return azureConnectionSummary(connection), nil
}

func (a *azureSetup) resourceAction(ctx context.Context, session azureSession, action string, options map[string]any) (map[string]any, error) {
	var connection azureConnection
	var output map[string]any
	err := a.store.change(ctx, "organization:"+session.AccountID, &connection, func(found bool) error {
		if !found || connection.AccountID != session.AccountID || connection.Status == "disconnected" {
			return fmt.Errorf("choose the organization's email resources first")
		}
		plan := connection.Plan
		if plan.ClusterID == "" || plan.ClusterID != session.ClusterID {
			return fmt.Errorf("this domain's Azure settings differ from the cluster connection; disconnect the domain and configure it again")
		}
		if action != "domainStatus" && argString(options, "planId") != azurePlanID(connection) {
			return fmt.Errorf("the email resource plan changed; reopen the connection and review it before continuing")
		}
		if action == "provision" {
			confirmed, _ := options["confirmed"].(bool)
			if !confirmed {
				return fmt.Errorf("review and confirm the Azure resource plan before creating it")
			}
			if err := a.claimResources(ctx, session.AccountID, plan); err != nil {
				return err
			}
			ready, err := a.protocol.ensureSubscription(ctx, session.AccessToken, session.ClusterID, plan)
			if err != nil {
				return err
			}
			if !ready {
				connection.Status = "provisioning"
				output = azureConnectionSummary(connection)
				return nil
			}
			for _, path := range []string{plan.emailPath(), plan.communicationPath()} {
				ready, err := a.protocol.ensureService(ctx, session.AccessToken, path, session.AccountID, plan.DataLocation, session.ClusterID)
				if err != nil {
					return err
				}
				if !ready {
					connection.Status = "provisioning"
					output = azureConnectionSummary(connection)
					return nil
				}
			}
			resource, err := a.protocol.domain(ctx, session.AccessToken, plan, true)
			if err != nil {
				return err
			}
			ready, err = azureResourceReady(resource.Properties.ProvisioningState)
			if err != nil {
				return err
			}
			if !ready {
				connection.Status = "provisioning"
				output = azureConnectionSummary(connection)
				return nil
			}
			output = azureDomainSummary(resource)
			connection.Status = output["status"].(string)
			output["plan"] = plan
			return nil
		}
		if action == "domainStatus" || action == "verify" {
			resource, err := a.protocol.domain(ctx, session.AccessToken, plan, false)
			if err != nil {
				return err
			}
			if action == "verify" {
				for _, kind := range []string{"Domain", "SPF", "DKIM", "DKIM2"} {
					status := resource.Properties.VerificationStates[kind].Status
					if status == "Verified" || status == "VerificationInProgress" {
						continue
					}
					if err = a.protocol.arm(ctx, http.MethodPost, azureVersion(plan.domainPath()+"/initiateVerification"), session.AccessToken, map[string]string{"verificationType": kind}, nil); err != nil {
						return err
					}
				}
				resource, err = a.protocol.domain(ctx, session.AccessToken, plan, false)
				if err != nil {
					return err
				}
			}
			output = azureDomainSummary(resource)
			if connection.Status != "ready" {
				connection.Status = output["status"].(string)
			}
			output["plan"] = plan
			return nil
		}
		if action == "sender" {
			if err := a.claimResources(ctx, session.AccountID, plan); err != nil {
				return err
			}
			cfg, replyTo, err := a.protocol.finish(ctx, session.AccessToken, session.AccountID, plan, options)
			if err != nil {
				return err
			}
			connection.Status = "ready"
			connection.Config = cfg
			connection.RequestedSender = nil
			connection.RequestedDNS = nil
			connection.ReplyTo = replyTo
			identityID, err := a.saveSenderIdentity(ctx, connection)
			if err != nil {
				return err
			}
			connection.SenderIdentityID = identityID
			output = azureConnectionSummary(connection)
			return nil
		}
		return fmt.Errorf("unknown Azure resource action")
	})
	if err == nil && output != nil {
		output["planId"] = azurePlanID(connection)
	}
	return output, err
}

// Azure may return 404 briefly after accepting a create. Persist the claim
// before that request, so another organization on another replica cannot
// overwrite the first create during that visibility window. Claims survive a
// disconnect: disconnecting does not delete or transfer the Azure resources.
func (a *azureSetup) claimResources(ctx context.Context, account string, plan azurePlan) error {
	for _, path := range []string{plan.emailPath(), plan.communicationPath()} {
		decoded, err := url.PathUnescape(path)
		if err != nil {
			return fmt.Errorf("invalid Azure resource path")
		}
		var claim struct {
			AccountID string `json:"accountId"`
		}
		if err := a.store.change(ctx, "azure-resource:"+strings.ToLower(decoded), &claim, func(found bool) error {
			if found && claim.AccountID != account {
				return fmt.Errorf("this Azure service name is reserved by another organization's connection; choose another name")
			}
			claim.AccountID = account
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}
