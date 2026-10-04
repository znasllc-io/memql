// Package emailconfig defines the public email choices carried by a MemQL
// package. It contains neither credentials nor claims of verification.
package emailconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/mail"
	"regexp"
	"strings"
)

type Campaigns struct {
	Azure   Azure    `json:"azure" yaml:"azure"`
	Domains []Domain `json:"domains" yaml:"domains"`
}

type Azure struct {
	TenantID              string `json:"tenantId,omitempty" yaml:"tenantId,omitempty"`
	SubscriptionID        string `json:"subscriptionId" yaml:"subscriptionId"`
	ResourceGroup         string `json:"resourceGroup" yaml:"resourceGroup"`
	CreateResourceGroup   bool   `json:"createResourceGroup,omitempty" yaml:"createResourceGroup,omitempty"`
	ResourceGroupLocation string `json:"resourceGroupLocation,omitempty" yaml:"resourceGroupLocation,omitempty"`
	DataLocation          string `json:"dataLocation" yaml:"dataLocation"`
}

type Domain struct {
	// Organization is a portable label. Import requires an explicit binding to
	// an authorized account on the destination; this label grants no authority.
	Organization         string      `json:"organization" yaml:"organization"`
	Domain               string      `json:"domain" yaml:"domain"`
	EmailService         string      `json:"emailService" yaml:"emailService"`
	CommunicationService string      `json:"communicationService" yaml:"communicationService"`
	Sender               *Sender     `json:"sender,omitempty" yaml:"sender,omitempty"`
	DNS                  []DNSRecord `json:"dns,omitempty" yaml:"dns,omitempty"`
}

type Sender struct {
	Username    string `json:"username" yaml:"username"`
	DisplayName string `json:"displayName" yaml:"displayName"`
	ReplyTo     string `json:"replyTo,omitempty" yaml:"replyTo,omitempty"`
}

type DNSRecord struct {
	Purpose string `json:"purpose" yaml:"purpose"`
	Name    string `json:"name" yaml:"name"`
	Type    string `json:"type" yaml:"type"`
	Value   string `json:"value" yaml:"value"`
	TTL     int    `json:"ttl" yaml:"ttl"`
}

var uuid = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
var resourceName = regexp.MustCompile(`^[a-zA-Z0-9-]{1,63}$`)
var groupName = regexp.MustCompile(`^[a-zA-Z0-9_(). -]{1,90}$`)
var label = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
var username = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)

func (c *Campaigns) Validate() error {
	if c == nil {
		return nil
	}
	a := c.Azure
	if !uuid.MatchString(a.SubscriptionID) || a.TenantID != "" && !uuid.MatchString(a.TenantID) {
		return fmt.Errorf("campaigns.azure requires valid subscriptionId and optional tenantId")
	}
	if !groupName.MatchString(a.ResourceGroup) || strings.TrimSpace(a.ResourceGroup) != a.ResourceGroup || strings.HasSuffix(a.ResourceGroup, ".") {
		return fmt.Errorf("invalid campaigns.azure.resourceGroup")
	}
	if a.CreateResourceGroup && !regexp.MustCompile(`^[a-z][a-z0-9]{1,39}$`).MatchString(a.ResourceGroupLocation) {
		return fmt.Errorf("choose campaigns.azure.resourceGroupLocation")
	}
	switch a.DataLocation {
	case "United States", "Europe", "United Kingdom", "Australia", "Asia Pacific", "Brazil", "Canada", "France", "Germany", "India", "Japan", "Korea", "Norway", "Switzerland", "United Arab Emirates", "Africa":
	default:
		return fmt.Errorf("choose campaigns.azure.dataLocation")
	}
	if len(c.Domains) > 100 {
		return fmt.Errorf("a package can declare at most 100 email domains")
	}
	organizations, domains, services := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, d := range c.Domains {
		if d.Organization == "" || d.Organization != strings.TrimSpace(d.Organization) || len(d.Organization) > 200 || strings.ContainsAny(d.Organization, "\r\n\x00") {
			return fmt.Errorf("each campaign domain needs an organization label")
		}
		key := strings.ToLower(d.Organization)
		if organizations[key] {
			return fmt.Errorf("duplicate campaign organization %q", d.Organization)
		}
		organizations[key] = true
		if !validDomain(d.Domain) {
			return fmt.Errorf("invalid campaign domain %q", d.Domain)
		}
		if domains[d.Domain] {
			return fmt.Errorf("duplicate campaign domain %q", d.Domain)
		}
		domains[d.Domain] = true
		for kind, name := range map[string]string{"email": d.EmailService, "delivery": d.CommunicationService} {
			if !resourceName.MatchString(name) {
				return fmt.Errorf("invalid campaign %s service name", kind)
			}
			key := kind + ":" + strings.ToLower(name)
			if services[key] {
				return fmt.Errorf("campaign organizations must use separate Azure %s services", kind)
			}
			services[key] = true
		}
		if d.Sender != nil {
			s := d.Sender
			if !username.MatchString(s.Username) || strings.TrimSpace(s.DisplayName) == "" || len(s.DisplayName) > 100 || strings.ContainsAny(s.DisplayName, "\r\n\x00") {
				return fmt.Errorf("invalid campaign sender for %q", d.Organization)
			}
			if s.ReplyTo != "" {
				a, err := mail.ParseAddress(s.ReplyTo)
				if err != nil || a.Address != s.ReplyTo || strings.ContainsAny(s.ReplyTo, "\r\n") {
					return fmt.Errorf("invalid reply mailbox for %q", d.Organization)
				}
			}
		}
		purposes := map[string]bool{}
		for _, r := range d.DNS {
			if r.Purpose != "Domain" && r.Purpose != "SPF" && r.Purpose != "DKIM" && r.Purpose != "DKIM2" {
				return fmt.Errorf("unknown campaign DNS purpose %q", r.Purpose)
			}
			if purposes[r.Purpose] {
				return fmt.Errorf("duplicate campaign DNS purpose %q", r.Purpose)
			}
			purposes[r.Purpose] = true
			if r.Type != "TXT" && r.Type != "CNAME" || r.Name == "" || r.Value == "" || r.TTL < 1 || r.TTL > 2147483647 || len(r.Name) > 255 || len(r.Value) > 4096 || strings.ContainsAny(r.Name+r.Value, "\r\n\x00") {
				return fmt.Errorf("invalid campaign DNS record for %q", d.Organization)
			}
		}
	}
	return nil
}

func validDomain(domain string) bool {
	if len(domain) > 253 || !strings.Contains(domain, ".") || net.ParseIP(domain) != nil {
		return false
	}
	for _, part := range strings.Split(domain, ".") {
		if !label.MatchString(part) {
			return false
		}
	}
	return true
}

// Decode refuses undeclared fields, including pasted credentials and status.
func Decode(raw []byte) (*Campaigns, error) {
	var c Campaigns
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("invalid campaign configuration: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("campaign configuration must contain one JSON object")
	}

	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// SameResources compares the actual Azure destination. Creation preferences are
// setup instructions, not a different resource once that group exists.
func SameResources(a, b Azure) bool {
	return a.SubscriptionID == b.SubscriptionID && strings.EqualFold(a.ResourceGroup, b.ResourceGroup) && a.DataLocation == b.DataLocation && (a.TenantID == "" || b.TenantID == "" || a.TenantID == b.TenantID)
}
