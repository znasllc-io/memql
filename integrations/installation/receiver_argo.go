package installation

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/integrations/argocd"
)

// This cache is deliberately confined to the single Application observation;
// the protocol decoder consumes the same bytes that the read set later fences.
type receiverApplicationAPI struct {
	path string
	body []byte
}

func (a receiverApplicationAPI) Do(_ context.Context, method, path, _ string, body []byte) ([]byte, error) {
	if method != http.MethodGet || path != a.path || len(body) != 0 {
		return nil, errors.New("installation Application read is outside its scope")
	}
	return append([]byte(nil), a.body...), nil
}
func (s *receiverSnapshot) observeArgo(ctx context.Context, r *receiverReads, active *argocd.Intent) error {
	cfg := s.configuration
	app, err := r.object(ctx, "argoproj.io/v1alpha1", "Application", "applications", cfg.Application.Namespace, cfg.Application.Name)
	if err != nil {
		return err
	}
	spec := resourceMap(app, "spec")
	source := resourceMap(spec, "source")
	destination := resourceMap(spec, "destination")
	if resourceText(destination, "server") != "https://kubernetes.default.svc" || resourceText(destination, "name") != "" || len(destination) != 2 || !receiverDNS.MatchString(resourceText(destination, "namespace")) {
		return errors.New("installation Application must target this receiving cluster and one explicit namespace")
	}
	revision := resourceText(source, "targetRevision")
	if !commitDigest.MatchString(revision) || spec["sources"] != nil {
		return errors.New("installation Application requires one immutable revision")
	}
	if active == nil && (app["operation"] != nil || !sameJSON(resourceMap(resourceMap(app, "status"), "sync")["revision"], revision) || resourceText(resourceMap(resourceMap(app, "status"), "sync"), "status") != "Synced" || resourceText(resourceMap(resourceMap(app, "status"), "health"), "status") != "Healthy") {
		return errors.New("installation requires an idle healthy Application at its exact immutable baseline")
	}
	body, _ := json.Marshal(app)
	path, _ := receiverPath("argoproj.io/v1alpha1", "applications", cfg.Application.Namespace, cfg.Application.Name)
	client, _ := argocd.New(receiverApplicationAPI{path, body})
	target := argocd.Target{Namespace: cfg.Application.Namespace, Name: cfg.Application.Name, UID: resourceText(resourceMap(app, "metadata"), "uid")}
	s.applicationTarget = target
	if active != nil {
		if active.Target != target {
			return errors.New("installation continuation targets a different Application")
		}
		facts, err := client.Observe(ctx, *active)
		if err != nil || !facts.IntentObserved || (!facts.OperationPending && facts.OperationPhase == "") {
			return errors.New("installation continuation requires the actual owned Application intent and operation")
		}
	}
	s.application, err = client.Read(ctx, target)
	if err != nil {
		return errors.New("installation Application baseline is unsupported")
	}
	project := resourceText(spec, "project")
	p, err := r.object(ctx, "argoproj.io/v1alpha1", "AppProject", "appprojects", cfg.Application.Namespace, project)
	if err != nil {
		return err
	}
	repositories, err := receiverStringList(resourceMap(p, "spec")["sourceRepos"])
	if err != nil {
		return err
	}
	repository := resourceText(source, "repoURL")
	allowed := false
	for _, value := range repositories {
		if value == "*" || value == repository {
			allowed = true
		}
		if strings.HasPrefix(value, "!") {
			return errors.New("installation repository exclusions require a qualified project matcher")
		}
	}
	if !allowed {
		return errors.New("installation repository is not explicitly allowed by its Argo project")
	}
	destinations, ok := resourceMap(p, "spec")["destinations"].([]any)
	if !ok || len(destinations) > 256 {
		return errors.New("installation project destination is invalid")
	}
	allowed = false
	for _, entry := range destinations {
		d, ok := entry.(map[string]any)
		if !ok {
			return errors.New("installation project destination is invalid")
		}
		namespace, server := resourceText(d, "namespace"), resourceText(d, "server")
		if (namespace == "*" || namespace == resourceText(destination, "namespace")) && (server == "*" || server == resourceText(destination, "server")) && resourceText(d, "name") == "" {
			allowed = true
		}
	}
	if !allowed {
		return errors.New("installation destination is not explicitly allowed by its Argo project")
	}
	cm, err := r.object(ctx, "v1", "ConfigMap", "configmaps", cfg.Application.Namespace, cfg.Renderer.ConfigMap)
	if err != nil {
		return err
	}
	// Argo CD's settings manager reads this ConfigMap only while it is labelled
	// app.kubernetes.io/part-of=argocd; without it Argo stops applying the
	// settings this receiver renders with, so that one label is bound with the
	// content. No other label is, because labels are otherwise bookkeeping.
	if cmPath, err := receiverPath("v1", "configmaps", cfg.Application.Namespace, cfg.Renderer.ConfigMap); err == nil {
		r.bindLabel(cmPath, "app.kubernetes.io/part-of")
	}
	data := resourceMap(cm, "data")
	// These defaults are the qualified Argo 2.13.3 protocol defaults, never a
	// transport fallback. Values explicitly present in the live CM win.
	label, tracking := "app.kubernetes.io/instance", "label"
	if value, ok := data["application.instanceLabelKey"]; ok {
		label, _ = value.(string)
	}
	if value, ok := data["application.resourceTrackingMethod"]; ok {
		tracking, _ = value.(string)
	}
	buildOptions := resourceText(data, "kustomize.buildOptions")
	version := resourceText(resourceMap(source, "kustomize"), "version")
	if version != "" {
		return errors.New("installation renderer profile does not qualify a custom Kustomize binary")
	}
	for key := range data {
		if strings.HasPrefix(key, "kustomize.") && key != "kustomize.buildOptions" {
			return errors.New("installation renderer has unqualified Kustomize configuration")
		}
	}
	kube, apis, err := r.discovery(ctx)
	if err != nil {
		return err
	}
	var repositoryProject string
	if cfg.RepositoryScope == nil {
		repositoryProject, err = r.repositoryProject(ctx, cfg.Application.Namespace, project, repository)
	} else {
		var secret receiverNamed
		secret, err = cfg.RepositoryScope.secretFor(cfg.Application)
		if err == nil {
			repositoryProject, err = r.namedRepositoryProject(ctx, secret, project, repository)
		}
	}
	if err != nil {
		return err
	}
	sourceBody, _ := json.Marshal(source)
	render := argocd.RenderSpec{Source: sourceBody, AppName: cfg.Application.Name, AppLabelKey: label, Namespace: resourceText(destination, "namespace"), ProjectName: project, RepositoryProject: repositoryProject, ProjectSourceRepos: repositories, TrackingMethod: tracking, InstallationID: resourceText(data, "installationID"), KubeVersion: kube, APIVersions: apis, KustomizeBuildOptions: buildOptions}
	raw, _ := json.Marshal(render)
	s.render, err = argocd.DecodeRenderSpec(raw)
	if err != nil {
		return errors.New("installation renderer configuration is unsupported")
	}
	// Exercise the same full-source intent validation before acquisition.
	if active != nil {
		return nil // The exact active intent was independently verified above.
	}
	_, err = argocd.PlanRevision(s.application, "configuration-observation", revision, false)
	return err
}

