// Package frontdoor composes the cluster's front-door hostnames from the
// product of ROLE and ENVIRONMENT.
//
// # Why this is a package and not four format strings
//
// The same host has to be spelled identically in two places that are compiled,
// reviewed and deployed separately: the Ingress rules under deploy/k8s (written
// by cmd/frontdoorhosts) and the env values every node derives at boot
// (component/genesis/domain.go). When those two disagree the cluster does not
// fail to start -- identity issues tokens naming an issuer nothing is served at,
// every other node's verifier rejects them, and the symptom is "sign-in is
// broken" with both manifests looking correct. memql#3315 was one forgotten CORS
// origin and presented exactly that way. One derivation cannot drift against
// itself, so both sides call in here.
//
// # The model
//
// An environment is a VALUE: a single DNS LABEL, empty for the unprefixed one.
// Everything else follows from where that label is allowed to land, and the two
// positions are NOT interchangeable:
//
//	role host   api    + label -> api-staging.<domain>      ONE label
//	site host   portal + label -> portal.staging.<domain>   TWO labels
//
// # Why role hosts hyphenate instead of nesting
//
// A TLS wildcard matches exactly ONE label. `*.<domain>` covers
// `api-staging.<domain>` and does NOT cover `api.staging.<domain>`. Keeping the
// role hosts to a single label means every environment's api / identity / mcp
// host is covered by the certificate the cluster already has, however many
// environments there are.
//
// Sites cannot have that, because a site's own name occupies the label a role
// would hyphenate into: `shop.<domain>` under a label becomes
// `shop.staging.<domain>`, which is two labels by construction. So a labelled
// environment needs one additional wildcard, `*.<label>.<domain>` -- ONE
// certificate for all of that environment's sites, not one per site, which is
// the property memql#3700 exists to protect.
//
// # A labelled environment's sites live under the CLUSTER's domain
//
// The staging host for `shop.acme.com` is `shop.staging.<clusterdomain>` and
// never `shop.staging.acme.com`. A labelled environment is the OPERATOR's
// testing surface; requiring DNS under a customer's domain would make standing
// one up depend on a zone the operator may not control, and would put the
// operator's test traffic on the customer's name.
package frontdoor

import (
	"fmt"
	"regexp"
	"strings"
)

// Role is a front-door role: one of the fixed set of services the cluster
// exposes under its own hostname.
//
// The role set is CLOSED and adding to it is a design change, not a
// configuration change (docs/public/operate/front-door.md). The host COUNT is
// allowed to grow with environments -- a closed set the operator chooses -- and
// must never grow with customers, apps or sites, which is what the sites
// wildcard is for.
type Role string

const (
	// RoleAPI is the engine's API edge: gRPC on :50051 plus the HTTP surface on
	// :8085. Everything that speaks to the engine dials it.
	RoleAPI Role = "api"
	// RoleIdentity is the identity service's web + OAuth surface.
	RoleIdentity Role = "identity"
	// RoleMCP is the MCP Streamable HTTP protocol head.
	RoleMCP Role = "mcp"
)

// Roles is the closed role set, in the order the generated manifests emit them.
func Roles() []Role { return []Role{RoleAPI, RoleIdentity, RoleMCP} }

// labelPattern is one DNS label: lowercase alphanumerics and interior hyphens.
//
// Strict on purpose. The label is concatenated into a hostname that ends up in
// a TLS SAN and an Ingress rule, and a label carrying a dot would silently turn
// a role host into two labels -- which is precisely the case the wildcard
// certificate does not cover, and which would present as a TLS error at a host
// nobody thinks is new.
var labelPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// ValidateLabel refuses a label that cannot safely be concatenated. The empty
// label is valid and means "unprefixed".
func ValidateLabel(label string) error {
	if label == "" {
		return nil
	}
	if !labelPattern.MatchString(label) {
		return fmt.Errorf("environment host label %q is not a single DNS label: "+
			"it must be lowercase alphanumerics with interior hyphens, and in particular may not "+
			"contain a dot -- a dotted label makes a role host two labels, which the *.<domain> "+
			"wildcard certificate does not cover", label)
	}
	return nil
}

