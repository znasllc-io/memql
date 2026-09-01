package customdomain

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/znasllc-io/memql/component/automations/steps"
	"github.com/znasllc-io/memql/component/deploycontrol"
)

// provision.go -- the two objects a bound custom domain needs, and the two
// substrates that can put them there.
//
// ===========================================================================
// WHY THERE ARE TWO SUBSTRATES, AND WHY THAT IS NOT TWO MECHANISMS
// ===========================================================================
//
// The design (D2/D6) specifies capability-script reconciliation: an idempotent
// `scripts/deploy/bind-custom-domain.sh` that applies an exact-host Ingress and
// a cert-manager Certificate, dispatched through the action seam. Those scripts
// exist, are contract-conformant, are on the runner allowlist, and are what an
// operator or the cockpit surface runs.
//
// THEY CANNOT RUN INSIDE AN ENGINE POD. The runtime image is
// gcr.io/distroless/base-debian12: no bash, no kubectl, and no scripts/ tree --
// `.dockerignore` excludes deploy/k8s from the build context and nothing mounts
// the repository. This is not a hypothesis; it is the same wall memql#4257 hit
// with deploy-control, whose header in component/deploycontrol/k8sapi.go
// records it in detail along with why kubectl-in-the-image, a sidecar and
// client-go were each rejected. A sweep that dispatched only the script would
// have left every binding in `issuing` forever on every deployed cluster, while
// passing every test and running perfectly from a developer's shell.
//
// So the reconciler holds ONE Provisioner interface with two implementations,
// chosen by what the process can actually do rather than by what environment it
// thinks it is in (there is no tier word anywhere in this file --
// TestNoEnvironmentBranchingInEngineCode would fail the build on one, and
// rightly):
//
//	in a pod  -> apiProvisioner    the Kubernetes API server, with the pod's own
//	                               ServiceAccount. deploycontrol.ClusterAPI, the
//	                               same transport and the same reasoning.
//	elsewhere -> scriptProvisioner the allowlisted capability script, through
//	                               steps.RunCapabilityScript and
//	                               deploycontrol.ParseCapabilityResult.
//
// Both write the SAME two objects with the same names and the same field
// manager, so a domain bound by one and re-reconciled by the other converges
// rather than fighting. That is what makes this one mechanism with two
// transports rather than the two competing reconcilers D2 rejects: neither is
// reachable except through this sweep, and neither can express anything the
// other cannot.

// Outcome is what one provisioning attempt saw.
type Outcome struct {
	// Applied is true when the objects are in place, whether or not the
	// certificate is Ready yet.
	Applied bool
	// CertificateReady gates `live` (design D6). Applying a Certificate is not
	// the same as holding one: a status that went live on the apply would tell
	// somebody their domain was serving while browsers were still being handed
	// the ingress controller's self-signed default.
	CertificateReady bool
	// Note is what to show while waiting -- the certificate's own condition
	// message when there is one.
	Note string
	// Reason is a typed failure, or empty on success.
	Reason string
	// Detail is the substrate's own sentence for that failure.
	Detail string
}

// Provisioner applies and removes a custom domain's cluster objects.
type Provisioner interface {
	// Bind applies the exact-host Ingress and the Certificate, idempotently,
	// and reports whether the certificate is Ready.
	Bind(ctx context.Context, req BindRequest) (Outcome, error)
	// Unbind removes both objects. Absent is success.
	Unbind(ctx context.Context, req BindRequest) (Outcome, error)
	// Describe names the substrate, for the boot log.
	Describe() string
}

// BindRequest is everything either substrate needs.
type BindRequest struct {
	Hostname     string
	DomainID     string
	SiteID       string
	Namespace    string
	Issuer       string
	IngressClass string
	Service      string
	Port         int
}

// objectName is the name both objects carry: `custom-domain-<domainId>`.
//
// KEYED ON THE ROW ID, NOT THE HOSTNAME, and the difference matters twice. A
// hostname is not a legal Kubernetes object name (dots are allowed but a
// leading digit or a 253-character host is not, and a name must be a DNS-1123
// SUBDOMAIN), so hostname-derived names need sanitising -- and any sanitiser
// maps two distinct hostnames onto one name for some pair of inputs, which
// means one binding silently overwriting another's Ingress. The row id is
// already short, already unique and already the thing being reconciled.
func objectName(domainID string) string {
	id := strings.ToLower(strings.TrimSpace(domainID))
	id = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		default:
			return '-'
		}
	}, id)
	id = strings.Trim(id, "-")
	if id == "" {
		id = "unknown"
	}
	if len(id) > 200 {
		id = id[:200]
	}
	return "custom-domain-" + id
}

