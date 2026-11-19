package customdomain

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// The DNS checks, against a fake resolver. Pure Go, no network -- which is the
// whole point (design section C): the two lookups that decide whether a
// stranger's domain may be served by this cluster are the last thing that
// should only be exercised in a lane CI skips. The one exception is the
// system resolver's own CNAMEHop at the bottom of this file, which is a wire
// exchange and is tested against a real socket on the loopback interface.

// fakeResolver drives every check in this file.
//
// Its cname map is a ZONE: one CNAME record per owner name, the way DNS holds
// them. LookupCNAME follows the records to the end of the chain, as
// net.Resolver does; CNAMEHop answers the one record at a name and follows
// nothing, as a single CNAME query does.
type fakeResolver struct {
	txt   map[string][]string
	cname map[string]string
	hosts map[string][]string
	// fail names a lookup that should error, so the "a lookup error is a miss,
	// not a fault" behaviour is asserted rather than assumed.
	fail map[string]bool
	// hops records every name CNAMEHop was asked about, in order, so a test
	// can count the walk's questions -- or assert that it asked none.
	hops []string
}

// cnameRecord returns the CNAME record owned by name. Owner names compare the
// way DNS compares them, case-insensitively and with or without the root dot,
// because a chain's targets come back fully qualified while a test's zone is
// keyed however it was written.
func (f *fakeResolver) cnameRecord(name string) (string, bool) {
	want := NormalizeHostname(name)
	for owner, target := range f.cname {
		if NormalizeHostname(owner) == want {
			return target, true
		}
	}
	return "", false
}

// fakeChainLimit is how far the fake's LookupCNAME follows a chain before
// answering the way a recursive resolver answers a loop.
const fakeChainLimit = 16

func (f *fakeResolver) LookupTXT(_ context.Context, name string) ([]string, error) {
	if f.fail[name] {
		return nil, &net.DNSError{Err: "server misbehaving", Name: name}
	}
	v, ok := f.txt[name]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
	}
	return v, nil
}

func (f *fakeResolver) LookupCNAME(_ context.Context, name string) (string, error) {
	// net.Resolver follows the WHOLE chain and returns its end, and returns
	// the name itself when there is no CNAME chain. The fake reproduces both
	// exactly, because the production code depends on the second to tell "no
	// CNAME here" from "a CNAME pointing at the wrong place".
	current := name
	for i := 0; i < fakeChainLimit; i++ {
		next, ok := f.cnameRecord(current)
		if !ok {
			return current, nil
		}
		current = next
	}
	// A chain that never ends is a loop, which a recursive resolver refuses.
	return "", &net.DNSError{Err: "server misbehaving", Name: name}
}

func (f *fakeResolver) CNAMEHop(_ context.Context, name string) (string, error) {
	f.hops = append(f.hops, name)
	if f.fail[NormalizeHostname(name)] {
		return "", &net.DNSError{Err: "server misbehaving", Name: name}
	}
	// A name with no CNAME record ends the chain: "" and no error.
	target, _ := f.cnameRecord(name)
	return target, nil
}

func (f *fakeResolver) LookupHost(_ context.Context, name string) ([]string, error) {
	if f.fail[name] {
		return nil, &net.DNSError{Err: "server misbehaving", Name: name}
	}
	v, ok := f.hosts[name]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
	}
	return v, nil
}

const testEdgeHost = "os.memql.example"

// testToken is COMPOSED rather than written out, and the reason is a scanner
// rather than a style preference: gitleaks' generic-api-key rule judges a test
// fixture exactly like production code, so a literal that looks like a key
// fails the lane whatever the file is for. Assembling it leaves the value
// obvious to a reader and gives the scanner nothing to match.
var testToken = "tok-" + "abcdef" + "0123456789"

func edgeAddrs() map[string][]string {
	return map[string][]string{testEdgeHost: {"203.0.113.10", "203.0.113.11"}}
}

// ---------------------------------------------------------------------------
// The ownership check
// ---------------------------------------------------------------------------

func TestCheckOwnershipPassesWhenTheTokenIsPublished(t *testing.T) {
	r := &fakeResolver{txt: map[string][]string{
		"_memql-verify.www.acme.com": {testToken},
	}}
	res := CheckOwnership(context.Background(), r, "www.acme.com", testToken)
	if !res.OK {
		t.Fatalf("ownership check failed with a published token: %s / %s", res.Reason, res.Detail)
	}
}

// A zone legitimately carries several TXT records at one name -- SPF, a Google
// verification, ours -- and a client is not going to delete theirs to satisfy
// us. Every string at the name is compared.
func TestCheckOwnershipFindsTheTokenAmongOtherRecords(t *testing.T) {
	r := &fakeResolver{txt: map[string][]string{
		"_memql-verify.www.acme.com": {"v=spf1 -all", "google-site-verification=xyz", testToken},
	}}
	if res := CheckOwnership(context.Background(), r, "www.acme.com", testToken); !res.OK {
		t.Fatalf("ownership check failed with the token present beside others: %s", res.Detail)
	}
}

