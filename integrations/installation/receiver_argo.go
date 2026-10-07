package installation

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
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
func (s *receiverSnapshot) observeArgo(ctx context.Context, r *receiverReads) error {
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
	if !commitDigest.MatchString(revision) || app["operation"] != nil || spec["sources"] != nil || !sameJSON(resourceMap(resourceMap(app, "status"), "sync")["revision"], revision) || resourceText(resourceMap(resourceMap(app, "status"), "sync"), "status") != "Synced" || resourceText(resourceMap(resourceMap(app, "status"), "health"), "status") != "Healthy" {
		return errors.New("installation requires an idle healthy Application at its exact immutable baseline")
	}
	body, _ := json.Marshal(app)
	path, _ := receiverPath("argoproj.io/v1alpha1", "applications", cfg.Application.Namespace, cfg.Application.Name)
	client, _ := argocd.New(receiverApplicationAPI{path, body})
	target := argocd.Target{Namespace: cfg.Application.Namespace, Name: cfg.Application.Name, UID: resourceText(resourceMap(app, "metadata"), "uid")}
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
	repositoryProject, err := r.repositoryProject(ctx, cfg.Application.Namespace, project, repository)
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
	_, err = argocd.PlanRevision(s.application, "configuration-observation", revision, false)
	return err
}
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
	return kube, result, nil
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
	// Secret contents are deliberately excluded from the durable digest.
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].([]string)[0] < bindings[j].([]string)[0] })
	read := r.reads[path]
	read.stable = bindings
	r.reads[path] = read
	if found[project] {
		return project, nil
	}
	return "", nil
}
