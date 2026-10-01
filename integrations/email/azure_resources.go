package email

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
)

// A plan is saved before any Azure mutation. Repeated provisioning calls use
// the same named resources, so a dropped response cannot create another set.
type azurePlan struct {
	CreateResourceGroup   bool   `json:"createResourceGroup"`
	ResourceGroupLocation string `json:"resourceGroupLocation"`
	SubscriptionID        string `json:"subscriptionId"`
	ResourceGroup         string `json:"resourceGroup"`
	EmailService          string `json:"emailService"`
	CommunicationService  string `json:"communicationService"`
	Domain                string `json:"domain"`
	DataLocation          string `json:"dataLocation"`
}

var azureEmailServiceName = regexp.MustCompile(`^[a-zA-Z0-9-]{1,63}$`)
var azureGroupName = regexp.MustCompile(`^[a-zA-Z0-9_(). -]{1,90}$`)
var azureDomainLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
var azureSenderName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)

func readAzurePlan(options map[string]any) (azurePlan, error) {
	p := azurePlan{SubscriptionID: argString(options, "subscriptionId"), ResourceGroup: argString(options, "resourceGroup"), EmailService: argString(options, "emailService"), CommunicationService: argString(options, "communicationService"), Domain: strings.ToLower(argString(options, "domain")), DataLocation: argString(options, "dataLocation")}
	p.CreateResourceGroup, _ = options["createResourceGroup"].(bool)
	p.ResourceGroupLocation = argString(options, "resourceGroupLocation")
	if p.CreateResourceGroup && !regexp.MustCompile(`^[a-z][a-z0-9]{1,39}$`).MatchString(p.ResourceGroupLocation) {
		return p, fmt.Errorf("choose a region for the resource group")
	}
	if !p.CreateResourceGroup {
		p.ResourceGroupLocation = ""
	}
	if !azureUUID.MatchString(p.SubscriptionID) || !azureGroupName.MatchString(p.ResourceGroup) || strings.HasSuffix(p.ResourceGroup, ".") || p.ResourceGroup != strings.TrimSpace(p.ResourceGroup) {
		return p, fmt.Errorf("choose a subscription and resource group")
	}
	if !azureEmailServiceName.MatchString(p.EmailService) || !azureResourceName.MatchString(p.CommunicationService) {
		return p, fmt.Errorf("service names must contain 1–63 letters, digits, or hyphens")
	}
	labels := strings.Split(p.Domain, ".")
	if len(labels) < 2 || len(p.Domain) > 253 || net.ParseIP(p.Domain) != nil {
		return p, fmt.Errorf("enter the organization's email domain")
	}
	for _, label := range labels {
		if !azureDomainLabel.MatchString(label) {
			return p, fmt.Errorf("enter a valid email domain using its ASCII name")
		}
	}
	// These are the Email service's supported data locations, not VM regions.
	switch p.DataLocation {
	case "United States", "Europe", "United Kingdom", "Australia", "Asia Pacific", "Brazil", "Canada", "France", "Germany", "India", "Japan", "Korea", "Norway", "Switzerland", "United Arab Emirates", "Africa":
	default:
		return p, fmt.Errorf("choose an Azure email data location")
	}
	return p, nil
}

func (p azurePlan) base() string {
	return "/subscriptions/" + p.SubscriptionID + "/resourceGroups/" + url.PathEscape(p.ResourceGroup) + "/providers/Microsoft.Communication"
}
func (p azurePlan) emailPath() string { return p.base() + "/emailServices/" + p.EmailService }
func (p azurePlan) communicationPath() string {
	return p.base() + "/communicationServices/" + p.CommunicationService
}
func (p azurePlan) domainPath() string { return p.emailPath() + "/domains/" + p.Domain }
func azureVersion(path string) string {
	return path + "?api-version=" + azureCommunicationManagementVersion
}

type azureResource struct {
	ID         string            `json:"id"`
	Tags       map[string]string `json:"tags"`
	Properties struct {
		ProvisioningState   string                    `json:"provisioningState"`
		DataLocation        string                    `json:"dataLocation"`
		DomainManagement    string                    `json:"domainManagement"`
		HostName            string                    `json:"hostName"`
		LinkedDomains       []string                  `json:"linkedDomains"`
		DisableLocalAuth    bool                      `json:"disableLocalAuth"`
		VerificationRecords map[string]azureDNSRecord `json:"verificationRecords"`
		VerificationStates  map[string]struct {
			Status string `json:"status"`
		} `json:"verificationStates"`
		Username    string `json:"username"`
		DisplayName string `json:"displayName"`
	} `json:"properties"`
}
type azureDNSRecord struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Value string `json:"value"`
	TTL   int    `json:"ttl"`
}