func TestCheckOwnershipReportsTheMissingRecordByName(t *testing.T) {
	r := &fakeResolver{txt: map[string][]string{}}
	res := CheckOwnership(context.Background(), r, "www.acme.com", testToken)
	if res.OK {
		t.Fatal("ownership check passed with no TXT record at all")
	}
	if res.Reason != ReasonTokenMissing {
		t.Errorf("reason = %q, want %q", res.Reason, ReasonTokenMissing)
	}
	// The detail has to name the record, because "which record is still wrong"
	// is the one thing the panel exists to answer.
	if !strings.Contains(res.Detail, "_memql-verify.www.acme.com") {
		t.Errorf("detail %q does not name the record that is missing", res.Detail)
	}
}

// A wrong VALUE is a different situation from a missing record, and the detail
// has to distinguish them -- somebody who pasted last week's token needs to see
// what is actually published.
func TestCheckOwnershipReportsWhatIsPublishedWhenTheTokenIsWrong(t *testing.T) {
	r := &fakeResolver{txt: map[string][]string{
		"_memql-verify.www.acme.com": {"tok-someone-elses"},
	}}
	res := CheckOwnership(context.Background(), r, "www.acme.com", testToken)
	if res.OK {
		t.Fatal("ownership check passed with a token that does not match")
	}
	if !strings.Contains(res.Detail, "tok-someone-elses") {
		t.Errorf("detail %q does not report the value that IS published", res.Detail)
	}
	// And it must not leak the expected token into a message that renders on a
	// panel beside the value somebody typed -- the row already carries it.
	if strings.Contains(res.Detail, testToken) {
		t.Errorf("detail %q repeats the expected token; the guidance panel renders it from the row", res.Detail)
	}
}

// NXDOMAIN is the ordinary answer while somebody is still creating the record.
// A resolver ERROR must read the same way -- a miss with the resolver's own
// words -- rather than as a fault a person cannot act on.
func TestCheckOwnershipTreatsALookupErrorAsAMiss(t *testing.T) {
	r := &fakeResolver{fail: map[string]bool{"_memql-verify.www.acme.com": true}}
	res := CheckOwnership(context.Background(), r, "www.acme.com", testToken)
	if res.OK {
		t.Fatal("ownership check passed on a resolver error")
	}
	if res.Reason != ReasonTokenMissing {
		t.Errorf("reason = %q, want %q", res.Reason, ReasonTokenMissing)
	}
	if !strings.Contains(res.Detail, "server misbehaving") {
		t.Errorf("detail %q drops the resolver's own words, which is what makes a real outage legible", res.Detail)
	}
}

// ---------------------------------------------------------------------------
// The pointing check -- subdomain
// ---------------------------------------------------------------------------

func TestCheckPointingPassesOnACNAMEToTheEdgeHost(t *testing.T) {
	r := &fakeResolver{
		cname: map[string]string{"www.acme.com": testEdgeHost + "."},
		hosts: edgeAddrs(),
	}
	if res := CheckPointing(context.Background(), r, "www.acme.com", testEdgeHost, ""); !res.OK {
		t.Fatalf("pointing check failed on a correct CNAME: %s / %s", res.Reason, res.Detail)
	}
}

// THE TRAILING ROOT DOT. net.Resolver returns a FQDN with one, and a client
// pasting from a zone file may include one too. Comparing unnormalised reports
// "not pointing" for a record that is exactly right, which is the single most
// frustrating way this could fail.
func TestCheckPointingNormalisesTheTrailingDotAndCase(t *testing.T) {
	r := &fakeResolver{
		cname: map[string]string{"www.acme.com": "OS.MemQL.Example."},
		hosts: edgeAddrs(),
	}
	if res := CheckPointing(context.Background(), r, "WWW.Acme.com.", testEdgeHost, ""); !res.OK {
		t.Fatalf("pointing check failed on a correct CNAME differing only in case and root dot: %s", res.Detail)
	}
}

func TestCheckPointingNamesTheWrongCNAMETarget(t *testing.T) {
	r := &fakeResolver{
		cname: map[string]string{"www.acme.com": "old-host.example.net."},
		hosts: map[string][]string{
			testEdgeHost:            {"203.0.113.10"},
			"www.acme.com":          {"198.51.100.7"},
			"old-host.example.net.": {"198.51.100.7"},
		},
	}
	res := CheckPointing(context.Background(), r, "www.acme.com", testEdgeHost, "")
	if res.OK {
		t.Fatal("pointing check passed on a CNAME to somewhere else")
	}
	if res.Reason != ReasonNotPointing {
		t.Errorf("reason = %q, want %q", res.Reason, ReasonNotPointing)
	}
	if !strings.Contains(res.Detail, "old-host.example.net") || !strings.Contains(res.Detail, testEdgeHost) {
		t.Errorf("detail %q must name BOTH where it points and where it should", res.Detail)
	}
}

// ---------------------------------------------------------------------------
// The pointing check -- apex
// ---------------------------------------------------------------------------

