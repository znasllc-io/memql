package installation

import (
	"bytes"
	"context"
	"errors"

	"github.com/znasllc-io/memql/integrations/argocd"
)

// These exact controller versions are admitted only as unchanged installation
// dependencies. Release selection cannot create, replace, reconfigure or prune
// them. New versions and controllers still require an explicit codec review.
func preservedController(kind string) bool {
	switch kind {
	case "external-secrets.io/v1/ExternalSecret", "external-secrets.io/v1/SecretStore",
		"capacity.memql.io/v1alpha1/BackgroundCapacityPolicy",
		"kueue.x-k8s.io/v1beta2/ClusterQueue", "kueue.x-k8s.io/v1beta2/LocalQueue",
		"kueue.x-k8s.io/v1beta2/ResourceFlavor", "kueue.x-k8s.io/v1beta2/WorkloadPriorityClass",
		"admissionregistration.k8s.io/v1/ValidatingAdmissionPolicy", "admissionregistration.k8s.io/v1/ValidatingAdmissionPolicyBinding":
		return true
	}
	return false
}

func verifyPreservedControllers(before, after map[resourceIdentity]resourceObject) error {
	for _, inventory := range []map[resourceIdentity]resourceObject{before, after} {
		for key, object := range inventory {
			if !preservedController(object.version + "/" + key.Kind) {
				continue
			}
			old, existed := before[key]
			next, remains := after[key]
			if !existed || !remains || old.version != next.version || old.apiPath != next.apiPath || !bytes.Equal(old.body, next.body) {
				return errors.New("installation controller changes require maintenance review")
			}
		}
	}
	return nil
}

// A union can prove an artifact is present without proving which Application
// owns it. Bind each configured image slot to its declared member before the
// aggregate can authorize any deployment; names come from admitted native
// configuration, never from a caller-provided render inventory.
func verifyBindingApplications(ctx context.Context, api argocd.API, rendered argocd.RenderInventory, schemas map[string]map[string]resourceType, bindings []imageBinding) error {
	owners := map[string]map[resourceIdentity]resourceObject{}
	for _, member := range rendered.RenderMembers() {
		app := member.Spec().AppName
		if app == "" || owners[app] != nil {
			return errors.New("rendered application ownership is missing or duplicated")
		}
		inventory, err := resourceInventory(ctx, api, member, schemas)
		if err != nil {
			return err
		}
		owners[app] = inventory
	}
	for _, binding := range bindings {
		if binding.Application == "" && len(owners) == 1 {
			continue // Existing single-Application configuration has no member key.
		}
		if _, found := owners[binding.Application][binding.Slot.Resource]; !found {
			return errors.New("image binding is absent from its declared application")
		}
	}
	return nil
}
