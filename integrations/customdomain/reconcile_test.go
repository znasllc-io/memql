package customdomain

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/deploycontrol"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// The state machine, end to end, with no database, no cluster and no network.
//
// # Why the fake engine PARSES what it is handed
//
// A suite that only RECORDS the strings a writer produced proves the writer
// called something; it does not prove the engine could execute it. Every call
// this package renders goes through langparser.ParseExpression below, so a
// mutation whose argument list does not parse fails HERE rather than at the
// first real reconciliation pass -- which is a warning in a log on a cluster
// nobody is watching.

// fakeEngine records every rendered call, parses it, and answers reads from a
// fixture.
type fakeEngine struct {
	t     *testing.T
	rows  []map[string]any
	calls []string
	// siteRows answers `query siteById(...)`.
	siteRows []map[string]any
	// err, when set, is returned for every call.
	err error
}

func (f *fakeEngine) Execute(_ context.Context, query string) (any, error) {
	f.calls = append(f.calls, query)
	if _, perr := langparser.ParseExpression(query); perr != nil {
		f.t.Fatalf("the engine was handed a call it cannot parse:\n  %s\n  %v", query, perr)
	}
	if f.err != nil {
		return nil, f.err
	}
	switch {
	case strings.HasPrefix(query, "query customDomainsToReconcile"),
		strings.HasPrefix(query, "query customDomainsAll"),
		strings.HasPrefix(query, "query customDomainsForSite"),
		strings.HasPrefix(query, "query customDomainById"):
		// The fixture answers all four with the same rows, so a test can hand
		// the sweep a `live` row and assert it is skipped. That is what
		// TestATerminalRowIsNotTouchedAtAll measures, and it would be
		// unmeasurable if the fake narrowed the way the real query does.
		return f.rows, nil
	case strings.HasPrefix(query, "query siteById"):
		return f.siteRows, nil
	default:
		return []map[string]any{}, nil
	}
}

// mutations returns the names of every mutation the engine was asked to run.
func (f *fakeEngine) mutations() []string {
	var out []string
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "mutation ") {
			continue
		}
		rest := strings.TrimPrefix(c, "mutation ")
		if i := strings.Index(rest, "("); i > 0 {
			rest = rest[:i]
		}
		out = append(out, rest)
	}
	return out
}

func (f *fakeEngine) ranMutation(name string) bool {
	for _, m := range f.mutations() {
		if m == name {
			return true
		}
	}
	return false
}

// recordingProvisioner counts dispatches so a test can assert that NONE
// happened.
type recordingProvisioner struct {
	binds   []BindRequest
	unbinds []BindRequest
	out     Outcome
	err     error
}

func (p *recordingProvisioner) Describe() string { return "recording (test)" }

func (p *recordingProvisioner) Bind(_ context.Context, req BindRequest) (Outcome, error) {
	p.binds = append(p.binds, req)
	return p.out, p.err
}

func (p *recordingProvisioner) Unbind(_ context.Context, req BindRequest) (Outcome, error) {
	p.unbinds = append(p.unbinds, req)
	return p.out, p.err
}

func fixedNow() time.Time { return time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC) }

func newTestReconciler(t *testing.T, eng *fakeEngine, r Resolver, p Provisioner) *Reconciler {
	t.Helper()
	rec := NewReconciler(NewStore(eng), Config{
		EdgeHost:     testEdgeHost,
		ACMEIssuer:   "letsencrypt-prod",
		Namespace:    "memql",
		IngressClass: "nginx",
		EdgeService:  "edge",
		EdgePort:     8085,
		MaxPerSite:   5,
	}, p, nil)
	rec.resolver = r
	rec.now = fixedNow
	return rec
}

func row(id, hostname, status string) map[string]any {
	return map[string]any{
		"id":       "v1:platform:customDomain:" + id,
		"siteId":   "v1:platform:site:site1",
		"hostname": hostname,
		"token":    testToken,
		"status":   status,
	}
}

// ===========================================================================
// THE NEVER-ISSUE PROOF (design D4)
// ===========================================================================