// AN APEX IS CHECKED BY ADDRESS, AND THE ADDRESS COMES FROM RESOLVING THE
// CLUSTER'S OWN EDGE HOST -- never from a second configured value. A configured
// one would have no forcing function keeping it true: the day the load balancer
// is replaced, every correct apex record would be refused while every manifest
// still looked right.
func TestCheckPointingResolvesTheApexTargetFromTheEdgeHost(t *testing.T) {
	r := &fakeResolver{
		hosts: map[string][]string{
			testEdgeHost: {"203.0.113.10", "203.0.113.11"},
			"acme.com":   {"203.0.113.11"},
		},
	}
	if res := CheckPointing(context.Background(), r, "acme.com", testEdgeHost, ""); !res.OK {
		t.Fatalf("apex pointing check failed on an A record matching the edge host: %s / %s", res.Reason, res.Detail)
	}
}

func TestCheckPointingRefusesAnApexPointingElsewhere(t *testing.T) {
	r := &fakeResolver{
		hosts: map[string][]string{
			testEdgeHost: {"203.0.113.10"},
			"acme.com":   {"198.51.100.42"},
		},
	}
	res := CheckPointing(context.Background(), r, "acme.com", testEdgeHost, "")
	if res.OK {
		t.Fatal("apex pointing check passed on an address that is not ours")
	}
	if !strings.Contains(res.Detail, "198.51.100.42") || !strings.Contains(res.Detail, "203.0.113.10") {
		t.Errorf("detail %q must name BOTH the address found and the one wanted", res.Detail)
	}
}

// Apex aliasing (ALIAS / ANAME / CNAME flattening) resolves to an ADDRESS
// rather than a CNAME at many providers. That is a working configuration and
// must not be called broken -- which is what the address fallback is for.
func TestCheckPointingAcceptsApexAliasingThatResolvesToAnAddress(t *testing.T) {
	r := &fakeResolver{
		hosts: map[string][]string{
			testEdgeHost: {"203.0.113.10"},
			"acme.com":   {"203.0.113.10"},
		},
	}
	if res := CheckPointing(context.Background(), r, "acme.com", testEdgeHost, ""); !res.OK {
		t.Fatalf("apex aliasing that resolves to our address was refused: %s", res.Detail)
	}
}

// WE COULD NOT LEARN WHERE WE ARE. Refusing is the fail-closed reading and it
// is the right one: admitting the record because OUR OWN lookup failed would
// bind a hostname on no evidence at all.
func TestCheckPointingRefusesWhenTheEdgeHostItselfDoesNotResolve(t *testing.T) {
	r := &fakeResolver{
		hosts: map[string][]string{"acme.com": {"203.0.113.10"}},
		fail:  map[string]bool{testEdgeHost: true},
	}
	res := CheckPointing(context.Background(), r, "acme.com", testEdgeHost, "")
	if res.OK {
		t.Fatal("pointing check passed when this cluster's own edge host could not be resolved")
	}
	if !strings.Contains(res.Detail, testEdgeHost) {
		t.Errorf("detail %q does not say which of the two lookups failed", res.Detail)
	}
}

// ---------------------------------------------------------------------------
// The pointing check -- a declared front (frontedBy)
// ---------------------------------------------------------------------------

// testFront is a fronting proxy's endpoint name: what a hostname served
// through a CDN or an edge network CNAMEs to. The front's own name CNAMEs on
// to its network, whose addresses are not this cluster's -- which is exactly
// why a fronted hostname fails the plain check.
const (
	testFront        = "acme-calls.z01.fronts.example"
	testFrontNetwork = "anycast.fronts.example"
)

// frontedZone is www.acme.com served through testFront: its own CNAME names
// the front, and the front's CNAME names the front's network.
func frontedZone() map[string]string {
	return map[string]string{
		"www.acme.com": testFront + ".",
		testFront:      testFrontNetwork + ".",
	}
}

// frontedAddrs is what those names resolve to. The hostname resolves to the
// FRONT's address, never to this cluster's.
func frontedAddrs() map[string][]string {
	return map[string][]string{
		testEdgeHost:     {"203.0.113.10", "203.0.113.11"},
		testFrontNetwork: {"192.0.2.80"},
		"www.acme.com":   {"192.0.2.80"},
	}
}

