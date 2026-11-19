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
// seam (component/automations/steps -> capabilityresult.ParseCapabilityResult). An
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
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// Resolver is the DNS surface the checks need: four lookups, and nothing else.
//
// Narrow on purpose, and it is what makes the whole verification path testable
// with no network (design section C). The production implementation wraps
// net.Resolver, plus one question of its own for CNAMEHop; every check in this
// package is tested against a map.
type Resolver interface {
	// LookupTXT returns the TXT records at name.
	LookupTXT(ctx context.Context, name string) ([]string, error)
	// LookupCNAME returns the canonical name name resolves through, or name
	// itself when there is no CNAME chain -- which is what net.Resolver does.
	LookupCNAME(ctx context.Context, name string) (string, error)
	// LookupHost returns the A/AAAA addresses for name.
	LookupHost(ctx context.Context, name string) ([]string, error)
	// CNAMEHop returns the target of the CNAME record AT name -- one hop of
	// the chain, never the rest of it -- or "" with no error when name carries
	// no CNAME, which is where a chain ends. The target is returned as the
	// wire reports it, fully qualified; callers normalize.
	//
	// It exists beside LookupCNAME because LookupCNAME reports only the END of
	// a chain, and a fronted binding is proved by a name in its MIDDLE: the
	// front's endpoint, which itself CNAMEs on into the front's network
	// (CheckPointing).
	CNAMEHop(ctx context.Context, name string) (string, error)
}

// SystemResolver is the production Resolver: the host's own recursive
// resolver, through net.Resolver, and for CNAMEHop the first nameserver the
// host's resolver configuration names.
type SystemResolver struct {
	r net.Resolver
	// nameserver is the host:port CNAMEHop asks. Empty -- the production value
	// -- reads the first nameserver of resolvConfPath at every call. A test
	// points it at a server on the loopback interface.
	nameserver string
}

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

// resolvConfPath is the system resolver configuration CNAMEHop takes its
// nameserver from.
const resolvConfPath = "/etc/resolv.conf"

// cnameHopTimeout bounds one exchange with the nameserver; the TCP retry gets
// one of its own, and the caller's context ends either sooner. Five seconds is
// the system resolver's own per-attempt default (resolv.conf's `timeout` when
// unset), so one hop waits no longer than one ordinary lookup would.
const cnameHopTimeout = 5 * time.Second

// CNAMEHop asks for the CNAME record at name, once, and returns its target
// without following it.
//
// # Where the question goes, and why there
//
// To the FIRST nameserver in /etc/resolv.conf, over UDP, asked again over TCP
// when the reply comes back truncated. net.Resolver has no method that answers
// one hop -- its LookupCNAME follows the chain and reports the end -- so this is
// a question of our own, built with golang.org/x/net/dns/dnsmessage, and it is
// sent where the host's other lookups go. In a pod that file names the
// cluster's DNS service, which forwards to the resolver the node uses; on a
// developer machine it names the network's resolver. One file, read the same
// way in both, is what makes the walk behave the same in each. It is read at
// every call, so a laptop that changes networks asks its new resolver.
//
// THE FIRST NAMESERVER ONLY, with no failover to the second. The sweep runs
// again in two minutes, and a hop that could not be read is reported on the
// row as exactly that (frontMissDetail) -- a retry loop here would buy a slower
// pass and, on the next one, the same question anyway.
//
// A REPLY IS BELIEVED ONLY WHEN IT ANSWERS THIS QUESTION: its ID, which is
// random, and its question's name, type and class must match. Over UDP anyone
// who can reach the socket can send a reply, so one that does not match is
// ignored and the read goes on until the deadline (parseReply).
//
// KNOWN LIMITATION, macOS: the file there is generated from the network
// configuration and lists the primary resolver only. A scoped resolver the
// system consults for some domains (split DNS from a VPN) is not in it, so on
// such a machine a hop for one of those domains can disagree with LookupCNAME.
// A pod has no such resolver.
func (s *SystemResolver) CNAMEHop(ctx context.Context, name string) (string, error) {
	server := s.nameserver
	if server == "" {
		var err error
		if server, err = firstNameserver(resolvConfPath); err != nil {
			return "", &net.DNSError{Err: err.Error(), Name: NormalizeHostname(name)}
		}
	}
	return askCNAME(ctx, server, name)
}

