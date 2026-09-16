package customdomain

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/frontdoor"
)

// accountdoor_provision.go -- the cluster objects behind an account's reserved
// front door (epic memql#5168, design E).
//
// # WHY THIS LIVES IN THE CUSTOM-DOMAIN PACKAGE
//
// It is the same job one level up. A custom domain is one client hostname
// pointed at the edge; a reserved MemQL name is three hostnames pointed at
// three different services. Same DNS resolver, same pointing check, same
// capability-script seam, same two substrates chosen by the same probe, same
// status ladder, same one-transition-per-pass reconciler. A second package
// would be a second copy of CheckPointing's apex handling and a second
// substrate selection to keep in agreement with this one.
//
// # WHAT DIFFERS: FIVE OBJECTS, NOT TWO
//
// One Certificate naming all three hosts, and FOUR Ingresses -- app. to the
// edge, id. to identity, and TWO for api., because an ingress controller's
// backend protocol is a per-Service annotation and the bff's h2c edge
// (:50051) and its HTTP edge (:8085) therefore cannot share an object. The
// cluster's own front door carries the same pair for the same reason.
//
// The gRPC one carries TWO rules, and the first is not the bff's (epic
// memql#5218, D10): the worker stream's service prefix
// (frontdoor.WorkerServicePath) to the agent, then `/` to the bff.
// WorkerService.Stream is served by the agent node and nothing else, and a
// door that routed only the catch-all would answer a cockpit dialling a
// client's api. host `Unimplemented: unknown service`, exactly as the
// cluster's own api host did before the rule existed.
//
// # THE CERTIFICATE IS THE ACTIVATION RULE (design D8)
//
// An HTTP-01 order cannot go Ready unless every dnsName in it solves, so
// naming all three hosts on ONE certificate makes the door all-or-nothing with
// nothing for the reconciler to police: one order, one rate-limit unit against
// Let's Encrypt, one Ready condition to promote on. Three certificates would
// have needed a rule saying "wait for all of them", and that rule would have
// been the thing to get wrong.

// DoorBindRequest is everything either substrate needs to serve one reserved
// name.
type DoorBindRequest struct {
	// AccountID keys every object's name. NOT the door row id: a door that is
	// torn down and reopened for the same account must converge on the same
	// objects rather than leave orphans behind it, and the account is the
	// thing that persists across that.
	AccountID string
	// DoorID is recorded as a label so an operator reading `kubectl get ing`
	// can find the row.
	DoorID string
	// ReservedName is the name the three hosts sit beneath.
	ReservedName string

	Namespace    string
	Issuer       string
	IngressClass string

	// EdgeService / EdgePort serve app.
	EdgeService string
	EdgePort    int
	// BFFHTTPService / BFFHTTPPort serve api.'s generated path block.
	BFFHTTPService string
	BFFHTTPPort    int
	// BFFGRPCService / BFFGRPCPort serve api.'s h2c catch-all.
	BFFGRPCService string
	BFFGRPCPort    int
	// AgentGRPCService / AgentGRPCPort serve api.'s worker-stream prefix, the
	// one gRPC rule ahead of the catch-all (frontdoor.WorkerServicePath).
	AgentGRPCService string
	AgentGRPCPort    int
	// IdentityService / IdentityPort serve id.
	IdentityService string
	IdentityPort    int
}

// doorObjectName is the base name every object carries:
// `account-front-door-<accountId>`, with a per-object suffix for the four
// Ingresses.
//
// Keyed on the account id for objectName's reasons -- a hostname is not a
// legal Kubernetes object name and any sanitiser collides some pair of inputs
// -- plus the one above: the account outlives the door row.
func doorObjectName(accountID string) string {
	id := strings.ToLower(strings.TrimSpace(accountID))
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
	return "account-front-door-" + id
}

// doorFieldManager is the server-side-apply owner both substrates declare, and
// it is DELIBERATELY DISTINCT from fieldManager.
//
// Custom domains and front doors never write the same object, so sharing a
// manager would buy nothing -- and it would make `kubectl get ... -o yaml`
// unable to say which of the two reconcilers owns a field, which is the one
// question managed-fields exists to answer.
const doorFieldManager = "memql-account-front-door"

// doorLabels are on every object: enough for an operator to find every piece
// of one account's door with a single selector.
func doorLabels(req DoorBindRequest) map[string]any {
	return map[string]any{
		"app.kubernetes.io/part-of":   "memql",
		"app.kubernetes.io/name":      "account-front-door",
		"memql/account-id":            req.AccountID,
		"memql/account-front-door-id": req.DoorID,
	}
}