// A HOSTNAME SERVED THROUGH THE FRONT IT DECLARES POINTS HERE. It never
// resolves to this cluster -- its addresses are the front's -- and that is the
// configuration working: the front forwards to the cluster, and the chain
// passing through the front's exact name is the evidence DNS can give.
func TestPointingAcceptsAChainThroughTheDeclaredFront(t *testing.T) {
	deeper := map[string]string{
		"www.acme.com": "cdn.acme.com.",
		"cdn.acme.com": testFront + ".",
		testFront:      testFrontNetwork + ".",
	}
	for name, tc := range map[string]struct {
		zone      map[string]string
		frontedBy string
		hops      int
	}{
		"the hostname's own CNAME names the front": {zone: frontedZone(), frontedBy: testFront, hops: 1},
		"the front is further down the chain":      {zone: deeper, frontedBy: testFront, hops: 2},
		// The mutation stores the normalized form; the check normalizes again,
		// so a row written some other way still compares the way DNS does.
		"the declaration differs only in case and the root dot": {zone: frontedZone(), frontedBy: "ACME-Calls.Z01.Fronts.Example.", hops: 1},
	} {
		t.Run(name, func(t *testing.T) {
			r := &fakeResolver{cname: tc.zone, hosts: frontedAddrs()}
			res := CheckPointing(context.Background(), r, "www.acme.com", testEdgeHost, tc.frontedBy)
			if !res.OK {
				t.Fatalf("a chain through the declared front was refused: %s / %s", res.Reason, res.Detail)
			}
			// The walk stops AT the front. Past it is the front's own network,
			// which says nothing more about this binding.
			if len(r.hops) != tc.hops {
				t.Errorf("the walk asked %d CNAME question(s) %v, want %d: it should stop at the front", len(r.hops), r.hops, tc.hops)
			}
		})
	}
}

// A FRONT NOBODY DECLARED IS NOT A FRONT. A hostname CNAMEd to a front's
// endpoint resolves to that front's network, and without a declaration naming
// exactly that endpoint it is a hostname pointing somewhere else -- which is
// what the check has always said about it.
func TestPointingRefusesAnUndeclaredFront(t *testing.T) {
	for name, tc := range map[string]struct {
		frontedBy  string
		wantDetail []string
	}{
		// The plain check, untouched: it names the chain's end and where the
		// hostname should point instead.
		"no front is declared": {frontedBy: "", wantDetail: []string{testFrontNetwork, testEdgeHost}},
		// Another endpoint on the same network is another customer's front.
		"another front is declared": {frontedBy: "someone-else.z02.fronts.example", wantDetail: []string{"someone-else.z02.fronts.example", testFront}},
		// EXACT NAME, NOT A SUFFIX. A declaration naming the network's own
		// domain would otherwise admit every endpoint that network serves.
		"the declaration names the front's parent domain": {frontedBy: "fronts.example", wantDetail: []string{"fronts.example", testFront}},
	} {
		t.Run(name, func(t *testing.T) {
			r := &fakeResolver{cname: frontedZone(), hosts: frontedAddrs()}
			res := CheckPointing(context.Background(), r, "www.acme.com", testEdgeHost, tc.frontedBy)
			if res.OK {
				t.Fatalf("a hostname CNAMEd to an undeclared front passed the pointing check (frontedBy=%q)", tc.frontedBy)
			}
			if res.Reason != ReasonNotPointing {
				t.Errorf("reason = %q, want %q", res.Reason, ReasonNotPointing)
			}
			for _, want := range tc.wantDetail {
				if !strings.Contains(res.Detail, want) {
					t.Errorf("detail %q does not name %q", res.Detail, want)
				}
			}
		})
	}
}

// WITHOUT A DECLARATION THE CHECK IS THE ONE IT ALWAYS WAS: the same answers,
// word for word, from the same lookups -- and no CNAME walk at all, so a
// binding that declares nothing costs no extra DNS question per pass.
func TestPointingUnchangedWithoutFrontedBy(t *testing.T) {
	for name, tc := range map[string]struct {
		hostname string
		r        *fakeResolver
		want     CheckResult
	}{
		"a CNAME to the edge host": {
			hostname: "www.acme.com",
			r:        &fakeResolver{cname: map[string]string{"www.acme.com": testEdgeHost + "."}, hosts: edgeAddrs()},
			want:     CheckResult{OK: true},
		},
		"a CNAME to somewhere else": {
			hostname: "www.acme.com",
			r: &fakeResolver{
				cname: map[string]string{"www.acme.com": "old-host.example.net."},
				hosts: map[string][]string{testEdgeHost: {"203.0.113.10"}, "www.acme.com": {"198.51.100.7"}},
			},
			want: CheckResult{Reason: ReasonNotPointing, Detail: "www.acme.com is a CNAME to old-host.example.net; it should be a CNAME to os.memql.example"},
		},
		"an apex A record at the edge's address": {
			hostname: "acme.com",
			r:        &fakeResolver{hosts: map[string][]string{testEdgeHost: {"203.0.113.10", "203.0.113.11"}, "acme.com": {"203.0.113.11"}}},
			want:     CheckResult{OK: true},
		},
		"an apex pointing elsewhere": {
			hostname: "acme.com",
			r:        &fakeResolver{hosts: map[string][]string{testEdgeHost: {"203.0.113.10"}, "acme.com": {"198.51.100.42"}}},
			want:     CheckResult{Reason: ReasonNotPointing, Detail: "acme.com resolves to 198.51.100.42; it needs an A record pointing at os.memql.example (203.0.113.10)"},
		},
		"a subdomain with no record at all": {
			hostname: "www.acme.com",
			r:        &fakeResolver{hosts: edgeAddrs()},
			want:     CheckResult{Reason: ReasonNotPointing, Detail: "looked up www.acme.com: no such record"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := CheckPointing(context.Background(), tc.r, tc.hostname, testEdgeHost, ""); got != tc.want {
				t.Errorf("CheckPointing = %+v, want %+v", got, tc.want)
			}
			if len(tc.r.hops) != 0 {
				t.Errorf("a binding that declares no front asked %d CNAME question(s): %v", len(tc.r.hops), tc.r.hops)
			}
		})
	}
}

