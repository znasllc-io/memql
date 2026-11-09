package installation

import (
	"errors"
	"fmt"

	"github.com/znasllc-io/memql/integrations/argocd"
)

// receiverOwnConfiguration is the ConfigMap the receiver reads installation.json
// from, with the content it had when this snapshot read it.
type receiverOwnConfiguration struct {
	namespace, name string
	content         map[string]any
}

// receiverConfigMapContent is what a ConfigMap declares, normalized so a
// rendered manifest and the live object compare equal when Argo would apply no
// change: absent data is empty and absent immutability is false.
func receiverConfigMapContent(object map[string]any) map[string]any {
	out := map[string]any{"data": map[string]any{}, "binaryData": map[string]any{}, "immutable": object["immutable"] == true}
	for _, field := range []string{"data", "binaryData"} {
		if values, ok := object[field].(map[string]any); ok {
			out[field] = values
		}
	}
	return out
}

// ownConfigurationUnchanged refuses a plan whose render would change the
// receiver's own configuration. The receiver verifies every step against the
// installation.json it read at preparation, and the configuration ConfigMap is
// itself rendered by an Application the plan syncs (in the reference instance
// the memql Application renders deploy/k8s/instance/installation). A candidate
// that edits it therefore rewrites, in the middle of the plan, the input every
// later Apply, completion and rollback compares with preparation: completion
// can never be recorded, and the plan holds the installation head until its
// operator closes it. So it is refused before any plan exists or any
// Application is written. The preparation's reservation (taken earlier in the
// recipe) still holds the installation, so the reason tells the operator to
// cancel it, land the configuration change outside Cluster > Updates, and
// prepare again.
//
// Refused: a candidate that declares the ConfigMap with other content than the
// live one; a candidate that stops declaring a ConfigMap the current release
// declares, because the leftover object would be pruned or, kept by
// Prune=false, leave its Application out of sync so completion never
// converges; and a current release whose declaration differs from the live
// object, which a restoration would write back. A render that never declares
// it (the ConfigMap applied by hand) changes nothing and passes. Labels and
// annotations are metadata the receiver does not read and may change.
//
// Not covered, and stranding rather than unsafe: a candidate that changes
// another input the receiver reads (a rendered Secret or AppProject, or the
// MEMQL_INSTALLATION_CONFIG_NAME a rendered environment ConfigMap selects); the
// invariant still refuses every later step of such a plan.
// Regression: TestPreparationRefusesACandidateThatChangesTheReceiverConfiguration
// and, at the start fence, TestRevisionStartRefusesAnOlderPlanThatChangesTheReceiverConfiguration.
func (s *receiverSnapshot) ownConfigurationUnchanged(before, after argocd.RenderInventory) error {
	if s == nil || s.own.name == "" || s.own.content == nil || !validRenderInventory(before) || !validRenderInventory(after) {
		return errors.New("installation receiver configuration check requires both native renders and the observed configuration")
	}
	current, currentDeclared, err := s.own.rendered(before)
	if err != nil {
		return err
	}
	next, nextDeclared, err := s.own.rendered(after)
	if err != nil {
		return err
	}
	where := s.own.namespace + "/" + s.own.name
	switch {
	case nextDeclared && !sameJSON(next, s.own.content):
		return fmt.Errorf("this update changes the installation's own update configuration (ConfigMap %s), which Cluster > Updates checks every step against, so it could never be verified; cancel this preparation, deploy that change by hand by syncing the Application that renders the ConfigMap to a revision that declares it, then prepare the update again", where)
	case currentDeclared && !nextDeclared:
		return fmt.Errorf("this update stops declaring the installation's own update configuration (ConfigMap %s), which would leave it pruned or out of sync, so the update could never be verified; cancel this preparation, deploy that change by hand, then prepare the update again", where)
	case currentDeclared && !sameJSON(current, s.own.content):
		return fmt.Errorf("the installation's own update configuration (ConfigMap %s) differs from what the current release declares, so restoring that release would change it; cancel this preparation, then by hand either sync the Application that renders it to a revision that declares the live content or restore the declared content, and prepare the update again", where)
	}
	return nil
}

// rendered finds this ConfigMap in a render. A namespaced object without a
// namespace is applied in the render's destination namespace.
func (o receiverOwnConfiguration) rendered(inventory argocd.RenderInventory) (map[string]any, bool, error) {
	var found map[string]any
	for _, body := range inventory.Resources() {
		value, err := resourceJSON(body)
		if err != nil {
			return nil, false, err
		}
		meta := resourceMap(value, "metadata")
		namespace := resourceText(meta, "namespace")
		if namespace == "" {
			namespace = inventory.Namespace()
		}
		if resourceText(value, "apiVersion") != "v1" || resourceText(value, "kind") != "ConfigMap" || resourceText(meta, "name") != o.name || namespace != o.namespace {
			continue
		}
		if found != nil {
			return nil, false, errors.New("installation render declares the receiver configuration more than once")
		}
		found = receiverConfigMapContent(value)
	}
	return found, found != nil, nil
}
