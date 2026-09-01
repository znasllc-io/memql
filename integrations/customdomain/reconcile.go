package customdomain

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/deploycontrol"
)

// reconcile.go -- the sweep. One pass over every non-terminal binding, on the
// schedule dsl/platform/automations.memql sets.
//
// # The state machine, and the one rule that makes it safe
//
//	pending_dns --both checks pass--> issuing --certificate Ready--> live
//	     ^                                |
//	     +------ verifying <--------------+ (a check missed; typed reason on the row)
//
//	<any> --removeCustomDomain--> removing --unbind applied--> removed (terminal, row stays)
//
// NOTHING ISSUES BEFORE BOTH CHECKS PASS (design D4). That is not a
// nice-to-have ordering: Let's Encrypt's rate limits are real and shared across
// everyone on a registered domain, and a hostname that merely POINTS at this
// cluster must not be claimable by whoever asks for it first -- on a shared
// install, "points here" is a property many unrelated tenants share. So
// `issuing` is reachable from exactly one place in this file, and the test that
// passes one check and asserts no dispatch is the gate on it.
//
// # Why a refusal keeps the row in `issuing`
//
// A cluster with no ACME issuer refuses issuance on every pass, forever. That
// is the correct behaviour on local k3d (design D7) and the row says so:
// `issuing` plus `no_acme_issuer` plus the script's own sentence. Walking back
// to `verifying` would re-run two DNS lookups that already passed and would
// read, to somebody watching, as losing ground.
//
// Re-applying is cheap and safe because the bind script is idempotent: a second
// `kubectl apply` of an unchanged Certificate is a no-op, so cert-manager's own
// backoff -- not this loop -- remains what paces ACME.

// Reconciler walks custom-domain bindings.
type Reconciler struct {
	store       *Store
	resolver    Resolver
	provisioner Provisioner
	logger      *slog.Logger

	// edgeHost is the hostname a client points their own domain AT. See
	// Config.EdgeHost.
	edgeHost string
	// acmeIssuer names the cert-manager ClusterIssuer the bind script requests
	// a Certificate from. Empty means this target declares none, and the
	// script refuses with no_acme_issuer rather than applying a Certificate
	// nothing will ever fulfil.
	acmeIssuer string
	// namespace is where the Ingress and Certificate objects live.
	namespace string
	// ingressClass is the controller that serves them.
	ingressClass string
	// edgeService / edgePort name the backend every custom-domain Ingress
	// points at -- the same edge Service the wildcard rule already reaches.
	edgeService string
	edgePort    int

	// now is injectable so a test can assert the exact timestamps written.
	now func() time.Time
}

// Config is the reconciler's environment-derived half.
type Config struct {
	// EdgeHost is the hostname a client CNAMEs their subdomain to, and whose
	// addresses an apex A record must match.
	//
	// DERIVED from the cluster's own domain rather than configured twice: it
	// is a host this cluster already serves itself at, so it cannot drift out
	// of agreement with where traffic actually lands. A second "our ingress
	// address" value would have no forcing function keeping it true -- the day
	// the load balancer is replaced, every correct apex record would be
	// refused while every manifest still looked right.
	EdgeHost string
	// ACMEIssuer is the cert-manager ClusterIssuer name, or empty.
	ACMEIssuer string
	// Namespace, IngressClass, EdgeService and EdgePort describe where the
	// applied objects go. All four are VALUES -- the same flow shape runs on
	// k3d and on AKS, and only these differ (design D7).
	Namespace    string
	IngressClass string
	EdgeService  string
	EdgePort     int
	// MaxPerSite caps how many domains one deployable may bind (design D10).
	MaxPerSite int
}

// NewReconciler builds the production reconciler over the substrate this
// process can actually use (see provision.go's header).
func NewReconciler(store *Store, cfg Config, provisioner Provisioner, logger *slog.Logger) *Reconciler {
	return &Reconciler{
		store:        store,
		resolver:     NewSystemResolver(),
		provisioner:  provisioner,
		logger:       logger,
		edgeHost:     cfg.EdgeHost,
		acmeIssuer:   cfg.ACMEIssuer,
		namespace:    cfg.Namespace,
		ingressClass: cfg.IngressClass,
		edgeService:  cfg.EdgeService,
		edgePort:     cfg.EdgePort,
		now:          func() time.Time { return time.Now().UTC() },
	}
}

// PassResult is what one sweep did, for the automation's step result and the
// log line.
type PassResult struct {
	Checked  int `json:"checked"`
	Verified int `json:"verified"`
	Issued   int `json:"issued"`
	Removed  int `json:"removed"`
	Failed   int `json:"failed"`
}

