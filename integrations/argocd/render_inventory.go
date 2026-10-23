package argocd

import (
	"encoding/json"
	"errors"
	"sort"

	"github.com/znasllc-io/memql/core/id"
)

// RenderInventory is a native render or an explicitly bound set of native
// renders. It has no representative Source: every member remains available for
// scope comparison, and its digest includes every exact member specification.
type RenderInventory interface {
	Digest() string
	Resources() []json.RawMessage
	Namespace() string
	RenderScopes() []RenderSpec
	RenderMembers() []RenderedRevision
	renderInventory()
}

func (RenderedRevision) renderInventory()                    {}
func (r RenderedRevision) Namespace() string                 { return r.Spec().Namespace }
func (r RenderedRevision) RenderScopes() []RenderSpec        { return []RenderSpec{r.Spec()} }
func (r RenderedRevision) RenderMembers() []RenderedRevision { return []RenderedRevision{r} }

type RenderedSet struct {
	members []RenderedRevision
	digest  string
}

func (RenderedSet) renderInventory() {}
func (r RenderedSet) Digest() string { return r.digest }
func (r RenderedSet) Namespace() string {
	if len(r.members) == 0 {
		return ""
	}
	return r.members[0].Namespace()
}
func (r RenderedSet) RenderScopes() []RenderSpec {
	out := make([]RenderSpec, len(r.members))
	for i, member := range r.members {
		out[i] = member.Spec()
	}
	return out
}
func (r RenderedSet) RenderMembers() []RenderedRevision {
	return append([]RenderedRevision(nil), r.members...)
}
func (r RenderedSet) Resources() []json.RawMessage {
	var out []json.RawMessage
	for _, member := range r.members {
		out = append(out, member.Resources()...)
	}
	return out
}

// CombineRenderedRevisions does not render, grant authority or close source
// inputs. It binds a complete set of already-native renderer observations.
// Resource discovery subsequently rejects duplicate ownership identities.
func CombineRenderedRevisions(members []RenderedRevision) (RenderedSet, error) {
	if len(members) == 0 || len(members) > 8 {
		return RenderedSet{}, errors.New("render set requires one to eight applications")
	}
	owned := append([]RenderedRevision(nil), members...)
	sort.Slice(owned, func(i, j int) bool { return owned[i].Spec().AppName < owned[j].Spec().AppName })
	base := owned[0].Spec()
	seen := map[string]bool{}
	total := 0
	var bindings []any
	for _, member := range owned {
		spec := member.Spec()
		if member.Digest() == "" || spec.AppName == "" || seen[spec.AppName] || spec.Namespace != base.Namespace || spec.KubeVersion != base.KubeVersion || !sameJSONValue(spec.APIVersions, base.APIVersions) || spec.KustomizeBuildOptions != base.KustomizeBuildOptions || spec.KustomizeBinaryPath != base.KustomizeBinaryPath {
			return RenderedSet{}, errors.New("render set has missing, duplicated or incompatible application scope")
		}
		seen[spec.AppName] = true
		for _, body := range member.resources {
			total += len(body)
		}
		if total > maxRenderBytes {
			return RenderedSet{}, errors.New("render set exceeds aggregate byte bound")
		}
		bindings = append(bindings, []any{spec, member.Digest()})
	}
	out := RenderedSet{members: owned}
	if len(out.Resources()) > maxRenderResources {
		return RenderedSet{}, errors.New("render set exceeds aggregate resource bound")
	}
	body, _ := json.Marshal(bindings)
	out.digest = "memql-id:" + string(id.NewUntracked().FromString("installation-render-set-v1:"+string(body)))
	return out, nil
}

func sameJSONValue(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