// doorBackend is how ONE of a door's Ingresses reaches its Service, including
// the annotations that decide how the proxy talks to it.
//
// # THE ANNOTATIONS ARE THE PART THAT WAS MISSING, AND THEY ARE NOT DECORATION
//
// The first version of this file set only `cert-manager.io/cluster-issuer` plus
// the gRPC protocol flag, and both omissions were breakages rather than
// oversights. The cluster's own generated front door carries the answers and
// says why in its comments; a door has to carry the same ones, because it
// reaches the same Services.
//
//   - identity serves TLS IN-CLUSTER under the internal CA
//     (deploy/k8s/base/identity.yaml sets MEMQL_HTTP_TLS_CERT_FILE, and its
//     probes use scheme: HTTPS), which the ingress controller's trust store
//     does not carry. Without `backend-protocol: HTTPS` + `proxy-ssl-verify:
//     off` the proxy speaks plain HTTP to a TLS port and every request to
//     `id.<reserved>` answers 502. Nothing upstream would have caught it: the
//     three-SAN certificate goes Ready on its own solver Ingress, the
//     reconciler promotes on that alone, and the rail says `live` while
//     sign-in -- the whole of the per-door surface -- is dead.
//   - ingress-nginx's default `proxy-body-size` is 1m (memql#4782), which
//     makes every documented upload cap on the api host quietly unreachable:
//     the 413 comes from the proxy, names no knob, and a client uploading a
//     5 MiB artifact through their own api host fails where the identical call
//     to the cluster's own api host succeeds. Traefik enforces no default
//     limit, so a local door is no evidence here.
type doorBackend struct {
	suffix      string
	host        string
	service     string
	port        int
	annotations map[string]string
}

// doorRoute is one path entry of an Ingress rule and the Service it reaches.
// Most of a door's Ingresses route every path to the one Service the
// doorBackend names; the gRPC one is the exception, with two backends behind
// one host, which is why the route names its own.
type doorRoute struct {
	path    string
	service string
	port    int
}

// doorIngress renders one host -> one service Ingress with a `/` Prefix rule.
func doorIngress(req DoorBindRequest, b doorBackend) map[string]any {
	return doorIngressWithPaths(req, b, []string{"/"})
}

// doorIngressWithPaths renders an Ingress carrying one rule per path, every
// one to the backend's Service.
func doorIngressWithPaths(req DoorBindRequest, b doorBackend, paths []string) map[string]any {
	routes := make([]doorRoute, 0, len(paths))
	for _, p := range paths {
		routes = append(routes, doorRoute{path: p, service: b.service, port: b.port})
	}
	return doorIngressWithRoutes(req, b, routes)
}

// doorIngressWithRoutes renders an Ingress carrying one rule per route, in the
// order given. The backend's own service and port are ignored here -- each
// route names its own -- and it is passed for its suffix, host and
// annotations.
//
// EVERY ENTRY IS pathType: Prefix, matching cmd/frontdoorpaths' render(). That
// generator's comment carries the reasoning; repeating the choice here rather
// than the reasoning is deliberate, because the place a decision is argued
// should be the place that produces the list.
func doorIngressWithRoutes(req DoorBindRequest, b doorBackend, routes []doorRoute) map[string]any {
	annotations := map[string]any{}
	// The explicit multi-host Certificate owns this shared TLS Secret.
	// Enabling ingress-shim here would create a competing certificate.
	for k, v := range b.annotations {
		annotations[k] = v
	}

	rules := make([]any, 0, len(routes))
	for _, r := range routes {
		rules = append(rules, map[string]any{
			"path":     r.path,
			"pathType": "Prefix",
			"backend": map[string]any{
				"service": map[string]any{
					"name": r.service,
					"port": map[string]any{"number": r.port},
				},
			},
		})
	}

	name := doorObjectName(req.AccountID) + b.suffix
	return map[string]any{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "Ingress",
		"metadata": map[string]any{
			"name":        name,
			"namespace":   req.Namespace,
			"labels":      doorLabels(req),
			"annotations": annotations,
		},
		"spec": map[string]any{
			"ingressClassName": req.IngressClass,
			"tls": []any{map[string]any{
				"hosts":      []any{b.host},
				"secretName": doorObjectName(req.AccountID) + "-tls",
			}},
			"rules": []any{map[string]any{
				"host": b.host,
				"http": map[string]any{"paths": rules},
			}},
		},
	}
}