// PASSING EXACTLY ONE CHECK MUST DISPATCH NOTHING. Both directions are pinned,
// because the two checks fail for different reasons and the bug would be
// invisible in whichever direction was untested: a domain that merely POINTS
// here must not be claimable by whoever asks for it first, and a domain whose
// owner published our token but has not pointed it at us has no certificate we
// could serve.
func TestOnlyTheOwnershipCheckPassingIssuesNothing(t *testing.T) {
	eng := &fakeEngine{t: t, rows: []map[string]any{row("d1", "www.acme.com", StatusPendingDNS)}}
	res := &fakeResolver{
		txt: map[string][]string{"_memql-verify.www.acme.com": {testToken}},
		// No CNAME and no address: the domain does not point here.
		hosts: edgeAddrs(),
	}
	prov := &recordingProvisioner{}
	rec := newTestReconciler(t, eng, res, prov)

	if _, err := rec.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(prov.binds) != 0 {
		t.Fatalf("the bind dispatched %d time(s) with only ONE of the two checks passing", len(prov.binds))
	}
	if eng.ranMutation("markCustomDomainVerified") {
		t.Fatal("the row was promoted to `issuing` with only the ownership check passing")
	}
	if !eng.ranMutation("recordCustomDomainCheck") {
		t.Fatal("the miss was not recorded at all -- the panel would have nothing to show")
	}
	if !strings.Contains(strings.Join(eng.calls, "\n"), ReasonNotPointing) {
		t.Errorf("the recorded reason does not name %q, so the panel cannot say which record is wrong", ReasonNotPointing)
	}
}

func TestOnlyThePointingCheckPassingIssuesNothing(t *testing.T) {
	eng := &fakeEngine{t: t, rows: []map[string]any{row("d1", "www.acme.com", StatusVerifying)}}
	res := &fakeResolver{
		// The domain points here, and NOBODY has proved they own it. This is
		// the dangerous half: on a shared install, "points at this cluster" is
		// a property many unrelated tenants share.
		txt:   map[string][]string{},
		cname: map[string]string{"www.acme.com": testEdgeHost + "."},
		hosts: edgeAddrs(),
	}
	prov := &recordingProvisioner{}
	rec := newTestReconciler(t, eng, res, prov)

	if _, err := rec.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(prov.binds) != 0 {
		t.Fatalf("the bind dispatched %d time(s) for a domain nobody proved they own", len(prov.binds))
	}
	if eng.ranMutation("markCustomDomainVerified") {
		t.Fatal("the row was promoted to `issuing` with no ownership proof")
	}
	if !strings.Contains(strings.Join(eng.calls, "\n"), ReasonTokenMissing) {
		t.Errorf("the recorded reason does not name %q", ReasonTokenMissing)
	}
}

// ===========================================================================
// The walk
// ===========================================================================

func TestBothChecksPassingPromotesToIssuingAndDispatchesNothingYet(t *testing.T) {
	eng := &fakeEngine{t: t, rows: []map[string]any{row("d1", "www.acme.com", StatusVerifying)}}
	res := &fakeResolver{
		txt:   map[string][]string{"_memql-verify.www.acme.com": {testToken}},
		cname: map[string]string{"www.acme.com": testEdgeHost + "."},
		hosts: edgeAddrs(),
	}
	prov := &recordingProvisioner{}
	rec := newTestReconciler(t, eng, res, prov)

	out, err := rec.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Verified != 1 {
		t.Fatalf("verified = %d, want 1", out.Verified)
	}
	if !eng.ranMutation("markCustomDomainVerified") {
		t.Fatal("both checks passed and the row was not promoted")
	}
	// ONE STATE PER PASS. The dispatch is driven by the ROW, not by a variable
	// in the function that just wrote it -- which is what lets two replicas
	// run this sweep without both acting on a binding each had just promoted.
	if len(prov.binds) != 0 {
		t.Errorf("the bind dispatched in the SAME pass that promoted the row (%d time(s)); "+
			"the next pass reads the row and acts on it", len(prov.binds))
	}
}

