package packages

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// ManifestDeployment carries portable placement defaults. Slug is relative to
// the receiving cluster. Domains are additive; omission never removes bindings.
type ManifestDeployment struct {
	Slug    string   `yaml:"slug,omitempty" json:"slug,omitempty"`
	Domains []string `yaml:"domains,omitempty" json:"domains,omitempty"`
}

var manifestSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,38}[a-z0-9]$`)
var manifestDNSLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func validateDeployment(d *ManifestDeployment) error {
	if d == nil {
		return nil
	}
	d.Slug = strings.TrimSpace(d.Slug)
	if d.Slug != "" && !manifestSlug.MatchString(d.Slug) {
		return fmt.Errorf("deployment.slug must be a lowercase DNS label of 3–40 characters")
	}
	seen := map[string]bool{}
	for i, raw := range d.Domains {
		host := strings.ToLower(strings.TrimSpace(raw))
		if len(host) > 253 || !strings.Contains(host, ".") {
			return fmt.Errorf("invalid deployment domain %q", raw)
		}
		for _, label := range strings.Split(host, ".") {
			if !manifestDNSLabel.MatchString(label) {
				return fmt.Errorf("invalid deployment domain %q; use a hostname without a scheme, path or wildcard", raw)
			}
		}
		if seen[host] {
			return fmt.Errorf("duplicate deployment domain %q", host)
		}
		seen[host] = true
		d.Domains[i] = host
	}
	return nil
}

func deploymentFingerprint(d *ManifestDeployment) string {
	if d == nil {
		return ""
	}
	domains := append([]string{}, d.Domains...)
	sort.Strings(domains)
	return d.Slug + ":" + strings.Join(domains, ",")
}

func manifestPlacement(d *ManifestDeployment, p Placement) Placement {
	if d == nil || p.Skip {
		return p
	}
	if p.Hostname == "" && d.Slug != "" {
		domain := strings.ToLower(strings.TrimSpace(os.Getenv("MEMQL_DOMAIN")))
		if domain == "" {
			domain = "memql.localhost"
		}
		p.Hostname = d.Slug + "." + domain
	}
	if p.Domains == nil && p.OwnDomain == "" {
		p.Domains = append([]string{}, d.Domains...)
	}
	return p
}

func placementDomains(fields map[string]any) []string {
	raw, present := fields["domains"]
	if !present {
		return nil
	}
	out := []string{}
	switch values := raw.(type) {
	case []any:
		for _, v := range values {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
	case []string:
		out = append(out, values...)
	}
	return out
}

func placementDomainNames(p Placement) []string {
	all := append([]string{}, p.Domains...)
	if p.OwnDomain != "" {
		all = append(all, p.OwnDomain)
	}
	out := []string{}
	seen := map[string]bool{}
	for _, raw := range all {
		host := strings.ToLower(strings.TrimSpace(raw))
		if host != "" && !seen[host] {
			out = append(out, host)
			seen[host] = true
		}
	}
	return out
}
