package installation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/argocd"
)

// These inputs are native installation configuration, not builtin arguments.
// DSL selects the installation/candidate; callers cannot supply an image map
// or a resource inventory as evidence that a deployment is safe.
type resourceIdentity struct {
	Group, Kind, Namespace, Name string
}

func (r resourceIdentity) String() string {
	return r.Group + "/" + r.Kind + "/" + r.Namespace + "/" + r.Name
}

type imageSlot struct {
	Resource         resourceIdentity
	Field, Container string // containers, initContainers, ephemeralContainers, volumes or imageName
}

type imageBinding struct {
	Slot                imageSlot
	Component, Artifact string
}

type resourceChange struct {
	Resource  resourceIdentity
	Operation string
}

// resourceEvidence is a private observation, not installation authority. Its
// digest binds both COMPLETE renders and the exact signed publication. Live
// protected-resource and renderer/configuration checks remain mandatory too.
type resourceEvidence struct {
	digest                     string
	publication, before, after string
	changes                    []resourceChange
}

func (e resourceEvidence) String() string {
	return fmt.Sprintf("installation resources changes=%d digest=%s", len(e.changes), e.digest)
}

func (e resourceEvidence) GoString() string { return e.String() }

type resourceObject struct {
	identity         resourceIdentity
	version, apiPath string
	body             json.RawMessage
	value            map[string]any
}

type resourceType struct {
	name       string
	namespaced bool
}

// A rendered object's API path and scope come from the receiving cluster,
// not the manifest. Include that resolution even when its body is unchanged.
type resourceAddress struct {
	Resource         resourceIdentity
	Version, APIPath string
}

func inventoryAddresses(inventory map[resourceIdentity]resourceObject) []resourceAddress {
	addresses := make([]resourceAddress, 0, len(inventory))
	for key, object := range inventory {
		addresses = append(addresses, resourceAddress{key, object.version, object.apiPath})
	}
	sort.Slice(addresses, func(i, j int) bool { return addresses[i].Resource.String() < addresses[j].Resource.String() })
	return addresses
}

var kubernetesSegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,252}$`)

// verifyImagesAndDiff binds configured installation image slots to published
// OCI artifacts for this platform. Every other workload image must remain at
// its already-pinned baseline reference. No mutable tag may enter either the
// candidate or rollback render. A complete diff is retained even for resources
// with no container images; checking only Deployments would omit destructive
// changes to storage, identity, routing or RBAC.
func verifyImagesAndDiff(ctx context.Context, api argocd.API, published pipelines.VerifiedPublishedRelease, before, after argocd.RenderedRevision, platform string, bindings []imageBinding) (resourceEvidence, error) {
	if api == nil || before.Digest() == "" || after.Digest() == "" || len(bindings) == 0 || len(bindings) > 512 || (platform != "linux/arm64" && platform != "linux/amd64") {
		return resourceEvidence{}, errors.New("resource verification requires native renders, image bindings, platform and cluster discovery")
	}
	release, err := published.Release()
	if err != nil {
		return resourceEvidence{}, err
	}
	if err := sameRenderScope(before.Spec(), after.Spec()); err != nil {
		return resourceEvidence{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	// Share only this invocation's discovery; another replica obtains its own
	// facts, and a later preparation cannot inherit a process-local cache.
	schemas := map[string]map[string]resourceType{}
	old, err := resourceInventory(ctx, api, before, schemas)
	if err != nil {
		return resourceEvidence{}, err
	}
	next, err := resourceInventory(ctx, api, after, schemas)
	if err != nil {
		return resourceEvidence{}, err
	}
	oldImages, err := inventoryImages(old)
	if err != nil {
		return resourceEvidence{}, err
	}
	nextImages, err := inventoryImages(next)
	if err != nil {
		return resourceEvidence{}, err
	}
	selected := map[imageSlot]bool{}
	for _, binding := range bindings {
		if selected[binding.Slot] {
			return resourceEvidence{}, errors.New("installation contains duplicate image bindings")
		}
		selected[binding.Slot] = true
		image, found := nextImages[binding.Slot]
		if !found || !publishedImageMatches(release, binding, platform, image) {
			return resourceEvidence{}, fmt.Errorf("image for %s is absent or differs from its signed artifact and platform", binding.Slot.Resource)
		}
	}
	for slot, image := range nextImages {
		if !selected[slot] && oldImages[slot] != image {
			return resourceEvidence{}, fmt.Errorf("changed image for %s has no published artifact binding", slot.Resource)
		}
	}
	// Removing an explicitly bound workload is refused above. Other removals
	// remain visible in the full diff, for the separate preservation/approval
	// gate; this observation cannot itself authorize pruning.
	changes := []resourceChange{}
	for key, previous := range old {
		current, present := next[key]
		if !present {
			changes = append(changes, resourceChange{key, "delete"})
		} else if !bytes.Equal(previous.body, current.body) {
			changes = append(changes, resourceChange{key, "update"})
		}
	}
	for key := range next {
		if _, present := old[key]; !present {
			changes = append(changes, resourceChange{key, "create"})
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Resource.String() < changes[j].Resource.String() })
	orderedBindings := append([]imageBinding(nil), bindings...)
	sort.Slice(orderedBindings, func(i, j int) bool {
		a, _ := json.Marshal(orderedBindings[i])
		b, _ := json.Marshal(orderedBindings[j])
		return string(a) < string(b)
	})
	body, err := json.Marshal(struct {
		Publication, Before, After, Platform string
		Bindings                             []imageBinding
		Changes                              []resourceChange
		BeforeResources, AfterResources      []resourceAddress
	}{published.Digest(), before.Digest(), after.Digest(), platform, orderedBindings, changes, inventoryAddresses(old), inventoryAddresses(next)})
	if err != nil {
		return resourceEvidence{}, errors.New("resource evidence could not be encoded")
	}
	return resourceEvidence{digest: "memql-id:" + string(id.NewUntracked().FromString("installation-resource-evidence-v1:"+string(body))), publication: published.Digest(), before: before.Digest(), after: after.Digest(), changes: changes}, nil
}

func sameRenderScope(before, after argocd.RenderSpec) error {
	var a, b map[string]any
	if json.Unmarshal(before.Source, &a) != nil || json.Unmarshal(after.Source, &b) != nil || a["targetRevision"] == b["targetRevision"] {
		return errors.New("resource verification requires distinct immutable revisions")
	}
	delete(a, "targetRevision")
	delete(b, "targetRevision")
	before.Source, _ = json.Marshal(a)
	after.Source, _ = json.Marshal(b)
	x, _ := json.Marshal(before)
	y, _ := json.Marshal(after)
	if !bytes.Equal(x, y) {
		return errors.New("candidate and rollback renders have different source or renderer scope")
	}
	return nil
}

func resourceInventory(ctx context.Context, api argocd.API, rendered argocd.RenderedRevision, schemas map[string]map[string]resourceType) (map[resourceIdentity]resourceObject, error) {
	objects := map[resourceIdentity]resourceObject{}
	for _, body := range rendered.Resources() {
		value, err := resourceJSON(body)
		if err != nil {
			return nil, err
		}
		version, kind := resourceText(value, "apiVersion"), resourceText(value, "kind")
		base, group, err := apiVersionPath(version)
		if err != nil {
			return nil, err
		}
		types, found := schemas[version]
		if !found {
			if len(schemas) >= 64 {
				return nil, errors.New("rendered resource API versions exceed discovery bound")
			}
			types, err = discoverResourceTypes(ctx, api, base, version)
			if err != nil {
				return nil, err
			}
			schemas[version] = types
		}
		typ, found := types[kind]
		if !found {
			return nil, errors.New("rendered resource kind is absent from cluster discovery")
		}
		meta := resourceMap(value, "metadata")
		key := resourceIdentity{group, kind, resourceText(meta, "namespace"), resourceText(meta, "name")}
		if !kubernetesSegment.MatchString(key.Name) {
			return nil, errors.New("rendered resource name is invalid")
		}
		if typ.namespaced {
			if key.Namespace == "" {
				key.Namespace = rendered.Spec().Namespace
			}
			if !kubernetesSegment.MatchString(key.Namespace) || len(key.Namespace) > 63 {
				return nil, errors.New("rendered namespace is invalid")
			}
			base += "/namespaces/" + key.Namespace
		} else if key.Namespace != "" {
			return nil, errors.New("cluster-scoped resource carries a namespace")
		}
		if _, duplicate := objects[key]; duplicate {
			return nil, errors.New("rendered resources alias one cluster object")
		}
		objects[key] = resourceObject{key, version, base + "/" + typ.name + "/" + key.Name, body, value}
	}
	return objects, nil
}

func apiVersionPath(version string) (string, string, error) {
	parts := strings.Split(version, "/")
	for _, part := range parts {
		if !kubernetesSegment.MatchString(part) {
			return "", "", errors.New("rendered API version is invalid")
		}
	}
	if len(parts) == 1 && version == "v1" {
		return "api/v1", "", nil
	}
	if len(parts) == 2 {
		return "apis/" + version, parts[0], nil
	}
	return "", "", errors.New("rendered API version is unsupported")
}

func discoverResourceTypes(ctx context.Context, api argocd.API, path, version string) (map[string]resourceType, error) {
	body, err := api.Do(ctx, http.MethodGet, path, "", nil)
	if err != nil {
		return nil, errors.New("cluster resource discovery failed")
	}
	value, err := resourceJSON(body)
	if err != nil || resourceText(value, "groupVersion") != version {
		return nil, errors.New("cluster resource discovery identity differs")
	}
	items, ok := value["resources"].([]any)
	if !ok || len(items) == 0 || len(items) > 4096 {
		return nil, errors.New("cluster resource discovery has an invalid inventory")
	}
	out := map[string]resourceType{}
	plurals := map[string]bool{}
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			return nil, errors.New("cluster resource discovery is malformed")
		}
		plural, kind := resourceText(object, "name"), resourceText(object, "kind")
		if strings.Contains(plural, "/") {
			continue
		} // subresources are not objects to apply
		namespaced, ok := object["namespaced"].(bool)
		if !ok || !kubernetesSegment.MatchString(plural) || !kubernetesSegment.MatchString(kind) {
			return nil, errors.New("cluster resource discovery is malformed")
		}
		if _, duplicate := out[kind]; duplicate || plurals[plural] {
			return nil, errors.New("cluster discovery has ambiguous resource kinds")
		}
		plurals[plural] = true
		out[kind] = resourceType{plural, namespaced}
	}
	return out, nil
}

func resourceJSON(body []byte) (map[string]any, error) {
	if len(body) == 0 || len(body) > 4<<20 {
		return nil, errors.New("resource object exceeds its bound")
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	var value map[string]any
	if d.Decode(&value) != nil || value == nil {
		return nil, errors.New("resource object is malformed")
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, errors.New("resource object has trailing data")
	}
	return value, nil
}

func resourceText(value map[string]any, key string) string { s, _ := value[key].(string); return s }
func resourceMap(value map[string]any, key string) map[string]any {
	m, _ := value[key].(map[string]any)
	return m
}

func inventoryImages(inventory map[resourceIdentity]resourceObject) (map[imageSlot]string, error) {
	out := map[imageSlot]string{}
	for key, object := range inventory {
		spec := resourceMap(object.value, "spec")
		var pod map[string]any
		switch key.Group + "/" + key.Kind {
		case "apps/Deployment", "apps/StatefulSet", "apps/DaemonSet", "apps/ReplicaSet", "batch/Job":
			pod = resourceMap(resourceMap(spec, "template"), "spec")
		case "batch/CronJob":
			pod = resourceMap(resourceMap(resourceMap(resourceMap(spec, "jobTemplate"), "spec"), "template"), "spec")
		case "/Pod":
			pod = spec
		case "postgresql.cnpg.io/Cluster":
			image, err := immutableImage(resourceText(spec, "imageName"))
			if err != nil {
				return nil, fmt.Errorf("%s: database image is not immutable", key)
			}
			out[imageSlot{key, "imageName", ""}] = image
			continue
		default:
			if !knownNonWorkload(object.version+"/"+key.Kind) || containsImageField(spec) {
				return nil, errors.New("resource kind or image fields need a qualified installation codec")
			}
			continue
		}
		if pod == nil {
			return nil, errors.New("workload lacks a pod specification")
		}
		for _, field := range []string{"containers", "initContainers", "ephemeralContainers"} {
			raw, present := pod[field]
			if !present && field != "containers" {
				continue
			}
			containers, ok := raw.([]any)
			if !ok || (field == "containers" && len(containers) == 0) || len(containers) > 256 {
				return nil, errors.New("workload container inventory is malformed")
			}
			for _, raw := range containers {
				container, ok := raw.(map[string]any)
				containerName := resourceText(container, "name")
				if !ok || !kubernetesSegment.MatchString(containerName) {
					return nil, errors.New("workload container identity is invalid")
				}
				slot := imageSlot{key, field, containerName}
				if _, exists := out[slot]; exists {
					return nil, errors.New("workload has duplicate container identities")
				}
				image, err := immutableImage(resourceText(container, "image"))
				if err != nil {
					return nil, fmt.Errorf("%s: workload image is not immutable", key)
				}
				out[slot] = image
			}
		}
		// Kubernetes image volumes pull OCI artifacts independently of the
		// containers above. They must not introduce an unbound image input.
		if raw, present := pod["volumes"]; present {
			volumes, ok := raw.([]any)
			if !ok || len(volumes) > 256 {
				return nil, errors.New("workload volume inventory is malformed")
			}
			seen := map[string]bool{}
			for _, raw := range volumes {
				volume, ok := raw.(map[string]any)
				volumeName := resourceText(volume, "name")
				if !ok || !kubernetesSegment.MatchString(volumeName) || seen[volumeName] {
					return nil, errors.New("workload volume identity is invalid")
				}
				seen[volumeName] = true
				if _, present := volume["image"]; !present {
					continue
				}
				image, err := immutableImage(resourceText(resourceMap(volume, "image"), "reference"))
				if err != nil {
					return nil, fmt.Errorf("%s: volume image is not immutable", key)
				}
				out[imageSlot{key, "volumes", volumeName}] = image
			}
		}
	}
	return out, nil
}

// Nested Applications, arbitrary custom controllers and new workload kinds
// cannot hide additional unverified source/image inputs behind discovery.
// This is a codec inventory, not a caller-supplied trust/approval allowlist.
func knownNonWorkload(kind string) bool {
	switch kind {
	case "v1/ConfigMap", "v1/Secret", "v1/Namespace", "v1/Service", "v1/ServiceAccount", "v1/PersistentVolumeClaim", "v1/PersistentVolume", "v1/LimitRange", "v1/ResourceQuota",
		"rbac.authorization.k8s.io/v1/Role", "rbac.authorization.k8s.io/v1/RoleBinding", "rbac.authorization.k8s.io/v1/ClusterRole", "rbac.authorization.k8s.io/v1/ClusterRoleBinding",
		"networking.k8s.io/v1/Ingress", "networking.k8s.io/v1/NetworkPolicy", "networking.k8s.io/v1/IngressClass", "policy/v1/PodDisruptionBudget",
		"cert-manager.io/v1/Certificate", "cert-manager.io/v1/Issuer", "cert-manager.io/v1/ClusterIssuer",
		"traefik.io/v1alpha1/IngressRouteTCP", "traefik.io/v1alpha1/ServersTransport",
		"barmancloud.cnpg.io/v1/ObjectStore", "postgresql.cnpg.io/v1/Database", "postgresql.cnpg.io/v1/ScheduledBackup":
		return true
	}
	return false
}

func containsImageField(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			if key == "image" || key == "imageName" || containsImageField(child) {
				return true
			}
		}
	case []any:
		for _, child := range v {
			if containsImageField(child) {
				return true
			}
		}
	}
	return false
}

func immutableImage(reference string) (string, error) {
	if len(reference) > 2048 {
		return "", errors.New("image reference exceeds its bound")
	}
	digest, err := name.NewDigest(reference, name.StrictValidation)
	if err != nil || !artifactDigest.MatchString(digest.DigestStr()) {
		return "", errors.New("image must name an exact SHA-256 digest")
	}
	return digest.Name(), nil
}

func publishedImageMatches(release pipelines.PublishedRelease, binding imageBinding, platform, image string) bool {
	for _, component := range release.Components {
		if component.Name != binding.Component {
			continue
		}
		for _, artifact := range component.Artifacts {
			if artifact.Name != binding.Artifact || artifact.Kind != "oci" || artifact.Platform != platform {
				continue
			}
			for _, location := range artifact.Locations {
				u, err := url.Parse(location.Origin)
				if err != nil {
					continue
				}
				ref, err := immutableImage(u.Host + "/" + location.Repository + "@" + artifact.ImageDigest)
				if err == nil && ref == image {
					return true
				}
			}
		}
	}
	return false
}