// Run performs one reconciliation pass.
//
// A per-row failure never aborts the pass. One binding whose DNS provider is
// timing out must not stop another binding's certificate from being noticed
// Ready -- and a sweep that gave up on the first error would make the slowest
// domain in the cluster the pace of every other one.
func (r *Reconciler) Run(ctx context.Context) (PassResult, error) {
	var out PassResult
	if r == nil || r.store == nil {
		return out, fmt.Errorf("customdomain: reconciler has no store")
	}
	bindings, err := r.store.ToReconcile(ctx)
	if err != nil {
		return out, err
	}
	for _, b := range bindings {
		// The query already excludes the two settled statuses; this is the
		// same rule stated once more in Go, as a guard rather than a filter --
		// a future widening of that filter must not silently start dispatching
		// cluster operations for rows that are done.
		if !NonTerminal(b.Status) {
			continue
		}
		out.Checked++
		if err := r.step(ctx, b, &out); err != nil {
			out.Failed++
			r.warn("custom domain reconciliation step failed", "hostname", b.Hostname, "status", b.Status, "error", err)
		}
	}
	return out, nil
}

// step advances one binding by at most one state.
//
// ONE STATE PER PASS, deliberately. A binding that verifies now could have its
// bind dispatched in the same pass, and the temptation to do that is real
// because it is two minutes faster. It is refused because the row is the state
// machine: writing `issuing` and then immediately acting on it means the
// dispatch is driven by a variable in this function rather than by a row every
// replica can see, and two replicas running the sweep would both act on the
// same binding having each written it. Letting the next pass pick the row up
// makes the row the only coordination point there is.
func (r *Reconciler) step(ctx context.Context, b Binding, out *PassResult) error {
	switch b.Status {
	case StatusPendingDNS, StatusVerifying:
		return r.verify(ctx, b, out)
	case StatusIssuing:
		return r.provision(ctx, b, out)
	case StatusRemoving:
		return r.unprovision(ctx, b, out)
	default:
		return nil
	}
}

// verify runs BOTH DNS checks and promotes only when both pass.
//
// The ownership check runs first because it is the one whose failure a person
// can act on immediately -- "publish this TXT record" is a complete
// instruction, while "your domain does not point here yet" often means "wait
// for propagation". Reporting the actionable miss first is the difference
// between a panel that tells somebody what to do and one that tells them to
// wait.
func (r *Reconciler) verify(ctx context.Context, b Binding, out *PassResult) error {
	now := r.now()

	if res := CheckOwnership(ctx, r.resolver, b.Hostname, b.Token); !res.OK {
		return r.store.RecordCheck(ctx, b.ID, res.Reason, res.Detail, now)
	}
	if res := CheckPointing(ctx, r.resolver, b.Hostname, r.edgeHost); !res.OK {
		return r.store.RecordCheck(ctx, b.ID, res.Reason, res.Detail, now)
	}

	// BOTH passed. This is the ONLY line in the tree that reaches `issuing`.
	if err := r.store.MarkVerified(ctx, b.ID, now); err != nil {
		return err
	}
	out.Verified++
	r.info("custom domain verified", "hostname", b.Hostname, "site", b.SiteID)
	return nil
}

// provision applies the exact-host Ingress + Certificate, idempotently, and
// promotes to `live` only when the certificate is Ready.
func (r *Reconciler) provision(ctx context.Context, b Binding, out *PassResult) error {
	now := r.now()
	res, err := r.provisioner.Bind(ctx, r.request(b))
	if err != nil {
		// The substrate could not RUN -- an unregistered script id, a missing
		// backend, no envelope on stdout. That is an engine-side fault rather
		// than a statement about this domain, so it is recorded on the row as
		// issuance_failed with the reason, and logged.
		return r.store.RecordIssuanceFailure(ctx, b.ID, ReasonIssuanceFailed, err.Error(), now)
	}
	if res.Reason != "" {
		return r.store.RecordIssuanceFailure(ctx, b.ID, res.Reason, res.Detail, now)
	}
	if !res.CertificateReady {
		// APPLIED, NOT YET READY -- the ordinary case for the first minute of
		// an HTTP-01 order. Not a failure and deliberately not recorded as
		// one: `issuing` with no failureReason is exactly "we asked and are
		// waiting", and putting a reason here would make a normal wait look
		// like a problem. lastCheckedAt still moves, so the panel can say when
		// we last looked.
		return r.store.RecordIssuingProgress(ctx, b.ID, res.Note, now)
	}
	if err := r.store.MarkLive(ctx, b.ID, now); err != nil {
		return err
	}
	out.Issued++
	r.info("custom domain live", "hostname", b.Hostname, "site", b.SiteID)
	return nil
}

