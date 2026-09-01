package deploycontrol

// k8sapi.go is the IN-CLUSTER execution substrate for deploy-control's
// Argo effects (memql#4257).
//
// ===========================================================================
// THE PROBLEM
// ===========================================================================
//
// Every DeployControl write verb executes through the Executor on the identity
// node, and the Executor shelled out to `kubectl`. In a deployed cluster none
// of that works, and it has never worked:
//
//   - the identity runtime image is DISTROLESS. There is no kubectl and no
//     git. `/app` holds exactly `healthcheck`, `memql` and `portal/`.
//   - `deploy/k8s/base/identity.yaml` bound no ServiceAccount with any
//     permission on `applications.argoproj.io`, so even a kubectl that
//     existed would have been refused by the API server.
//   - no overlay sets MEMQL_DEPLOY_REPO_ROOT, so the repo root fell back to
//     `os.Getwd()` = `/app`, and `/app/deploy` is in no image and mounted by
//     nothing (.dockerignore excludes `deploy/k8s` from the build context).
//
// All three are identical for local and cloud, so a cloud identity pod failed
// exactly as a k3d one did. The verbs answered honestly -- ok=false,
// reason=kickoff_failed, recorded, audited -- which is why this was a gap
// rather than a corruption. But the portal's cluster-operations surface could
// not complete a single write on a real install.
//
// ===========================================================================
// WHY THE KUBERNETES REST API, AND NOT THE THREE ALTERNATIVES
// ===========================================================================
//
// The issue named three candidate substrates. This is the fourth, and it
// dominates all of them for the effects actually required.
//
//   - kubectl + git IN THE IMAGE. Two more binaries in the most
//     security-sensitive node in the mesh, a distroless base abandoned, and
//     kubectl's whole surface reachable from any code path that can reach
//     os/exec. The RBAC would still be needed. The image grows for two HTTP
//     calls.
//   - A SIDECAR. Same binaries, now in a container sharing the pod's
//     ServiceAccount, plus an IPC channel to design and secure.
//   - client-go IN PROCESS. Correct in shape, expensive in fact: no k8s.io
//     dependency exists anywhere in this workspace today, and pulling
//     client-go + apimachinery + k8s.io/api into a module for two verbs is
//     hundreds of packages and a permanent upgrade obligation, for typed
//     access to a CRD (argoproj.io/v1alpha1) whose types are not in it anyway.
//
// What the effects need is: GET one Application, PATCH one Application twice,
// LIST two collections. That is plain HTTP against the API server with the
// pod's own ServiceAccount token -- exactly what kubectl does underneath,
// minus kubectl. Zero new dependencies, and least privilege is expressible
// PRECISELY: get/patch on one Application BY NAME, get/list on Rollouts and
// AnalysisRuns in one namespace. See the Role in deploy/k8s/base/identity.yaml.
//
// ===========================================================================
// WHAT THIS SUBSTRATE DELIBERATELY CANNOT DO
// ===========================================================================
//
// Two Executor methods have no in-cluster form and say so by NAME rather than
// failing as a generic kickoff error:
//
//   - Git / RunRollback need a repository. Rollback is `git revert` of the
//     overlay commit, which is the point of it -- reverting the one commit
//     that changed the digests re-pins exactly the prior ones. There is no
//     checkout in the pod and this file does not invent one; delivering a
//     checkout (initContainer, git-sync, deploy key) is the separate decision
//     epic memql#4275 exists to make. Until then the refusal names the missing
//     thing.
//   - RunRolloutAction is `kubectl argo rollouts promote|abort`, an
//     out-of-tree kubectl PLUGIN. Its in-process equivalent is not one API
//     call: promote manipulates pause conditions whose representation is
//     argo-rollouts-version-dependent, and guessing at it would be a
//     write-shaped guess against a live rollout. Refused by name.
//
// Both were already impossible in a deployed cluster. What changes is that the
// operator is told which prerequisite is absent instead of reading
// "kickoff_failed".

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// The projected ServiceAccount paths every pod carries. Kubernetes mounts them
// at this exact location; their presence is also how this file decides it is
// running in a cluster at all.
const (
	saTokenPath  = "/var/run/secrets/kubernetes.io/serviceaccount/token" // #nosec G101 -- a path, not a credential
	saCACertPath = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
)

// argoGroupVersion is the API group both Argo CD Applications and Argo
// Rollouts live under. One constant because one Role grants against it.
const argoGroupVersion = "apis/argoproj.io/v1alpha1"

// k8sRequestTimeout bounds a single API-server call. A repair is two calls and
// the caller's context bounds the whole operation; this is the per-call floor
// so one wedged connection cannot hold a deploy verb open indefinitely.
const k8sRequestTimeout = 30 * time.Second

