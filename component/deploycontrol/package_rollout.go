package deploycontrol

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
)

const packageRolloutAnnotation = "memql.io/package-deployment"

var packageWorkloadName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)

// PackageRolloutIntent retains identity and hashes only, never a Deployment's
// environment or credentials. The fixed marker is selected before any patch.
type PackageRolloutIntent struct {
	Namespace            string `json:"namespace"`
	Name                 string `json:"name"`
	UID                  string `json:"uid"`
	Marker               string `json:"marker"`
	BeforeTemplateSHA256 string `json:"beforeTemplateSHA256"`
	AfterTemplateSHA256  string `json:"afterTemplateSHA256"`
	Replicas             int32  `json:"replicas"`
	BeforeGeneration     int64  `json:"beforeGeneration"`
}
type PackageRolloutObservation struct{ Applied, Ready bool }
type packageDeployment struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		UID, Name, Namespace, ResourceVersion string
		Generation                            int64
	} `json:"metadata"`
	Spec struct {
		Replicas *int32         `json:"replicas"`
		Template map[string]any `json:"template"`
	} `json:"spec"`
	Status struct {
		ObservedGeneration                                          int64 `json:"observedGeneration"`
		Replicas, UpdatedReplicas, ReadyReplicas, AvailableReplicas int32
		UnavailableReplicas                                         int32
	} `json:"status"`
}

func packageDeploymentPath(ns, name string) (string, error) {
	if !packageWorkloadName.MatchString(ns) || !packageWorkloadName.MatchString(name) {
		return "", fmt.Errorf("invalid package rollout resource name")
	}
	return "/apis/apps/v1/namespaces/" + ns + "/deployments/" + name, nil
}
func templateSHA(template map[string]any) string {
	b, _ := json.Marshal(template)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func templateWithPackageMarker(template map[string]any, marker string) (map[string]any, error) {
	raw, err := json.Marshal(template)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if json.Unmarshal(raw, &out) != nil || out == nil {
		return nil, fmt.Errorf("invalid rollout template")
	}
	meta, ok := out["metadata"].(map[string]any)
	if !ok {
		meta = map[string]any{}
		out["metadata"] = meta
	}
	annotations, ok := meta["annotations"].(map[string]any)
	if !ok {
		annotations = map[string]any{}
		meta["annotations"] = annotations
	}
	annotations[packageRolloutAnnotation] = marker
	return out, nil
}
func (e *ClusterAPI) packageDeployment(ctx context.Context, ns, name string) (packageDeployment, error) {
	path, err := packageDeploymentPath(ns, name)
	if err != nil {
		return packageDeployment{}, err
	}
	raw, err := e.do(ctx, "GET", path, "", nil)
	if err != nil {
		return packageDeployment{}, err
	}
	var d packageDeployment
	if json.Unmarshal(raw, &d) != nil || d.APIVersion != "apps/v1" || d.Kind != "Deployment" || d.Metadata.Name != name || d.Metadata.Namespace != ns || d.Metadata.UID == "" || d.Metadata.ResourceVersion == "" || d.Spec.Template == nil {
		return d, fmt.Errorf("package rollout target shape differs")
	}
	if d.Spec.Replicas == nil {
		n := int32(1)
		d.Spec.Replicas = &n
	}
	if *d.Spec.Replicas < 0 {
		return d, fmt.Errorf("invalid rollout replicas")
	}
	return d, nil
}
func (e *ClusterAPI) PreparePackageRollout(ctx context.Context, ns, name, marker string) (PackageRolloutIntent, error) {
	if marker == "" || len(marker) > 160 {
		return PackageRolloutIntent{}, fmt.Errorf("package rollout requires a stable native marker")
	}
	d, err := e.packageDeployment(ctx, ns, name)
	if err != nil {
		return PackageRolloutIntent{}, err
	}
	next, err := templateWithPackageMarker(d.Spec.Template, marker)
	if err != nil {
		return PackageRolloutIntent{}, err
	}
	before, after := templateSHA(d.Spec.Template), templateSHA(next)
	if before == after {
		return PackageRolloutIntent{}, fmt.Errorf("package rollout marker already exists before selection")
	}
	return PackageRolloutIntent{Namespace: ns, Name: name, UID: d.Metadata.UID, Marker: marker, BeforeTemplateSHA256: before, AfterTemplateSHA256: after, Replicas: *d.Spec.Replicas, BeforeGeneration: d.Metadata.Generation}, nil
}
func (e *ClusterAPI) ObservePackageRollout(ctx context.Context, i PackageRolloutIntent) (PackageRolloutObservation, error) {
	d, err := e.packageDeployment(ctx, i.Namespace, i.Name)
	if err != nil {
		return PackageRolloutObservation{}, err
	}
	if d.Metadata.UID != i.UID || *d.Spec.Replicas != i.Replicas {
		return PackageRolloutObservation{}, fmt.Errorf("package rollout resource identity changed")
	}
	hash := templateSHA(d.Spec.Template)
	if hash == i.BeforeTemplateSHA256 {
		return PackageRolloutObservation{}, nil
	}
	if hash != i.AfterTemplateSHA256 {
		return PackageRolloutObservation{}, fmt.Errorf("package rollout template changed outside this intent")
	}
	return PackageRolloutObservation{Applied: true, Ready: d.Metadata.Generation > i.BeforeGeneration && d.Status.ObservedGeneration >= d.Metadata.Generation && d.Status.Replicas == i.Replicas && d.Status.UpdatedReplicas == i.Replicas && d.Status.ReadyReplicas == i.Replicas && d.Status.AvailableReplicas == i.Replicas && d.Status.UnavailableReplicas == 0}, nil
}

// ApplyPackageRollout is called only after the native journal records an
// attempted effect. A lost reply is reconciled with Observe, never called again.
func (e *ClusterAPI) ApplyPackageRollout(ctx context.Context, i PackageRolloutIntent) error {
	d, err := e.packageDeployment(ctx, i.Namespace, i.Name)
	if err != nil {
		return err
	}
	if d.Metadata.UID != i.UID || *d.Spec.Replicas != i.Replicas || templateSHA(d.Spec.Template) != i.BeforeTemplateSHA256 {
		return fmt.Errorf("package rollout target changed before its one patch")
	}
	next, err := templateWithPackageMarker(d.Spec.Template, i.Marker)
	if err != nil {
		return err
	}
	if templateSHA(next) != i.AfterTemplateSHA256 {
		return fmt.Errorf("package rollout selection differs")
	}
	patch, err := json.Marshal([]map[string]any{
		{"op": "test", "path": "/metadata/uid", "value": i.UID},
		{"op": "test", "path": "/metadata/resourceVersion", "value": d.Metadata.ResourceVersion},
		{"op": "test", "path": "/spec/template", "value": d.Spec.Template},
		{"op": "replace", "path": "/spec/template", "value": next},
	})
	if err != nil {
		return err
	}
	path, err := packageDeploymentPath(i.Namespace, i.Name)
	if err != nil {
		return err
	}
	_, err = e.do(ctx, "PATCH", path, "application/json-patch+json", patch)
	return err
}