// DoorCertificateObject is the ONE Certificate, naming all three hosts.
func DoorCertificateObject(req DoorBindRequest) map[string]any {
	name := doorObjectName(req.AccountID)
	sans := make([]any, 0, len(frontdoor.AccountRoles()))
	for _, h := range frontdoor.AccountCertificateSANs(req.ReservedName) {
		sans = append(sans, h)
	}
	return map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "Certificate",
		"metadata": map[string]any{
			"name":      name,
			"namespace": req.Namespace,
			"labels":    doorLabels(req),
		},
		"spec": map[string]any{
			"secretName": name + "-tls",
			"dnsNames":   sans,
			"issuerRef": map[string]any{
				"name":  req.Issuer,
				"kind":  "ClusterIssuer",
				"group": "cert-manager.io",
			},
		},
	}
}

// doorIngressObjects is the four Ingresses, in apply order, each with the
// object-name suffix its unbind counterpart deletes.
//
// THE SUFFIXES ARE THE CONTRACT WITH unbind-account-front-door.sh, which
// deletes exactly these four names. account_front_door_test.go pins the bind
// and unbind scripts against each other, and TestTheGoProvisionerNamesTheSame
// ObjectsTheScriptDoes pins this against both.
func doorIngressObjects(req DoorBindRequest, apiPaths []string) []map[string]any {
	appHost := frontdoor.AccountRoleHost(frontdoor.AccountRoleApp, req.ReservedName)
	apiHost := frontdoor.AccountRoleHost(frontdoor.AccountRoleAPI, req.ReservedName)
	idHost := frontdoor.AccountRoleHost(frontdoor.AccountRoleID, req.ReservedName)

	out := []map[string]any{
		// app. -> the edge, plain HTTP on :8085, exactly as the cluster's own
		// os. rule reaches it.
		doorIngress(req, doorBackend{suffix: "-app", host: appHost, service: req.EdgeService, port: req.EdgePort}),
		// api. gRPC -> the worker stream's prefix to the agent, then the bff's
		// h2c edge for everything else. The agent rule is FIRST for the
		// reader (nginx orders locations by prefix length regardless), and
		// it is in THIS object because backend-protocol is Ingress-scoped
		// and both backends speak h2c. See the file comment.
		doorIngressWithRoutes(req, doorBackend{suffix: "-api-grpc", host: apiHost,
			annotations: map[string]string{"nginx.ingress.kubernetes.io/backend-protocol": "GRPC"}},
			[]doorRoute{
				{path: frontdoor.WorkerServicePath, service: req.AgentGRPCService, port: req.AgentGRPCPort},
				{path: "/", service: req.BFFGRPCService, port: req.BFFGRPCPort},
			}),
		// id. -> identity, which speaks TLS in-cluster. See doorBackend.
		doorIngress(req, doorBackend{suffix: "-id", host: idHost, service: req.IdentityService, port: req.IdentityPort,
			annotations: map[string]string{
				"nginx.ingress.kubernetes.io/backend-protocol": "HTTPS",
				"nginx.ingress.kubernetes.io/proxy-ssl-verify": "off",
			}}),
	}
	// NO HTTP INGRESS WHEN THERE ARE NO PATHS, rather than one with an empty
	// rule list, which the API server rejects -- taking the other three hosts
	// down with it. In practice frontdoor.BFFHTTPPaths is never empty; this is
	// what happens if it ever is, and it is a door missing its HTTP routes
	// rather than a door that failed to come up.
	if len(apiPaths) > 0 {
		out = append(out, doorIngressWithPaths(req, doorBackend{
			suffix: "-api", host: apiHost, service: req.BFFHTTPService, port: req.BFFHTTPPort,
			// The upload cap. See doorBackend.
			annotations: map[string]string{"nginx.ingress.kubernetes.io/proxy-body-size": doorProxyBodySize},
		}, apiPaths))
	}
	return out
}

// doorIngressSuffixes is every suffix an unbind must delete, including the
// HTTP one that may legitimately not exist.
var doorIngressSuffixes = []string{"-app", "-api", "-api-grpc", "-id"}

// DoorProvisioner applies and removes one account front door's objects.
//
// A SEPARATE INTERFACE FROM Provisioner rather than two more methods on it.
// The two are chosen by the same probe and implemented by the same types, but
// a caller holding one has no business being able to call the other: the
// custom-domain reconciler must not be able to apply a front door, and the
// door reconciler must not be able to unbind somebody's website.
type DoorProvisioner interface {
	BindDoor(ctx context.Context, req DoorBindRequest, apiPaths []string) (Outcome, error)
	UnbindDoor(ctx context.Context, req DoorBindRequest) (Outcome, error)
	Describe() string
}

// ---------------------------------------------------------------------------
// The API-server substrate
// ---------------------------------------------------------------------------

