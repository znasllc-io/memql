package email

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

type azureSetup struct {
	store           *connectionStore
	protocol        *azureProtocol
	applicationID   func(context.Context) (string, error)
	canManage       func(context.Context, string) bool
	now             func() time.Time
	operationClient *http.Client
}

type azureSession struct {
	ID              string    `json:"id"`
	ClusterRevision string    `json:"clusterRevision"`
	ClientID        string    `json:"clientId"`
	OwnerID         string    `json:"ownerId"`
	AccountID       string    `json:"accountId"`
	ClusterID       string    `json:"clusterId,omitempty"`
	Tenant          string    `json:"tenant"`
	Status          string    `json:"status"`
	DeviceCode      string    `json:"deviceCode,omitempty"`
	UserCode        string    `json:"userCode,omitempty"`
	VerificationURI string    `json:"verificationUri,omitempty"`
	ExpiresAt       time.Time `json:"expiresAt"`
	NextPollAt      time.Time `json:"nextPollAt"`
	Interval        int       `json:"interval"`
	AccessToken     string    `json:"accessToken,omitempty"`
	RefreshToken    string    `json:"refreshToken,omitempty"`
	TokenExpiresAt  time.Time `json:"tokenExpiresAt"`
}

func (a *azureSetup) clock() time.Time {
	if a.now != nil {
		return a.now().UTC()
	}
	return time.Now().UTC()
}