// firstNameserver returns the first usable `nameserver` line of a
// resolv.conf-format file, as host:port.
//
// An address that does not parse is skipped, the way the system resolver skips
// it. A file naming none is an error rather than a guess at 127.0.0.1: a hop
// sent to a resolver nobody configured fails as a timeout naming nothing,
// where this names the file to look at.
func firstNameserver(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("cannot read the resolver configuration %s: %w", path, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		if _, perr := netip.ParseAddr(fields[1]); perr != nil {
			continue
		}
		return net.JoinHostPort(fields[1], "53"), nil
	}
	return "", fmt.Errorf("the resolver configuration %s names no nameserver", path)
}

// askCNAME sends one CNAME question for name to server and returns the target
// of the CNAME record the reply carries for that name, or "" when it carries
// none. Every failure is a *net.DNSError, so dnsErrorText renders it the way it
// renders the other lookups' failures.
func askCNAME(ctx context.Context, server, name string) (string, error) {
	host := NormalizeHostname(name)
	fail := func(text string) error {
		return &net.DNSError{Err: text, Name: host, Server: server}
	}
	if host == "" {
		return "", fail("no name to ask about")
	}
	qname, err := dnsmessage.NewName(host + ".")
	if err != nil {
		return "", fail(err.Error())
	}
	q := dnsmessage.Question{Name: qname, Type: dnsmessage.TypeCNAME, Class: dnsmessage.ClassINET}
	var idBytes [2]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return "", fail(err.Error())
	}
	id := binary.BigEndian.Uint16(idBytes[:])
	// Recursion is requested: the server is a recursive resolver, answering
	// for zones it does not hold.
	query, err := (&dnsmessage.Message{
		Header:    dnsmessage.Header{ID: id, RecursionDesired: true},
		Questions: []dnsmessage.Question{q},
	}).Pack()
	if err != nil {
		return "", fail(err.Error())
	}

	reply, err := exchange(ctx, "udp", server, query, id, q)
	if err == nil && reply.header.Truncated {
		// The reply did not fit a datagram. TCP carries it whole.
		reply, err = exchange(ctx, "tcp", server, query, id, q)
	}
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return "", &net.DNSError{Err: server + " did not answer in time", Name: host, Server: server, IsTimeout: true}
		}
		return "", fail(err.Error())
	}

	switch reply.header.RCode {
	case dnsmessage.RCodeSuccess:
	case dnsmessage.RCodeNameError:
		return "", &net.DNSError{Err: "no such host", Name: host, Server: server, IsNotFound: true}
	default:
		return "", fail("server misbehaving (" + reply.header.RCode.String() + ")")
	}
	for _, a := range reply.answers {
		if a.Header.Type != dnsmessage.TypeCNAME || !strings.EqualFold(a.Header.Name.String(), qname.String()) {
			continue
		}
		if c, ok := a.Body.(*dnsmessage.CNAMEResource); ok {
			return c.CNAME.String(), nil
		}
	}
	return "", nil
}

// dnsReply is the part of a nameserver's reply askCNAME reads.
type dnsReply struct {
	header  dnsmessage.Header
	answers []dnsmessage.Resource
}

// exchange sends query to server over network ("udp" or "tcp") and returns the
// reply that answers it, within cnameHopTimeout.
func exchange(ctx context.Context, network, server string, query []byte, id uint16, q dnsmessage.Question) (dnsReply, error) {
	ctx, cancel := context.WithTimeout(ctx, cnameHopTimeout)
	defer cancel()
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, network, server)
	if err != nil {
		return dnsReply{}, err
	}
	defer conn.Close()
	// The deadline ends a read that is waiting; AfterFunc ends one when the
	// caller cancels without a deadline of its own.
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()

	if network == "tcp" {
		return exchangeStream(conn, query, id, q)
	}
	return exchangeDatagram(conn, query, id, q)
}

// exchangeDatagram is the UDP half: write the question, then read until a reply
// answers it or the deadline passes.
func exchangeDatagram(conn net.Conn, query []byte, id uint16, q dnsmessage.Question) (dnsReply, error) {
	if _, err := conn.Write(query); err != nil {
		return dnsReply{}, err
	}
	buf := make([]byte, 65535)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return dnsReply{}, err
		}
		if reply, ok := parseReply(buf[:n], id, q); ok {
			return reply, nil
		}
		// Not a reply to this question. Ignored rather than believed: see
		// CNAMEHop.
	}
}