// fieldManager is the server-side-apply owner both substrates declare.
//
// ONE VALUE FOR BOTH, deliberately: server-side apply tracks ownership per
// manager, so a script binding under one name and the engine re-applying under
// another would leave the API server arbitrating between two managers that
// think they own the same fields. They are the same actor doing the same job.
const fieldManager = "memql-custom-domain"

// ---------------------------------------------------------------------------
// The objects
// ---------------------------------------------------------------------------

// IngressObject is the exact-host Ingress that routes a client's domain to the
// edge.
//
// AN EXACT HOST, NEVER A WILDCARD (memql#4224). The cluster's own `*.<domain>`
// rule cannot match a client's domain at all -- it is a different zone -- so
// every custom domain needs a rule of its own, and ingress-nginx builds a
// certificate-bearing server block per RULE host rather than per tls host, so
// the rule and the tls entry both have to name it.
func IngressObject(req BindRequest) map[string]any {
	name := objectName(req.DomainID)
	annotations := map[string]any{}
	if strings.TrimSpace(req.Issuer) != "" {
		// The issuer annotation is what makes cert-manager pick the Ingress up
		// at all on the ingress-shim path; the standalone Certificate below is
		// the belt to its braces, and the two converge on the same Secret.
		annotations["cert-manager.io/cluster-issuer"] = strings.TrimSpace(req.Issuer)
	}
	obj := map[string]any{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "Ingress",
		"metadata": map[string]any{
			"name":      name,
			"namespace": req.Namespace,
			"labels": map[string]any{
				"app.kubernetes.io/part-of":  "memql",
				"app.kubernetes.io/name":     "custom-domain",
				"memql/custom-domain-id":     req.DomainID,
				"memql/custom-domain-siteId": req.SiteID,
			},
			"annotations": annotations,
		},
		"spec": map[string]any{
			"ingressClassName": req.IngressClass,
			"tls": []any{map[string]any{
				"hosts":      []any{req.Hostname},
				"secretName": name + "-tls",
			}},
			"rules": []any{map[string]any{
				"host": req.Hostname,
				"http": map[string]any{
					"paths": []any{map[string]any{
						"path":     "/",
						"pathType": "Prefix",
						"backend": map[string]any{
							"service": map[string]any{
								"name": req.Service,
								"port": map[string]any{"number": req.Port},
							},
						},
					}},
				},
			}},
		},
	}
	return obj
}

// CertificateObject is the cert-manager Certificate for the one exact host.
//
// ONE dnsName. A wildcard SAN fails the WHOLE ACME order rather than just its
// own name (memql#4224), and HTTP-01 cannot issue one in any case -- which is
// precisely why a client's domain works over HTTP-01 here: the DNS now points
// at this cluster, so the challenge is servable, which is the whole reason the
// pointing check has to pass before this object is created.
func CertificateObject(req BindRequest) map[string]any {
	name := objectName(req.DomainID)
	return map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "Certificate",
		"metadata": map[string]any{
			"name":      name,
			"namespace": req.Namespace,
			"labels": map[string]any{
				"app.kubernetes.io/part-of": "memql",
				"app.kubernetes.io/name":    "custom-domain",
				"memql/custom-domain-id":    req.DomainID,
			},
		},
		"spec": map[string]any{
			"secretName": name + "-tls",
			"dnsNames":   []any{req.Hostname},
			"issuerRef": map[string]any{
				"name":  strings.TrimSpace(req.Issuer),
				"kind":  "ClusterIssuer",
				"group": "cert-manager.io",
			},
		},
	}
}

// ---------------------------------------------------------------------------
// The API-server substrate
// ---------------------------------------------------------------------------

type apiProvisioner struct{ api *deploycontrol.ClusterAPI }

// NewAPIProvisioner builds the in-cluster provisioner, or reports why it
// cannot.
func NewAPIProvisioner() (Provisioner, error) {
	api, err := deploycontrol.NewClusterAPI()
	if err != nil {
		return nil, err
	}
	return &apiProvisioner{api: api}, nil
}

func (p *apiProvisioner) Describe() string { return "kubernetes api (in-cluster ServiceAccount)" }

