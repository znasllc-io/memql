package argocd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/argocd/gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

//go:generate sh -c "cd ../.. && make proto-gen PROTO_GEN_ONLY=integrations/argocd"

const (
	generateManifestRPC = "/repository.RepoServerService/GenerateManifest"
	maxRenderBytes      = 32 << 20
	maxRenderResources  = 4096
)

// RenderSpec names every supported input to the repository renderer. It must
// come from native installation configuration, not from a client capability.
// Source is the COMPLETE source object, including an immutable targetRevision.
// Unsupported fields fail rather than silently disappearing in the wire codec.
// The eventual preparer must also verify a closed source tree, the running
// renderer/configuration and the current Application before authorizing effects.
type RenderSpec struct {
	Source                json.RawMessage `json:"source"`
	AppName               string          `json:"appName"`
	AppLabelKey           string          `json:"appLabelKey"`
	Namespace             string          `json:"namespace"`
	ProjectName           string          `json:"projectName"`
	RepositoryProject     string          `json:"repositoryProject"`
	ProjectSourceRepos    []string        `json:"projectSourceRepos"`
	TrackingMethod        string          `json:"trackingMethod"`
	InstallationID        string          `json:"installationId"`
	KubeVersion           string          `json:"kubeVersion"`
	APIVersions           []string        `json:"apiVersions"`
	KustomizeBuildOptions string          `json:"kustomizeBuildOptions"`
	KustomizeBinaryPath   string          `json:"kustomizeBinaryPath"`
}

// DecodeRenderSpec reads a native collector specification without accepting
// duplicate fields, unknown options or trailing JSON. Semantic validation uses
// the same codec as the repository renderer, before any source is collected.
func DecodeRenderSpec(body []byte) (RenderSpec, error) {
	if len(body) > 512<<10 {
		return RenderSpec{}, errors.New("render specification exceeds its bound")
	}
	fields, err := decodeObject(body)
	if err != nil {
		return RenderSpec{}, errors.New("render specification contains invalid or ambiguous JSON")
	}
	var spec RenderSpec
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if d.Decode(&spec) != nil {
		return RenderSpec{}, errors.New("render specification contains unsupported fields")
	}
	// encoding/json otherwise accepts case variants of a field name, allowing
	// both source and Source to assign the same struct field in one object.
	encoded, _ := json.Marshal(spec)
	var canonical map[string]json.RawMessage
	_ = json.Unmarshal(encoded, &canonical)
	for field := range fields {
		if _, known := canonical[field]; !known {
			return RenderSpec{}, errors.New("render specification contains unsupported fields")
		}
	}
	if _, _, err := renderRequest(spec, RepositoryCredentials{}); err != nil {
		return RenderSpec{}, err
	}
	return spec, nil
}

// RepositoryCredentials are transient HTTPS credentials, including an already
// resolved GitHub App installation token. They never enter render evidence.
// SSH, LFS and custom repository transports require a separately qualified
// codec; this initial adapter does not silently downgrade their configuration.
type RepositoryCredentials struct {
	Username string `json:"-"`
	Password string `json:"-"`
}

func (RepositoryCredentials) String() string   { return "[repository credentials]" }
func (RepositoryCredentials) GoString() string { return "[repository credentials]" }

// ManifestRPC is satisfied by a gRPC ClientConn. Its TLS identity, repository
// trust and network reachability belong to native installation configuration.
// There is deliberately no public builtin for supplying an arbitrary endpoint.
type ManifestRPC interface {
	Invoke(context.Context, string, any, any, ...grpc.CallOption) error
}

// RenderedRevision owns the full unredacted resources returned by the renderer.
// This is an observation, not approval, source-closure proof, or permission to
// update. In particular, a Git SHA alone does not pin remote Kustomize inputs.
// No rendered Secret values should be returned through a user-facing API.
type RenderedRevision struct {
	spec      []byte
	resources []json.RawMessage
	digest    string
}