func (p *apiProvisioner) BindDoor(ctx context.Context, req DoorBindRequest, apiPaths []string) (Outcome, error) {
	if strings.TrimSpace(req.Issuer) == "" {
		// REFUSE, DO NOT APPROXIMATE (custom domains D7). A Certificate with
		// an empty issuerRef is accepted and then sits Pending forever with a
		// condition nobody reads.
		return Outcome{Reason: ReasonNoACMEIssuer, Detail: "this cluster declares no ACME issuer, so no certificate can be requested for " + req.ReservedName}, nil
	}
	if strings.TrimSpace(req.ReservedName) == "" {
		return Outcome{Reason: ReasonIssuanceFailed, Detail: "the door carries no reserved name, so there are no hosts to serve"}, nil
	}

	name := doorObjectName(req.AccountID)
	for _, obj := range doorIngressObjects(req, apiPaths) {
		meta, _ := obj["metadata"].(map[string]any)
		objName, _ := meta["name"].(string)
		if err := p.applyAs(ctx, doorFieldManager, ingressPath(req.Namespace, objName), obj); err != nil {
			return Outcome{Reason: ReasonIssuanceFailed, Detail: err.Error()}, nil
		}
	}
	// THE CERTIFICATE LAST. The HTTP-01 challenge is served through the
	// Ingresses, so requesting it before they exist starts an order whose
	// first attempt is guaranteed to fail -- and cert-manager backs off after
	// a failure, which would make every door slower to come up for no reason.
	if err := p.applyAs(ctx, doorFieldManager, certificatePath(req.Namespace, name), DoorCertificateObject(req)); err != nil {
		return Outcome{Applied: true, Reason: ReasonIssuanceFailed, Detail: err.Error()}, nil
	}

	ready, note := p.certificateReady(ctx, req.Namespace, name)
	return Outcome{Applied: true, CertificateReady: ready, Note: note}, nil
}

func (p *apiProvisioner) UnbindDoor(ctx context.Context, req DoorBindRequest) (Outcome, error) {
	name := doorObjectName(req.AccountID)
	// The Certificate first, so cert-manager stops renewing before the routes
	// disappear -- the same window Unbind's ordering closes.
	if err := p.delete(ctx, certificatePath(req.Namespace, name)); err != nil {
		return Outcome{Reason: ReasonIssuanceFailed, Detail: err.Error()}, nil
	}
	for _, suffix := range doorIngressSuffixes {
		if err := p.delete(ctx, ingressPath(req.Namespace, name+suffix)); err != nil {
			return Outcome{Reason: ReasonIssuanceFailed, Detail: err.Error()}, nil
		}
	}
	return Outcome{Applied: true}, nil
}

// ---------------------------------------------------------------------------
// The capability-script substrate
// ---------------------------------------------------------------------------

// The two capability-script ids, registered in
// component/automations/steps.capabilityScriptAllowlist.
const (
	ScriptDoorBind   = "frontdoor.bind"
	ScriptDoorUnbind = "frontdoor.unbind"
)

func (p *scriptProvisioner) BindDoor(ctx context.Context, req DoorBindRequest, apiPaths []string) (Outcome, error) {
	return p.dispatch(ctx, ScriptDoorBind, map[string]any{
		"accountId":    req.AccountID,
		"doorId":       req.DoorID,
		"reservedName": req.ReservedName,
		"namespace":    req.Namespace,
		"issuer":       req.Issuer,
		"ingressClass": req.IngressClass,
		// COMMA-SEPARATED, and paths cannot contain a comma, so the encoding
		// is lossless without escaping. The script's own dry run asserts the
		// rule count it rendered against the count it was handed, because a
		// count taken only from the input cannot notice a dropped field.
		"apiPaths":       strings.Join(apiPaths, ","),
		"edgeService":    req.EdgeService,
		"edgePort":       fmt.Sprintf("%d", req.EdgePort),
		"bffHttpService": req.BFFHTTPService,
		"bffHttpPort":    fmt.Sprintf("%d", req.BFFHTTPPort),
		"bffGrpcService": req.BFFGRPCService,
		"bffGrpcPort":    fmt.Sprintf("%d", req.BFFGRPCPort),
		// PASSED, NOT LEFT TO THE SCRIPT'S DEFAULT. The script spells the
		// same prefix as a default for the operator's manual path and a test
		// pins the two; the engine still hands over the constant it holds,
		// so a door bound from here routes what this binary routes.
		"workerServicePath": frontdoor.WorkerServicePath,
		"agentGrpcService":  req.AgentGRPCService,
		"agentGrpcPort":     fmt.Sprintf("%d", req.AgentGRPCPort),
		"identityService":   req.IdentityService,
		"identityPort":      fmt.Sprintf("%d", req.IdentityPort),
	})
}