func azureNotFound(err error) bool {
	var api *azureAPIError
	return errors.As(err, &api) && api.Status == http.StatusNotFound
}

// Existing resources are reused only when this connection created them. A
// matching human-readable name alone is never permission to overwrite one.
func (a *azureProtocol) ensureService(ctx context.Context, token, path, account, location string) (bool, error) {
	var resource azureResource
	err := a.arm(ctx, http.MethodGet, azureVersion(path), token, nil, &resource)
	if azureNotFound(err) {
		body := map[string]any{"location": "Global", "tags": map[string]string{"memql-organization": account}, "properties": map[string]any{"dataLocation": location}}
		if err = a.arm(ctx, http.MethodPut, azureVersion(path), token, body, &resource); err != nil {
			return false, err
		}
		return false, nil // observe completion with a separate GET
	}
	if err != nil {
		return false, err
	}
	decodedPath, decodeErr := url.PathUnescape(path)
	if decodeErr != nil || resource.Tags["memql-organization"] != account || !strings.EqualFold(resource.ID, decodedPath) {
		return false, fmt.Errorf("this Azure service already exists outside this organization's connection; choose another name")
	}
	if resource.Properties.DataLocation != location {
		return false, fmt.Errorf("this Azure service uses a different data location; choose another name")
	}
	return azureResourceReady(resource.Properties.ProvisioningState)
}

func azureResourceReady(status string) (bool, error) {
	switch status {
	case "Succeeded":
		return true, nil
	case "Failed", "Canceled", "Deleting":
		return false, fmt.Errorf("Azure could not provision this resource; inspect it in Azure before continuing")
	default:
		return false, nil
	}
}

func (a *azureProtocol) domain(ctx context.Context, token string, plan azurePlan, create bool) (azureResource, error) {
	var resource azureResource
	err := a.arm(ctx, http.MethodGet, azureVersion(plan.domainPath()), token, nil, &resource)
	if azureNotFound(err) && create {
		body := map[string]any{"location": "Global", "properties": map[string]any{"domainManagement": "CustomerManaged", "userEngagementTracking": "Disabled"}}
		err = a.arm(ctx, http.MethodPut, azureVersion(plan.domainPath()), token, body, &resource)
	}
	if err != nil {
		return resource, err
	}
	if resource.Properties.DomainManagement != "CustomerManaged" {
		return resource, fmt.Errorf("choose a customer-owned email domain")
	}
	return resource, nil
}

func azureDomainVerified(resource azureResource) bool {
	if resource.Properties.ProvisioningState != "Succeeded" {
		return false
	}
	for _, kind := range []string{"Domain", "SPF", "DKIM", "DKIM2"} {
		if resource.Properties.VerificationStates[kind].Status != "Verified" {
			return false
		}
	}
	return true
}

func azureDomainSummary(resource azureResource) map[string]any {
	records := make([]map[string]any, 0, 4)
	for _, kind := range []string{"Domain", "SPF", "DKIM", "DKIM2"} {
		record, ok := resource.Properties.VerificationRecords[kind]
		if ok {
			records = append(records, map[string]any{"purpose": kind, "name": record.Name, "type": record.Type, "value": record.Value, "ttl": record.TTL, "status": resource.Properties.VerificationStates[kind].Status})
		}
	}
	status := "dns"
	if azureDomainVerified(resource) {
		status = "verified"
	}
	return map[string]any{"status": status, "records": records}
}