// Argo 2.13.3 getRepositorySecret returns a matching nonempty project before
// considering its global fallback (util/db/repository_secrets.go). A positive
// binding therefore proves the project/cache-scope value even if another
// registration matches that same project: either choice returns that project.
// It proves neither unique credential selection nor a global binding's absence.
// Native rendering uses its independently minted repository-scoped CloneToken;
// no credential field from this Secret is used or returned by this observation.
// The read set binds the named Secret's UID and keyed content, checks it again
// before admission, and invalidates recovery on rotation or replacement; an
// annotation-only write does not.
// Pinned resolver: https://github.com/argoproj/argo-cd/blob/v2.13.3/util/db/repository_secrets.go#L430-L460
func (r *receiverReads) namedRepositoryProject(ctx context.Context, ref receiverNamed, project, repository string) (string, error) {
	if !receiverDNS.MatchString(project) || len(project) > 63 {
		return "", errors.New("installation named repository requires a nonempty application project")
	}
	secret, err := r.object(ctx, "v1", "Secret", "secrets", ref.Namespace, ref.Name)
	if err != nil {
		return "", err
	}
	if resourceText(resourceMap(resourceMap(secret, "metadata"), "labels"), "argocd.argoproj.io/secret-type") != "repository" {
		return "", errors.New("installation named repository label is invalid")
	}
	data := resourceMap(secret, "data")
	repo, repoErr := base64.StdEncoding.Strict().DecodeString(resourceText(data, "url"))
	scope, scopeErr := base64.StdEncoding.Strict().DecodeString(resourceText(data, "project"))
	if repoErr != nil || scopeErr != nil || len(repo) == 0 || len(repo) > 8192 ||
		strings.TrimSuffix(strings.ToLower(strings.TrimSpace(string(repo))), ".git") != strings.TrimSuffix(strings.ToLower(repository), ".git") || string(scope) != project {
		return "", errors.New("installation named repository does not match the application's repository and project")
	}
	return project, nil
}