func TestAnIssuingRowDispatchesTheBindAndGoesLiveOnlyWhenTheCertificateIsReady(t *testing.T) {
	eng := &fakeEngine{t: t, rows: []map[string]any{row("d1", "www.acme.com", StatusIssuing)}}
	prov := &recordingProvisioner{out: Outcome{Applied: true, CertificateReady: true}}
	rec := newTestReconciler(t, eng, &fakeResolver{}, prov)

	out, err := rec.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(prov.binds) != 1 {
		t.Fatalf("bind dispatched %d time(s), want 1", len(prov.binds))
	}
	if got := prov.binds[0]; got.Hostname != "www.acme.com" || got.Issuer != "letsencrypt-prod" || got.Port != 8085 {
		t.Errorf("bind request = %+v, want the row's hostname and the configured issuer/backend", got)
	}
	if out.Issued != 1 || !eng.ranMutation("markCustomDomainLive") {
		t.Fatal("a Ready certificate did not take the row live")
	}
}

// APPLYING A CERTIFICATE IS NOT HOLDING ONE. A row whose objects are applied
// and whose certificate is still pending stays `issuing` with NO failure
// reason -- waiting for ACME is not something a person should go and fix.
func TestAnAppliedButNotReadyCertificateStaysIssuingWithNoFailureReason(t *testing.T) {
	eng := &fakeEngine{t: t, rows: []map[string]any{row("d1", "www.acme.com", StatusIssuing)}}
	prov := &recordingProvisioner{out: Outcome{
		Applied: true,
		Note:    "Waiting for http-01 challenge propagation",
	}}
	rec := newTestReconciler(t, eng, &fakeResolver{}, prov)

	out, err := rec.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Issued != 0 || eng.ranMutation("markCustomDomainLive") {
		t.Fatal("the row went live on an apply, before the certificate was Ready")
	}
	if !eng.ranMutation("recordCustomDomainIssuingProgress") {
		t.Fatal("a pending certificate recorded nothing, so lastCheckedAt never moves and the panel cannot tell a slow order from a stuck sweep")
	}
	if eng.ranMutation("recordCustomDomainIssuanceFailure") {
		t.Fatal("waiting for ACME was recorded as a FAILURE; the panel would tell somebody to fix a normal wait")
	}
}

// A cluster with no ACME issuer refuses every pass, forever, and the row says
// so (design D7). It must stay `issuing` rather than walking backwards -- the
// DNS is verified, and re-running two lookups that already passed proves
// nothing while reading as losing ground.
func TestARefusedIssuanceKeepsTheRowIssuingAndRecordsTheTypedReason(t *testing.T) {
	eng := &fakeEngine{t: t, rows: []map[string]any{row("d1", "www.acme.com", StatusIssuing)}}
	prov := &recordingProvisioner{out: Outcome{
		Reason: ReasonNoACMEIssuer,
		Detail: "this cluster declares no ACME issuer",
	}}
	rec := newTestReconciler(t, eng, &fakeResolver{}, prov)

	if _, err := rec.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !eng.ranMutation("recordCustomDomainIssuanceFailure") {
		t.Fatal("a refusal recorded nothing")
	}
	joined := strings.Join(eng.calls, "\n")
	if !strings.Contains(joined, ReasonNoACMEIssuer) {
		t.Errorf("the typed reason %q did not reach the row", ReasonNoACMEIssuer)
	}
	if eng.ranMutation("recordCustomDomainCheck") {
		t.Error("a refused issuance walked the row back to `verifying`")
	}
}

func TestARemovingRowDispatchesTheUnbindAndClosesTheWalk(t *testing.T) {
	eng := &fakeEngine{t: t, rows: []map[string]any{row("d1", "www.acme.com", StatusRemoving)}}
	prov := &recordingProvisioner{out: Outcome{Applied: true}}
	rec := newTestReconciler(t, eng, &fakeResolver{}, prov)

	out, err := rec.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(prov.unbinds) != 1 {
		t.Fatalf("unbind dispatched %d time(s), want 1", len(prov.unbinds))
	}
	if out.Removed != 1 || !eng.ranMutation("markCustomDomainRemoved") {
		t.Fatal("the walk was not closed at `removed`")
	}
}