// RoleHost is `<role>.<domain>` unprefixed, `<role>-<label>.<domain>` otherwise.
// Single-label by construction -- see the package comment.
func RoleHost(role Role, label, domain string) string {
	if label == "" {
		return string(role) + "." + domain
	}
	return string(role) + "-" + label + "." + domain
}

// SiteHost is the host a NAMED site is served at: `<name>.<domain>` unprefixed,
// `<name>.<label>.<domain>` otherwise. Two labels under a labelled environment,
// which is why SitesWildcard exists.
func SiteHost(name, label, domain string) string {
	if label == "" {
		return name + "." + domain
	}
	return name + "." + label + "." + domain
}

// SitesWildcard is the one Ingress rule that routes every present and future
// site in this environment to the edge node.
func SitesWildcard(label, domain string) string {
	if label == "" {
		return "*." + domain
	}
	return "*." + label + "." + domain
}

// Apex is the bare domain, and it is served by ONE environment: the unprefixed
// one. For a customer cluster the apex IS their main website, so it is a site
// row like any other -- it just happens to be the one whose hostname has no
// label in front of it. A labelled environment has no apex to claim, because
// the bare domain is already taken by the environment without a label.
//
// Returns "" when the environment is labelled; callers emit no rule for it.
func Apex(label, domain string) string {
	if label == "" {
		return domain
	}
	return ""
}

// Host is one generated front-door rule.
type Host struct {
	// Role is the role that owns it, or "sites" for the wildcard and the apex.
	Role string
	// Name is the hostname itself.
	Name string
	// Sites is true for the wildcard and the apex -- the two rules that reach
	// the edge node rather than a named service.
	Sites bool
}

// SitesRole labels the two host rules that are not one of the closed Roles.
const SitesRole = "sites"

// Hosts is the whole role x environment product for one environment, in the
// order the generated manifests emit them.
func Hosts(label, domain string) []Host {
	out := make([]Host, 0, len(Roles())+2)
	for _, r := range Roles() {
		out = append(out, Host{Role: string(r), Name: RoleHost(r, label, domain)})
	}
	out = append(out, Host{Role: SitesRole, Name: SitesWildcard(label, domain), Sites: true})
	if apex := Apex(label, domain); apex != "" {
		out = append(out, Host{Role: SitesRole, Name: apex, Sites: true})
	}
	return out
}

// CertificateSANs is the SAN set one environment's front-door certificate must
// carry, in issue order.
//
// `*.<domain>` is ALWAYS present, including for a labelled environment, and
// that is the point of hyphenating role hosts: `api-staging.<domain>` is a
// single label, so it needs no certificate of its own. A labelled environment
// adds exactly one more SAN, `*.<label>.<domain>`, for its sites.
//
// The unprefixed environment additionally carries the apex, which no wildcard
// covers -- `*.<domain>` matches one label and the bare domain has none.
//
// Each environment gets its OWN certificate even though the SAN sets overlap.
// A Kubernetes Secret is namespaced and cannot be referenced across namespaces,
// so a shared certificate is not expressible; two orders for the same wildcard
// is the price of the namespace isolation the whole epic rests on.
func CertificateSANs(label, domain string) []string {
	out := []string{"*." + domain}
	if apex := Apex(label, domain); apex != "" {
		out = append(out, apex)
	}
	if label != "" {
		out = append(out, SitesWildcard(label, domain))
	}
	return out
}

// DomainDerivationSuffix is the hostname suffix a role host carries under this
// label -- `.<domain>` unprefixed, `-<label>.<domain>` otherwise.
//
// Exists so a caller composing many role hosts at once (genesis's env
// derivation) can do it with one string concatenation per name and still be
// using this package's rule rather than its own copy of it.
func DomainDerivationSuffix(label, domain string) string {
	if label == "" {
		return "." + domain
	}
	return "-" + label + "." + domain
}

// SiteSuffix is the site-host counterpart of DomainDerivationSuffix.
func SiteSuffix(label, domain string) string {
	if label == "" {
		return "." + domain
	}
	return "." + label + "." + domain
}

// NormalizeLabel trims a label read from configuration. Whitespace around a
// value in a ConfigMap or an env var is invisible in every UI that displays it
// and would otherwise be concatenated into a hostname.
func NormalizeLabel(label string) string { return strings.TrimSpace(label) }