// discovery reads the cluster's complete API discovery, for the renderer's
// KubeVersion and APIVersions (the same full surface Argo itself passes), and
// leaves in the saved digest only the contract the receiver depends on
// (receiverDiscoveryContract). Every document stays exact within this one
// observation (reobserve).
func (r *receiverReads) discovery(ctx context.Context) (string, []string, error) {
	version, err := r.get(ctx, "version")
	if err != nil {
		return "", nil, err
	}
	kube := resourceText(version, "gitVersion")
	if kube == "" || len(kube) > 128 {
		return "", nil, errors.New("receiving Kubernetes version is missing")
	}
	core, err := r.get(ctx, "api")
	if err != nil {
		return "", nil, err
	}
	coreVersions, err := receiverStringList(core["versions"])
	if err != nil {
		return "", nil, err
	}
	groups, err := r.get(ctx, "apis")
	if err != nil {
		return "", nil, err
	}
	lists := map[string]map[string]any{}
	discoveryPaths := []string{"version", "api", "apis"}
	entries, ok := groups["groups"].([]any)
	if !ok || len(entries) > 128 {
		return "", nil, errors.New("receiving API discovery exceeds its bound")
	}
	versions := append([]string(nil), coreVersions...)
	for _, entry := range entries {
		group, ok := entry.(map[string]any)
		if !ok {
			return "", nil, errors.New("receiving API group is malformed")
		}
		list, ok := group["versions"].([]any)
		if !ok || len(list) == 0 || len(list) > 16 {
			return "", nil, errors.New("receiving API group versions are malformed")
		}
		for _, entry := range list {
			v, ok := entry.(map[string]any)
			if !ok {
				return "", nil, errors.New("receiving API version is malformed")
			}
			gv := resourceText(v, "groupVersion")
			if gv != resourceText(group, "name")+"/"+resourceText(v, "version") {
				return "", nil, errors.New("receiving API version identity differs")
			}
			versions = append(versions, gv)
		}
	}
	if len(versions) == 0 || len(versions) > 128 {
		return "", nil, errors.New("receiving API discovery exceeds its bound")
	}
	seen := map[string]bool{}
	out := map[string]bool{}
	for _, version := range versions {
		path, _, err := apiVersionPath(version)
		if err != nil || seen[version] {
			return "", nil, errors.New("receiving API versions are invalid or duplicated")
		}
		seen[version] = true
		resource, err := r.get(ctx, path)
		if err != nil {
			return "", nil, err
		}
		if resourceText(resource, "groupVersion") != version {
			return "", nil, errors.New("receiving API resource version differs")
		}
		items, ok := resource["resources"].([]any)
		if !ok || len(items) > 4096 {
			return "", nil, errors.New("receiving API resources are invalid")
		}
		lists[version] = resource
		discoveryPaths = append(discoveryPaths, path)
		out[version] = true
		for _, item := range items {
			value, ok := item.(map[string]any)
			if !ok {
				return "", nil, errors.New("receiving API resource is malformed")
			}
			kind := resourceText(value, "kind")
			if !kubernetesSegment.MatchString(kind) {
				return "", nil, errors.New("receiving API kind is invalid")
			}
			out[version+"/"+kind] = true
		}
	}
	if len(out) > 4096 {
		return "", nil, errors.New("receiving API kinds exceed renderer bound")
	}
	result := make([]string, 0, len(out))
	for value := range out {
		result = append(result, value)
	}
	sort.Strings(result)
	for _, path := range discoveryPaths {
		r.fresh(path)
	}
	r.contract = receiverDiscoveryContract(kube, lists)
	return kube, result, nil
}