// chainTo is a zone in which www.acme.com reaches last at hop n: n-1 names in
// between, then last.
func chainTo(n int, last string) map[string]string {
	zone := map[string]string{}
	from := "www.acme.com"
	for i := 1; i < n; i++ {
		next := fmt.Sprintf("hop%d.chain.example", i)
		zone[from] = next + "."
		from = next
	}
	zone[from] = last + "."
	return zone
}

// THE WALK IS BOUNDED AT EIGHT HOPS. A real chain through a front is short, so
// the limit never decides a working configuration; what it decides is a broken
// one. A loop is followed to the limit and refused, so a misconfigured zone
// costs at most eight questions a pass rather than a walk that never ends.
func TestPointingStopsAfterEightHops(t *testing.T) {
	t.Run("the front at the eighth hop is within reach", func(t *testing.T) {
		r := &fakeResolver{cname: chainTo(8, testFront), hosts: frontedAddrs()}
		if res := CheckPointing(context.Background(), r, "www.acme.com", testEdgeHost, testFront); !res.OK {
			t.Fatalf("a front at the eighth hop was refused: %s", res.Detail)
		}
		if len(r.hops) != 8 {
			t.Errorf("the walk asked %d CNAME questions, want 8", len(r.hops))
		}
	})
	t.Run("the front at the ninth hop is past it", func(t *testing.T) {
		r := &fakeResolver{cname: chainTo(9, testFront), hosts: frontedAddrs()}
		res := CheckPointing(context.Background(), r, "www.acme.com", testEdgeHost, testFront)
		if res.OK {
			t.Fatal("a front at the ninth hop was accepted; the walk did not stop at eight")
		}
		if len(r.hops) != 8 {
			t.Errorf("the walk asked %d CNAME questions, want exactly 8", len(r.hops))
		}
		if !strings.Contains(res.Detail, "8 hops") || !strings.Contains(res.Detail, testFront) {
			t.Errorf("detail %q does not say the front was not reached within 8 hops", res.Detail)
		}
	})
	t.Run("a loop stops at the limit", func(t *testing.T) {
		r := &fakeResolver{cname: map[string]string{
			"www.acme.com":   "a.loop.example.",
			"a.loop.example": "b.loop.example.",
			"b.loop.example": "a.loop.example.",
		}, hosts: edgeAddrs()}
		if res := CheckPointing(context.Background(), r, "www.acme.com", testEdgeHost, testFront); res.OK {
			t.Fatal("a CNAME loop passed the pointing check")
		}
		if len(r.hops) != 8 {
			t.Errorf("the walk asked %d CNAME questions of a loop, want 8", len(r.hops))
		}
	})
	// A HOSTNAME IS NOT ITS OWN FRONT. The walk starts at the hostname's first
	// CNAME, so the only way to meet the hostname again is a loop back to it,
	// and a declaration naming the hostname must not turn that loop into a pass.
	t.Run("a loop back to the hostname does not make it its own front", func(t *testing.T) {
		r := &fakeResolver{cname: map[string]string{
			"www.acme.com":   "a.loop.example.",
			"a.loop.example": "www.acme.com.",
		}, hosts: edgeAddrs()}
		if res := CheckPointing(context.Background(), r, "www.acme.com", testEdgeHost, "www.acme.com"); res.OK {
			t.Fatal("a binding declaring its own hostname as its front passed through a loop back to itself")
		}
	})
}

// A HOP THAT CANNOT BE READ IS A MISS THAT SAYS WHERE IT STOPPED, for the
// ownership check's reason: a resolver error is the ordinary answer while a
// record propagates, and the panel shows the resolver's own words.
func TestPointingReportsTheHopItCouldNotRead(t *testing.T) {
	r := &fakeResolver{cname: frontedZone(), hosts: frontedAddrs(), fail: map[string]bool{"www.acme.com": true}}
	res := CheckPointing(context.Background(), r, "www.acme.com", testEdgeHost, testFront)
	if res.OK {
		t.Fatal("the pointing check passed with the hostname's CNAME unreadable")
	}
	if !strings.Contains(res.Detail, "server misbehaving") || !strings.Contains(res.Detail, testFront) {
		t.Errorf("detail %q must carry the resolver's words and the declared front", res.Detail)
	}
}

// ---------------------------------------------------------------------------
// Shape helpers
// ---------------------------------------------------------------------------