// unprovision removes the pair and closes the walk.
//
// THE ROW IS ALREADY NOT SERVING by the time this runs: removeCustomDomain
// wrote `removing`, and liveCustomDomainByHostname filters on `status=="live"`,
// so the edge stopped answering at that write. This is the cleanup, and its
// failure is loud but not urgent.
func (r *Reconciler) unprovision(ctx context.Context, b Binding, out *PassResult) error {
	now := r.now()
	res, err := r.provisioner.Unbind(ctx, r.request(b))
	if err != nil {
		return r.store.RecordIssuanceFailure(ctx, b.ID, ReasonIssuanceFailed, err.Error(), now)
	}
	if res.Reason != "" {
		return r.store.RecordIssuanceFailure(ctx, b.ID, res.Reason, res.Detail, now)
	}
	if err := r.store.MarkRemoved(ctx, b.ID, now); err != nil {
		return err
	}
	out.Removed++
	r.info("custom domain removed", "hostname", b.Hostname, "site", b.SiteID)
	return nil
}

// request composes the substrate-independent description of one binding's
// cluster objects.
func (r *Reconciler) request(b Binding) BindRequest {
	return BindRequest{
		Hostname:     b.Hostname,
		DomainID:     b.ID,
		SiteID:       b.SiteID,
		Namespace:    r.namespace,
		Issuer:       r.acmeIssuer,
		IngressClass: r.ingressClass,
		Service:      r.edgeService,
		Port:         r.edgePort,
	}
}

// refusalReason maps a capability envelope onto one of the two typed issuance
// reasons.
//
// The script names `no_acme_issuer` in its result payload, because an exit code
// says "refused" and not WHICH refusal -- and the panel renders the typed
// reason, not the code. Anything else the script refuses on is issuance_failed
// with its own sentence in the detail: an unrecognised refusal must still reach
// the row rather than being dropped for not being on a list.
func refusalReason(res deploycontrol.CapabilityResult) string {
	if reasonFromResult(res) == ReasonNoACMEIssuer {
		return ReasonNoACMEIssuer
	}
	return ReasonIssuanceFailed
}

func refusalDetail(res deploycontrol.CapabilityResult, cerr error) string {
	if note := stringField(res, "detail"); note != "" {
		return note
	}
	return cerr.Error()
}

func reasonFromResult(res deploycontrol.CapabilityResult) string {
	return stringField(res, "reason")
}

// certificateReady reads the bind envelope's readiness answer.
//
// `live` is reachable only through this (design D6): applying a Certificate
// object is not the same as holding a certificate, and a status that went live
// on the apply would tell somebody their domain was serving while browsers
// were still getting the ingress controller's self-signed default.
func certificateReady(res deploycontrol.CapabilityResult) bool {
	return boolField(res, "certificateReady")
}

func certificateNote(res deploycontrol.CapabilityResult) string {
	if note := stringField(res, "certificateStatus"); note != "" {
		return note
	}
	return "the Ingress and Certificate are applied; waiting for the certificate to become Ready"
}

// decodeResult decodes a capability envelope's capability-specific fields.
//
// A miss returns an empty map rather than an error, and every reader above
// treats an absent field as its zero value. That is the fail-closed reading
// for the one field it actually matters on: an envelope with no
// `certificateReady` is read as NOT ready, so a script that stopped reporting
// readiness would leave bindings waiting rather than declaring them live on no
// evidence.
func decodeResult(res deploycontrol.CapabilityResult) map[string]any {
	if len(res.Result) == 0 {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(res.Result, &m); err != nil || m == nil {
		return map[string]any{}
	}
	return m
}

func stringField(res deploycontrol.CapabilityResult, key string) string {
	m, ok := decodeResult(res)[key]
	if !ok {
		return ""
	}
	s, _ := m.(string)
	return strings.TrimSpace(s)
}

func boolField(res deploycontrol.CapabilityResult, key string) bool {
	v, ok := decodeResult(res)[key]
	if !ok {
		return false
	}
	if b, isBool := v.(bool); isBool {
		return b
	}
	// A shell script's JSON is only as typed as the script wrote it, and
	// cap_result_set emits every value as a STRING unless the caller reaches
	// for cap_result_set_raw. Accepting "true" as well as true is what keeps a
	// working script from being read as a permanently-not-ready certificate.
	s, _ := v.(string)
	return strings.EqualFold(strings.TrimSpace(s), "true")
}

func (r *Reconciler) info(msg string, args ...any) {
	if r.logger != nil {
		r.logger.Info(msg, append([]any{"component", "customDomain"}, args...)...)
	}
}

func (r *Reconciler) warn(msg string, args ...any) {
	if r.logger != nil {
		r.logger.Warn(msg, append([]any{"component", "customDomain"}, args...)...)
	}
}
