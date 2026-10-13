package email

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/znasllc-io/memql/core/emailconfig"
)

const emailInstallationEnv = "MEMQL_EMAIL_INSTALLATION"
const azureAssertionPath = "/var/run/secrets/azure-email/token"

// Installation choices contain only public identifiers. The operator's CLI
// authorization stays on the operator's machine. Each cluster exchanges its
// own projected service-account assertion, reread for every token rotation.
type emailInstallation struct {
	TenantID             string `json:"tenantId"`
	ClientID             string `json:"clientId"`
	SubscriptionID       string `json:"subscriptionId"`
	ResourceGroup        string `json:"resourceGroup"`
	DataLocation         string `json:"dataLocation"`
	EmailService         string `json:"emailService"`
	CommunicationService string `json:"communicationService"`
	ReplyTo              string `json:"replyTo,omitempty"`
}

func readEmailInstallation(raw string) (*emailInstallation, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	if len(raw) > 8192 {
		return nil, fmt.Errorf("email installation configuration is too large")
	}
	var c emailInstallation
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return nil, fmt.Errorf("email installation configuration is invalid")
	}
	var extra any
	if d.Decode(&extra) != io.EOF || !azureUUID.MatchString(c.TenantID) || !azureUUID.MatchString(c.ClientID) {
		return nil, fmt.Errorf("email installation requires one configuration with valid tenant and client identifiers")
	}
	check := emailconfig.Campaigns{Azure: emailconfig.Azure{TenantID: c.TenantID, SubscriptionID: c.SubscriptionID, ResourceGroup: c.ResourceGroup, DataLocation: c.DataLocation}, Domains: []emailconfig.Domain{{Organization: "self", Domain: "bootstrap.invalid", EmailService: c.EmailService, CommunicationService: c.CommunicationService, Sender: &emailconfig.Sender{Username: "donotreply", DisplayName: "DoNotReply", ReplyTo: c.ReplyTo}}}}
	if err := check.Validate(); err != nil {
		return nil, fmt.Errorf("email installation choices are invalid: %w", err)
	}
	return &c, nil
}

func (c emailInstallation) clusterPlan() azurePlan {
	return azurePlan{SubscriptionID: c.SubscriptionID, ResourceGroup: c.ResourceGroup, DataLocation: c.DataLocation}
}

func (a *azureSetup) workloadSession(ctx context.Context, cluster azureClusterConnection) (azureSession, error) {
	c := a.installation
	if c == nil || cluster.WorkloadClientID != c.ClientID || cluster.TenantID != c.TenantID || cluster.Plan != c.clusterPlan() {
		return azureSession{}, fmt.Errorf("this node's email workload identity does not match the cluster configuration")
	}
	read := a.readAssertion
	if read == nil {
		read = func(path string) ([]byte, error) {
			f, err := os.Open(path)
			if err != nil {
				return nil, err
			}
			defer f.Close()
			return io.ReadAll(io.LimitReader(f, 16385))
		}
	}
	raw, err := read(azureAssertionPath)
	if err != nil || len(raw) == 0 || len(raw) > 16384 {
		return azureSession{}, fmt.Errorf("this node cannot read its projected Azure email identity")
	}
	assertion := strings.TrimSpace(string(raw))
	if len(strings.Split(assertion, ".")) != 3 || strings.ContainsAny(assertion, "\r\n \t") {
		return azureSession{}, fmt.Errorf("this node's projected Azure email identity is invalid")
	}
	p := &azureProtocol{clientID: c.ClientID, client: a.protocol.client}
	reply, err := p.token(ctx, c.TenantID, url.Values{"grant_type": {"client_credentials"}, "scope": {"https://management.azure.com/.default"}, "client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"}, "client_assertion": {assertion}})
	if err != nil {
		return azureSession{}, err
	}
	if reply.Error != "" {
		return azureSession{}, fmt.Errorf("Azure refused this cluster's email workload identity; check its federation and resource-group grant")
	}
	return azureSession{ClientID: c.ClientID, Tenant: c.TenantID, WorkloadClientID: c.ClientID, AccessToken: reply.AccessToken, ClusterID: cluster.ID}, nil
}