// finish never changes an existing sender's name implicitly. Domain linking
// preserves every existing link and modifies no auth or network properties.
func (a *azureProtocol) finish(ctx context.Context, token, account string, plan azurePlan, options map[string]any) (ACSConfig, string, error) {
	var empty ACSConfig
	username, name, replyTo := argString(options, "username"), argString(options, "displayName"), argString(options, "replyTo")
	if !azureSenderName.MatchString(username) || len(name) > 100 || strings.TrimSpace(name) == "" || headerUnsafe(name) {
		return empty, "", fmt.Errorf("enter a sender name and display name")
	}
	if replyTo != "" {
		mailbox, err := mail.ParseAddress(replyTo)
		if err != nil || mailbox.Address != replyTo || headerUnsafe(replyTo) {
			return empty, "", fmt.Errorf("Reply-To must be an existing mailbox address")
		}
	}
	for _, path := range []string{plan.emailPath(), plan.communicationPath()} {
		ready, err := a.ensureService(ctx, token, path, account, plan.DataLocation)
		if err != nil {
			return empty, "", err
		}
		if !ready {
			return empty, "", fmt.Errorf("Azure is still preparing the email services")
		}
	}
	domain, err := a.domain(ctx, token, plan, false)
	if err != nil {
		return empty, "", err
	}
	if !azureDomainVerified(domain) {
		return empty, "", fmt.Errorf("verify all domain records before adding a sender")
	}
	// ARM resource identifiers in JSON are not escaped HTTP request paths.
	// A resource group containing a space must link the actual domain ID.
	domainID, err := url.PathUnescape(plan.domainPath())
	if err != nil {
		return empty, "", fmt.Errorf("invalid Azure domain identifier")
	}
	path := plan.domainPath() + "/senderUsernames/" + username
	var sender azureResource
	err = a.arm(ctx, http.MethodGet, azureVersion(path), token, nil, &sender)
	if azureNotFound(err) {
		err = a.arm(ctx, http.MethodPut, azureVersion(path), token, map[string]any{"properties": map[string]string{"username": username, "displayName": name}}, &sender)
	}
	if err != nil {
		return empty, "", err
	}
	if sender.Properties.Username != username || sender.Properties.DisplayName != name {
		return empty, "", fmt.Errorf("this sender already has a different display name; choose another sender name")
	}
	var service azureResource
	if err = a.arm(ctx, http.MethodGet, azureVersion(plan.communicationPath()), token, nil, &service); err != nil {
		return empty, "", err
	}
	if service.Tags["memql-organization"] != account || service.Properties.DisableLocalAuth {
		return empty, "", fmt.Errorf("this Azure service cannot provide this organization's email connection")
	}
	linked := false
	for _, entry := range service.Properties.LinkedDomains {
		if strings.EqualFold(entry, domainID) {
			linked = true
		}
	}
	if !linked {
		links := append(service.Properties.LinkedDomains, domainID)
		if err = a.arm(ctx, http.MethodPatch, azureVersion(plan.communicationPath()), token, map[string]any{"properties": map[string]any{"linkedDomains": links}}, nil); err != nil {
			return empty, "", err
		}
		// Linking can still be asynchronous. Read it back before enabling mail.
		if err = a.arm(ctx, http.MethodGet, azureVersion(plan.communicationPath()), token, nil, &service); err != nil {
			return empty, "", err
		}
		for _, entry := range service.Properties.LinkedDomains {
			if strings.EqualFold(entry, domainID) {
				linked = true
			}
		}
	}
	ready, err := azureResourceReady(service.Properties.ProvisioningState)
	if err != nil {
		return empty, "", err
	}
	if !linked || !ready {
		return empty, "", fmt.Errorf("Azure is still connecting this domain; check again shortly")
	}
	var keys struct {
		PrimaryKey string `json:"primaryKey"`
	}
	if err = a.arm(ctx, http.MethodPost, azureVersion(plan.communicationPath()+"/listKeys"), token, nil, &keys); err != nil {
		return empty, "", err
	}
	address := strings.ToLower(username + "@" + plan.Domain)
	cfg := ACSConfig{AccountID: account, Endpoint: "https://" + service.Properties.HostName, AccessKey: keys.PrimaryKey, Senders: map[string]string{address: name}, Default: address, ReplyTo: replyTo}
	usernames, err := a.list(ctx, azureVersion(plan.domainPath()+"/senderUsernames"), token)
	if err != nil {
		return empty, "", err
	}
	for _, row := range usernames {
		props, _ := row["properties"].(map[string]any)
		user, display := argString(props, "username"), argString(props, "displayName")
		if azureSenderName.MatchString(user) {
			cfg.Senders[strings.ToLower(user+"@"+plan.Domain)] = display
		}
	}
	if _, err = NewACSSender(cfg); err != nil {
		return empty, "", fmt.Errorf("Azure returned an unusable email endpoint or credential")
	}
	return cfg, replyTo, nil
}