// exchangeStream is the TCP half: one question and one reply, each prefixed
// with its two-byte length (RFC 1035 section 4.2.2).
func exchangeStream(conn net.Conn, query []byte, id uint16, q dnsmessage.Question) (dnsReply, error) {
	if _, err := conn.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(query))), query...)); err != nil {
		return dnsReply{}, err
	}
	var size [2]byte
	if _, err := io.ReadFull(conn, size[:]); err != nil {
		return dnsReply{}, err
	}
	msg := make([]byte, binary.BigEndian.Uint16(size[:]))
	if _, err := io.ReadFull(conn, msg); err != nil {
		return dnsReply{}, err
	}
	reply, ok := parseReply(msg, id, q)
	if !ok || reply.header.Truncated {
		return dnsReply{}, errors.New("the reply over TCP does not answer the question asked")
	}
	return reply, nil
}

// parseReply reads msg as the reply to question q asked under id, and answers
// false for anything else: a malformed message, a query rather than a
// response, another ID, or another question.
func parseReply(msg []byte, id uint16, q dnsmessage.Question) (dnsReply, bool) {
	var p dnsmessage.Parser
	h, err := p.Start(msg)
	if err != nil || !h.Response || h.ID != id {
		return dnsReply{}, false
	}
	got, err := p.Question()
	if err != nil || got.Type != q.Type || got.Class != q.Class || !strings.EqualFold(got.Name.String(), q.Name.String()) {
		return dnsReply{}, false
	}
	if h.Truncated {
		// The sections after the question may be cut off mid-record; the
		// caller asks again over TCP rather than reading them.
		return dnsReply{header: h}, true
	}
	if err := p.SkipAllQuestions(); err != nil {
		return dnsReply{}, false
	}
	answers, err := p.AllAnswers()
	if err != nil {
		return dnsReply{}, false
	}
	return dnsReply{header: h, answers: answers}, true
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
	// ReasonRemovalFailed is the unbind not going through. Its own reason
	// rather than issuance_failed, because nothing is being issued: a panel
	// that read "the certificate could not be issued" on a binding somebody
	// asked to take down would be describing the opposite of what happened.
	ReasonRemovalFailed = "removal_failed"
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
//
// # A declared front (frontedBy)
//
// A hostname served through a fronting proxy -- a CDN, or an edge network that
// takes the visitor's connection and forwards it here -- does not point at this
// cluster in DNS at all. It CNAMEs to the front's endpoint name, and resolves
// to the front's addresses. Both shapes above refuse it, and on DNS evidence
// alone they are right to.
//
// frontedBy is the binding naming its front. When it is set, a CNAME chain from
// the hostname that passes through EXACTLY that name within frontChainHopLimit
// hops counts as pointing. Exactly, not by suffix: a declaration naming the
// front's parent domain would otherwise admit every endpoint that network
// serves, for every one of its customers. The front is a hostname and nothing
// more -- no branch here knows any provider -- and it widens rather than
// narrows: a fronted binding whose hostname points at this cluster directly
// still passes the plain check, which is also all that runs when no front is
// declared. The walk is CNAMEHop, one question per hop, because LookupCNAME
// reports only a chain's end and the front is a name in its middle.
//
// OWNERSHIP IS NOT RELAXED, and that is what makes relaxing this safe. The
// declaration widens only what counts as pointing. Who may bind the hostname is
// still decided by CheckOwnership's TXT token, which the reconciler requires
// before this check on every pass (Reconciler.verify) -- so naming somebody
// else's front gets a caller nothing: the chain may well pass through it, but
// the token cannot be published in a zone they do not control.
//
// WHAT A CHAIN CANNOT SHOW is where the front forwards. That is proved later,
// and not by DNS: `live` still waits for a Ready certificate, and an HTTP-01
// order goes Ready only when the challenge, requested at the hostname, reaches
// this cluster through the front. A front forwarding anywhere else leaves the
// binding at `issuing`, saying why.
//
// KNOWN LIMITATION: an APEX cannot be fronted this way. A front at a zone apex
// is reached through an alias or a flattened record, which resolves to the
// front's addresses with no CNAME to follow, so the walk ends at once and the
// plain check refuses the front's addresses.
func CheckPointing(ctx context.Context, r Resolver, hostname, edgeHost, frontedBy string) CheckResult {
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

	front := NormalizeHostname(frontedBy)
	// A HOSTNAME IS NOT ITS OWN FRONT. The walk starts at the hostname's first
	// CNAME, so the only way it could meet the hostname again is a loop back to
	// it -- a zone that does not resolve -- and that must not read as passing
	// through the declared name. Such a declaration is treated as none.
	if front == "" || front == host {
		return checkPointingHere(ctx, r, host, edge)
	}
	passedThrough, reached, err := followChainToFront(ctx, r, host, front)
	if reached {
		return passed()
	}
	if direct := checkPointingHere(ctx, r, host, edge); direct.OK {
		return direct
	}
	// Both failed. The detail names the FRONT rather than the edge host: the
	// operator declared one, and "it should be a CNAME to <edge>" would tell
	// them to route around it.
	return failed(ReasonNotPointing, frontMissDetail(host, front, passedThrough, err))
}

// frontChainHopLimit is how many hops from the hostname the declared front may
// sit in its CNAME chain.
//
// EIGHT, because a real chain through a front is short -- the hostname's own
// CNAME usually names the front at the first hop, and the front's network adds
// a few more beyond it -- so the limit never decides a working configuration.
// What it decides is a broken one: a loop (a -> b -> a) is followed to the
// limit and refused, so a misconfigured zone costs at most eight questions a
// pass rather than a walk that never ends.
const frontChainHopLimit = 8

// followChainToFront walks the CNAME chain from host, one record at a time,
// looking for front. It returns the names it passed through (host itself
// excluded), whether front was among them, and the error that stopped it.
//
// It stops at the first of four: the front; a name with no CNAME, which is the
// chain's end; a hop that could not be read; frontChainHopLimit hops.
func followChainToFront(ctx context.Context, r Resolver, host, front string) (passedThrough []string, reached bool, err error) {
	name := host
	for len(passedThrough) < frontChainHopLimit {
		target, herr := r.CNAMEHop(ctx, name)
		if herr != nil {
			return passedThrough, false, herr
		}
		next := NormalizeHostname(target)
		if next == "" {
			return passedThrough, false, nil
		}
		passedThrough = append(passedThrough, next)
		if next == front {
			return passedThrough, true, nil
		}
		name = next
	}
	return passedThrough, false, nil
}

// frontMissDetail says why a fronted binding's chain did not reach its front:
// the front it declared, and the chain as far as it was followed, which is the
// line of a zone file somebody has to change.
func frontMissDetail(host, front string, passedThrough []string, err error) string {
	chain := strings.Join(append([]string{host}, passedThrough...), " -> ")
	switch {
	case err != nil:
		at := host
		if len(passedThrough) > 0 {
			at = passedThrough[len(passedThrough)-1]
		}
		return fmt.Sprintf("%s is declared fronted by %s, but the CNAME at %s could not be read (%s); followed so far: %s",
			host, front, at, dnsErrorText(err), chain)
	case len(passedThrough) == 0:
		return fmt.Sprintf("%s is declared fronted by %s but carries no CNAME; it needs a CNAME to %s",
			host, front, front)
	case len(passedThrough) >= frontChainHopLimit:
		return fmt.Sprintf("%s is declared fronted by %s, but its CNAME chain (%s) does not reach it within %d hops; it needs a CNAME to %s",
			host, front, chain, frontChainHopLimit, front)
	default:
		return fmt.Sprintf("%s is declared fronted by %s, but its CNAME chain (%s) does not pass through it; it needs a CNAME to %s",
			host, front, chain, front)
	}
}

// checkPointingHere is the plain check: the hostname reaches this cluster
// itself, by CNAME to the edge host or by addresses that are all the edge
// host's. host and edge arrive normalized and non-empty.
func checkPointingHere(ctx context.Context, r Resolver, host, edge string) CheckResult {
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
	// Leaving an old address beside the new one sends some visitors elsewhere.
	// Every published A/AAAA destination must belong to the current edge.
	allPointHere := len(addrs) > 0
	for _, a := range addrs {
		if !want[strings.TrimSpace(a)] {
			allPointHere = false
		}
	}
	if allPointHere {
		return passed()
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
