package customdomain

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/frontdoor"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// The account front door's state machine, end to end, with no database, no
// cluster and no network.
//
// The fake engine PARSES every call for reconcile_test.go's reason: a suite
// that only records the strings a writer produced proves the writer called
// something, not that the engine could execute it. A mutation whose argument
// list does not parse fails here rather than as a warning in a log on a
// cluster nobody is watching.

type fakeDoorEngine struct {
	t *testing.T
	// doors answers the door reads; reservations answers the account read.
	doors        []map[string]any
	reservations []map[string]any
	calls        []string
	err          error
	// reservationErr fails ONLY the account read. `err` fails every query,
	// which cannot express "the reservations are unreadable and the doors are
	// fine" -- the exact state the pass has to report honestly.
	reservationErr error
}

func (f *fakeDoorEngine) Execute(_ context.Context, query string) (any, error) {
	f.calls = append(f.calls, query)
	if _, perr := langparser.ParseExpression(query); perr != nil {
		f.t.Fatalf("the engine was handed a call it cannot parse:\n  %s\n  %v", query, perr)
	}
	if f.err != nil {
		return nil, f.err
	}
	switch {
	case strings.HasPrefix(query, "query accountsWithAReservedName"):
		if f.reservationErr != nil {
			return nil, f.reservationErr
		}
		// THROUGH THE REAL SHAPE (account_walk_shape_test.go). This fake once
		// answered with every fixture field, including a reason the shape
		// did not project, and the reason-skip tests below passed against a
		// sweep that rewrote the reason on every pass on a real engine.
		return accountWalkRows(f.t, f.reservations), nil
	case strings.HasPrefix(query, "query accountFrontDoor"):
		// Every door read answers the same fixture, so a test can hand the
		// sweep a `live` row and assert it is skipped by the work loop while
		// still being seen by the reservation comparison. Narrowing the way
		// the real queries do would make that unmeasurable.
		return f.doors, nil
	case strings.HasPrefix(query, "mutation "):
		return []map[string]any{}, nil
	default:
		// AN UNRECOGNISED CONSTRUCT IS A TEST FAILURE, NOT AN EMPTY RESULT.
		//
		// The first version fell through to `[]map[string]any{}` here, and a
		// review found what that hid: `accountsHoldingAReservedName` does not
		// exist in the DSL yet, so on a real engine the sweep's reservation
		// read fails -- and every test in this file passed anyway, because
		// the fake answered a query no engine would. ParseExpression proves
		// SYNTAX, never existence, so nothing else here could have noticed.
		//
		// This does not make the fake know the real registry. It makes it
		// refuse to invent an answer for a name this file has not deliberately
		// taught it, which is the property that was missing.
		f.t.Fatalf("the engine was asked for a construct this fake does not know:\n  %s\n"+
			"Teach it here deliberately, or fix the caller -- an empty result for an unknown "+
			"name is how a query that exists nowhere passes a whole suite.", query)
		return nil, nil
	}
}

func (f *fakeDoorEngine) wrote(prefix string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(c, "mutation "+prefix) {
			return true
		}
	}
	return false
}

// stubDoorResolver answers CheckPointing per host from a fixture.
type stubDoorResolver struct {
	// pointing maps a hostname to whether its CNAME reaches the edge host.
	pointing map[string]bool
}

func (s stubDoorResolver) LookupTXT(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func (s stubDoorResolver) LookupCNAME(_ context.Context, host string) (string, error) {
	if s.pointing[host] {
		return doorEdgeHost, nil
	}
	return "somewhere-else.example.", nil
}

func (s stubDoorResolver) LookupHost(_ context.Context, host string) ([]string, error) {
	if host == doorEdgeHost || s.pointing[host] {
		return []string{"203.0.113.10"}, nil
	}
	return []string{"198.51.100.7"}, nil
}

const (
	doorEdgeHost     = "os.memql.localhost"
	testDoorReserved = "memql.acme.com"
	testDoorAccount  = "acct-1"
)

type stubDoorProvisioner struct {
	outcome  Outcome
	err      error
	bound    int
	unbound  int
	gotPaths []string
}

func (p *stubDoorProvisioner) BindDoor(_ context.Context, _ DoorBindRequest, apiPaths []string) (Outcome, error) {
	p.bound++
	p.gotPaths = apiPaths
	return p.outcome, p.err
}

func (p *stubDoorProvisioner) UnbindDoor(_ context.Context, _ DoorBindRequest) (Outcome, error) {
	p.unbound++
	return p.outcome, p.err
}

func (p *stubDoorProvisioner) Describe() string { return "stub" }

func doorRow(status string) map[string]any {
	return map[string]any{
		"id":           "door-1",
		"accountId":    testDoorAccount,
		"reservedName": testDoorReserved,
		"status":       status,
	}
}

func newDoorReconciler(t *testing.T, eng *fakeDoorEngine, prov *stubDoorProvisioner, res Resolver) *DoorReconciler {
	t.Helper()
	r := NewDoorReconciler(NewDoorStore(eng), NewDoorAccountReader(eng), DoorConfig{
		EdgeHost:         doorEdgeHost,
		ACMEIssuer:       "letsencrypt-prod",
		Namespace:        "memql",
		IngressClass:     "nginx",
		EdgeService:      "edge",
		EdgePort:         8085,
		BFFHTTPService:   "bff-http",
		BFFHTTPPort:      8085,
		BFFGRPCService:   "bff",
		BFFGRPCPort:      50051,
		AgentGRPCService: "agent",
		AgentGRPCPort:    50051,
		IdentityService:  "identity",
		IdentityPort:     8085,
	}, prov, nil)
	r.resolver = res
	r.now = func() time.Time { return time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC) }
	r.newID = func() string { return "door-new" }
	return r
}