// receiverAPIContract is every resource the receiver itself reads or writes,
// with the verbs it uses: configuration and credentials, the renderer it
// authenticates, the Applications it applies (get, Apply's patch, and the
// history guard's name-filtered list and watch) and their projects.
var receiverAPIContract = []struct {
	groupVersion, resource string
	verbs                  []string
}{
	{"v1", "configmaps", []string{"get"}},
	{"v1", "pods", []string{"get"}},
	{"v1", "secrets", []string{"get", "list"}},
	{"v1", "services", []string{"get"}},
	{"apps/v1", "deployments", []string{"get"}},
	{"apps/v1", "replicasets", []string{"get"}},
	{"discovery.k8s.io/v1", "endpointslices", []string{"list"}},
	{"argoproj.io/v1alpha1", "applications", []string{"get", "list", "patch", "watch"}},
	{"argoproj.io/v1alpha1", "appprojects", []string{"get"}},
}

var receiverKubernetesRelease = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.[0-9]`)

// receiverDiscoveryContract is the discovery surface a saved plan depends on.
// The full surface stranded every started plan whenever anything around it
// moved: an AKS control-plane patch rewrites /version's build fields, every
// added or upgraded CRD (External Secrets, CloudNativePG, Kueue, cert-manager,
// AKS add-ons) rewrites /apis and adds a group version, and /api reports the
// API server's current address. None of that changes how this receiver reads
// or what the plan's render applies.
//
// Bound: the Kubernetes major and minor version, and for each resource in
// receiverAPIContract whether its group version is served and each matching
// entry's kind, scope and the verbs the receiver needs (in the canonical entry
// order, as for receiverDiscoveryIdentity). A minor upgrade, or a removed or
// rescoped resource the receiver reads, still refuses.
//
// Not bound here, because each call checks it from live discovery instead:
// every API the plan's own render uses. Resource evidence (verifyImagesAndDiff)
// resolves each rendered object's kind, scope and path through fresh discovery
// at Apply, completion and rollback, and that resolution belongs to the
// ResourceDiffDigest they compare with the plan; a kind the render uses that is
// no longer served refuses there. The configuration is observed before any
// render exists, so it could not bind those APIs itself.
//
// Leaving the full API list unbound is sound only because the qualified profile
// cannot consume it: the renderer admits Git/Kustomize sources only and no
// build option but `--load-restrictor LoadRestrictionsNone`, so no Helm
// inflation reads KubeVersion or APIVersions. A profile that admits Helm must
// bind the whole list, because Argo's sync would render with live values while
// Apply re-renders from the spec captured at preparation
// (TestReceiverRenderProfileCannotConsumeDiscovery pins the restriction).
// Regressions: TestReceiverDiscoveryBindsOnlyTheContractTheReceiverUses and
// TestReceiverDiscoveryLeavesTheRenderedAPIsToResourceEvidence.
func receiverDiscoveryContract(gitVersion string, lists map[string]map[string]any) any {
	release := "unparsed:" + gitVersion // stricter, never looser, for a version this does not recognize
	if match := receiverKubernetesRelease.FindStringSubmatch(gitVersion); match != nil {
		release = match[1] + "." + match[2]
	}
	resources := make([]any, 0, len(receiverAPIContract))
	for _, want := range receiverAPIContract {
		list, served := lists[want.groupVersion]
		entries := []string{}
		items, _ := list["resources"].([]any)
		for _, raw := range items {
			item, _ := raw.(map[string]any)
			if resourceText(item, "name") != want.resource {
				continue
			}
			offered := map[string]bool{}
			verbs, _ := item["verbs"].([]any)
			for _, verb := range verbs {
				if name, ok := verb.(string); ok {
					offered[name] = true
				}
			}
			used := []string{}
			for _, verb := range want.verbs {
				if offered[verb] {
					used = append(used, verb)
				}
			}
			entry, _ := json.Marshal(map[string]any{"kind": item["kind"], "namespaced": item["namespaced"], "verbs": used})
			entries = append(entries, string(entry))
		}
		sort.Strings(entries)
		resources = append(resources, []any{want.groupVersion, want.resource, served, entries})
	}
	return map[string]any{"kubernetes": release, "resources": resources}
}

// Argo's repository clone/cache scope comes from its registered repository,
// independently of the Application project. An authenticated complete list
// proves the absence of a project-scoped override for a public repository.
func (r *receiverReads) repositoryProject(ctx context.Context, namespace, project, repository string) (string, error) {
	path := "api/v1/namespaces/" + namespace + "/secrets?labelSelector=" + url.QueryEscape("argocd.argoproj.io/secret-type=repository") + "&limit=257"
	list, err := r.get(ctx, path)
	if err != nil {
		return "", err
	}
	items, ok := list["items"].([]any)
	if !ok || len(items) > 256 || resourceText(list, "kind") != "SecretList" || resourceText(list, "apiVersion") != "v1" || resourceText(resourceMap(list, "metadata"), "continue") != "" || resourceMap(list, "metadata")["remainingItemCount"] != nil {
		return "", errors.New("installation repository inventory is missing or truncated")
	}
	found := map[string]bool{}
	bindings := make([]any, 0, len(items))
	for _, raw := range items {
		secret, ok := raw.(map[string]any)
		if !ok {
			return "", errors.New("installation repository inventory is malformed")
		}
		meta := resourceMap(secret, "metadata")
		if (resourceText(secret, "kind") != "" && resourceText(secret, "kind") != "Secret") || (resourceText(secret, "apiVersion") != "" && resourceText(secret, "apiVersion") != "v1") || validLiveStorage(secret, resourceIdentity{Kind: "Secret", Namespace: namespace, Name: resourceText(meta, "name")}) != nil || resourceText(resourceMap(meta, "labels"), "argocd.argoproj.io/secret-type") != "repository" {
			return "", errors.New("installation repository identity is invalid")
		}
		bindings = append(bindings, []string{resourceText(meta, "name"), resourceText(meta, "uid"), resourceText(meta, "resourceVersion")})
		data := resourceMap(secret, "data")
		repo, err := base64.StdEncoding.Strict().DecodeString(resourceText(data, "url"))
		if err != nil || len(repo) == 0 {
			return "", errors.New("installation repository URL is malformed")
		}
		// The admitted source is HTTPS. Argo 2.13.3 SameURL lowercases,
		// trims whitespace and removes .git for this transport.
		if strings.TrimSuffix(strings.ToLower(strings.TrimSpace(string(repo))), ".git") != strings.TrimSuffix(strings.ToLower(repository), ".git") {
			continue
		}
		scope, err := base64.StdEncoding.Strict().DecodeString(resourceText(data, "project"))
		if err != nil {
			return "", errors.New("installation repository project is malformed")
		}
		if string(scope) != "" && string(scope) != project {
			continue
		}
		if found[string(scope)] {
			return "", errors.New("installation repository scope is ambiguous")
		}
		found[string(scope)] = true
	}
	// v1 bound each registration's version; v2 binds each one's name, uid and
	// keyed content, so a rewrite that leaves the registration unchanged (an
	// annotation, a label) no longer strands a plan and a changed URL, project or
	// credential still refuses. No value or unkeyed hash of one is saved.
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].([]string)[0] < bindings[j].([]string)[0] })
	read := r.reads[path]
	read.legacy = bindings
	r.reads[path] = read
	r.contentList(path)
	if found[project] {
		return project, nil
	}
	return "", nil
}