// The two settled statuses cost nothing. This is what makes a two-minute
// schedule affordable on a cluster with a hundred bound domains.
func TestATerminalRowIsNotTouchedAtAll(t *testing.T) {
	eng := &fakeEngine{t: t, rows: []map[string]any{
		row("d1", "www.acme.com", StatusLive),
		row("d2", "old.acme.com", StatusRemoved),
	}}
	prov := &recordingProvisioner{}
	res := &fakeResolver{}
	rec := newTestReconciler(t, eng, res, prov)

	out, err := rec.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Checked != 0 {
		t.Errorf("checked = %d, want 0 -- `live` and `removed` are settled", out.Checked)
	}
	if len(eng.mutations()) != 0 {
		t.Errorf("a settled row was written: %v", eng.mutations())
	}
	if len(prov.binds)+len(prov.unbinds) != 0 {
		t.Error("a settled row dispatched a cluster operation")
	}
}

// ONE ROW'S FAILURE MUST NOT STOP THE PASS. A binding whose DNS provider is
// timing out cannot be allowed to set the pace for every other domain on the
// cluster.
func TestOneRowsFailureDoesNotAbortThePass(t *testing.T) {
	eng := &fakeEngine{t: t, rows: []map[string]any{
		row("d1", "broken.acme.com", StatusIssuing),
		row("d2", "fine.acme.com", StatusRemoving),
	}}
	prov := &failingBindProvisioner{}
	rec := newTestReconciler(t, eng, &fakeResolver{}, prov)

	out, err := rec.Run(context.Background())
	if err != nil {
		t.Fatalf("Run returned an error for a PER-ROW failure: %v", err)
	}
	if out.Checked != 2 {
		t.Errorf("checked = %d, want 2 -- the pass stopped early", out.Checked)
	}
	if out.Removed != 1 {
		t.Errorf("removed = %d, want 1 -- the second row was not reached", out.Removed)
	}
}

type failingBindProvisioner struct{}

func (failingBindProvisioner) Describe() string { return "failing bind (test)" }
func (failingBindProvisioner) Bind(_ context.Context, _ BindRequest) (Outcome, error) {
	return Outcome{Reason: ReasonIssuanceFailed, Detail: "the substrate is down"}, nil
}
func (failingBindProvisioner) Unbind(_ context.Context, _ BindRequest) (Outcome, error) {
	return Outcome{Applied: true}, nil
}

// ===========================================================================
// The capability-script substrate's round trip (task memql#4803)
// ===========================================================================

// loop -> script -> envelope -> ParseCapabilityResult, with the parse being the
// real one. A refusal envelope has to come back as a typed OUTCOME rather than
// as a Go error, because an error would end the pass before the reason could
// reach the row.
func TestScriptSubstrateFoldsARefusalEnvelopeIntoATypedOutcome(t *testing.T) {
	stdout := []byte(`{"ok":false,"capability":"domain.bind","changed":false,` +
		`"result":{"reason":"no_acme_issuer","detail":"this cluster declares no ACME issuer"},` +
		`"error":{"code":3,"message":"no ACME issuer configured"}}`)
	env, err := deploycontrol.ParseCapabilityResult(stdout)
	if err != nil {
		t.Fatalf("ParseCapabilityResult: %v", err)
	}

	p := &scriptProvisioner{run: stubScriptRunner{res: env}}
	out, err := p.Bind(context.Background(), BindRequest{Hostname: "www.acme.com", DomainID: "d1"})
	if err != nil {
		t.Fatalf("a REFUSAL envelope came back as a Go error, which would end the pass before the row could record it: %v", err)
	}
	if out.Reason != ReasonNoACMEIssuer {
		t.Errorf("reason = %q, want %q -- the exit code says 'refused' and not WHICH refusal, so the typed reason has to come from the result", out.Reason, ReasonNoACMEIssuer)
	}
	if !strings.Contains(out.Detail, "no ACME issuer") {
		t.Errorf("detail = %q, want the script's own sentence", out.Detail)
	}
}

