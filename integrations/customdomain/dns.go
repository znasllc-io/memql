// Package customdomain is the engine half of custom domains (epic memql#4805):
// the two DNS checks that decide whether a client's own hostname may be bound
// to one of this cluster's deployables, and the reconciliation sweep that walks
// each binding from `pending_dns` to `live`.
//
// # What this package deliberately is not
//
// It holds NO Kubernetes client, and nothing in this repo's module graph gains
// one because of it (design D2). The objects a bound domain needs -- an
// exact-host Ingress and a cert-manager Certificate -- are applied by an
// idempotent capability script, reached through the existing action-dispatch
// seam (component/automations/steps -> deploycontrol.ParseCapabilityResult). An
// in-engine client-go reconciler would be a SECOND way to touch cluster
// objects beside the GitOps/script path, which is precisely what
// environment-parity review rejects.
//
// # Why the DNS half is here and the Kubernetes half is not
//
// A DNS lookup is a read of the public internet with no credentials, no
// cluster-scoped permissions and no rate limit worth respecting. Applying an
// Ingress is none of those things. The split follows the blast radius rather
// than the language.
package customdomain

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
)

// Resolver is the DNS surface the checks need: two lookups, and nothing else.
//
// Narrow on purpose, and it is what makes the whole verification path testable
// with no network (design section C). The production implementation is a thin
// wrapper over net.Resolver; every test in this package drives a map.
type Resolver interface {
	// LookupTXT returns the TXT records at name.
	LookupTXT(ctx context.Context, name string) ([]string, error)
	// LookupCNAME returns the canonical name name resolves through, or name
	// itself when there is no CNAME chain -- which is what net.Resolver does.
	LookupCNAME(ctx context.Context, name string) (string, error)
	// LookupHost returns the A/AAAA addresses for name.
	LookupHost(ctx context.Context, name string) ([]string, error)
}

// SystemResolver is the production Resolver: the host's own recursive
// resolver, through net.Resolver.
type SystemResolver struct{ r net.Resolver }

// NewSystemResolver returns the default system resolver.
func NewSystemResolver() *SystemResolver { return &SystemResolver{} }

func (s *SystemResolver) LookupTXT(ctx context.Context, name string) ([]string, error) {
	return s.r.LookupTXT(ctx, name)
}

func (s *SystemResolver) LookupCNAME(ctx context.Context, name string) (string, error) {
	return s.r.LookupCNAME(ctx, name)
}

func (s *SystemResolver) LookupHost(ctx context.Context, name string) ([]string, error) {
	return s.r.LookupHost(ctx, name)
}

// The typed failure reasons. These four strings are a CONTRACT: they are
// written to v1:platform:customDomain.failureReason, broadcast to the Domains
// panel, and rendered there as the sentence a person acts on. Renaming one
// silently turns a named refusal into an unrecognised one, which the panel
// renders as the raw token -- so they are constants here and mirrored, by
// value, in clients/os/src/apps/deployables/domains.ts.
const (
	// ReasonTokenMissing -- the TXT ownership record is absent, or present
	// with a value that is not this row's token.
	ReasonTokenMissing = "dns_token_missing"
	// ReasonNotPointing -- the hostname does not resolve to this cluster.
	ReasonNotPointing = "dns_not_pointing"
	// ReasonNoACMEIssuer -- the target declares no ACME issuer, so no
	// certificate can be requested for the hostname. The honest answer on a
	// local k3d cluster, and permanent there (design D7).
	ReasonNoACMEIssuer = "no_acme_issuer"
	// ReasonIssuanceFailed -- the bind script ran and the objects did not
	// come up. failureDetail carries the envelope's own message.
	ReasonIssuanceFailed = "issuance_failed"
)

// VerifyPrefix is the label the ownership TXT record sits under:
// `_memql-verify.<hostname>`.
//
// An underscore-prefixed label, which is the convention every other
// verification record follows (_acme-challenge, _dmarc, _github-challenge) for
// a reason worth restating: an underscore is not legal in a HOSTNAME, so the
// name can never collide with something the client is actually serving.
const VerifyPrefix = "_memql-verify"

// VerifyRecordName is the fully qualified name of the ownership record for a
// hostname.
func VerifyRecordName(hostname string) string {
	h := NormalizeHostname(hostname)
	if h == "" {
		return ""
	}
	return VerifyPrefix + "." + h
}

// NormalizeHostname lowercases, trims and strips a trailing root dot.
//
// The trailing dot matters more than it looks: `net.Resolver.LookupCNAME`
// returns a FQDN with the root dot on it, and a client pasting from a zone
// file may well include one too. Comparing an un-normalised pair reports "not
// pointing" for a record that is exactly right, which is the single most
// frustrating way this feature could fail.
func NormalizeHostname(h string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
}