// ReasonNoClusterAPI and ReasonNoRolloutPlugin are machine-readable prefixes,
// in the same style as ReasonNoOverlayCheckout / ReasonLocalCluster: a client
// branches on the prefix without parsing the prose after it.
const (
	// ReasonNoClusterAPI: the process is not running inside a cluster, so
	// there is no ServiceAccount token and no API server to reach.
	ReasonNoClusterAPI = "no_cluster_api"
	// ReasonNoRolloutPlugin: the verb is an out-of-tree kubectl plugin with
	// no single-call API form.
	ReasonNoRolloutPlugin = "no_rollout_plugin"
)

// inClusterExecutor implements Executor against the Kubernetes API server
// using the pod's own ServiceAccount.
//
// The transport is *ClusterAPI, shared with every other in-process caller that
// needs the API server (component/customdomain's provisioner is the second).
// ONE implementation of the token read, the CA pool and the request loop: a
// second copy is a copy that stops re-reading the projected token, and the
// symptom of that is a substrate that works on the day it is deployed.
type inClusterExecutor struct {
	*ClusterAPI
	// repoRoot is retained ONLY so the git-dependent verbs can say whether a
	// checkout was configured at all. Empty is the deployed case.
	repoRoot string
}

// InClusterAvailable reports whether this process can reach the Kubernetes API
// with a ServiceAccount identity: the projected token exists AND the API
// server's address is in the environment.
//
// Both halves are required and neither implies the other. KUBERNETES_SERVICE_*
// is injected into every pod in the cluster including ones with
// automountServiceAccountToken false; the token file exists only where a
// ServiceAccount is actually projected. A substrate chosen on the address
// alone would build a client that 401s on its first call.
func InClusterAvailable() bool {
	if strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST")) == "" {
		return false
	}
	info, err := os.Stat(saTokenPath)
	return err == nil && !info.IsDir()
}

// newInClusterExecutor builds the API-server-backed Executor. It reads the
// token ONCE at construction, which is a deliberate limitation worth stating:
// a projected token is refreshed in place by the kubelet, so a long-lived
// process holding the first read will eventually present an expired one. The
// re-read is in tokenNow() below rather than here; this field is the fallback
// for the case where the file becomes unreadable later.
func newInClusterExecutor(repoRoot string) (*inClusterExecutor, error) {
	api, err := NewClusterAPI()
	if err != nil {
		return nil, err
	}
	return &inClusterExecutor{ClusterAPI: api, repoRoot: repoRoot}, nil
}

// ClusterAPI is a minimal Kubernetes API-server client authenticated with the
// pod's own projected ServiceAccount.
//
// It is the whole of this repository's in-cluster write substrate, and it is
// deliberately small: a request method, a token that is re-read, and the
// cluster CA. Everything above it composes paths and bodies. See the header of
// this file for why the API server rather than kubectl, a sidecar or client-go
// -- the reasoning applies unchanged to every caller, which is why this is one
// type rather than one per feature.
type ClusterAPI struct {
	base   string // https://host:port
	token  string
	client *http.Client
}

// NewClusterAPI builds the API-server client from the pod's projected
// ServiceAccount. It reads the token ONCE here, which is a deliberate
// limitation worth stating: a projected token is refreshed in place by the
// kubelet, so a long-lived process holding the first read would eventually
// present an expired one. The re-read is in tokenNow() below; this field is
// the fallback for the case where the file becomes unreadable later.
func NewClusterAPI() (*ClusterAPI, error) {
	host := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST"))
	port := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT"))
	if port == "" {
		port = "443"
	}
	if host == "" {
		return nil, fmt.Errorf("%s: KUBERNETES_SERVICE_HOST is unset", ReasonNoClusterAPI)
	}
	tok, err := os.ReadFile(saTokenPath)
	if err != nil {
		return nil, fmt.Errorf("%s: no ServiceAccount token at %s: %w", ReasonNoClusterAPI, saTokenPath, err)
	}
	pem, err := os.ReadFile(saCACertPath)
	if err != nil {
		return nil, fmt.Errorf("%s: no cluster CA at %s: %w", ReasonNoClusterAPI, saCACertPath, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		// Refuse rather than fall back to the system pool: the API server is
		// presenting a certificate from the cluster's own CA, so a system pool
		// would reject it anyway, and InsecureSkipVerify on the channel that
		// carries a cluster-admin-adjacent bearer token is not a fallback.
		return nil, fmt.Errorf("%s: cluster CA at %s parsed to zero certificates", ReasonNoClusterAPI, saCACertPath)
	}
	return &ClusterAPI{
		base:  "https://" + net_JoinHostPort(host, port),
		token: strings.TrimSpace(string(tok)),
		client: &http.Client{
			Timeout: k8sRequestTimeout,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
			},
		},
	}, nil
}

