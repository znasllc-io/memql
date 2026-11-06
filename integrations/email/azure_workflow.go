package email

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/emailconfig"
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

// summary is the connection's summary as a page listing its senders reads it:
// with where replies to each sender go, when the sender records can be read.
func (a *azureSetup) summary(ctx context.Context, connection azureConnection) map[string]any {
	summary := azureConnectionSummary(connection)
	if replies := a.senderReplies(ctx, connection); replies != nil {
		summary["replies"] = replies
	}
	return summary
}

// Organization status, disconnect and the choice of default sender require
// MemQL permission but no live Microsoft session. Disconnect drops the active
// credential; it never deletes Azure resources, changes DNS, or cancels
// billing on the user's behalf. Make default only chooses among the senders
// Azure has already registered, so it asks Azure nothing; the sender's own
// Reply-To comes from its Campaigns record.
func (a *azureSetup) connectionAction(ctx context.Context, account, action string, options map[string]any) (map[string]any, error) {
	var connection azureConnection
	if action == "makeDefault" {
		if err := a.store.change(ctx, "organization:"+account, &connection, func(found bool) error {
			if !found || connection.AccountID != account {
				return fmt.Errorf("finish this organization's sending domain before choosing its default sender")
			}
			address, err := connection.defaultCandidate(argString(options, "sender"))
			if err != nil {
				return err
			}
			records, err := a.senderRecords(ctx, account)
			if err != nil {
				return fmt.Errorf("could not read this sender's reply address; try again")
			}
			identityID, replyTo := azureSenderIdentityID(account, address), ""
			if record := azureSenderRecord(records, account, address); record != nil {
				identityID, replyTo = memql.BareShortId(argString(record, "id")), strings.TrimSpace(argString(record, "replyTo"))
			}
			return connection.makeDefault(address, replyTo, identityID)
		}); err != nil {
			return nil, err
		}
		return a.summary(ctx, connection), nil
	}
	if action == "operations" {
		rows, err := (&storedACSOperations{connection: a.store}).recent(ctx, account)
		if err != nil {
			return nil, err
		}
		status, err := a.feedbackStatus(ctx, account)
		return map[string]any{"status": "ok", "operations": rows, "deliveryFeedback": status}, err
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
		summary := map[string]any{"status": "unconfigured"}
		if found {
			summary = a.summary(ctx, connection)
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
		if _, provided := options["username"]; provided {
			requested := &emailconfig.Sender{Username: argString(options, "username"), DisplayName: argString(options, "displayName"), ReplyTo: argString(options, "replyTo")}
			checked := emailconfig.Campaigns{Azure: emailconfig.Azure{SubscriptionID: plan.SubscriptionID, ResourceGroup: plan.ResourceGroup, CreateResourceGroup: plan.CreateResourceGroup, ResourceGroupLocation: plan.ResourceGroupLocation, DataLocation: plan.DataLocation}, Domains: []emailconfig.Domain{{Organization: account, Domain: plan.Domain, EmailService: plan.EmailService, CommunicationService: plan.CommunicationService, Sender: requested}}}
			if err := checked.Validate(); err != nil {
				return err
			}
			connection.RequestedSender = requested
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
			// One Azure create or update per call keeps the request short enough
			// to outlast a proxy's idle timeout. The saved plan and Azure's own
			// state carry progress; the next check does the next step.
			writes := &azureWrites{left: 1}
			ready, err := a.protocol.ensureSubscription(ctx, session.AccessToken, session.ClusterID, plan, writes, session.WorkloadClientID != "")
			if err != nil {
				return err
			}
			if !ready {
				connection.Status = "provisioning"
				output = azureConnectionSummary(connection)
				return nil
			}
			for _, path := range []string{plan.emailPath(), plan.communicationPath()} {
				ready, err := a.protocol.ensureService(ctx, session.AccessToken, path, session.AccountID, plan.DataLocation, session.ClusterID, writes)
				if err != nil {
					return err
				}
				if !ready {
					connection.Status = "provisioning"
					output = azureConnectionSummary(connection)
					return nil
				}
			}
			resource, err := a.protocol.domain(ctx, session.AccessToken, plan, true, writes)
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
			resource, err := a.protocol.domain(ctx, session.AccessToken, plan, false, nil)
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
				resource, err = a.protocol.domain(ctx, session.AccessToken, plan, false, nil)
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
			// Adding a sender keeps the default unless the caller asks to move it.
			makeDefault, _ := options["makeDefault"].(bool)
			cfg, added, err := a.protocol.finish(ctx, session.AccessToken, session.AccountID, plan, connection.Config, options, makeDefault)
			if err != nil {
				return err
			}
			identityID, err := a.saveSenderIdentity(ctx, session.AccountID, added)
			if err != nil {
				return err
			}
			connection.Status = "ready"
			connection.Config = cfg
			connection.RequestedSender = nil
			connection.RequestedDNS = nil
			connection.ReplyTo = cfg.ReplyTo
			if cfg.Default == added.Address {
				connection.SenderIdentityID = identityID
			}
			output = a.summary(ctx, connection)
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

// defaultCandidate is the address a sender becomes the default under: one the
// domain has registered with Azure, on a domain that is sending.
func (c *azureConnection) defaultCandidate(address string) (string, error) {
	if c.Status != "ready" {
		return "", fmt.Errorf("finish this organization's sending domain before choosing its default sender")
	}
	address = strings.ToLower(strings.TrimSpace(address))
	if _, registered := c.Config.Senders[address]; !registered {
		return "", fmt.Errorf("choose a sender this domain has registered with Azure")
	}
	return address, nil
}

// makeDefault moves the connection's default, the address mail that names
// none goes out as, to a sender the domain has registered with Azure, and the
// connection's Reply-To with it: that sender's own (replyTo, "" for none), as
// adding a sender as the default does. identityID is the sender's Campaigns
// record. Nothing else changes -- not the senders or their names, not the
// credential -- and nothing in it needs Azure, so it needs no Microsoft session.
func (c *azureConnection) makeDefault(address, replyTo, identityID string) error {
	address, err := c.defaultCandidate(address)
	if err != nil {
		return err
	}
	c.Config.Default = address
	c.Config.ReplyTo, c.ReplyTo = replyTo, replyTo
	c.SenderIdentityID = identityID
	return nil
}
