package installation

import (
	"context"
	"errors"
	"net/http"

	"github.com/znasllc-io/memql/integrations/argocd"
)

// Owner references may cross from a namespaced dependent to a cluster-scoped
// owner. Resolve every hop through receiving-cluster discovery, and include
// ancestors: deleting a grandparent can garbage-collect an unchanged Secret.
type protectedOwnerObservation struct {
	Address     resourceAddress
	UID, Digest string
}

type protectedOwners struct {
	api       argocd.API
	schemas   map[string]map[string]resourceType
	old, next map[resourceIdentity]resourceObject
	seen      map[resourceIdentity]protectedOwnerObservation
	visiting  map[resourceIdentity]bool
}

func (p *protectedOwners) observe(ctx context.Context, child resourceIdentity, meta map[string]any, depth int) error {
	if ctx.Err() != nil || depth > 8 {
		return errors.New("protected resource owner verification exceeded its bound")
	}
	raw, present := meta["ownerReferences"]
	if !present {
		return nil
	}
	owners, ok := raw.([]any)
	if !ok || len(owners) > 16 {
		return errors.New("protected resource ownership is malformed")
	}
	unique := map[resourceIdentity]bool{}
	for _, raw := range owners {
		owner, ok := raw.(map[string]any)
		if !ok {
			return errors.New("protected resource ownership is malformed")
		}
		version, uid := resourceText(owner, "apiVersion"), resourceText(owner, "uid")
		path, group, err := apiVersionPath(version)
		if err != nil || !identifier.MatchString(uid) || !kubernetesSegment.MatchString(resourceText(owner, "name")) {
			return errors.New("protected resource owner identity is invalid")
		}
		types, found := p.schemas[version]
		if !found {
			if len(p.schemas) >= 64 {
				return errors.New("protected resource owner API versions exceed discovery bound")
			}
			types, err = discoverResourceTypes(ctx, p.api, path, version)
			if err != nil {
				return err
			}
			p.schemas[version] = types
		}
		typ, found := types[resourceText(owner, "kind")]
		if !found || (typ.namespaced && child.Namespace == "") {
			return errors.New("protected resource owner scope is invalid")
		}
		parent := resourceIdentity{Group: group, Kind: resourceText(owner, "kind"), Name: resourceText(owner, "name")}
		if typ.namespaced {
			parent.Namespace = child.Namespace
			path += "/namespaces/" + child.Namespace
		}
		path += "/" + typ.name + "/" + parent.Name
		if unique[parent] || p.visiting[parent] || parent == child {
			return errors.New("protected resource ownership is duplicated or cyclic")
		}
		unique[parent] = true
		previous, existed := p.old[parent]
		candidate, remains := p.next[parent]
		if existed != remains || (existed && !sameJSON(previous.value, candidate.value)) {
			return errors.New("protected resource controller changes require maintenance review")
		}
		if observed, found := p.seen[parent]; found {
			if observed.UID != uid || observed.Address.Version != version {
				return errors.New("protected resource owner references disagree")
			}
			continue
		}
		if len(p.seen)+len(p.visiting) >= 128 {
			return errors.New("protected resource owner inventory exceeds its bound")
		}
		body, err := p.api.Do(ctx, http.MethodGet, path, "", nil)
		if err != nil {
			return errors.New("protected resource owner could not be read")
		}
		live, err := resourceJSON(body)
		if err != nil || resourceText(live, "apiVersion") != version || resourceText(live, "kind") != parent.Kind || validLiveStorage(live, parent) != nil || resourceText(resourceMap(live, "metadata"), "uid") != uid {
			return errors.New("protected resource owner is absent, replaced or deleting")
		}
		for _, value := range []map[string]any{live, previous.value, candidate.value} {
			if err := protectedApplyOptions(resourceMap(value, "metadata")); err != nil {
				return err
			}
		}
		p.visiting[parent] = true
		if err := p.observe(ctx, parent, resourceMap(live, "metadata"), depth+1); err != nil {
			return err
		}
		delete(p.visiting, parent)
		p.seen[parent] = protectedOwnerObservation{Address: resourceAddress{parent, version, path}, UID: uid, Digest: sensitiveDigest("owner", live)}
	}
	return nil
}

func (p *protectedOwners) digests() map[string]string {
	out := map[string]string{}
	for key, observation := range p.seen {
		out[key.String()] = sensitiveDigest("owner-address", observation)
	}
	return out
}