// Do issues one API-server request and returns the body. The exported form of
// `do`, for callers outside this file.
//
// `path` is everything after the API server's host, e.g.
// `apis/networking.k8s.io/v1/namespaces/memql/ingresses/x`.
func (e *ClusterAPI) Do(ctx context.Context, method, path, contentType string, body []byte) ([]byte, error) {
	return e.do(ctx, method, path, contentType, body)
}

// net_JoinHostPort brackets an IPv6 literal. Spelled out rather than importing
// net for one call, and named to read as what it is.
func net_JoinHostPort(host, port string) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]:" + port
	}
	return host + ":" + port
}

// tokenNow re-reads the projected token, falling back to the one read at
// construction. Projected ServiceAccount tokens are bound and rotated -- the
// kubelet rewrites the file well before expiry -- so a process that caches the
// first read starts 401ing after the token's lifetime, which for a deploy
// console is "the console worked on the day it was deployed".
func (e *ClusterAPI) tokenNow() string {
	if b, err := os.ReadFile(saTokenPath); err == nil {
		if t := strings.TrimSpace(string(b)); t != "" {
			return t
		}
	}
	return e.token
}

// do issues one API-server request and returns the body. A non-2xx is an
// error carrying the status line and the body, because the API server's
// message ("applications.argoproj.io \"memql\" is forbidden: User
// \"system:serviceaccount:memql:memql-deploy\" cannot patch resource") is the
// single most useful thing an operator can be handed when the RBAC is wrong.
func (e *ClusterAPI) do(ctx context.Context, method, path, contentType string, body []byte) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, e.base+"/"+strings.TrimPrefix(path, "/"), rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+e.tokenNow())
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// A TYPED error, carrying the code. Callers that branch on a status --
		// "absent is success" on a delete is the first -- were left matching
		// the rendered string, which reads a 500 whose BODY mentions 404 as a
		// successful deletion. The API server's message stays in the text
		// because it is the single most useful thing an operator can be handed
		// when the RBAC is wrong.
		return out, &StatusError{
			Method: method,
			Path:   path,
			Code:   resp.StatusCode,
			Status: resp.Status,
			Body:   strings.TrimSpace(string(out)),
		}
	}
	return out, readErr
}

// StatusError is a non-2xx response from the API server.
//
// It exists so a caller can branch on the CODE rather than on the rendered
// message. The one that needs it today is a delete treating NotFound as
// success -- unbinding twice is a legitimate thing for a retry to do, and a
// string match for "404" would also swallow a 500 that merely mentioned one.
type StatusError struct {
	Method string
	Path   string
	Code   int
	Status string
	Body   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s %s: %s: %s", e.Method, e.Path, e.Status, e.Body)
}

// IsNotFound reports whether err is a 404 from the API server.
func IsNotFound(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == http.StatusNotFound
}

// argoPath composes the collection or single-resource path for one of the
// three kinds this substrate reaches.
func argoPath(namespace, plural, name string) string {
	p := fmt.Sprintf("%s/namespaces/%s/%s", argoGroupVersion, namespace, plural)
	if name != "" {
		p += "/" + name
	}
	return p
}

// kubectlPlural maps the resource words the call sites use onto API plurals.
// Closed on purpose: this substrate grants three kinds and reaching a fourth
// should be a compile-time decision with a matching Role rule, not a string
// that happens to route.
var kubectlPlural = map[string]string{
	"app": "applications", "apps": "applications", "application": "applications", "applications": "applications",
	"rollout": "rollouts", "rollouts": "rollouts",
	"analysisrun": "analysisruns", "analysisruns": "analysisruns",
}

// KubectlJSON serves the read call sites by translating their kubectl argv
// into an API path.
//
// Parsing argv rather than changing the Executor interface is deliberate. That
// interface is a shared boundary -- the deploy PACK (examples/deploypack)
// drives the same methods through the IntegrationProvider layer -- so widening
// it to a typed cluster-read API would be a second migration in every consumer
// for no behavioural gain. The four shapes in this repository are enumerated
// in the switch below; anything else is refused by name rather than guessed at.
func (e *inClusterExecutor) KubectlJSON(ctx context.Context, args ...string) ([]byte, error) {
	ns, verb, resource, name, err := parseKubectlGet(args)
	if err != nil {
		return nil, err
	}
	if verb != "get" {
		return nil, fmt.Errorf("in-cluster substrate: only `get` is served on the read path, got %q", verb)
	}
	plural, ok := kubectlPlural[resource]
	if !ok {
		return nil, fmt.Errorf("in-cluster substrate: resource %q is not one this node's Role grants "+
			"(applications, rollouts, analysisruns)", resource)
	}
	return e.do(ctx, http.MethodGet, argoPath(ns, plural, name), "", nil)
}