func (p *scriptProvisioner) UnbindDoor(ctx context.Context, req DoorBindRequest) (Outcome, error) {
	return p.dispatch(ctx, ScriptDoorUnbind, map[string]any{
		"accountId":    req.AccountID,
		"doorId":       req.DoorID,
		"reservedName": req.ReservedName,
		"namespace":    req.Namespace,
	})
}

// ---------------------------------------------------------------------------
// The substrate that cannot
// ---------------------------------------------------------------------------

func (p refusingProvisioner) BindDoor(_ context.Context, _ DoorBindRequest, _ []string) (Outcome, error) {
	return Outcome{Reason: ReasonIssuanceFailed, Detail: "this node cannot provision cluster objects: " + p.reason}, nil
}

func (p refusingProvisioner) UnbindDoor(_ context.Context, _ DoorBindRequest) (Outcome, error) {
	return Outcome{Reason: ReasonIssuanceFailed, Detail: "this node cannot remove cluster objects: " + p.reason}, nil
}

// SelectDoorProvisioner picks the substrate this process can actually use, by
// the same probe SelectProvisioner uses and for the same reason: "is there a
// projected ServiceAccount token AND an API server address" is a question
// about capability rather than about which environment somebody thinks they
// are in.
//
// It returns the SAME concrete value SelectProvisioner would, which is why
// there is no risk of a node provisioning custom domains one way and front
// doors another.
func SelectDoorProvisioner() (DoorProvisioner, error) {
	p, err := SelectProvisioner()
	if err != nil {
		return nil, err
	}
	dp, ok := p.(DoorProvisioner)
	if !ok {
		return nil, fmt.Errorf("customdomain: substrate %q cannot provision account front doors", p.Describe())
	}
	return dp, nil
}

// The Services a door's four Ingresses point at.
//
// CONSTANTS, NOT ENVIRONMENT. The first version made all six env-tunable and
// the env-registry drift gate was right to refuse them: the cluster's own
// generated front door names these same Services as literals in
// cmd/frontdoorhosts/manifest.go, and a per-account door pointing at a
// different bff from the one `api.<domain>` reaches is not a configuration
// anybody wants -- it is a way to serve a client's people a different engine
// by accident.
//
// EdgeService and EdgePort are NOT here: those come from the custom-domain
// Config, which already carries them because the wildcard rule and every
// bound domain reach the same edge, and having two answers to "which edge" is
// exactly what this block avoids for the other three.
// doorProxyBodySize matches the cluster's own api Ingress. It is a constant
// for the reason the Services are: a door whose upload cap differs from the
// cluster's own api host is a client discovering that the same call works at
// one address and 413s at another.
const doorProxyBodySize = "48m"

const (
	doorBFFHTTPService = "bff-http"
	doorBFFHTTPPort    = 8085
	doorBFFGRPCService = "bff"
	doorBFFGRPCPort    = 50051
	// The agent's gRPC edge, which WorkerService.Stream is registered on and
	// the bff is not (epic memql#5218, D10). The same literal
	// cmd/frontdoorhosts/manifest.go writes for the cluster's own api host.
	doorAgentGRPCService = "agent"
	doorAgentGRPCPort    = 50051
	doorIdentityService  = "identity"
	doorIdentityPort     = 8085
)

// doorConfig derives the front-door half of the reconciler's configuration
// from the custom-domain Config.
//
// THE SHARED VALUES ARE SHARED, not re-read. EdgeHost, ACMEIssuer, Namespace
// and IngressClass answer the same questions for both reconcilers, and reading
// them twice would let one sweep bind against a different issuer from the
// other on the same cluster.
func (c Config) doorConfig() DoorConfig {
	return DoorConfig{
		EdgeHost:     c.EdgeHost,
		ACMEIssuer:   c.ACMEIssuer,
		Namespace:    c.Namespace,
		IngressClass: c.IngressClass,

		// app. reaches the same edge Service the wildcard rule already does.
		EdgeService: c.EdgeService,
		EdgePort:    c.EdgePort,

		BFFHTTPService:   doorBFFHTTPService,
		BFFHTTPPort:      doorBFFHTTPPort,
		BFFGRPCService:   doorBFFGRPCService,
		BFFGRPCPort:      doorBFFGRPCPort,
		AgentGRPCService: doorAgentGRPCService,
		AgentGRPCPort:    doorAgentGRPCPort,
		IdentityService:  doorIdentityService,
		IdentityPort:     doorIdentityPort,
	}
}