func (r RenderedRevision) Digest() string { return r.digest }

func (r RenderedRevision) String() string {
	return fmt.Sprintf("ArgoCD render resources=%d digest=%s", len(r.resources), r.digest)
}

func (r RenderedRevision) GoString() string { return r.String() }

func (r RenderedRevision) Spec() RenderSpec {
	var spec RenderSpec
	_ = json.Unmarshal(r.spec, &spec)
	return spec
}

func (r RenderedRevision) Resources() []json.RawMessage {
	resources := make([]json.RawMessage, len(r.resources))
	for i, resource := range r.resources {
		resources[i] = append(json.RawMessage(nil), resource...)
	}
	return resources
}

// RenderRevision reads the actual repo-server over its existing gRPC protocol.
// ApplicationService.GetManifests is NOT suitable here: v2.13.3 resolves its
// source from an informer, hides Secret data and discards the resolved revision.
// Direct generation carries the exact source and options and bypasses both
// caches. The call is bounded, read-only and never changes an Application.
func RenderRevision(ctx context.Context, rpc ManifestRPC, spec RenderSpec, credentials RepositoryCredentials) (RenderedRevision, error) {
	if rpc == nil {
		return RenderedRevision{}, errors.New("ArgoCD repository renderer is unavailable")
	}
	request, specBody, err := renderRequest(spec, credentials)
	if err != nil {
		return RenderedRevision{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	response := &gen.ManifestResponse{}
	if err := rpc.Invoke(ctx, generateManifestRPC, request, response,
		grpc.MaxCallSendMsgSize(1<<20), grpc.MaxCallRecvMsgSize(maxRenderBytes)); err != nil {
		// Upstream errors can contain Git credentials, command output or Secret
		// values. Preserve a diagnostic code, never interpolate that message.
		return RenderedRevision{}, fmt.Errorf("ArgoCD repository render failed (%s)", status.Code(err))
	}
	if response.Revision != request.Revision || response.SourceType != "Kustomize" {
		return RenderedRevision{}, errors.New("ArgoCD render did not confirm the exact commit and Kustomize source")
	}
	// These fields are empty in the pinned repo-server. Refuse unexpected
	// destination assertions rather than assigning them invented semantics.
	if response.Namespace != "" || response.Server != "" {
		return RenderedRevision{}, errors.New("ArgoCD render returned unsupported destination metadata")
	}
	resources, err := renderedResources(response.Manifests, request.Namespace)
	if err != nil {
		return RenderedRevision{}, err
	}
	body, err := json.Marshal(struct {
		Spec      json.RawMessage   `json:"spec"`
		Resources []json.RawMessage `json:"resources"`
	}{specBody, resources})
	if err != nil {
		return RenderedRevision{}, errors.New("ArgoCD render could not be encoded")
	}
	// This digest names private native evidence, not an OCI or exported
	// artifact. Keep it on the same core/id convention as the intent journal.
	digest := "memql-id:" + string(id.NewUntracked().FromString("memql-argocd-render-v1\n"+string(body)))
	return RenderedRevision{spec: specBody, resources: resources, digest: digest}, nil
}

func renderRequest(spec RenderSpec, credentials RepositoryCredentials) (*gen.ManifestRequest, []byte, error) {
	source, err := decodeObject(spec.Source)
	if err != nil {
		return nil, nil, errors.New("ArgoCD render source is invalid")
	}
	for key := range source {
		if key != "repoURL" && key != "path" && key != "targetRevision" && key != "kustomize" {
			return nil, nil, errors.New("ArgoCD renderer supports only a complete Git/Kustomize source")
		}
	}
	repository, sourcePath, revision := stringAt(source, "repoURL"), stringAt(source, "path"), stringAt(source, "targetRevision")
	u, err := url.Parse(repository)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		sourcePath == "" || path.IsAbs(sourcePath) || path.Clean(sourcePath) != sourcePath || sourcePath == ".." ||
		strings.HasPrefix(sourcePath, "../") || strings.ContainsAny(sourcePath, "\\\x00\r\n") || !commitSHA.MatchString(revision) || !canonicalSourceValues(source) {
		return nil, nil, errors.New("ArgoCD render requires an HTTPS repository, repository-relative path and exact commit")
	}
	if len(spec.AppName) == 0 || len(spec.AppName) > 253 || len(spec.AppLabelKey) == 0 || len(spec.AppLabelKey) > 253 ||
		!dnsLabel.MatchString(spec.Namespace) || len(spec.Namespace) > 63 || !dnsLabel.MatchString(spec.ProjectName) || len(spec.ProjectName) > 63 ||
		len(spec.KubeVersion) == 0 || len(spec.KubeVersion) > 128 || len(spec.InstallationID) > 253 ||
		(spec.TrackingMethod != "label" && spec.TrackingMethod != "annotation" && spec.TrackingMethod != "annotation+label") ||
		len(spec.ProjectSourceRepos) == 0 || len(spec.ProjectSourceRepos) > 256 || len(spec.APIVersions) > 4096 {
		return nil, nil, errors.New("ArgoCD render requires bounded application, project, destination and renderer configuration")
	}
	if spec.KustomizeBuildOptions != "" && spec.KustomizeBuildOptions != "--load-restrictor LoadRestrictionsNone" {
		return nil, nil, errors.New("ArgoCD render does not support these Kustomize build options")
	}
	// Repository credential/cache scope is independent of Application project.
	// A globally configured repository has an empty project even when the
	// Application belongs to a named project. Never invent a new clone scope.
	if spec.RepositoryProject != "" && (len(spec.RepositoryProject) > 63 || !dnsLabel.MatchString(spec.RepositoryProject)) {
		return nil, nil, errors.New("ArgoCD repository project is invalid")
	}
	if spec.KustomizeBinaryPath != "" && (!path.IsAbs(spec.KustomizeBinaryPath) || path.Clean(spec.KustomizeBinaryPath) != spec.KustomizeBinaryPath || strings.ContainsAny(spec.KustomizeBinaryPath, "\x00\r\n")) {
		return nil, nil, errors.New("ArgoCD render Kustomize binary path is invalid")
	}
	var kustomize *gen.ApplicationSourceKustomize
	if value, present := source["kustomize"]; present {
		options, ok := value.(map[string]any)
		if !ok {
			return nil, nil, errors.New("ArgoCD render Kustomize options are invalid")
		}
		for key, value := range options {
			_, stringValue := value.(string)
			if (key != "namespace" && key != "version") || !stringValue {
				return nil, nil, errors.New("ArgoCD render requires source overrides to be committed in the overlay")
			}
		}
		kustomize = &gen.ApplicationSourceKustomize{Namespace: stringAt(options, "namespace"), Version: stringAt(options, "version")}
		if kustomize.Version != "" && spec.KustomizeBinaryPath == "" {
			return nil, nil, errors.New("ArgoCD render requires the configured binary for a selected Kustomize version")
		}
	}
	if len(credentials.Username) > 4096 || len(credentials.Password) > 64<<10 || (credentials.Username == "") != (credentials.Password == "") {
		return nil, nil, errors.New("ArgoCD repository credentials are invalid")
	}
	// Encode the entire request description to impose a total input bound and
	// own the caller's slices/maps before the RPC can outlive them.
	spec.Source, err = canonical(source)
	if err != nil {
		return nil, nil, errors.New("ArgoCD render source could not be encoded")
	}
	body, err := json.Marshal(spec)
	if err != nil || len(body) > 512<<10 {
		return nil, nil, errors.New("ArgoCD render configuration exceeds its encoding bound")
	}
	var owned RenderSpec
	if err := json.Unmarshal(body, &owned); err != nil {
		return nil, nil, errors.New("ArgoCD render configuration could not be copied")
	}
	request := &gen.ManifestRequest{
		Repo:     &gen.Repository{Repo: repository, Username: credentials.Username, Password: credentials.Password, Type: "git", Project: owned.RepositoryProject},
		Revision: revision, NoCache: true, NoRevisionCache: true,
		ApplicationSource: &gen.ApplicationSource{RepoURL: repository, Path: sourcePath, TargetRevision: revision, Kustomize: kustomize},
		AppName:           owned.AppName, AppLabelKey: owned.AppLabelKey, Namespace: owned.Namespace,
		ProjectName: owned.ProjectName, ProjectSourceRepos: owned.ProjectSourceRepos,
		TrackingMethod: owned.TrackingMethod, InstallationID: owned.InstallationID,
		KubeVersion: owned.KubeVersion, ApiVersions: owned.APIVersions,
		KustomizeOptions:   &gen.KustomizeOptions{BuildOptions: owned.KustomizeBuildOptions, BinaryPath: owned.KustomizeBinaryPath},
		EnabledSourceTypes: map[string]bool{"Kustomize": true, "Helm": false, "Directory": false, "Plugin": false},
	}
	return request, body, nil
}

func renderedResources(manifests []string, defaultNamespace string) ([]json.RawMessage, error) {
	if len(manifests) == 0 || len(manifests) > maxRenderResources {
		return nil, errors.New("ArgoCD render has an empty or excessive resource inventory")
	}
	byIdentity := make(map[string]json.RawMessage, len(manifests))
	total := 0
	for _, manifest := range manifests {
		total += len(manifest)
		if total > maxRenderBytes {
			return nil, errors.New("ArgoCD rendered resources exceed 32 MiB")
		}
		resource, err := decodeObject([]byte(manifest))
		if err != nil {
			return nil, errors.New("ArgoCD rendered resource contains invalid or ambiguous JSON")
		}
		version, kind := stringAt(resource, "apiVersion"), stringAt(resource, "kind")
		meta := object(resource, "metadata")
		name, namespace := stringAt(meta, "name"), stringAt(meta, "namespace")
		if value, present := meta["namespace"]; present {
			if _, ok := value.(string); !ok {
				return nil, errors.New("ArgoCD rendered resource namespace is invalid")
			}
		}
		if version == "" || kind == "" || name == "" || len(version) > 253 || len(kind) > 253 || len(name) > 253 || len(namespace) > 63 ||
			strings.ContainsAny(version+kind+name+namespace, "\x00\r\n") || meta["generateName"] != nil || resource["items"] != nil {
			return nil, errors.New("ArgoCD rendered resource lacks one stable object identity")
		}
		group := ""
		if strings.Contains(version, "/") {
			parts := strings.Split(version, "/")
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				return nil, errors.New("ArgoCD rendered resource has an invalid API version")
			}
			group = parts[0]
		}
		// API versions of the same group/kind are the SAME Kubernetes object.
		// Keeping both would let apply order decide what was actually reviewed.
		// A namespaced object without metadata.namespace is applied in the
		// destination namespace. Conservatively collapse that alias here; the
		// installation's discovery/schema verifier still checks actual scope.
		effectiveNamespace := namespace
		if effectiveNamespace == "" {
			effectiveNamespace = defaultNamespace
		}
		keyBody, _ := json.Marshal([]string{group, kind, effectiveNamespace, name})
		key := string(keyBody)
		if _, exists := byIdentity[key]; exists {
			return nil, errors.New("ArgoCD render contains duplicate resource identities")
		}
		body, err := canonical(resource)
		if err != nil {
			return nil, errors.New("ArgoCD rendered resource could not be encoded")
		}
		byIdentity[key] = body
	}
	keys := make([]string, 0, len(byIdentity))
	for key := range byIdentity {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	resources := make([]json.RawMessage, 0, len(keys))
	for _, key := range keys {
		resources = append(resources, byIdentity[key])
	}
	return resources, nil
}