// parseKubectlGet reads the argv shapes the read call sites build:
//
//	-n <ns> get <resource> [<name>] -o json
//
// It is intentionally strict. A lenient parser here would silently reinterpret
// a call it did not understand as a different, valid one -- reading the wrong
// namespace, or a collection where a single object was meant -- and a deploy
// console that reports another namespace's state is worse than one that errors.
func parseKubectlGet(args []string) (ns, verb, resource, name string, err error) {
	var positional []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-n", "--namespace":
			if i+1 >= len(args) {
				return "", "", "", "", fmt.Errorf("in-cluster substrate: %v ends after %s", args, args[i])
			}
			ns = args[i+1]
			i++
		case "-o", "--output":
			if i+1 >= len(args) {
				return "", "", "", "", fmt.Errorf("in-cluster substrate: %v ends after %s", args, args[i])
			}
			if args[i+1] != "json" {
				return "", "", "", "", fmt.Errorf("in-cluster substrate: only -o json is served, got %q", args[i+1])
			}
			i++
		default:
			positional = append(positional, args[i])
		}
	}
	if ns == "" {
		return "", "", "", "", fmt.Errorf("in-cluster substrate: %v names no namespace; "+
			"an implicit namespace would read whatever this pod happens to run in", args)
	}
	switch len(positional) {
	case 2:
		return ns, positional[0], positional[1], "", nil
	case 3:
		return ns, positional[0], positional[1], positional[2], nil
	default:
		return "", "", "", "", fmt.Errorf("in-cluster substrate: cannot read %v as `get <resource> [<name>]`", args)
	}
}

// RunRepair is the whole reason this substrate exists: a hard refresh of the
// installation's Application followed by an explicit sync operation, exactly
// the two effects the kubectl form performs and in the same order.
//
// The refresh comes first so the sync diffs against manifests fetched now
// rather than against a cached render of them -- a stale repo-server cache is
// one of the states a repair exists to leave.
func (e *inClusterExecutor) RunRepair(ctx context.Context, marker string) (string, error) {
	path := argoPath(argoNamespace, "applications", argoApplication)

	// `kubectl annotate --overwrite` is a merge patch on metadata.annotations.
	refreshBody, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]any{"argocd.argoproj.io/refresh": "hard"},
		},
	})
	if err != nil {
		return "", err
	}
	refresh, err := e.do(ctx, http.MethodPatch, path, "application/merge-patch+json", refreshBody)
	if err != nil {
		return string(refresh), fmt.Errorf("hard refresh of application %s: %w", argoApplication, err)
	}

	patch, err := repairSyncPatch(marker)
	if err != nil {
		return string(refresh), err
	}
	sync, err := e.do(ctx, http.MethodPatch, path, "application/merge-patch+json", []byte(patch))
	if err != nil {
		return string(refresh) + string(sync), fmt.Errorf("sync operation on application %s: %w", argoApplication, err)
	}
	return string(refresh) + string(sync), nil
}

// RunRollback needs a git repository, and this node has none. The refusal
// names the missing prerequisite because the alternative -- a generic kickoff
// failure -- sent operators looking at ArgoCD.
func (e *inClusterExecutor) RunRollback(ctx context.Context, sha string) (string, error) {
	return "", e.noCheckout("rollback (git revert " + shortSHA(sha) + ")")
}

// Git is the same refusal for the same reason, reached by the read helpers.
func (e *inClusterExecutor) Git(ctx context.Context, args ...string) (string, error) {
	return "", e.noCheckout("git " + strings.Join(args, " "))
}

func (e *inClusterExecutor) noCheckout(what string) error {
	where := "MEMQL_DEPLOY_REPO_ROOT is unset"
	if strings.TrimSpace(e.repoRoot) != "" {
		where = "MEMQL_DEPLOY_REPO_ROOT is " + e.repoRoot + ", which holds no repository"
	}
	return fmt.Errorf("%s: %s needs a deploy checkout on this node and there is none (%s). "+
		"The identity image is distroless -- no git binary -- and no overlay mounts a checkout. "+
		"Delivering one is memql#4275; the Argo verbs that do not need it (repair, status) work "+
		"through the cluster API", ReasonNoOverlayCheckout, what, where)
}

// RunRolloutAction is an out-of-tree kubectl PLUGIN, not an API call. Refused
// by name rather than approximated: promote manipulates pause conditions whose
// representation is argo-rollouts-version-dependent, and a write-shaped guess
// against a live rollout is worse than an honest no.
func (e *inClusterExecutor) RunRolloutAction(ctx context.Context, rollout, action string) (string, error) {
	return "", fmt.Errorf("%s: `%s` on rollout %q is a kubectl argo-rollouts plugin verb with no "+
		"single API-call form, and this node runs the in-cluster substrate rather than kubectl. "+
		"Run it from an operator machine with the plugin installed",
		ReasonNoRolloutPlugin, action, rollout)
}

// shortSHA trims a sha for a message without pretending to validate it.
func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