// A success envelope's certificateReady gates `live`. cap_result_set_raw emits
// a real boolean, and cap_result_set emits a string -- both have to read as
// ready, or a working script would look like a permanently-pending certificate.
func TestScriptSubstrateReadsCertificateReadyInBothEncodings(t *testing.T) {
	for _, encoded := range []string{`true`, `"true"`} {
		stdout := []byte(`{"ok":true,"capability":"domain.bind","changed":true,` +
			`"result":{"certificateReady":` + encoded + `,"certificateStatus":"the certificate is Ready"}}`)
		env, err := deploycontrol.ParseCapabilityResult(stdout)
		if err != nil {
			t.Fatalf("ParseCapabilityResult(%s): %v", encoded, err)
		}
		p := &scriptProvisioner{run: stubScriptRunner{res: env}}
		out, err := p.Bind(context.Background(), BindRequest{Hostname: "www.acme.com", DomainID: "d1"})
		if err != nil {
			t.Fatalf("Bind(%s): %v", encoded, err)
		}
		if !out.CertificateReady {
			t.Errorf("certificateReady encoded as %s did not read as ready", encoded)
		}
	}
}

type stubScriptRunner struct {
	res deploycontrol.CapabilityResult
	err error
}

func (s stubScriptRunner) Run(_ context.Context, _ string, _ map[string]any) (deploycontrol.CapabilityResult, error) {
	return s.res, s.err
}

// ===========================================================================
// The objects
// ===========================================================================

// The Ingress names an EXACT host and never a wildcard: the cluster's own
// `*.<domain>` rule is a different zone, and ACME cannot issue a wildcard over
// HTTP-01 in any case (memql#4224).
func TestTheIngressAndCertificateNameExactlyOneHost(t *testing.T) {
	req := BindRequest{
		Hostname: "www.acme.com", DomainID: "d1", SiteID: "site1",
		Namespace: "memql", Issuer: "letsencrypt-prod",
		IngressClass: "nginx", Service: "edge", Port: 8085,
	}
	ing, _ := json.Marshal(IngressObject(req))
	cert, _ := json.Marshal(CertificateObject(req))

	for name, doc := range map[string]string{"Ingress": string(ing), "Certificate": string(cert)} {
		if strings.Contains(doc, "*") {
			t.Errorf("the %s carries a wildcard: %s", name, doc)
		}
		if !strings.Contains(doc, "www.acme.com") {
			t.Errorf("the %s does not name the hostname", name)
		}
	}
	// Both must resolve to the SAME Secret, or the Ingress terminates TLS with
	// a certificate cert-manager is writing somewhere else.
	if !strings.Contains(string(ing), "custom-domain-d1-tls") || !strings.Contains(string(cert), "custom-domain-d1-tls") {
		t.Error("the Ingress and the Certificate do not agree on the TLS Secret name")
	}
}

// KEYED ON THE ROW ID, NOT THE HOSTNAME. Two hostnames that sanitise to one
// object name would be one binding silently overwriting another's Ingress.
func TestObjectNameIsDerivedFromTheRowIdAndIsAlwaysLegal(t *testing.T) {
	for in, want := range map[string]string{
		"abc123":      "custom-domain-abc123",
		"ABC-123":     "custom-domain-abc-123",
		"weird_id!!":  "custom-domain-weird-id",
		"--leading--": "custom-domain-leading",
	} {
		if got := objectName(in); got != want {
			t.Errorf("objectName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := objectName(""); got != "custom-domain-unknown" {
		t.Errorf("objectName(\"\") = %q, want a usable fallback", got)
	}
}

// The API substrate refuses rather than applying a Certificate with an empty
// issuerRef, which the API server ACCEPTS and then leaves Pending forever.
func TestTheAPISubstrateRefusesWithNoIssuerRatherThanApplyingAPendingCertificate(t *testing.T) {
	p := &apiProvisioner{api: nil} // never reached: the refusal is before any call
	out, err := p.Bind(context.Background(), BindRequest{Hostname: "www.acme.com", DomainID: "d1", Namespace: "memql"})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if out.Reason != ReasonNoACMEIssuer {
		t.Fatalf("reason = %q, want %q", out.Reason, ReasonNoACMEIssuer)
	}
	if out.Applied {
		t.Error("the refusal claimed the objects were applied")
	}
}