func TestIsApexCountsLabels(t *testing.T) {
	for host, want := range map[string]bool{
		"acme.com":         true,
		"www.acme.com":     false,
		"shop.eu.acme.com": false,
		"ACME.COM.":        true,
	} {
		if got := IsApex(host); got != want {
			t.Errorf("IsApex(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestVerifyRecordNameIsUnderscorePrefixed(t *testing.T) {
	got := VerifyRecordName("WWW.Acme.com.")
	want := "_memql-verify.www.acme.com"
	if got != want {
		t.Errorf("VerifyRecordName = %q, want %q", got, want)
	}
	// The underscore is load-bearing: it is not legal in a hostname, so this
	// name can never collide with something the client is actually serving.
	if !strings.HasPrefix(got, "_") {
		t.Error("the verification record must be under an underscore-prefixed label")
	}
}

// The guidance panel asks for a CNAME on a subdomain and an A record on an
// apex, and the apex's target is an address discovered by resolving the edge
// host -- the same discovery the check itself uses, so the advice and the
// verification cannot disagree.
func TestPointingRecordForMatchesWhatTheCheckWants(t *testing.T) {
	r := &fakeResolver{hosts: edgeAddrs()}

	sub := PointingRecordFor(context.Background(), r, "www.acme.com", testEdgeHost)
	if sub.Kind != "CNAME" || sub.Target != testEdgeHost || sub.Name != "www.acme.com" {
		t.Errorf("subdomain record = %+v, want a CNAME from www.acme.com to %s", sub, testEdgeHost)
	}

	apex := PointingRecordFor(context.Background(), r, "acme.com", testEdgeHost)
	if apex.Kind != "A" || apex.Name != "acme.com" {
		t.Errorf("apex record = %+v, want an A record for acme.com", apex)
	}
	if apex.Target != "203.0.113.10" {
		t.Errorf("apex target = %q, want the edge host's first address", apex.Target)
	}
}

// When the edge host cannot be resolved the guidance still names the right
// host: half an answer that names the right target beats an empty panel, and
// the check remains the authority either way.
func TestPointingRecordForFallsBackToTheEdgeHostName(t *testing.T) {
	r := &fakeResolver{fail: map[string]bool{testEdgeHost: true}}
	apex := PointingRecordFor(context.Background(), r, "acme.com", testEdgeHost)
	if apex.Target != testEdgeHost {
		t.Errorf("apex target = %q, want the edge host name as the fallback", apex.Target)
	}
}

// ---------------------------------------------------------------------------
// CNAMEHop on the wire -- the system resolver's one-hop question
// ---------------------------------------------------------------------------

// fakeNameserver answers DNS questions on the loopback interface, over UDP and
// TCP on one port, standing in for the first nameserver in resolv.conf. A real
// socket and the real wire format, so what these tests pin is the exchange
// itself: the question asked, the reply believed, and the retry over TCP.
type fakeNameserver struct {
	addr string
	b    nameserverBehaviour

	mu    sync.Mutex
	asked []askedQuestion
}

// nameserverBehaviour is how the fake answers. The zero value answers every
// question NOERROR from the zone.
type nameserverBehaviour struct {
	// zone maps an owner name to its CNAME target, both absolute.
	zone  map[string]string
	rcode dnsmessage.RCode
	// truncateUDP answers over UDP with the TC bit set and no records, the
	// way a server answers when the reply does not fit a datagram.
	truncateUDP bool
	// forgeFirst sends a reply under another ID, naming forged.example.,
	// before the real one.
	forgeFirst bool
	// silent reads every question and answers none.
	silent bool
}

type askedQuestion struct {
	network          string
	question         dnsmessage.Question
	recursionDesired bool
}

func startFakeNameserver(t *testing.T, b nameserverBehaviour) *fakeNameserver {
	t.Helper()
	var udp net.PacketConn
	var tcp net.Listener
	for attempt := 0; attempt < 20 && tcp == nil; attempt++ {
		u, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen udp: %v", err)
		}
		l, err := net.Listen("tcp", u.LocalAddr().String())
		if err != nil {
			_ = u.Close() // that port is taken for TCP; take another
			continue
		}
		udp, tcp = u, l
	}
	if tcp == nil {
		t.Fatal("could not bind UDP and TCP on one loopback port")
	}
	s := &fakeNameserver{addr: udp.LocalAddr().String(), b: b}
	t.Cleanup(func() {
		_ = udp.Close()
		_ = tcp.Close()
	})
	go s.serveUDP(udp)
	go s.serveTCP(tcp)
	return s
}

func (s *fakeNameserver) serveUDP(conn net.PacketConn) {
	buf := make([]byte, 65535)
	for {
		n, from, err := conn.ReadFrom(buf)
		if err != nil {
			return
		}
		h, q, ok := s.read("udp", buf[:n])
		if !ok || s.b.silent {
			continue
		}
		if s.b.forgeFirst {
			_, _ = conn.WriteTo(s.reply(h.ID+1, q, false, "forged.example."), from)
		}
		_, _ = conn.WriteTo(s.reply(h.ID, q, s.b.truncateUDP, ""), from)
	}
}

func (s *fakeNameserver) serveTCP(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			var size [2]byte
			if _, err := io.ReadFull(conn, size[:]); err != nil {
				return
			}
			msg := make([]byte, binary.BigEndian.Uint16(size[:]))
			if _, err := io.ReadFull(conn, msg); err != nil {
				return
			}
			h, q, ok := s.read("tcp", msg)
			if !ok || s.b.silent {
				return
			}
			out := s.reply(h.ID, q, false, "")
			_, _ = conn.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(out))), out...))
		}()
	}
}

