// Package frontdoor composes the cluster's front-door hostnames from its role
// set and its domain.
//
// # Why this is a package and not four format strings
//
// The same host has to be spelled identically in three places that are
// compiled, reviewed and deployed separately: the Ingress rules and the
// certificate SANs under deploy/k8s (written by cmd/frontdoorhosts), the env
// values every node derives at boot (component/envregistry/domain.go), and the
// portal site row the engine seeds from MEMQL_DOMAIN (component/memql's
// SeedMaterializer). When those disagree the cluster does not fail to start --
// identity issues tokens naming an issuer nothing is served at, every other
// node's verifier rejects them, and the symptom is "sign-in is broken" with
// both manifests looking correct. memql#3315 was one forgotten CORS origin and
// presented exactly that way. One derivation cannot drift against itself, so
// every side calls in here.
//
// # There is no environment dimension
//
// The host set used to be the product of role x ENVIRONMENT, with an
// environment carried as a DNS label that hyphenated into a role host
// (api-staging.<d>) and nested into a site host (shop.staging.<d>). Epic
// memql#3943 removed "environment" as a product concept: MemQL ships ONE
// installation shape, and an operator who wants a second environment installs a
// second instance, which brings its own domain and therefore its own host set.
// So the product has one factor left, and every host here is a single label
// under the domain.
//
// The single-label property is a ROUTING fact: an Ingress wildcard matches
// exactly ONE label, so the one `*.<domain>` rule routes `shop.<domain>` to the
// edge and would NOT route `shop.eu.<domain>`. Every host this package
// composes stays inside that one rule.
//
// # The certificate names exact hosts, not a wildcard (memql#4224)
//
// The single-label property used to be the certificate story as well: one
// `*.<domain>` SAN covering every role host and every site, plus the apex. It
// was never issued. The cloud ClusterIssuer solves HTTP-01 only, ACME cannot
// serve an HTTP-01 challenge for a name that is not a host, and a single
// wildcard dnsName fails the WHOLE order -- so the Certificate sat Pending, the
// operator hand-edited it to exact names, and the edge Ingress whose tls.hosts
// still said `*.<domain>` served ingress-nginx's self-signed default for
// portal.<domain> ("This Connection Is Not Private").
//
// So CertificateSANs is every EXACT host the front door serves -- the three
// role hosts, the portal, and the apex -- and the wildcard is an Ingress RULE
// with no certificate behind it. The portal is the one site the platform ships
// itself, which is why it is the one site this package can name in advance;
// every other site needs its own Certificate until a DNS-01 solver exists
// (docs/public/operate/site-hosting.md). A server block under ingress-nginx is
// created per Ingress RULE host, never per tls host, so naming the portal in
// tls.hosts is not enough: it carries its own exact rule, pointing at the same
// edge Service the wildcard does.
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

// PortalSite is the name of the one site the platform ships itself: the MemQL
// Portal, site #1 (memql#3711). It is the seed id of its v1:platform:site row
// and the label its hostname carries.
const PortalSite = "portal"

// PortalHost is the host the portal is served at: `portal.<domain>`.
//
// The portal is a site like any other to the edge, which resolves the request
// Host against the graph and cannot tell it apart from a customer's SPA. What
// makes it different to THIS package is that it is the only site whose name is
// known before any operator creates a row, so it is the only site the
// front-door certificate can name (memql#4224) -- and therefore the only site
// with an exact Ingress rule of its own, because ingress-nginx builds a
// certificate-bearing server block per RULE host, not per tls host.
//
// Every consumer of the portal's hostname composes it here: the engine's
// SeedMaterializer (the site row's hostname), envregistry (the portal's OAuth
// redirect URI and CORS origin) and cmd/frontdoorhosts (its rule and SAN). A
// second spelling would be a certificate for a host the site row does not
// carry.
func PortalHost(domain string) string { return SiteHost(PortalSite, domain) }

// SitesWildcard is the one Ingress rule that routes every present and future
// site to the edge node.
//
// A RULE, not a certificate (memql#4224): the cloud issuer is HTTP-01 and
// cannot issue `*.<domain>`, so a site routed by this rule terminates TLS with
// the controller's default certificate until it has a Certificate of its own.
func SitesWildcard(domain string) string { return "*." + domain }

// Apex is the bare domain. For a customer cluster the apex IS their main
// website, so it is a site row like any other -- it just happens to be the one
// whose hostname is the domain itself.
func Apex(domain string) string { return domain }

// Host is one generated front-door rule.
type Host struct {
	// Role is the role that owns it, or "sites" for the three rules that reach
	// the edge: the portal, the wildcard and the apex.
	Role string
	// Name is the hostname itself.
	Name string
	// Sites is true for the rules that reach the edge node rather than a named
	// service: the portal, the wildcard and the apex.
	Sites bool
	// Wildcard is true for the one rule whose host is `*.<domain>`. It is the
	// only rule with no certificate SAN behind it (memql#4224).
	Wildcard bool
}

// SitesRole labels the host rules that are not one of the closed Roles.
const SitesRole = "sites"

// Hosts is the whole front-door host set, in the order the generated manifests
// emit them: the roles, then the portal, then the sites wildcard, then the
// apex.
func Hosts(domain string) []Host {
	out := make([]Host, 0, len(Roles())+3)
	for _, r := range Roles() {
		out = append(out, Host{Role: string(r), Name: RoleHost(r, domain)})
	}
	out = append(out, Host{Role: SitesRole, Name: PortalHost(domain), Sites: true})
	out = append(out, Host{Role: SitesRole, Name: SitesWildcard(domain), Sites: true, Wildcard: true})
	out = append(out, Host{Role: SitesRole, Name: Apex(domain), Sites: true})
	return out
}

// CertificateSANs is the SAN set the front-door certificate must carry, in
// issue order: every EXACT host in Hosts, which is every host but the wildcard.
//
// NO WILDCARD, DELIBERATELY (memql#4224). The cloud ClusterIssuer solves
// HTTP-01 only, and a wildcard dnsName fails the whole ACME order -- not the
// one name, the order -- so requesting it would leave the cluster with no
// certificate at all. Derived from Hosts rather than listed so that a host
// cannot be served without a SAN or SAN'd without a rule; the overlay render
// gates (deploy/k8s/overlays/frontdoor_hosts_test.go) assert the same equality
// against the manifests this produces.
func CertificateSANs(domain string) []string {
	hosts := Hosts(domain)
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		if h.Wildcard {
			continue
		}
		out = append(out, h.Name)
	}
	return out
}

// DomainDerivationSuffix is the hostname suffix every front-door host carries:
// `.<domain>`.
//
// Exists so a caller composing many host names at once (envregistry's env
// derivation) can do it with one string concatenation per name and still be
// using this package's rule rather than its own copy of it.
//
// It used to have a site-host counterpart, SiteSuffix, because a labelled
// environment nested site hosts one label deeper than it hyphenated role hosts.
// With no label the two returned the identical string, so epic memql#3943
// collapsed them into this one.
func DomainDerivationSuffix(domain string) string { return "." + domain }