// IsApex reports whether hostname is the registrable domain itself rather than
// a subdomain of it -- `acme.com` as against `www.acme.com`.
//
// A LABEL COUNT, not a public-suffix lookup, and the approximation is
// deliberate. The only thing this decides is whether the guidance panel asks
// for a CNAME or an A record, and the rule that actually governs that is
// "CNAME is illegal at a zone apex" -- which is about where the client's zone
// starts, something no library can know from the string alone. Two labels is
// right for acme.com and wrong for acme.co.uk; a client on a multi-label TLD
// gets told to create an A record where a CNAME would also have worked, which
// is a records-that-work answer rather than a records-that-fail one. Embedding
// a public-suffix list to improve on that would add a dependency that goes
// stale, to make a suggestion marginally tidier.
func IsApex(hostname string) bool {
	h := NormalizeHostname(hostname)
	if h == "" {
		return false
	}
	return strings.Count(h, ".") <= 1
}

// CheckResult is what one verification pass saw.
//
// `Detail` is the OBSERVATION, in the words of whatever produced it: the value
// found at the TXT name, the address the hostname resolves to, the resolver's
// own error. The panel renders it beside the typed reason, because
// "dns_not_pointing" says which record is wrong and this says what is in it --
// and somebody editing a zone file needs both halves.
type CheckResult struct {
	OK     bool
	Reason string
	Detail string
}

func passed() CheckResult { return CheckResult{OK: true} }

func failed(reason, detail string) CheckResult {
	return CheckResult{Reason: reason, Detail: detail}
}

// CheckOwnership verifies that `TXT _memql-verify.<hostname>` carries token.
//
// # Why an ownership token exists at all when the pointing check is coming
//
// Pointing proves that whoever controls the DNS wants traffic to arrive here.
// It does NOT prove that the person who typed the hostname into this cluster's
// form is that someone. Without this record, the first operator to claim
// `www.acme.com` on a cluster acme's DNS already points at would be handed
// their traffic -- and on a shared install, "points at this cluster" is a
// property many unrelated tenants share. So both checks are required and
// neither is redundant (design D4).
//
// The token is compared against EVERY TXT string at the name, because a zone
// legitimately carries several (SPF, a Google verification, this one), and a
// client is not going to delete theirs to satisfy us.
func CheckOwnership(ctx context.Context, r Resolver, hostname, token string) CheckResult {
	name := VerifyRecordName(hostname)
	want := strings.TrimSpace(token)
	if name == "" || want == "" {
		return failed(ReasonTokenMissing, "no hostname or no token to look for")
	}
	records, err := r.LookupTXT(ctx, name)
	if err != nil {
		// A LOOKUP ERROR IS A MISS, NOT A FAULT. NXDOMAIN is the ordinary
		// answer while somebody is still creating the record, and the whole
		// surface is built around saying so plainly rather than showing an
		// error somebody cannot act on. The resolver's own words go into the
		// detail so a genuine outage is still legible.
		return failed(ReasonTokenMissing, fmt.Sprintf("looked up TXT %s: %s", name, dnsErrorText(err)))
	}
	for _, rec := range records {
		if strings.TrimSpace(rec) == want {
			return passed()
		}
	}
	if len(records) == 0 {
		return failed(ReasonTokenMissing, fmt.Sprintf("TXT %s exists but carries no records", name))
	}
	return failed(ReasonTokenMissing, fmt.Sprintf(
		"TXT %s carries %d record(s), none matching this domain's token (found: %s)",
		name, len(records), strings.Join(quoteAll(records), ", ")))
}