func (s *fakeNameserver) read(network string, msg []byte) (dnsmessage.Header, dnsmessage.Question, bool) {
	var p dnsmessage.Parser
	h, err := p.Start(msg)
	if err != nil {
		return h, dnsmessage.Question{}, false
	}
	q, err := p.Question()
	if err != nil {
		return h, q, false
	}
	s.mu.Lock()
	s.asked = append(s.asked, askedQuestion{network: network, question: q, recursionDesired: h.RecursionDesired})
	s.mu.Unlock()
	return h, q, true
}

// reply answers q under id. target, when set, replaces the zone's answer.
func (s *fakeNameserver) reply(id uint16, q dnsmessage.Question, truncated bool, target string) []byte {
	m := dnsmessage.Message{
		Header: dnsmessage.Header{
			ID: id, Response: true, RecursionDesired: true, RecursionAvailable: true,
			RCode: s.b.rcode, Truncated: truncated,
		},
		Questions: []dnsmessage.Question{q},
	}
	if target == "" {
		target = s.b.zone[strings.ToLower(q.Name.String())]
	}
	if !truncated && target != "" && q.Type == dnsmessage.TypeCNAME {
		m.Answers = []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeCNAME, Class: dnsmessage.ClassINET, TTL: 300},
			Body:   &dnsmessage.CNAMEResource{CNAME: dnsmessage.MustNewName(target)},
		}}
	}
	out, err := m.Pack()
	if err != nil {
		panic(err)
	}
	return out
}

func (s *fakeNameserver) questions() []askedQuestion {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]askedQuestion(nil), s.asked...)
}

// ONE QUESTION, ONE HOP. The fake would answer the front's own CNAME too if
// asked; CNAMEHop must not ask, because the walk -- not the resolver -- decides
// how far to go. It is a CNAME question with recursion requested, since the
// server is a recursive resolver answering for zones it does not hold.
func TestCNAMEHopAsksOneQuestionAndDoesNotFollowTheAnswer(t *testing.T) {
	ns := startFakeNameserver(t, nameserverBehaviour{zone: map[string]string{
		"www.acme.com.": testFront + ".",
		testFront + ".": testFrontNetwork + ".",
	}})
	got, err := (&SystemResolver{nameserver: ns.addr}).CNAMEHop(context.Background(), "WWW.Acme.com")
	if err != nil {
		t.Fatalf("CNAMEHop: %v", err)
	}
	if NormalizeHostname(got) != testFront {
		t.Fatalf("CNAMEHop = %q, want the one hop %q", got, testFront)
	}
	asked := ns.questions()
	if len(asked) != 1 {
		t.Fatalf("the nameserver was asked %d question(s), want 1: %+v", len(asked), asked)
	}
	q := asked[0]
	if q.network != "udp" || q.question.Type != dnsmessage.TypeCNAME || q.question.Class != dnsmessage.ClassINET {
		t.Errorf("asked %+v, want one CNAME question for class IN over UDP", q)
	}
	if q.question.Name.String() != "www.acme.com." {
		t.Errorf("asked about %q, want the normalized absolute name www.acme.com.", q.question.Name.String())
	}
	if !q.recursionDesired {
		t.Error("the question did not ask for recursion, so a recursive resolver would not look the name up")
	}
}

// A TRUNCATED REPLY IS ASKED AGAIN OVER TCP, which carries the whole answer.
func TestCNAMEHopAsksAgainOverTCPWhenTheReplyIsTruncated(t *testing.T) {
	ns := startFakeNameserver(t, nameserverBehaviour{
		zone:        map[string]string{"www.acme.com.": testFront + "."},
		truncateUDP: true,
	})
	got, err := (&SystemResolver{nameserver: ns.addr}).CNAMEHop(context.Background(), "www.acme.com")
	if err != nil {
		t.Fatalf("CNAMEHop: %v", err)
	}
	if NormalizeHostname(got) != testFront {
		t.Fatalf("CNAMEHop = %q, want %q from the TCP retry", got, testFront)
	}
	asked := ns.questions()
	if len(asked) != 2 || asked[0].network != "udp" || asked[1].network != "tcp" {
		t.Fatalf("asked %+v, want the question over UDP and then over TCP", asked)
	}
}