func (i *Integration) handleAzureSetup(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := configureAuthorized(ctx); err != nil {
		return nil, err
	}
	ac, _ := auth.AccessFromContext(ctx)
	if ac.Synthetic || strings.TrimSpace(ac.UserId) == "" {
		return nil, fmt.Errorf("Azure setup requires a signed-in owner or developer")
	}
	a := i.azure
	if a == nil || a.store == nil || a.protocol == nil || a.canManage == nil {
		return nil, fmt.Errorf("Azure email setup is unavailable on this node")
	}
	account := memql.BareShortId(argString(args, "accountId"))
	if account == "" || !a.canManage(ctx, account) {
		return nil, fmt.Errorf("choose an organization you can manage")
	}
	q := strings.Replace(renderConfigCall("clientAccountById", map[string]string{"accountId": account}), "mutation ", "query ", 1)
	result, err := a.store.engine.Execute(memql.ContextWithFreshRead(ctx), q)
	if err != nil {
		return nil, fmt.Errorf("could not read the selected organization")
	}
	rows := memql.MaterializeRows(result)
	if len(rows) != 1 || rows[0]["status"] != "active" {
		return nil, fmt.Errorf("the organization is inactive or inaccessible")
	}
	action := argString(args, "action")
	options, _ := args["options"].(map[string]any)
	if action == "clusterStatus" || action == "disconnectCluster" {
		if account != "self" {
			return nil, fmt.Errorf("Azure account configuration belongs to the cluster")
		}
		output, err := a.clusterAction(ctx, action, options)
		if err != nil {
			return nil, err
		}
		_, capture := captureSender(i.sender, ctx)
		output["capture"] = capture
		return configureResult(output)
	}
	if action == "status" || action == "disconnect" || action == "prepare" || action == "operations" {
		output, err := a.connectionAction(ctx, account, action, options)
		if err != nil {
			return nil, err
		}
		return configureResult(output)
	}
	if action == "provision" || action == "verify" || action == "domainStatus" || action == "sender" {
		output, err := a.withCluster(ctx, account, func(session azureSession) (map[string]any, error) {
			return a.resourceAction(ctx, session, action, options)
		})
		if err != nil {
			return nil, err
		}
		return configureResult(output)
	}
	if account != "self" {
		return nil, fmt.Errorf("sign in to Microsoft once for the cluster, not for each organization")
	}
	if action == "begin" {
		var cluster azureClusterConnection
		if _, _, err := a.store.read(ctx, azureClusterKey, &cluster); err != nil {
			return nil, err
		}
		protocol := a.protocol
		if a.applicationID != nil {
			clientID, err := a.applicationID(ctx)
			if err != nil {
				return nil, err
			}
			protocol = &azureProtocol{clientID: clientID, client: a.protocol.client}
		}
		tenant := argString(options, "tenantId")
		if tenant == "" {
			tenant = "organizations"
		}
		grant, err := protocol.begin(ctx, tenant)
		if err != nil {
			return nil, err
		}
		sessionID := id.NewShortId()
		session := azureSession{}
		err = a.store.change(ctx, "azure-session:"+sessionID, &session, func(found bool) error {
			if found {
				return fmt.Errorf("could not allocate an Azure setup session")
			}
			session = azureSession{ID: sessionID, ClusterRevision: cluster.Revision, ClientID: protocol.clientID, OwnerID: memql.BareShortId(ac.UserId), AccountID: account, Tenant: tenant, Status: "waiting", DeviceCode: grant.DeviceCode, UserCode: grant.UserCode, VerificationURI: grant.VerificationURI, Interval: grant.Interval, ExpiresAt: a.clock().Add(time.Duration(grant.ExpiresIn) * time.Second), NextPollAt: a.clock().Add(time.Duration(grant.Interval) * time.Second)}
			return nil
		})
		if err != nil {
			return nil, err
		}
		return configureResult(map[string]any{"sessionId": sessionID, "status": "waiting", "userCode": session.UserCode, "verificationUri": session.VerificationURI, "expiresAt": session.ExpiresAt, "interval": session.Interval})
	}
	if action != "poll" && action != "subscriptions" && action != "directories" && action != "resourceGroups" && action != "locations" && action != "cancel" && action != "saveCluster" {
		return nil, fmt.Errorf("unknown Azure setup action")
	}
	sessionID := argString(args, "sessionId")
	if !azureUUID.MatchString(sessionID) {
		return nil, fmt.Errorf("start a Microsoft sign-in first")
	}
	var session azureSession
	var output map[string]any
	err = a.store.change(ctx, "azure-session:"+sessionID, &session, func(found bool) error {
		if !found || session.OwnerID != memql.BareShortId(ac.UserId) || session.AccountID != account {
			return fmt.Errorf("this Azure setup belongs to another user or organization")
		}
		protocol := &azureProtocol{clientID: session.ClientID, client: a.protocol.client}
		if action == "cancel" || !a.clock().Before(session.ExpiresAt) {
			session.Status = "expired"
			session.AccessToken = ""
			session.RefreshToken = ""
			session.DeviceCode = ""
			session.UserCode = ""
			output = map[string]any{"status": "expired"}
			return nil
		}
		if action == "poll" {
			if session.Status == "waiting" && !a.clock().Before(session.NextPollAt) {
				reply, err := protocol.token(ctx, session.Tenant, url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "device_code": {session.DeviceCode}})
				if err != nil {
					return err
				}
				switch reply.Error {
				case "authorization_pending":
				case "slow_down":
					session.Interval += 5
					if session.Interval > 60 {
						session.Interval = 60
					}
				case "":
					session.Status = "connected"
					session.AccessToken = reply.AccessToken
					session.RefreshToken = reply.RefreshToken
					session.TokenExpiresAt = a.clock().Add(time.Duration(reply.ExpiresIn) * time.Second)
					session.ExpiresAt = a.clock().Add(time.Hour)
					session.DeviceCode = ""
					session.UserCode = ""
				default:
					session.Status = "expired"
					session.DeviceCode = ""
					session.UserCode = ""
				}
				session.NextPollAt = a.clock().Add(time.Duration(session.Interval) * time.Second)
			}
			output = map[string]any{"status": session.Status, "interval": session.Interval, "expiresAt": session.ExpiresAt}
			if session.Status == "waiting" {
				output["userCode"] = session.UserCode
				output["verificationUri"] = session.VerificationURI
			}
			return nil
		}
		if session.Status != "connected" {
			return fmt.Errorf("finish Microsoft sign-in before selecting Azure resources")
		}
		if !a.clock().Add(time.Minute).Before(session.TokenExpiresAt) {
			if session.RefreshToken == "" {
				return fmt.Errorf("your Microsoft session expired; connect again")
			}
			reply, err := protocol.token(ctx, session.Tenant, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {session.RefreshToken}, "scope": {azureManagementScope + " offline_access"}})
			if err != nil {
				return err
			}
			if reply.Error != "" {
				return fmt.Errorf("your Microsoft session expired; connect again")
			}
			session.AccessToken = reply.AccessToken
			if reply.RefreshToken != "" {
				session.RefreshToken = reply.RefreshToken
			}
			session.TokenExpiresAt = a.clock().Add(time.Duration(reply.ExpiresIn) * time.Second)
		}
		if action == "saveCluster" {
			var err error
			output, err = a.saveCluster(ctx, session, options)
			if err == nil {
				session.Status = "saved"
				session.AccessToken, session.RefreshToken = "", ""
			}
			return err
		}
		path := "/subscriptions?api-version=2022-12-01"
		if action == "directories" {
			path = "/tenants?api-version=2022-12-01"
		}
		if action == "resourceGroups" || action == "locations" {
			subscription := argString(options, "subscriptionId")
			if !azureUUID.MatchString(subscription) {
				return fmt.Errorf("choose an Azure subscription")
			}
			path = "/subscriptions/" + subscription + "/resourcegroups?api-version=2021-04-01"
			if action == "locations" {
				path = "/subscriptions/" + subscription + "/locations?api-version=2022-12-01"
			}
		}
		resources, err := a.protocol.list(ctx, path, session.AccessToken)
		if err != nil {
			return err
		}
		choices := make([]map[string]any, 0, len(resources))
		for _, resource := range resources {
			if action == "subscriptions" {
				if resource["state"] != "Enabled" {
					continue
				}
				choices = append(choices, map[string]any{"id": resource["subscriptionId"], "name": resource["displayName"], "tenantId": resource["tenantId"]})
			} else if action == "directories" {
				choices = append(choices, map[string]any{"id": resource["tenantId"], "name": resource["displayName"]})
			} else if action == "locations" {
				choices = append(choices, map[string]any{"id": resource["name"], "name": resource["displayName"]})
			} else {
				choices = append(choices, map[string]any{"id": resource["id"], "name": resource["name"], "location": resource["location"]})
			}
		}
		output = map[string]any{"status": "connected", "choices": choices}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return configureResult(output)
}