// CheckPointing verifies that hostname resolves to this cluster.
//
// TWO SHAPES, because DNS has two (design D4):
//
//   - A SUBDOMAIN is checked by CNAME. `www.acme.com CNAME os.memql.example`
//     is the record a client should create, and it is the one that keeps
//     working when the cluster's ingress address changes.
//   - An APEX cannot carry a CNAME -- RFC 1034 forbids a CNAME alongside the
//     SOA and NS records every zone apex has -- so it is checked by A record
//     against the addresses the cluster's own edge host resolves to.
//
// THE APEX TARGET IS DISCOVERED, NEVER CONFIGURED. It comes from resolving
// edgeHost, which is a value this cluster already holds and already serves
// itself at. A second configured "our ingress IP" would be a value with no
// forcing function to keep it true: the day the load balancer is replaced, the
// cluster would refuse every correct apex record while every manifest looked
// right.
//
// A CNAME that happens to be correct on an apex still passes: the CNAME branch
// is tried first for every shape, and only its miss falls through to addresses.
// Some providers implement apex aliasing (ALIAS / ANAME / CNAME flattening)
// that a resolver reports as an address rather than a CNAME, and both of those
// are a working configuration this must not call broken.
func CheckPointing(ctx context.Context, r Resolver, hostname, edgeHost string) CheckResult {
	host := NormalizeHostname(hostname)
	edge := NormalizeHostname(edgeHost)
	if host == "" {
		return failed(ReasonNotPointing, "no hostname to check")
	}
	if edge == "" {
		// Unreachable through the reconciler, which derives the edge host
		// from the cluster's own domain. Stated anyway: an empty target would
		// make every CNAME compare equal to nothing and every address list
		// compare against an empty set, which is a check that cannot pass and
		// reads to an operator as "DNS is broken".
		return failed(ReasonNotPointing, "this cluster's edge host did not resolve, so there is nothing to check the record against")
	}

	cname, cerr := r.LookupCNAME(ctx, host)
	if cerr == nil {
		if NormalizeHostname(cname) == edge {
			return passed()
		}
		// net.Resolver returns the name itself when there is no CNAME chain,
		// so "the CNAME is the hostname" means "there is no CNAME here" and
		// is not worth reporting as a wrong CNAME.
		if NormalizeHostname(cname) != host {
			return failed(ReasonNotPointing, fmt.Sprintf(
				"%s is a CNAME to %s; it should be a CNAME to %s",
				host, NormalizeHostname(cname), edge))
		}
	}

	addrs, aerr := r.LookupHost(ctx, host)
	if aerr != nil {
		return failed(ReasonNotPointing, fmt.Sprintf("looked up %s: %s", host, dnsErrorText(aerr)))
	}
	edgeAddrs, eerr := r.LookupHost(ctx, edge)
	if eerr != nil {
		// We could not learn where WE are. Refusing is the fail-closed
		// reading and it is the right one: admitting the record because our
		// own lookup failed would bind a hostname on no evidence at all.
		return failed(ReasonNotPointing, fmt.Sprintf(
			"could not resolve this cluster's own edge host %s to check %s against it: %s",
			edge, host, dnsErrorText(eerr)))
	}
	if len(edgeAddrs) == 0 {
		return failed(ReasonNotPointing, fmt.Sprintf(
			"this cluster's edge host %s resolves to no addresses, so there is nothing to check %s against", edge, host))
	}
	want := make(map[string]bool, len(edgeAddrs))
	for _, a := range edgeAddrs {
		want[strings.TrimSpace(a)] = true
	}
	for _, a := range addrs {
		if want[strings.TrimSpace(a)] {
			return passed()
		}
	}
	kind := "an A record pointing at"
	if !IsApex(host) {
		kind = "a CNAME to"
	}
	return failed(ReasonNotPointing, fmt.Sprintf(
		"%s resolves to %s; it needs %s %s (%s)",
		host, joinOrNone(addrs), kind, edge, joinOrNone(edgeAddrs)))
}

// PointingRecord describes the record a client has to create, in the
// vocabulary a registrar's own form uses.
//
// It is DERIVED from the hostname rather than stored, so a row created before
// this rule existed renders the same guidance as one created after it, and
// changing the advice is a code change rather than a migration.
type PointingRecord struct {
	// Kind is "CNAME" or "A" -- the value of a registrar's "Type" field.
	Kind string
	// Name is the record's name, fully qualified.
	Name string
	// Target is the value: an edge hostname for a CNAME, an address for an A.
	Target string
}

// PointingRecordFor returns the record a client should create for hostname.
//
// For an apex the target is an ADDRESS and needs a resolver; when that lookup
// fails the record is still returned with the edge host as its target and a
// CNAME kind, because half an answer that names the right host beats an empty
// panel. The verification check is the authority either way -- this only
// decides what the guidance says.
func PointingRecordFor(ctx context.Context, r Resolver, hostname, edgeHost string) PointingRecord {
	host := NormalizeHostname(hostname)
	edge := NormalizeHostname(edgeHost)
	if !IsApex(host) {
		return PointingRecord{Kind: "CNAME", Name: host, Target: edge}
	}
	if r != nil {
		if addrs, err := r.LookupHost(ctx, edge); err == nil && len(addrs) > 0 {
			return PointingRecord{Kind: "A", Name: host, Target: strings.TrimSpace(addrs[0])}
		}
	}
	return PointingRecord{Kind: "CNAME", Name: host, Target: edge}
}

// dnsErrorText renders a resolver error as something a person reading a panel
// can act on. A *net.DNSError's own string is already the useful form
// ("no such host"); everything else falls back to the error itself.
func dnsErrorText(err error) string {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		if dnsErr.IsNotFound {
			return "no such record"
		}
		return dnsErr.Err
	}
	return err.Error()
}

func quoteAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, `"`+strings.TrimSpace(s)+`"`)
	}
	return out
}

func joinOrNone(in []string) string {
	if len(in) == 0 {
		return "nothing"
	}
	return strings.Join(in, ", ")
}