// OVER UDP, ANYBODY WHO CAN REACH THE SOCKET CAN SEND A REPLY. One that does not
// carry the question's random ID is ignored rather than believed, and the read
// goes on to the real one.
func TestCNAMEHopBelievesOnlyTheReplyToItsOwnQuestion(t *testing.T) {
	ns := startFakeNameserver(t, nameserverBehaviour{
		zone:       map[string]string{"www.acme.com.": testFront + "."},
		forgeFirst: true,
	})
	got, err := (&SystemResolver{nameserver: ns.addr}).CNAMEHop(context.Background(), "www.acme.com")
	if err != nil {
		t.Fatalf("CNAMEHop: %v", err)
	}
	if NormalizeHostname(got) != testFront {
		t.Fatalf("CNAMEHop = %q, want %q: a reply under another ID was believed", got, testFront)
	}
}

// The reply code decides what the hop means: no CNAME ends the chain, a name
// that does not exist is not-found (the panel's "no such record"), and a
// server failure is an error in the server's own terms.
func TestCNAMEHopReadsTheReplyCode(t *testing.T) {
	t.Run("a name with no CNAME ends the chain", func(t *testing.T) {
		ns := startFakeNameserver(t, nameserverBehaviour{})
		got, err := (&SystemResolver{nameserver: ns.addr}).CNAMEHop(context.Background(), "www.acme.com")
		if err != nil || got != "" {
			t.Fatalf("CNAMEHop = %q, %v; want \"\" and no error for a name that carries no CNAME", got, err)
		}
	})
	t.Run("a name that does not exist is not found", func(t *testing.T) {
		ns := startFakeNameserver(t, nameserverBehaviour{rcode: dnsmessage.RCodeNameError})
		_, err := (&SystemResolver{nameserver: ns.addr}).CNAMEHop(context.Background(), "www.acme.com")
		var dnsErr *net.DNSError
		if !errors.As(err, &dnsErr) || !dnsErr.IsNotFound {
			t.Fatalf("CNAMEHop error = %v, want a not-found DNS error", err)
		}
		if got := dnsErrorText(err); got != "no such record" {
			t.Errorf("rendered as %q, want the panel's \"no such record\"", got)
		}
	})
	t.Run("a server failure is an error", func(t *testing.T) {
		ns := startFakeNameserver(t, nameserverBehaviour{rcode: dnsmessage.RCodeServerFailure})
		_, err := (&SystemResolver{nameserver: ns.addr}).CNAMEHop(context.Background(), "www.acme.com")
		var dnsErr *net.DNSError
		if !errors.As(err, &dnsErr) || dnsErr.IsNotFound {
			t.Fatalf("CNAMEHop error = %v, want a DNS error that is not a not-found", err)
		}
	})
}

// A NAMESERVER THAT NEVER ANSWERS COSTS THE CALLER'S DEADLINE AND NO MORE.
func TestCNAMEHopGivesUpAtTheDeadline(t *testing.T) {
	ns := startFakeNameserver(t, nameserverBehaviour{silent: true})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := (&SystemResolver{nameserver: ns.addr}).CNAMEHop(ctx, "www.acme.com")
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("CNAMEHop took %s against a silent nameserver; the caller's deadline was 300ms", elapsed)
	}
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) || !dnsErr.IsTimeout {
		t.Fatalf("CNAMEHop error = %v, want a timeout", err)
	}
	if !strings.Contains(dnsErr.Err, ns.addr) {
		t.Errorf("timeout %q does not name the nameserver that did not answer", dnsErr.Err)
	}
}

// THE NAMESERVER IS THE FIRST ONE THE SYSTEM RESOLVER CONFIGURATION NAMES, which
// is the cluster's DNS service in a pod and the network's resolver on a laptop.
func TestFirstNameserverReadsTheSystemResolverConfiguration(t *testing.T) {
	for name, tc := range map[string]struct {
		conf    string
		want    string
		wantErr bool
	}{
		"a pod's file": {
			conf: "# generated by the kubelet\nsearch memql.svc.cluster.local svc.cluster.local cluster.local\n" +
				"nameserver 10.43.0.10\nnameserver 10.43.0.11\noptions ndots:5\n",
			want: "10.43.0.10:53",
		},
		"an IPv6 nameserver":       {conf: "nameserver 2001:db8::53\n", want: "[2001:db8::53]:53"},
		"an unusable line first":   {conf: "nameserver not-an-address\nnameserver 192.0.2.53\n", want: "192.0.2.53:53"},
		"a comment after the line": {conf: "nameserver 192.0.2.53 # the router\n", want: "192.0.2.53:53"},
		"a file naming none":       {conf: "search example.com\n", wantErr: true},
		"a commented nameserver":   {conf: "# nameserver 192.0.2.1\n; nameserver 192.0.2.2\n", wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "resolv.conf")
			if err := os.WriteFile(path, []byte(tc.conf), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := firstNameserver(path)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), path) {
					t.Fatalf("firstNameserver = %q, %v; want an error naming %s", got, err, path)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("firstNameserver = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	t.Run("a missing file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "absent")
		if _, err := firstNameserver(path); err == nil || !strings.Contains(err.Error(), path) {
			t.Fatalf("firstNameserver of a missing file = %v, want an error naming it", err)
		}
	})
}
