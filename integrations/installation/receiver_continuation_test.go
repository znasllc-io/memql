package installation

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/integrations/argocd"
)

// A Kubernetes protocol response after the admitted request. The revision host
// tests separately exercise the actual native PATCH; these tests challenge the
// continuation verifier with owned, foreign and concurrently changed reads.
func receiverAppliedIntent(t *testing.T, receiver *receiverFixture, intent argocd.Intent, phase string) {
	t.Helper()
	digest, err := intent.Digest()
	require.NoError(t, err)
	var spec map[string]any
	require.NoError(t, json.Unmarshal(intent.BeforeSpec, &spec))
	source := resourceMap(spec, "source")
	source["targetRevision"] = intent.Revision
	sync := map[string]any{"revision": intent.Revision, "prune": intent.Prune}
	if options := resourceMap(spec, "syncPolicy")["syncOptions"]; options != nil {
		sync["syncOptions"] = options
	}
	operation := map[string]any{"initiatedBy": map[string]any{"username": "memql-cluster-update", "automated": false}, "info": []any{map[string]any{"name": "memql.io/update-intent", "value": digest}}, "sync": sync}
	path, err := receiverPath("argoproj.io/v1alpha1", "applications", intent.Target.Namespace, intent.Target.Name)
	require.NoError(t, err)
	app := receiver.objects[path]
	meta := resourceMap(app, "metadata")
	meta["resourceVersion"], meta["generation"] = "22", intent.BeforeGeneration+1
	meta["annotations"] = map[string]any{"memql.io/update-intent": digest}
	app["spec"], app["operation"] = spec, nil
	app["status"] = map[string]any{"health": map[string]any{"status": "Degraded"},
		"sync":           map[string]any{"status": "OutOfSync", "revision": intent.Revision, "comparedTo": map[string]any{"source": source}},
		"operationState": map[string]any{"phase": phase, "operation": operation, "syncResult": map[string]any{"revision": intent.Revision, "source": source}}}
}