func (p *apiProvisioner) Bind(ctx context.Context, req BindRequest) (Outcome, error) {
	if strings.TrimSpace(req.Issuer) == "" {
		// REFUSE, DO NOT APPROXIMATE (design D7). A Certificate with an empty
		// issuerRef is accepted by the API server and then sits Pending
		// forever with a condition nobody reads, which is a pretend success --
		// the honest answer on a target that declares no ACME issuer is to say
		// so, every pass, and let the panel show it.
		return Outcome{Reason: ReasonNoACMEIssuer, Detail: "this cluster declares no ACME issuer, so no certificate can be requested for " + req.Hostname}, nil
	}

	name := objectName(req.DomainID)
	if err := p.apply(ctx, ingressPath(req.Namespace, name), IngressObject(req)); err != nil {
		return Outcome{Reason: ReasonIssuanceFailed, Detail: err.Error()}, nil
	}
	if err := p.apply(ctx, certificatePath(req.Namespace, name), CertificateObject(req)); err != nil {
		return Outcome{Applied: true, Reason: ReasonIssuanceFailed, Detail: err.Error()}, nil
	}

	ready, note := p.certificateReady(ctx, req.Namespace, name)
	return Outcome{Applied: true, CertificateReady: ready, Note: note}, nil
}

func (p *apiProvisioner) Unbind(ctx context.Context, req BindRequest) (Outcome, error) {
	name := objectName(req.DomainID)
	// The Certificate first, so cert-manager stops renewing before the route
	// disappears. Order is the only thing that makes the pair removable
	// without a window where a renewal races the deletion.
	if err := p.delete(ctx, certificatePath(req.Namespace, name)); err != nil {
		return Outcome{Reason: ReasonIssuanceFailed, Detail: err.Error()}, nil
	}
	if err := p.delete(ctx, ingressPath(req.Namespace, name)); err != nil {
		return Outcome{Reason: ReasonIssuanceFailed, Detail: err.Error()}, nil
	}
	return Outcome{Applied: true}, nil
}

// apply is a server-side apply: idempotent by construction, which is what makes
// re-running the sweep over an already-bound domain free. A second apply of an
// unchanged object is a no-op at the API server and creates no new ACME order.
func (p *apiProvisioner) apply(ctx context.Context, path string, obj map[string]any) error {
	body, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	// force=true because THIS is the owner. Without it, a field another
	// manager touched (an operator's one-off kubectl edit) makes every
	// subsequent apply fail with a conflict the sweep cannot resolve and a
	// person cannot see, and the binding sticks in `issuing` for a reason
	// nothing on the panel can explain.
	q := path + "?fieldManager=" + fieldManager + "&force=true"
	_, err = p.api.Do(ctx, http.MethodPatch, q, "application/apply-patch+yaml", body)
	return err
}

// delete removes an object, treating absent as success. Unbinding twice is a
// legitimate thing for a retry to do, and a NotFound that failed would make
// every redelivered removal look like a broken cluster.
//
// ON THE STATUS CODE, not on the message. Matching the string "404" would also
// swallow a 500 whose body happened to mention one, and the consequence there
// is a row walked to `removed` with its Ingress and Certificate still in the
// cluster -- a hostname being served that the graph says nothing serves.
func (p *apiProvisioner) delete(ctx context.Context, path string) error {
	_, err := p.api.Do(ctx, http.MethodDelete, path, "", nil)
	if deploycontrol.IsNotFound(err) {
		return nil
	}
	return err
}

