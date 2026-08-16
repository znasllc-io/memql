// Package frontdoor composes the cluster's front-door hostnames from its role
// set and its domain.
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
// # There is no environment dimension
//
// The host set used to be the product of role x ENVIRONMENT, with an
// environment carried as a DNS label that hyphenated into a role host
// (api-staging.<d>) and nested into a site host (shop.staging.<d>). Epic
// memql#3943 removed "environment" as a product concept: memQL ships ONE
// installation shape, and an operator who wants a second environment installs a
// second instance, which brings its own domain and therefore its own host set.
// So the product has one factor left, and every host here is a single label
// under the domain.
//
// The TLS consequence is the one worth keeping in mind if a label is ever
// proposed again: a wildcard matches exactly ONE label, so `*.<domain>` covers
// `api.<domain>` and would NOT cover `api.<anything>.<domain>`. Every host this
// package composes stays inside that one wildcard, which is why
// CertificateSANs is two entries and not one per host.
package frontdoor

// Role is a front-door role: one of the fixed set of services the cluster
// exposes under its own hostname.
//
// The role set is CLOSED and adding to it is a design change, not a
// configuration change (docs/public/operate/front-door.md). The host COUNT must
// never grow with customers, apps or sites, which is what the sites wildcard is
// for.
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

// RoleHost is `<role>.<domain>`.
func RoleHost(role Role, domain string) string { return string(role) + "." + domain }

// SiteHost is the host a NAMED site is served at: `<name>.<domain>`.
func SiteHost(name, domain string) string { return name + "." + domain }

// SitesWildcard is the one Ingress rule that routes every present and future
// site to the edge node.
func SitesWildcard(domain string) string { return "*." + domain }

// Apex is the bare domain. For a customer cluster the apex IS their main
// website, so it is a site row like any other -- it just happens to be the one
// whose hostname is the domain itself.
func Apex(domain string) string { return domain }

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

// Hosts is the whole front-door host set, in the order the generated manifests
// emit them.
func Hosts(domain string) []Host {
	out := make([]Host, 0, len(Roles())+2)
	for _, r := range Roles() {
		out = append(out, Host{Role: string(r), Name: RoleHost(r, domain)})
	}
	out = append(out, Host{Role: SitesRole, Name: SitesWildcard(domain), Sites: true})
	out = append(out, Host{Role: SitesRole, Name: Apex(domain), Sites: true})
	return out
}

// CertificateSANs is the SAN set the front-door certificate must carry, in
// issue order.
//
// Two entries, and the second is why: `*.<domain>` matches exactly one label,
// so it covers every role host and every site host but NOT the bare domain,
// which has no label to match.
func CertificateSANs(domain string) []string {
	return []string{"*." + domain, Apex(domain)}
}

// DomainDerivationSuffix is the hostname suffix every front-door host carries:
// `.<domain>`.
//
// Exists so a caller composing many host names at once (genesis's env
// derivation) can do it with one string concatenation per name and still be
// using this package's rule rather than its own copy of it.
//
// It used to have a site-host counterpart, SiteSuffix, because a labelled
// environment nested site hosts one label deeper than it hyphenated role hosts.
// With no label the two returned the identical string, so epic memql#3943
// collapsed them into this one.
func DomainDerivationSuffix(domain string) string { return "." + domain }
