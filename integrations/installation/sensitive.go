package installation

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/argocd"
)

// required names come from native installation configuration, including
// credentials seeded outside Git. They are never a client-provided allowlist.
type sensitiveEvidence struct {
	digest, before, after string
	observations          []sensitiveObservation
}

type sensitiveObservation struct {
	Resource         resourceIdentity
	UID, ValueDigest string
	MetadataDigest   string
}

func (e sensitiveEvidence) String() string {
	return fmt.Sprintf("installation protected resources=%d digest=%s", len(e.observations), e.digest)
}

func (e sensitiveEvidence) GoString() string { return e.String() }

func verifySensitivePreservation(ctx context.Context, api argocd.API, before, after argocd.RenderedRevision, required []resourceIdentity) (sensitiveEvidence, error) {
	if api == nil || before.Digest() == "" || after.Digest() == "" || len(required) == 0 || len(required) > 128 {
		return sensitiveEvidence{}, errors.New("protected resource verification requires native renders, configured resources and cluster reads")
	}
	if err := sameRenderScope(before.Spec(), after.Spec()); err != nil {
		return sensitiveEvidence{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if ctx.Err() != nil {
		return sensitiveEvidence{}, errors.New("protected resource verification was cancelled")
	}
	targets := map[resourceIdentity]bool{}
	for _, key := range required {
		if _, err := sensitivePath(key); err != nil {
			return sensitiveEvidence{}, err
		}
		if targets[key] {
			return sensitiveEvidence{}, errors.New("protected resource configuration contains duplicate identities")
		}
		targets[key] = true
	}
	schemas := map[string]map[string]resourceType{}
	old, err := resourceInventory(ctx, api, before, schemas)
	if err != nil {
		return sensitiveEvidence{}, err
	}
	next, err := resourceInventory(ctx, api, after, schemas)
	if err != nil {
		return sensitiveEvidence{}, err
	}
	for _, inventory := range []map[resourceIdentity]resourceObject{old, next} {
		for key := range inventory {
			if key.Namespace != "" {
				targets[resourceIdentity{Kind: "Namespace", Name: key.Namespace}] = true
			}
			if key.Group == "" && (key.Kind == "Secret" || key.Kind == "Namespace") {
				targets[key] = true
			}
		}
	}
	for key := range targets {
		if key.Kind == "Secret" && key.Namespace != "" {
			targets[resourceIdentity{Kind: "Namespace", Name: key.Namespace}] = true
		}
	}
	if len(targets) > 256 {
		return sensitiveEvidence{}, errors.New("protected resource inventory exceeds its bound")
	}
	observations := []sensitiveObservation{}
	owners := &protectedOwners{api: api, schemas: schemas, old: old, next: next, seen: map[resourceIdentity]protectedOwnerObservation{}, visiting: map[resourceIdentity]bool{}}
	for key := range targets {
		if err := ctx.Err(); err != nil {
			return sensitiveEvidence{}, errors.New("protected resource verification was cancelled")
		}
		path, err := sensitivePath(key)
		if err != nil {
			return sensitiveEvidence{}, err
		}
		previous, existed := old[key]
		candidate, remains := next[key]
		if existed != remains {
			return sensitiveEvidence{}, errors.New("protected resource creation or removal needs a qualified maintenance contract")
		}
		if existed && (!sameJSON(previous.value["spec"], candidate.value["spec"]) || !sameOwnership(previous.value, candidate.value)) {
			return sensitiveEvidence{}, errors.New("protected resource ownership or namespace configuration changes require maintenance review")
		}
		for _, value := range []map[string]any{previous.value, candidate.value} {
			if err := protectedApplyOptions(resourceMap(value, "metadata")); err != nil {
				return sensitiveEvidence{}, err
			}
		}
		body, err := api.Do(ctx, http.MethodGet, path, "", nil)
		if err != nil {
			return sensitiveEvidence{}, errors.New("protected resource could not be read")
		}
		live, err := resourceJSON(body)
		if err != nil || resourceText(live, "apiVersion") != "v1" || resourceText(live, "kind") != key.Kind || validLiveStorage(live, key) != nil {
			return sensitiveEvidence{}, errors.New("protected resource identity is absent, changed or deleting")
		}
		meta := resourceMap(live, "metadata")
		if err := protectedApplyOptions(meta); err != nil {
			return sensitiveEvidence{}, err
		}
		if err := owners.observe(ctx, key, meta, 0); err != nil {
			return sensitiveEvidence{}, err
		}
		if existed {
			for _, field := range []string{"ownerReferences", "finalizers"} {
				if wanted, present := resourceMap(candidate.value, "metadata")[field]; present && !sameJSON(wanted, meta[field]) {
					return sensitiveEvidence{}, errors.New("rendered ownership differs from the protected resource")
				}
			}
		}
		var value any
		if key.Kind == "Secret" {
			current, err := secretValue(live)
			if err != nil {
				return sensitiveEvidence{}, err
			}
			value = current
			if existed {
				a, ea := secretValue(previous.value)
				b, eb := secretValue(candidate.value)
				if ea != nil || eb != nil || !sameJSON(a, b) || !sameJSON(b, current) {
					return sensitiveEvidence{}, errors.New("rendered secret would change existing credential material")
				}
			} else if !hasArgoOption(meta, "argocd.argoproj.io/sync-options", "Prune=false") || !hasArgoOption(meta, "argocd.argoproj.io/compare-options", "IgnoreExtraneous") {
				return sensitiveEvidence{}, errors.New("out-of-band secret requires explicit Argo prune and comparison protection")
			}
		} else {
			if resourceText(resourceMap(live, "status"), "phase") != "Active" {
				return sensitiveEvidence{}, errors.New("protected namespace is not active")
			}
			value = resourceMap(live, "spec")
			if existed {
				for field, wanted := range resourceMap(candidate.value, "spec") {
					if !sameJSON(wanted, resourceMap(live, "spec")[field]) {
						return sensitiveEvidence{}, errors.New("rendered namespace configuration differs from the active namespace")
					}
				}
			}
		}
		// Last-applied annotations can contain an entire Secret. Never retain
		// or format arbitrary annotations as supposedly harmless metadata.
		annotations := resourceMap(meta, "annotations")
		metadata := map[string]any{"owners": meta["ownerReferences"], "finalizers": meta["finalizers"],
			"prune": annotations["argocd.argoproj.io/sync-options"], "comparison": annotations["argocd.argoproj.io/compare-options"]}
		observations = append(observations, sensitiveObservation{Resource: key, UID: resourceText(meta, "uid"), ValueDigest: sensitiveDigest("value", value), MetadataDigest: sensitiveDigest("metadata", metadata)})
	}
	sort.Slice(observations, func(i, j int) bool { return observations[i].Resource.String() < observations[j].Resource.String() })
	digest := sensitiveDigest("evidence", struct {
		Before, After                   string
		BeforeResources, AfterResources []resourceAddress
		Observed                        []sensitiveObservation
		Owners                          map[string]string
	}{before.Digest(), after.Digest(), inventoryAddresses(old), inventoryAddresses(next), observations, owners.digests()})
	return sensitiveEvidence{digest, before.Digest(), after.Digest(), observations}, nil
}

func sensitivePath(key resourceIdentity) (string, error) {
	if key.Group != "" || !kubernetesSegment.MatchString(key.Name) {
		return "", errors.New("protected resource identity is invalid")
	}
	switch key.Kind {
	case "Namespace":
		if key.Namespace == "" && len(key.Name) <= 63 {
			return "api/v1/namespaces/" + key.Name, nil
		}
	case "Secret":
		if kubernetesSegment.MatchString(key.Namespace) && len(key.Namespace) <= 63 {
			return "api/v1/namespaces/" + key.Namespace + "/secrets/" + key.Name, nil
		}
	}
	return "", errors.New("protected resource kind or namespace is invalid")
}

func sameOwnership(a, b map[string]any) bool {
	x, y := resourceMap(a, "metadata"), resourceMap(b, "metadata")
	return sameJSON(x["ownerReferences"], y["ownerReferences"]) && sameJSON(x["finalizers"], y["finalizers"])
}

func hasArgoOption(meta map[string]any, key, expected string) bool {
	wanted, _, _ := strings.Cut(expected, "=")
	found := false
	for _, option := range strings.Split(resourceText(resourceMap(meta, "annotations"), key), ",") {
		option = strings.TrimSpace(option)
		name, _, _ := strings.Cut(option, "=")
		if name == wanted {
			if found || option != expected {
				return false
			}
			found = true
		}
	}
	return found
}

func protectedApplyOptions(meta map[string]any) error {
	annotations := resourceMap(meta, "annotations")
	for _, key := range []string{"argocd.argoproj.io/hook", "argocd.argoproj.io/hook-delete-policy"} {
		if _, present := annotations[key]; present {
			return errors.New("protected resource hooks require a qualified maintenance contract")
		}
	}
	raw, present := annotations["argocd.argoproj.io/sync-options"]
	if !present {
		return nil
	}
	options, ok := raw.(string)
	if !ok {
		return errors.New("protected resource sync options are malformed")
	}
	seen := map[string]bool{}
	for _, option := range strings.Split(options, ",") {
		name, value, _ := strings.Cut(strings.TrimSpace(option), "=")
		if seen[name] || ((name == "Force" || name == "Replace") && value != "false") {
			return errors.New("protected resource sync options may recreate the resource")
		}
		seen[name] = true
	}
	return nil
}

var secretDataKey = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,253}$`)

func secretValue(object map[string]any) (map[string]any, error) {
	data := map[string][]byte{}
	for _, field := range []string{"data", "stringData"} {
		raw, present := object[field]
		if !present {
			continue
		}
		values, ok := raw.(map[string]any)
		if !ok || len(values) > 2048 {
			return nil, errors.New("protected secret data is malformed")
		}
		for key, raw := range values {
			value, ok := raw.(string)
			if !ok || !secretDataKey.MatchString(key) {
				return nil, errors.New("protected secret value is malformed")
			}
			decoded := []byte(value)
			if field == "data" {
				var err error
				decoded, err = base64.StdEncoding.Strict().DecodeString(value)
				if err != nil {
					return nil, errors.New("protected secret encoding is invalid")
				}
			}
			data[key] = decoded
		}
	}
	size := 0
	for _, value := range data {
		size += len(value)
	}
	if size > 1<<20 {
		return nil, errors.New("protected secret data exceeds its bound")
	}
	immutable := false
	if raw, present := object["immutable"]; present {
		var ok bool
		immutable, ok = raw.(bool)
		if !ok {
			return nil, errors.New("protected secret immutability is malformed")
		}
	}
	typ := ""
	if raw, present := object["type"]; present {
		var ok bool
		typ, ok = raw.(string)
		if !ok || len(typ) > 253 {
			return nil, errors.New("protected secret type is malformed")
		}
	}
	if typ == "" {
		typ = "Opaque"
	}
	return map[string]any{"type": typ, "immutable": immutable, "data": data}, nil
}

func sensitiveDigest(domain string, value any) string {
	body, _ := json.Marshal(value)
	return "memql-id:" + string(id.NewUntracked().FromString("installation-sensitive-"+domain+"-v1:"+string(body)))
}