// ===========================================================================
// Verification
// ===========================================================================

// ALL THREE HOSTS, OR NOTHING. Two pointing and one not must not advance.
func TestADoorWithOneHostMissingDoesNotIssue(t *testing.T) {
	eng := &fakeDoorEngine{t: t, doors: []map[string]any{doorRow(StatusPendingDNS)}}
	prov := &stubDoorProvisioner{}
	res := stubDoorResolver{pointing: map[string]bool{
		"app." + testDoorReserved: true,
		"api." + testDoorReserved: true,
		// id. is missing.
	}}

	out, err := newDoorReconciler(t, eng, prov, res).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Verified != 0 || prov.bound != 0 {
		t.Errorf("verified=%d bound=%d: two of three hosts is not a front door", out.Verified, prov.bound)
	}
	if !eng.wrote("recordAccountFrontDoorCheck") {
		t.Error("the pass recorded no check, so the rail cannot say which CNAME is wrong")
	}
}

// EVERY HOST IS CHECKED ON EVERY PASS, even after the first miss -- so the
// rail can hand an operator the whole list at once instead of one record every
// two minutes.
func TestEveryHostIsReportedEvenAfterTheFirstMiss(t *testing.T) {
	eng := &fakeDoorEngine{t: t, doors: []map[string]any{doorRow(StatusVerifying)}}
	res := stubDoorResolver{pointing: map[string]bool{}} // none of the three

	if _, err := newDoorReconciler(t, eng, &stubDoorProvisioner{}, res).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var check string
	for _, c := range eng.calls {
		if strings.HasPrefix(c, "mutation recordAccountFrontDoorCheck") {
			check = c
		}
	}
	if check == "" {
		t.Fatal("no check was recorded")
	}
	for _, role := range frontdoor.AccountRoles() {
		if !strings.Contains(check, `"`+string(role)+`"`) {
			t.Errorf("the recorded hostChecks does not mention the %q host:\n  %s", role, check)
		}
	}
}

func TestAllThreeHostsPointingReachesIssuing(t *testing.T) {
	eng := &fakeDoorEngine{t: t, doors: []map[string]any{doorRow(StatusPendingDNS)}}
	res := stubDoorResolver{pointing: map[string]bool{
		"app." + testDoorReserved: true,
		"api." + testDoorReserved: true,
		"id." + testDoorReserved:  true,
	}}
	prov := &stubDoorProvisioner{}

	out, err := newDoorReconciler(t, eng, prov, res).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Verified != 1 {
		t.Errorf("verified = %d, want 1", out.Verified)
	}
	if !eng.wrote("markAccountFrontDoorVerified") {
		t.Error("the door did not reach issuing")
	}
	// ONE STATE PER PASS: verifying must not also dispatch the bind.
	if prov.bound != 0 {
		t.Errorf("the bind was dispatched %d time(s) in the same pass that verified; the row is the state machine, not a local variable", prov.bound)
	}
}

// ===========================================================================
// Issuance
// ===========================================================================

func TestACertificateThatIsNotReadyIsNotAFailure(t *testing.T) {
	eng := &fakeDoorEngine{t: t, doors: []map[string]any{doorRow(StatusIssuing)}}
	prov := &stubDoorProvisioner{outcome: Outcome{Applied: true, Note: "Waiting for http-01 challenge propagation"}}

	if _, err := newDoorReconciler(t, eng, prov, stubDoorResolver{}).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !eng.wrote("recordAccountFrontDoorIssuingProgress") {
		t.Error("an unready certificate was not recorded as progress")
	}
	if eng.wrote("recordAccountFrontDoorIssuanceFailure") {
		t.Error("waiting for a three-name ACME order was recorded as a failure, which makes an ordinary wait look like something to fix")
	}
}