// certificateReady reads the Certificate's Ready condition.
//
// Its MESSAGE is carried through verbatim as the note. cert-manager's own
// wording ("Issuing certificate as Secret does not exist", "Waiting for
// http-01 challenge propagation") is more useful than anything composed here,
// and it is what somebody debugging a stuck order would go and read anyway.
func (p *apiProvisioner) certificateReady(ctx context.Context, namespace, name string) (bool, string) {
	raw, err := p.api.Do(ctx, http.MethodGet, certificatePath(namespace, name), "", nil)
	if err != nil {
		return false, "could not read the certificate's status: " + err.Error()
	}
	var doc struct {
		Status struct {
			Conditions []struct {
				Type    string `json:"type"`
				Status  string `json:"status"`
				Message string `json:"message"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return false, "could not read the certificate's status: " + err.Error()
	}
	for _, c := range doc.Status.Conditions {
		if c.Type != "Ready" {
			continue
		}
		if strings.EqualFold(c.Status, "True") {
			return true, c.Message
		}
		return false, strings.TrimSpace(c.Message)
	}
	return false, "the certificate has no Ready condition yet -- cert-manager has not started the order"
}

func ingressPath(namespace, name string) string {
	return fmt.Sprintf("apis/networking.k8s.io/v1/namespaces/%s/ingresses/%s", namespace, name)
}

func certificatePath(namespace, name string) string {
	return fmt.Sprintf("apis/cert-manager.io/v1/namespaces/%s/certificates/%s", namespace, name)
}

// ---------------------------------------------------------------------------
// The capability-script substrate
// ---------------------------------------------------------------------------

// scriptProvisioner dispatches the allowlisted bind/unbind scripts through the
// engine's own capability-script runner.
//
// This is the design's D2/D6 path, and it is what runs on a surface that HAS a
// checkout and a kubectl -- a developer's machine, the cockpit runner. In a
// pod it is unreachable and the API substrate takes over; see the file header.
type scriptProvisioner struct{ run ScriptRunner }

// NewScriptProvisioner builds the capability-script provisioner over the
// engine's allowlisted runner.
func NewScriptProvisioner() Provisioner { return &scriptProvisioner{run: allowlistedScripts{}} }

func (p *scriptProvisioner) Describe() string {
	return "capability script (scripts/deploy/*-custom-domain.sh)"
}

func (p *scriptProvisioner) Bind(ctx context.Context, req BindRequest) (Outcome, error) {
	return p.dispatch(ctx, ScriptBind, map[string]any{
		"hostname":     req.Hostname,
		"domainId":     req.DomainID,
		"siteId":       req.SiteID,
		"namespace":    req.Namespace,
		"issuer":       req.Issuer,
		"ingressClass": req.IngressClass,
		"service":      req.Service,
		"port":         fmt.Sprintf("%d", req.Port),
	})
}

func (p *scriptProvisioner) Unbind(ctx context.Context, req BindRequest) (Outcome, error) {
	return p.dispatch(ctx, ScriptUnbind, map[string]any{
		"hostname":  req.Hostname,
		"domainId":  req.DomainID,
		"namespace": req.Namespace,
	})
}

// dispatch runs one script and folds its envelope into an Outcome.
//
// A REFUSAL IS A RESULT, NOT AN ERROR. steps.RunCapabilityScript returns the
// parsed envelope rather than turning a non-ok one into a Go error precisely so
// this can happen: the `no_acme_issuer` a local cluster answers with every two
// minutes has to reach the row as a typed reason, and an error would have ended
// the pass before anything could record it. A Go error here means the script
// did not RUN -- unknown id, missing backend, no envelope on stdout.
func (p *scriptProvisioner) dispatch(ctx context.Context, id string, params map[string]any) (Outcome, error) {
	res, err := p.run.Run(ctx, id, params)
	if err != nil {
		return Outcome{}, err
	}
	if cerr := res.Err(); cerr != nil {
		return Outcome{
			Reason: refusalReason(res),
			Detail: refusalDetail(res, cerr),
		}, nil
	}
	return Outcome{
		Applied:          true,
		CertificateReady: certificateReady(res),
		Note:             certificateNote(res),
	}, nil
}

// allowlistedScripts is the production ScriptRunner: the engine's own
// capability-script runner, over the same static allowlist an authored action
// resolves through.
type allowlistedScripts struct{}

func (allowlistedScripts) Run(ctx context.Context, id string, params map[string]any) (deploycontrol.CapabilityResult, error) {
	return steps.RunCapabilityScript(ctx, id, params)
}

// ScriptRunner is the capability-script seam, narrowed to what this package
// uses.
//
// An interface rather than a direct call so a test can drive the whole state
// machine with no scripts, no kubectl and no cluster -- which is what makes the
// never-issue-before-verified proof a unit test rather than a cluster-e2e lane
// that CI skips on every runner.
type ScriptRunner interface {
	Run(ctx context.Context, id string, params map[string]any) (deploycontrol.CapabilityResult, error)
}

// The two capability-script ids, registered in
// component/automations/steps.capabilityScriptAllowlist.
const (
	ScriptBind   = "domain.bind"
	ScriptUnbind = "domain.unbind"
)

// SelectProvisioner picks the substrate this process can actually use.
//
// The in-cluster one first, because a pod is where this runs in production and
// where the script substrate is structurally unavailable. The probe is
// deploycontrol.InClusterAvailable -- "is there a projected ServiceAccount
// token AND an API server address" -- which is a question about capability
// rather than about which environment somebody thinks they are in.
func SelectProvisioner() (Provisioner, error) {
	if deploycontrol.InClusterAvailable() {
		return NewAPIProvisioner()
	}
	return NewScriptProvisioner(), nil
}