func TestAReadyCertificateGoesLive(t *testing.T) {
	eng := &fakeDoorEngine{t: t, doors: []map[string]any{doorRow(StatusIssuing)}}
	prov := &stubDoorProvisioner{outcome: Outcome{Applied: true, CertificateReady: true}}

	out, err := newDoorReconciler(t, eng, prov, stubDoorResolver{}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Issued != 1 || !eng.wrote("markAccountFrontDoorLive") {
		t.Errorf("issued = %d, live written = %v", out.Issued, eng.wrote("markAccountFrontDoorLive"))
	}
}

// A cluster with no ACME issuer sits at `issuing` carrying the typed reason.
// That is the correct local answer (custom domains D7), not a stuck state.
func TestNoAcmeIssuerIsRecordedAndNotPromoted(t *testing.T) {
	eng := &fakeDoorEngine{t: t, doors: []map[string]any{doorRow(StatusIssuing)}}
	prov := &stubDoorProvisioner{outcome: Outcome{Reason: ReasonNoACMEIssuer, Detail: "this cluster declares no ACME issuer"}}

	if _, err := newDoorReconciler(t, eng, prov, stubDoorResolver{}).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if eng.wrote("markAccountFrontDoorLive") {
		t.Fatal("a door went live with no certificate")
	}
	var failure string
	for _, c := range eng.calls {
		if strings.HasPrefix(c, "mutation recordAccountFrontDoorIssuanceFailure") {
			failure = c
		}
	}
	if !strings.Contains(failure, ReasonNoACMEIssuer) {
		t.Errorf("the typed reason did not reach the row:\n  %s", failure)
	}
}

// The provisioner is handed the GENERATED path list, not one of the
// reconciler's own. Two lists would mean the next HTTP route added to the bff
// reaches the cluster's api host and no client's.
func TestTheProvisionerIsHandedTheGeneratedPathList(t *testing.T) {
	eng := &fakeDoorEngine{t: t, doors: []map[string]any{doorRow(StatusIssuing)}}
	prov := &stubDoorProvisioner{outcome: Outcome{Applied: true, CertificateReady: true}}

	if _, err := newDoorReconciler(t, eng, prov, stubDoorResolver{}).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(prov.gotPaths) == 0 {
		t.Fatal("the provisioner was handed no paths, so a door's api host would route nothing over HTTP")
	}
	if len(prov.gotPaths) != len(frontdoor.BFFHTTPPaths) {
		t.Errorf("the provisioner got %d paths, component/frontdoor holds %d", len(prov.gotPaths), len(frontdoor.BFFHTTPPaths))
	}
}

// ===========================================================================
// D9 -- the withdrawn reservation
// ===========================================================================

// THE ONLY ARROW OUT OF `live`. A door whose account no longer holds the name
// is the cluster serving three hostnames whose ownership proof was discarded.
func TestAWithdrawnReservationTearsALiveDoorDown(t *testing.T) {
	eng := &fakeDoorEngine{
		t:            t,
		doors:        []map[string]any{doorRow(StatusLive)},
		reservations: []map[string]any{}, // the account holds nothing any more
	}
	prov := &stubDoorProvisioner{}

	if _, err := newDoorReconciler(t, eng, prov, stubDoorResolver{}).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var removal string
	for _, c := range eng.calls {
		if strings.HasPrefix(c, "mutation requestAccountFrontDoorRemoval") {
			removal = c
		}
	}
	if removal == "" {
		t.Fatal("a live door whose reservation is gone was left serving")
	}
	if !strings.Contains(removal, ReasonReservationGone) {
		t.Errorf("the teardown carries no typed reason, so the rail cannot say why:\n  %s", removal)
	}
}

// A reservation that MOVED is the same conclusion for the same reason: proof
// of one name is not proof of another.
func TestAChangedReservationTearsTheOldDoorDown(t *testing.T) {
	eng := &fakeDoorEngine{
		t:            t,
		doors:        []map[string]any{doorRow(StatusLive)},
		reservations: []map[string]any{doorAccountRow("memql.newname.com", "2026-09-08T00:00:00Z", "")},
	}

	if _, err := newDoorReconciler(t, eng, &stubDoorProvisioner{}, stubDoorResolver{}).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !eng.wrote("requestAccountFrontDoorRemoval") {
		t.Error("the door for the OLD name was left serving after the reservation moved")
	}
	// And it must NOT open the new door in the same pass: the old one is still
	// holding the certificate's secret name.
	if eng.wrote("createAccountFrontDoor") {
		t.Error("a door for the new name was opened while the old one was still up")
	}
}

// A door already coming down must not be asked to come down again on every
// pass -- which would rewrite the row every two minutes and make the rail
// flicker for a door that had not moved.
func TestADoorAlreadyRemovingIsNotAskedAgain(t *testing.T) {
	eng := &fakeDoorEngine{t: t, doors: []map[string]any{doorRow(StatusRemoving)}, reservations: []map[string]any{}}
	prov := &stubDoorProvisioner{outcome: Outcome{Applied: true}}

	out, err := newDoorReconciler(t, eng, prov, stubDoorResolver{}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if eng.wrote("requestAccountFrontDoorRemoval") {
		t.Error("a door already at `removing` was asked to remove again")
	}
	if out.Removed != 1 || prov.unbound != 1 {
		t.Errorf("removed=%d unbound=%d: the removing door should have been unbound and closed", out.Removed, prov.unbound)
	}
}

// ===========================================================================
// Opening
// ===========================================================================

func TestAHeldReservationWithNoDoorOpensOne(t *testing.T) {
	eng := &fakeDoorEngine{
		t:            t,
		doors:        []map[string]any{},
		reservations: []map[string]any{doorAccountRow(testDoorReserved, "2026-09-08T00:00:00Z", "")},
	}

	out, err := newDoorReconciler(t, eng, &stubDoorProvisioner{}, stubDoorResolver{}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Opened != 1 || !eng.wrote("createAccountFrontDoor") {
		t.Errorf("opened = %d, create written = %v", out.Opened, eng.wrote("createAccountFrontDoor"))
	}
}

// A NAME WITH NO memqlReservedAt IS NOT HELD (record A, D10): the stamp lands
// in the same write that verifies the domain, and only when the guardrails
// passed. Opening a door for an unstamped name would request a certificate for
// a host whose ownership is unproven.
func TestAnUnstampedNameOpensNothing(t *testing.T) {
	eng := &fakeDoorEngine{
		t:     t,
		doors: []map[string]any{},
		// A name with NO memqlReservedAt: recorded, not held.
		reservations: []map[string]any{doorAccountRow(testDoorReserved, "", "")},
	}

	out, err := newDoorReconciler(t, eng, &stubDoorProvisioner{}, stubDoorResolver{}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Opened != 0 || eng.wrote("createAccountFrontDoor") {
		t.Error("a door was opened for a name this cluster has not agreed to serve")
	}
}

func TestAReservationThatAlreadyHasADoorOpensNothing(t *testing.T) {
	eng := &fakeDoorEngine{
		t:            t,
		doors:        []map[string]any{doorRow(StatusLive)},
		reservations: []map[string]any{doorAccountRow(testDoorReserved, "2026-09-08T00:00:00Z", "")},
	}

	if _, err := newDoorReconciler(t, eng, &stubDoorProvisioner{}, stubDoorResolver{}).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if eng.wrote("createAccountFrontDoor") {
		t.Error("a second door was opened for an account that already has one -- a second certificate order for the same three names")
	}
	if eng.wrote("requestAccountFrontDoorRemoval") {
		t.Error("a door matching its own reservation was asked to tear down")
	}
}

// ===========================================================================
// Object naming
// ===========================================================================

// The Go provisioner and both scripts must name the same objects, or a bind
// and its unbind act on different things and leave Ingresses serving a name
// the cluster no longer claims. The script half is pinned in
// scripts/deploy/account_front_door_test.go against these same literals.
func TestTheGoProvisionerNamesTheSameObjectsTheScriptDoes(t *testing.T) {
	if got, want := doorObjectName("acct-test"), "account-front-door-acct-test"; got != want {
		t.Errorf("doorObjectName = %q, want %q", got, want)
	}
	req := DoorBindRequest{AccountID: "acct-test", ReservedName: testDoorReserved, Namespace: "memql", Issuer: "le", IngressClass: "nginx"}

	names := map[string]bool{}
	for _, obj := range doorIngressObjects(req, []string{"/healthz"}) {
		meta, _ := obj["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		names[name] = true
	}
	for _, suffix := range doorIngressSuffixes {
		if !names["account-front-door-acct-test"+suffix] {
			t.Errorf("no Ingress named with the %q suffix, but the unbind deletes one", suffix)
		}
	}
}

// The certificate names all three hosts, which is what makes activation
// all-or-nothing without a rule for the reconciler to police.
func TestTheCertificateNamesEveryHost(t *testing.T) {
	obj := DoorCertificateObject(DoorBindRequest{AccountID: "acct-test", ReservedName: testDoorReserved, Issuer: "le"})
	spec, _ := obj["spec"].(map[string]any)
	sans, _ := spec["dnsNames"].([]any)
	if len(sans) != len(frontdoor.AccountRoles()) {
		t.Fatalf("the certificate names %d hosts, the door serves %d", len(sans), len(frontdoor.AccountRoles()))
	}
	for i, want := range frontdoor.AccountCertificateSANs(testDoorReserved) {
		if got, _ := sans[i].(string); got != want {
			t.Errorf("dnsName %d = %q, want %q", i, got, want)
		}
	}
}

// The api. host gets TWO Ingresses, and only one of them is the gRPC one.
// Backend protocol is a per-Service annotation, so they cannot share an object.
func TestTheApiHostGetsBothBackends(t *testing.T) {
	req := DoorBindRequest{AccountID: "a", ReservedName: testDoorReserved, IngressClass: "nginx",
		BFFHTTPService: "bff-http", BFFHTTPPort: 8085, BFFGRPCService: "bff", BFFGRPCPort: 50051}

	var grpc, http int
	apiHost := frontdoor.AccountRoleHost(frontdoor.AccountRoleAPI, testDoorReserved)
	for _, obj := range doorIngressObjects(req, []string{"/healthz", "/memql/ws"}) {
		spec, _ := obj["spec"].(map[string]any)
		rules, _ := spec["rules"].([]any)
		rule, _ := rules[0].(map[string]any)
		if rule["host"] != apiHost {
			continue
		}
		meta, _ := obj["metadata"].(map[string]any)
		ann, _ := meta["annotations"].(map[string]any)
		if ann["nginx.ingress.kubernetes.io/backend-protocol"] == "GRPC" {
			grpc++
		} else {
			http++
		}
	}
	if grpc != 1 || http != 1 {
		t.Errorf("the api host has %d gRPC and %d HTTP Ingress(es), want one of each", grpc, http)
	}
}

// No paths must produce a door WITHOUT an api HTTP Ingress, not one carrying
// an empty rule list -- which the API server rejects, taking the other three
// hosts down with it.
func TestNoPathsOmitsTheHttpIngressRatherThanEmptyingIt(t *testing.T) {
	req := DoorBindRequest{AccountID: "a", ReservedName: testDoorReserved, IngressClass: "nginx"}
	objs := doorIngressObjects(req, nil)
	if len(objs) != 3 {
		t.Fatalf("got %d Ingresses for a door with no HTTP paths, want 3", len(objs))
	}
	for _, obj := range objs {
		spec, _ := obj["spec"].(map[string]any)
		rules, _ := spec["rules"].([]any)
		rule, _ := rules[0].(map[string]any)
		httpBlock, _ := rule["http"].(map[string]any)
		paths, _ := httpBlock["paths"].([]any)
		if len(paths) == 0 {
			meta, _ := obj["metadata"].(map[string]any)
			t.Errorf("Ingress %v carries a zero-length paths list, which the API server rejects", meta["name"])
		}
	}
}

// A FRONT-DOOR HOST IS NEVER AN APEX, and this pins it rather than testing a
// branch that cannot be reached.
//
// The design record originally promised a test of "an apex reserved name",
// which was a promise about a case that does not exist. CheckPointing's apex
// path is a LABEL COUNT (`<= 1` dot), and every host a reserved name serves
// has the reserved name's labels plus one -- so the shortest possible door
// host is `app.<two-label-domain>`, which is three labels. The CNAME branch is
// the only one a door ever takes.
//
// It matters because the apex branch compares resolved ADDRESSES rather than a
// CNAME target, which is a weaker check: it would admit any host that happened
// to resolve to the same load balancer, including one belonging to somebody
// else this cluster serves.
func TestNoFrontDoorHostIsEverAnApex(t *testing.T) {
	for _, reserved := range []string{
		"memql.acme.com",
		"acme.com",      // a client who reserved their bare domain
		"a.b.c.d.e.com", // and a deep one
	} {
		for _, h := range frontdoor.AccountHosts(reserved) {
			if IsApex(h.Name) {
				t.Errorf("%q reads as an apex; the pointing check would compare addresses rather than the CNAME target, which admits any host resolving to the same load balancer", h.Name)
			}
		}
	}

	// The control: the predicate is not simply always false.
	if !IsApex("acme.com") {
		t.Fatal("IsApex returns false for a real apex -- this assertion would be vacuous")
	}
}

// ===========================================================================
// The teardown is not reversible
// ===========================================================================

// A FAILED UNBIND MUST NOT PROMOTE THE DOOR BACK TO `issuing`.
//
// The first version routed both of unprovision's failure paths through
// RecordIssuanceFailure, whose mutation stamps `status: "issuing"`
// unconditionally. Any unbind failure -- a missing RBAC verb, kubectl absent,
// an unregistered script id, a transient API error -- walked the row
// `removing` -> `issuing`, and the NEXT pass re-applied the certificate and
// all four Ingresses for a name whose reservation had been withdrawn. When the
// certificate reported Ready it went back to `live` and the edge served it
// again. D9's "the only transition that can leave live" was reversible by any
// transient failure.
func TestAFailedUnbindKeepsTheDoorRemoving(t *testing.T) {
	for name, prov := range map[string]*stubDoorProvisioner{
		"a typed refusal":              {outcome: Outcome{Reason: ReasonIssuanceFailed, Detail: "kubectl is not installed"}},
		"a substrate error":            {err: errStubUnbind},
		"applied:false with no reason": {outcome: Outcome{Applied: false}},
	} {
		t.Run(name, func(t *testing.T) {
			eng := &fakeDoorEngine{t: t, doors: []map[string]any{doorRow(StatusRemoving)}, reservations: []map[string]any{}}
			out, err := newDoorReconciler(t, eng, prov, stubDoorResolver{}).Run(context.Background())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if out.Removed != 0 {
				t.Errorf("removed = %d for a teardown that did not complete", out.Removed)
			}
			if eng.wrote("markAccountFrontDoorRemoved") {
				t.Error("the door was marked removed while its objects are still serving")
			}
			if eng.wrote("recordAccountFrontDoorIssuanceFailure") {
				t.Error("a failed teardown was recorded as an ISSUANCE failure, which stamps `issuing` -- the next pass would re-provision a door whose reservation was withdrawn")
			}
			if !eng.wrote("recordAccountFrontDoorRemovalFailure") {
				t.Error("the failure was not recorded at all, so the sweep has nothing to retry and the rail nothing to show")
			}
		})
	}
}

var errStubUnbind = fmt.Errorf("the substrate could not run")

// A pass that cannot read reservations must REPORT that, not answer a healthy
// zero. Both directions of D9 live in open(), so a failed read blocks opening
// AND teardown -- and the first version returned {0,0,0,0,0,0} with no error,
// which the automation records as a success every two minutes forever.
func TestAFailedReservationReadIsReportedRatherThanLookingHealthy(t *testing.T) {
	eng := &fakeDoorEngine{
		t:              t,
		doors:          []map[string]any{doorRow(StatusIssuing)},
		reservationErr: errStubUnbind,
	}
	prov := &stubDoorProvisioner{outcome: Outcome{Applied: true, CertificateReady: true}}

	out, err := newDoorReconciler(t, eng, prov, stubDoorResolver{}).Run(context.Background())

	// The step loop still ran: a door already issuing keeps advancing while
	// the account read is broken. That is the half the original comment got
	// right, and it must survive the fix to the half it got wrong.
	if out.Issued != 1 {
		t.Errorf("issued = %d; an unreadable reservation list must not stop a door that is already issuing", out.Issued)
	}
	if err == nil {
		t.Fatal("a pass that could not read reservations returned no error, so the automation records it as a success and a cluster that cannot see its reservations looks like one with nothing to do")
	}
	if !strings.Contains(err.Error(), "reservations") {
		t.Errorf("the error does not name what could not be read: %v", err)
	}
}

// ===========================================================================
// A live door is not permanently trusted
// ===========================================================================

func liveDoorRow(lastChecked string, driftFailures int) map[string]any {
	r := doorRow(StatusLive)
	r["lastCheckedAt"] = lastChecked
	r["driftFailures"] = driftFailures
	return r
}

const staleCheck = "2026-09-08T10:00:00Z" // well past LiveRecheckInterval before the fixture clock

// GOING LIVE IS NOT A PERMANENT FACT ABOUT DNS. A door whose app. host is
// repointed after it goes live keeps serving AND keeps its callback registered
// as an OAuth redirect URI -- which composes into delivering an authorization
// code to a server the account controls, through a consent page showing this
// platform's own name.
func TestALiveDoorIsRecheckedAndDemotedOnSustainedDrift(t *testing.T) {
	// THE THRESHOLD IS ASSERTED, NOT DERIVED. The first version of this test
	// started its fixture at `DriftDemotionThreshold-2`, so setting the
	// threshold to 1 -- demote on a single failed lookup, the exact behaviour
	// the counter exists to prevent -- moved the fixture with it and the test
	// still passed. A fixture computed from the constant under test cannot
	// measure that constant.
	if DriftDemotionThreshold < 2 {
		t.Fatalf("DriftDemotionThreshold is %d: a single unlucky DNS lookup would take a client's whole front door down, and the blast radius of that false positive is every one of their people",
			DriftDemotionThreshold)
	}

	// A door with ZERO recorded failures, drifting for the first time: still
	// serving, count incremented.
	eng := &fakeDoorEngine{
		t:            t,
		doors:        []map[string]any{liveDoorRow(staleCheck, 0)},
		reservations: []map[string]any{doorAccountRow(testDoorReserved, "2026-09-08T00:00:00Z", "")},
	}
	out, err := newDoorReconciler(t, eng, &stubDoorProvisioner{}, stubDoorResolver{}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Demoted != 0 {
		t.Errorf("demoted = %d on the FIRST drift; one unlucky lookup must not take a client's front door down", out.Demoted)
	}
	var firstDrift string
	for _, c := range eng.calls {
		if strings.HasPrefix(c, "mutation recordAccountFrontDoorDrift") {
			firstDrift = c
		}
	}
	if !strings.Contains(firstDrift, "driftFailures: 1") {
		t.Errorf("the first failure did not record a count of 1:\n  %s", firstDrift)
	}
	if !eng.wrote("recordAccountFrontDoorDrift") {
		t.Error("the failing re-check was not recorded, so the count never reaches the threshold")
	}

	// At the threshold: demoted.
	eng = &fakeDoorEngine{
		t:            t,
		doors:        []map[string]any{liveDoorRow(staleCheck, DriftDemotionThreshold-1)},
		reservations: []map[string]any{doorAccountRow(testDoorReserved, "2026-09-08T00:00:00Z", "")},
	}
	out, err = newDoorReconciler(t, eng, &stubDoorProvisioner{}, stubDoorResolver{}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Demoted != 1 {
		t.Fatalf("demoted = %d after %d consecutive failures", out.Demoted, DriftDemotionThreshold)
	}
	var demotion string
	for _, c := range eng.calls {
		if strings.HasPrefix(c, "mutation recordAccountFrontDoorCheck") {
			demotion = c
		}
	}
	if !strings.Contains(demotion, StatusVerifying) {
		t.Errorf("the demotion did not write `verifying`, so the edge keeps serving the host and the identity node keeps its callback registered:\n  %s", demotion)
	}
}

// A PASSING RE-CHECK RESETS THE COUNT. Drift means CONSECUTIVE failures; a door
// that fails twice, recovers, and fails once more has not drifted for the
// window the threshold is meant to represent.
func TestAPassingRecheckResetsTheDriftCount(t *testing.T) {
	eng := &fakeDoorEngine{
		t:            t,
		doors:        []map[string]any{liveDoorRow(staleCheck, DriftDemotionThreshold-1)},
		reservations: []map[string]any{doorAccountRow(testDoorReserved, "2026-09-08T00:00:00Z", "")},
	}
	res := stubDoorResolver{pointing: map[string]bool{
		"app." + testDoorReserved: true,
		"api." + testDoorReserved: true,
		"id." + testDoorReserved:  true,
	}}

	out, err := newDoorReconciler(t, eng, &stubDoorProvisioner{}, res).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Demoted != 0 {
		t.Fatal("a door whose names all point here was demoted")
	}
	var drift string
	for _, c := range eng.calls {
		if strings.HasPrefix(c, "mutation recordAccountFrontDoorDrift") {
			drift = c
		}
	}
	if !strings.Contains(drift, "driftFailures: 0") {
		t.Errorf("a passing re-check did not reset the count to 0:\n  %s", drift)
	}
}

// A RECENTLY CHECKED DOOR COSTS NO LOOKUPS. The sweep runs every two minutes
// and the re-check interval is fifteen, so the steady state of a healthy
// cluster must not be three DNS lookups per door per pass.
func TestARecentlyCheckedLiveDoorIsNotRecheckedAgain(t *testing.T) {
	eng := &fakeDoorEngine{
		t:            t,
		doors:        []map[string]any{liveDoorRow("2026-09-08T11:59:30Z", 0)}, // 30s before the fixture clock
		reservations: []map[string]any{doorAccountRow(testDoorReserved, "2026-09-08T00:00:00Z", "")},
	}
	if _, err := newDoorReconciler(t, eng, &stubDoorProvisioner{}, stubDoorResolver{}).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if eng.wrote("recordAccountFrontDoorDrift") {
		t.Error("a door checked 30 seconds ago was re-checked; the interval is 15 minutes and the sweep runs every 2")
	}
}

// A door with NO recorded check is due IMMEDIATELY -- absent is "never looked",
// not "looked recently", and the whole point is that a live door must not go
// unexamined forever.
func TestALiveDoorWithNoRecordedCheckIsDueImmediately(t *testing.T) {
	eng := &fakeDoorEngine{
		t:            t,
		doors:        []map[string]any{liveDoorRow("", 0)},
		reservations: []map[string]any{doorAccountRow(testDoorReserved, "2026-09-08T00:00:00Z", "")},
	}
	if _, err := newDoorReconciler(t, eng, &stubDoorProvisioner{}, stubDoorResolver{}).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !eng.wrote("recordAccountFrontDoorDrift") {
		t.Error("a live door that has never been checked was skipped")
	}
}

// ===========================================================================
// The reservation reason
// ===========================================================================

func doorAccountRow(reservedName, reservedAt, reason string) map[string]any {
	return map[string]any{
		"id":                     testDoorAccount,
		"memqlDomain":            reservedName,
		"memqlReservedAt":        reservedAt,
		"memqlReservationReason": reason,
	}
}

// THE ASK THE ACCOUNTS RAIL MADE: an absent memqlReservedAt meant two things,
// and the rail inferred which from the ownership stop beside it.
func TestAnUnheldNameGetsATypedReason(t *testing.T) {
	eng := &fakeDoorEngine{t: t, doors: []map[string]any{}, reservations: []map[string]any{
		doorAccountRow(testDoorReserved, "", ""),
	}}
	if _, err := newDoorReconciler(t, eng, &stubDoorProvisioner{}, stubDoorResolver{}).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var wrote string
	for _, c := range eng.calls {
		if strings.HasPrefix(c, "mutation recordAccountDomainCheck") {
			wrote = c
		}
	}
	if !strings.Contains(wrote, ReasonOwnershipUnproven) {
		t.Errorf("an unheld name got no typed reason, so the rail is still inferring:\n  %s", wrote)
	}
	if eng.wrote("createAccountFrontDoor") {
		t.Error("a door was opened for a name this cluster has not agreed to serve")
	}
}

// A HELD NAME CARRIES NO REASON. Without the clear, a name that was refused and
// then fixed keeps explaining a state it is no longer in.
func TestAHeldNameHasItsReasonCleared(t *testing.T) {
	eng := &fakeDoorEngine{t: t, doors: []map[string]any{}, reservations: []map[string]any{
		doorAccountRow(testDoorReserved, "2026-09-08T00:00:00Z", ReasonOwnershipUnproven),
	}}
	if _, err := newDoorReconciler(t, eng, &stubDoorProvisioner{}, stubDoorResolver{}).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var wrote string
	for _, c := range eng.calls {
		if strings.HasPrefix(c, "mutation recordAccountDomainCheck") {
			wrote = c
		}
	}
	if wrote == "" {
		t.Fatal("a held name kept a stale reason: nothing cleared it")
	}
	if !strings.Contains(wrote, `memqlReservationReason: ""`) {
		t.Errorf("the clear did not write an empty reason:\n  %s", wrote)
	}
}

// A NO-OP WRITE IS NOT MADE. The sweep runs every two minutes over every
// account with a name; writing a reason that already says what it says would
// version each of those rows on a timer -- the strobe the arrival cue's own
// rule exists to prevent.
func TestAReasonThatIsAlreadyRightIsNotRewritten(t *testing.T) {
	for name, res := range map[string][]map[string]any{
		"unheld, reason already set": {doorAccountRow(testDoorReserved, "", ReasonOwnershipUnproven)},
		"held, reason already empty": {doorAccountRow(testDoorReserved, "2026-09-08T00:00:00Z", "")},
	} {
		t.Run(name, func(t *testing.T) {
			eng := &fakeDoorEngine{t: t, doors: []map[string]any{}, reservations: res}
			if _, err := newDoorReconciler(t, eng, &stubDoorProvisioner{}, stubDoorResolver{}).Run(context.Background()); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if eng.wrote("recordAccountDomainCheck") {
				t.Error("the sweep rewrote a reason that was already correct, versioning the row on a two-minute timer")
			}
		})
	}
}

// The gRPC Ingress carries the worker stream's prefix to the AGENT before the
// `/` catch-all to the bff (epic memql#5218, D10). WorkerService.Stream is
// registered on the agent node and nowhere else, so a door routing only the
// catch-all answers a cockpit dialling a client's api. host `Unimplemented:
// unknown service` -- the cluster's own api host did exactly that until the
// same rule was added there, and a door must route what the cluster routes.
func TestTheApiGrpcIngressRoutesTheWorkerStreamToTheAgent(t *testing.T) {
	req := DoorBindRequest{AccountID: "a", ReservedName: testDoorReserved, IngressClass: "nginx",
		BFFHTTPService: "bff-http", BFFHTTPPort: 8085, BFFGRPCService: "bff", BFFGRPCPort: 50051,
		AgentGRPCService: "agent", AgentGRPCPort: 50051}

	var grpcRules []map[string]any
	for _, obj := range doorIngressObjects(req, []string{"/healthz"}) {
		meta, _ := obj["metadata"].(map[string]any)
		ann, _ := meta["annotations"].(map[string]any)
		if ann["nginx.ingress.kubernetes.io/backend-protocol"] != "GRPC" {
			// The worker prefix belongs in the GRPC object only: the same
			// path in the HTTP one would hand the stream to an HTTP/1.1 hop.
			spec, _ := obj["spec"].(map[string]any)
			for _, r := range pathsOf(spec) {
				if r["path"] == frontdoor.WorkerServicePath {
					t.Errorf("Ingress %v carries %q without backend-protocol GRPC", meta["name"], frontdoor.WorkerServicePath)
				}
			}
			continue
		}
		spec, _ := obj["spec"].(map[string]any)
		grpcRules = pathsOf(spec)
	}
	if len(grpcRules) != 2 {
		t.Fatalf("the gRPC Ingress carries %d rule(s), want 2: the worker prefix to the agent, then `/` to the bff", len(grpcRules))
	}
	for i, want := range []struct {
		path, service string
		port          int
	}{
		{frontdoor.WorkerServicePath, "agent", 50051},
		{"/", "bff", 50051},
	} {
		got := grpcRules[i]
		backend, _ := got["backend"].(map[string]any)
		svc, _ := backend["service"].(map[string]any)
		port, _ := svc["port"].(map[string]any)
		if got["path"] != want.path || svc["name"] != want.service || port["number"] != want.port {
			t.Errorf("rule %d = %v -> %v:%v, want %q -> %s:%d", i, got["path"], svc["name"], port["number"], want.path, want.service, want.port)
		}
	}
}

// pathsOf returns the path entries of an Ingress spec's first rule.
func pathsOf(spec map[string]any) []map[string]any {
	rules, _ := spec["rules"].([]any)
	if len(rules) == 0 {
		return nil
	}
	rule, _ := rules[0].(map[string]any)
	httpBlock, _ := rule["http"].(map[string]any)
	paths, _ := httpBlock["paths"].([]any)
	out := make([]map[string]any, 0, len(paths))
	for _, p := range paths {
		m, _ := p.(map[string]any)
		out = append(out, m)
	}
	return out
}
