package installation

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

func workloadCount(value map[string]any, field string, absent int64) int64 {
	if _, present := value[field]; !present {
		return absent
	}
	return receiverInteger(value[field])
}

// Server defaults may add fields, but cannot change an authored value or add
// entries to an authored array. This is a declared-field comparison, not a
// general equivalence test for every admission webhook's generated fields.
func workloadDeclaredMatches(want, actual any) bool {
	switch w := want.(type) {
	case map[string]any:
		a, ok := actual.(map[string]any)
		if !ok {
			return false
		}
		for key, value := range w {
			if !workloadDeclaredMatches(value, a[key]) {
				return false
			}
		}
		return true
	case []any:
		a, ok := actual.([]any)
		if !ok || len(a) != len(w) {
			return false
		}
		for n, value := range w {
			if !workloadDeclaredMatches(value, a[n]) {
				return false
			}
		}
		return true
	default:
		return sameJSON(want, actual)
	}
}

func workloadPodSpec(spec map[string]any, kind string) map[string]any {
	if kind == "Pod" {
		return spec
	}
	if kind == "CronJob" {
		spec = resourceMap(resourceMap(spec, "jobTemplate"), "spec")
	}
	return resourceMap(resourceMap(spec, "template"), "spec")
}

// Server defaults may fill fields inside a declared env/valueFrom entry, but
// may not introduce a command or environment the authored template omitted.
func workloadExecutableMatches(want, actual map[string]any) bool {
	for _, field := range []string{"containers", "initContainers", "ephemeralContainers"} {
		ws, _ := want[field].([]any)
		as, _ := actual[field].([]any)
		if len(ws) != len(as) {
			return false
		}
		for n, raw := range ws {
			w, wok := raw.(map[string]any)
			a, aok := as[n].(map[string]any)
			if !wok || !aok {
				return false
			}
			for _, key := range []string{"command", "args", "env", "envFrom", "workingDir"} {
				if w[key] == nil {
					if key == "workingDir" {
						if a[key] != nil && a[key] != "" {
							return false
						}
					} else if values, ok := a[key].([]any); a[key] != nil && (!ok || len(values) != 0) {
						return false
					}
				} else if !workloadDeclaredMatches(w[key], a[key]) {
					return false
				}
			}
		}
	}
	return true
}

func workloadCondition(status map[string]any, name string) bool {
	conditions, ok := status["conditions"].([]any)
	if !ok {
		return false
	}
	found := false
	for _, raw := range conditions {
		condition, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		if resourceText(condition, "type") != name {
			continue
		}
		if found || resourceText(condition, "status") != "True" {
			return false
		}
		found = true
	}
	return found
}

func workloadOwner(object map[string]any, version, kind, name, uid string) bool {
	refs, ok := resourceMap(object, "metadata")["ownerReferences"].([]any)
	if !ok || len(refs) != 1 {
		return false
	}
	ref, ok := refs[0].(map[string]any)
	return ok && ref["controller"] == true && resourceText(ref, "apiVersion") == version &&
		resourceText(ref, "kind") == kind && resourceText(ref, "name") == name && resourceText(ref, "uid") == uid
}

func workloadMentionsOwner(object map[string]any, kind, name, uid string) bool {
	refs, _ := resourceMap(object, "metadata")["ownerReferences"].([]any)
	for _, raw := range refs {
		ref, _ := raw.(map[string]any)
		if resourceText(ref, "uid") == uid || (resourceText(ref, "kind") == kind && resourceText(ref, "name") == name) {
			return true
		}
	}
	return false
}

// Kubernetes label-selector semantics identify orphaned or foreign objects
// which could otherwise disappear merely by dropping an owner reference.
// Ownership is still required; matching a selector never grants ownership.
func workloadSelector(spec map[string]any, kind, name string) (func(map[string]any) bool, error) {
	selector := resourceMap(spec, "selector")
	if kind == "Cluster" {
		selector = map[string]any{"matchLabels": map[string]any{"cnpg.io/cluster": name}}
	}
	labels, lok := selector["matchLabels"].(map[string]any)
	expressions, eok := selector["matchExpressions"].([]any)
	if (selector["matchLabels"] != nil && !lok) || (selector["matchExpressions"] != nil && !eok) || len(labels)+len(expressions) == 0 {
		return nil, errors.New("workload selector is absent or malformed")
	}
	for field := range selector {
		if field != "matchLabels" && field != "matchExpressions" {
			return nil, errors.New("workload selector has an unsupported field")
		}
	}
	tests := []func(map[string]any) bool{}
	for key, raw := range labels {
		value, ok := raw.(string)
		if key == "" || !ok {
			return nil, errors.New("workload selector label is malformed")
		}
		tests = append(tests, func(actual map[string]any) bool { return actual[key] == value })
	}
	for _, raw := range expressions {
		expression, ok := raw.(map[string]any)
		key, op := resourceText(expression, "key"), resourceText(expression, "operator")
		values, vok := expression["values"].([]any)
		if !ok || key == "" || (expression["values"] != nil && !vok) {
			return nil, errors.New("workload selector expression is malformed")
		}
		set := map[string]bool{}
		for _, raw := range values {
			value, ok := raw.(string)
			if !ok {
				return nil, errors.New("workload selector value is malformed")
			}
			set[value] = true
		}
		switch op {
		case "In", "NotIn":
			if len(values) == 0 {
				return nil, errors.New("workload selector has no values")
			}
		case "Exists", "DoesNotExist":
			if len(values) != 0 {
				return nil, errors.New("workload selector has unexpected values")
			}
		default:
			return nil, errors.New("workload selector operator is unsupported")
		}
		tests = append(tests, func(actual map[string]any) bool {
			value, exists := actual[key].(string)
			switch op {
			case "In":
				return exists && set[value]
			case "NotIn":
				return !exists || !set[value]
			case "Exists":
				return exists
			default:
				return !exists
			}
		})
	}
	return func(object map[string]any) bool {
		actual := resourceMap(resourceMap(object, "metadata"), "labels")
		for _, test := range tests {
			if !test(actual) {
				return false
			}
		}
		return true
	}, nil
}

// Lists cover the entire bounded namespace, not labels which a changed Pod
// could drop to disappear from the proof. Pagination and duplicate identities
// refuse; collection identity is reobserved before accepting the result.
func workloadList(ctx context.Context, reads *receiverReads, version, kind, plural, namespace string) ([]map[string]any, error) {
	base, _, err := apiVersionPath(version)
	if err != nil || !receiverDNS.MatchString(namespace) {
		return nil, errors.New("workload namespace is invalid")
	}
	list, err := reads.get(ctx, base+"/namespaces/"+namespace+"/"+plural+"?limit=256")
	if err != nil {
		return nil, err
	}
	items, ok := list["items"].([]any)
	if !ok || len(items) > 256 || resourceText(list, "apiVersion") != version || resourceText(list, "kind") != kind+"List" || resourceText(resourceMap(list, "metadata"), "continue") != "" {
		return nil, errors.New("workload collection is incomplete or malformed")
	}
	out := make([]map[string]any, 0, len(items))
	seen := map[string]bool{}
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		meta := resourceMap(item, "metadata")
		name, uid, rv := resourceText(meta, "name"), resourceText(meta, "uid"), resourceText(meta, "resourceVersion")
		if !ok || !kubernetesSegment.MatchString(name) || uid == "" || rv == "" || resourceText(meta, "namespace") != namespace || seen[name] {
			return nil, errors.New("workload collection identity is absent or repeated")
		}
		for field, expected := range map[string]string{"apiVersion": version, "kind": kind} {
			if value, present := item[field]; present && value != expected {
				return nil, errors.New("workload collection types disagree")
			}
		}
		seen[name] = true
		out = append(out, item)
	}
	return out, nil
}

func (s *workloadAdmission) observeWorkload(ctx context.Context, reads *receiverReads, object resourceObject) error {
	live, err := reads.get(ctx, object.apiPath)
	if err != nil {
		return err
	}
	if resourceText(live, "apiVersion") != object.version || resourceText(live, "kind") != object.identity.Kind || validLiveStorage(live, object.identity) != nil {
		return errors.New("workload identity is absent, changed or deleting")
	}
	want, spec, status := resourceMap(object.value, "spec"), resourceMap(live, "spec"), resourceMap(live, "status")
	if want == nil || spec == nil || !workloadDeclaredMatches(want, spec) {
		return errors.New("workload no longer matches its authored configuration")
	}
	if object.identity.Kind != "Cluster" && !workloadExecutableMatches(workloadPodSpec(want, object.identity.Kind), workloadPodSpec(spec, object.identity.Kind)) {
		return errors.New("workload command or environment differs from its authored template")
	}
	// Re-run the native image inventory against the live controller. This also
	// catches extra containers or newly introduced image fields absent in the
	// manifest; equality of the declared subset alone cannot do that.
	actual := object
	actual.value = live
	wantedImages, err := inventoryImages(map[resourceIdentity]resourceObject{object.identity: object})
	if err != nil {
		return err
	}
	liveImages, err := inventoryImages(map[resourceIdentity]resourceObject{object.identity: actual})
	if err != nil || !sameWorkloadImages(wantedImages, liveImages) {
		return errors.New("workload executable image inventory changed")
	}
	meta := resourceMap(live, "metadata")
	generation := receiverInteger(meta["generation"])
	var count int64
	podSpec := resourceMap(resourceMap(spec, "template"), "spec")
	completed := false
	switch object.identity.Group + "/" + object.identity.Kind {
	case "apps/Deployment", "apps/StatefulSet", "apps/ReplicaSet":
		// Cardinality belongs to the admitted render, including omitted API
		// defaults. A manual scale-down cannot redefine readiness as zero.
		count = workloadCount(want, "replicas", 1)
		if workloadCount(spec, "replicas", 1) != count {
			return errors.New("workload replica target differs from its admitted render")
		}
		if count < 0 || count > 256 || generation < 1 || receiverInteger(status["observedGeneration"]) != generation {
			return errors.New("workload controller has not observed its desired generation")
		}
		fields := []string{"replicas", "readyReplicas", "availableReplicas"}
		if object.identity.Kind == "StatefulSet" {
			fields = []string{"replicas", "readyReplicas", "currentReplicas", "updatedReplicas"}
		}
		if object.identity.Kind == "Deployment" {
			fields = append(fields, "updatedReplicas")
		}
		for _, field := range fields {
			if workloadCount(status, field, 0) != count {
				return errors.New("workload replicas are not fully converged")
			}
		}
		if workloadCount(status, "unavailableReplicas", 0) != 0 || workloadCount(status, "terminatingReplicas", 0) != 0 {
			return errors.New("workload retains unavailable or terminating replicas")
		}
		if object.identity.Kind == "StatefulSet" && count > 0 && (resourceText(status, "currentRevision") == "" || resourceText(status, "currentRevision") != resourceText(status, "updateRevision")) {
			return errors.New("stateful workload revisions differ")
		}
	case "apps/DaemonSet":
		count = workloadCount(status, "desiredNumberScheduled", -1)
		if count < 1 || count > 256 || generation < 1 || receiverInteger(status["observedGeneration"]) != generation {
			return errors.New("daemon workload has no observed placement")
		}
		for _, field := range []string{"currentNumberScheduled", "updatedNumberScheduled", "numberReady", "numberAvailable"} {
			if workloadCount(status, field, 0) != count {
				return errors.New("daemon workload is not fully converged")
			}
		}
		if workloadCount(status, "numberUnavailable", 0) != 0 || workloadCount(status, "numberMisscheduled", 0) != 0 {
			return errors.New("daemon workload has unavailable or misplaced pods")
		}
	case "batch/Job":
		count = workloadCount(want, "completions", 1)
		if workloadCount(spec, "completions", 1) != count {
			return errors.New("job completion target differs from its admitted render")
		}
		if count < 1 || count > 256 || workloadCount(status, "succeeded", 0) != count || workloadCount(status, "active", 0) != 0 || !workloadCondition(status, "Complete") || workloadCondition(status, "Failed") {
			return errors.New("installation job has not completed")
		}
		completed = true
	case "batch/CronJob":
		// A scheduled definition is armed, not a running server. Only a fully
		// quiescent CronJob can contribute this observation. The next invocation
		// and its application-level result require their own execution receipt.
		active, ok := status["active"].([]any)
		if (status["active"] != nil && !ok) || len(active) != 0 {
			return errors.New("scheduled workload still has active jobs")
		}
		jobs, err := workloadList(ctx, reads, "batch/v1", "Job", "jobs", object.identity.Namespace)
		if err != nil {
			return err
		}
		for _, job := range jobs {
			if !workloadMentionsOwner(job, "CronJob", object.identity.Name, resourceText(meta, "uid")) {
				continue
			}
			if !workloadOwner(job, "batch/v1", "CronJob", object.identity.Name, resourceText(meta, "uid")) {
				return errors.New("scheduled job ownership differs")
			}
			js := resourceMap(job, "status")
			if workloadCount(js, "active", 0) != 0 || (!workloadCondition(js, "Complete") && !workloadCondition(js, "Failed")) {
				return errors.New("scheduled workload has an unfinished owned job")
			}
		}
		return nil
	case "/Pod":
		return s.observeWorkloadPod(ctx, reads, live, spec, false, false)
	case "postgresql.cnpg.io/Cluster":
		count, err = databaseInstances(want)
		if err != nil || workloadCount(status, "readyInstances", 0) != count || !workloadCondition(status, "Ready") || resourceText(status, "currentPrimary") == "" || resourceText(status, "currentPrimary") != resourceText(status, "targetPrimary") {
			return errors.New("database instances are not ready on the requested primary")
		}
		// The CNPG operator generates its Pod template. The declared operand
		// image is the postgres container; auxiliary operator bootstrap code is
		// infrastructure and is not represented as a release workload image.
		podSpec = map[string]any{"containers": []any{map[string]any{"name": "postgres", "image": spec["imageName"]}}}
	default:
		return errors.New("workload kind requires a qualified readiness codec")
	}
	return s.observeOwnedWorkloadPods(ctx, reads, object, live, podSpec, count, completed)
}

func sameWorkloadImages(a, b map[imageSlot]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}

func (s *workloadAdmission) observeOwnedWorkloadPods(ctx context.Context, reads *receiverReads, object resourceObject, live, podSpec map[string]any, count int64, completed bool) error {
	selected, err := workloadSelector(resourceMap(live, "spec"), object.identity.Kind, object.identity.Name)
	if err != nil {
		return err
	}
	meta := resourceMap(live, "metadata")
	uid := resourceText(meta, "uid")
	owners := map[string]string{object.identity.Name: uid}
	ownerKind, ownerVersion := object.identity.Kind, object.version
	if ownerKind == "Deployment" {
		sets, err := workloadList(ctx, reads, "apps/v1", "ReplicaSet", "replicasets", object.identity.Namespace)
		if err != nil {
			return err
		}
		owners = map[string]string{}
		for _, set := range sets {
			if !workloadMentionsOwner(set, "Deployment", object.identity.Name, uid) && !selected(set) {
				continue
			}
			if !workloadOwner(set, "apps/v1", "Deployment", object.identity.Name, uid) {
				return errors.New("workload ReplicaSet ownership differs")
			}
			m := resourceMap(set, "metadata")
			owners[resourceText(m, "name")] = resourceText(m, "uid")
		}
		ownerKind, ownerVersion = "ReplicaSet", "apps/v1"
	}
	pods, err := workloadList(ctx, reads, "v1", "Pod", "pods", object.identity.Namespace)
	if err != nil {
		return err
	}
	var observed int64
	primaryFound := false
	for _, pod := range pods {
		owned := false
		for name, ownerUID := range owners {
			if !workloadMentionsOwner(pod, ownerKind, name, ownerUID) {
				continue
			}
			if !workloadOwner(pod, ownerVersion, ownerKind, name, ownerUID) {
				return errors.New("workload Pod ownership differs")
			}
			owned = true
		}
		if !owned {
			if selected(pod) {
				return errors.New("workload selector includes a Pod without its required controller")
			}
			continue
		}
		podMeta := resourceMap(pod, "metadata")
		if podMeta["deletionTimestamp"] != nil {
			return errors.New("workload still has a terminating Pod")
		}
		// A completed retryable Job can retain failed historical attempts. They
		// are not running replicas and cannot substitute for successful Pods.
		if completed && resourceText(resourceMap(pod, "status"), "phase") == "Failed" {
			continue
		}
		cnpg := object.identity.Kind == "Cluster"
		if err := s.observeWorkloadPod(ctx, reads, pod, podSpec, completed, cnpg); err != nil {
			return err
		}
		if resourceText(podMeta, "name") == resourceText(resourceMap(live, "status"), "currentPrimary") {
			primaryFound = true
		}
		observed++
	}
	if observed != count {
		return fmt.Errorf("workload ready Pod count differs: got %d, require %d", observed, count)
	}
	if object.identity.Kind == "Cluster" && !primaryFound {
		return errors.New("database primary is not among its ready owned Pods")
	}
	return nil
}

func (s *workloadAdmission) observeWorkloadPod(ctx context.Context, reads *receiverReads, pod, expected map[string]any, completed, cnpg bool) error {
	spec, status := resourceMap(pod, "spec"), resourceMap(pod, "status")
	if err := s.observeWorkloadNode(ctx, reads, resourceText(spec, "nodeName")); err != nil {
		return err
	}
	// Kubernetes 1.32 does not expose a resolved digest for an image volume.
	// Registry availability alone cannot prove the bytes actually mounted.
	if containsImageField(spec["volumes"]) {
		return errors.New("image volume readiness requires a qualified runtime digest codec")
	}
	if resourceMap(pod, "metadata")["deletionTimestamp"] != nil {
		return errors.New("workload Pod is deleting")
	}
	if completed {
		if resourceText(status, "phase") != "Succeeded" {
			return errors.New("job Pod did not succeed")
		}
	} else if resourceText(status, "phase") != "Running" || !workloadCondition(status, "Ready") {
		return errors.New("workload Pod is not ready")
	}
	for _, pair := range [][2]string{{"containers", "containerStatuses"}, {"initContainers", "initContainerStatuses"}, {"ephemeralContainers", "ephemeralContainerStatuses"}} {
		want, wok := expected[pair[0]].([]any)
		actual, aok := spec[pair[0]].([]any)
		states, sok := status[pair[1]].([]any)
		if (expected[pair[0]] != nil && !wok) || (spec[pair[0]] != nil && !aok) || (status[pair[1]] != nil && !sok) {
			return errors.New("workload Pod container inventory is malformed")
		}
		if cnpg && pair[0] == "initContainers" {
			// CNPG's one bootstrap container belongs to the separately installed
			// operator. Verify its completion, without claiming its image was in
			// the product release or reconstructed from a caller-supplied digest.
			if len(actual) != 1 || len(states) != 1 {
				return errors.New("database bootstrap profile is unsupported")
			}
			a, _ := actual[0].(map[string]any)
			st, _ := states[0].(map[string]any)
			if resourceText(a, "name") != "bootstrap-controller" || resourceText(st, "name") != "bootstrap-controller" || !workloadContainerSucceeded(st) {
				return errors.New("database bootstrap did not complete")
			}
			continue
		}
		if len(want) != len(actual) || len(want) != len(states) {
			return errors.New("workload Pod container inventory differs")
		}
		seen := map[string]bool{}
		for n, raw := range want {
			w, ok := raw.(map[string]any)
			a, aok := actual[n].(map[string]any)
			name := resourceText(w, "name")
			if !ok || !aok || name == "" || seen[name] || resourceText(a, "name") != name || resourceText(a, "image") != resourceText(w, "image") {
				return errors.New("workload Pod container identity or image differs")
			}
			seen[name] = true
			if !cnpg {
				for _, field := range []string{"command", "args", "env", "envFrom", "workingDir"} {
					if !sameJSON(w[field], a[field]) {
						return errors.New("workload Pod command or environment differs from its controller")
					}
				}
			}
			var state map[string]any
			for _, rawState := range states {
				candidate, _ := rawState.(map[string]any)
				if resourceText(candidate, "name") == name {
					if state != nil {
						return errors.New("workload Pod repeats a container status")
					}
					state = candidate
				}
			}
			runtime := strings.TrimPrefix(resourceText(state, "imageID"), "docker-pullable://")
			root := resourceText(w, "image")
			manifest := s.runtimeImages[root]
			// containerd may report the root index; other runtimes report the
			// selected manifest. Native OCI verification established exactly one
			// matching platform, and the Pod's actual Node was checked above.
			// A config ID, arbitrary digest or unknown root remains insufficient.
			if state == nil || manifest == "" || (runtime != manifest && runtime != root) {
				return errors.New("workload runtime image differs from its verified image and platform")
			}
			if completed || pair[0] == "initContainers" {
				if resourceText(w, "restartPolicy") == "Always" && !completed {
					if state["ready"] != true || resourceMap(resourceMap(state, "state"), "running") == nil {
						return errors.New("workload restartable init container is not ready")
					}
				} else if !workloadContainerSucceeded(state) {
					return errors.New("workload container has not completed successfully")
				}
			} else if state["ready"] != true || resourceMap(resourceMap(state, "state"), "running") == nil {
				return errors.New("workload container is not running and ready")
			}
		}
	}
	return nil
}

func (s *workloadAdmission) observeWorkloadNode(ctx context.Context, reads *receiverReads, name string) error {
	if !kubernetesSegment.MatchString(name) || len(name) > 253 {
		return errors.New("workload Pod has no valid assigned Node")
	}
	node, err := reads.get(ctx, "api/v1/nodes/"+name)
	if err != nil {
		return err
	}
	if resourceText(node, "apiVersion") != "v1" || resourceText(node, "kind") != "Node" || validLiveStorage(node, resourceIdentity{"", "Node", "", name}) != nil {
		return errors.New("workload Node identity is absent, changed or deleting")
	}
	status := resourceMap(node, "status")
	info := resourceMap(status, "nodeInfo")
	platform := resourceText(info, "operatingSystem") + "/" + resourceText(info, "architecture")
	if (platform != "linux/arm64" && platform != "linux/amd64") || platform != s.platform || !workloadCondition(status, "Ready") {
		return errors.New("workload Node is not ready on the verified platform")
	}
	return nil
}

func workloadContainerSucceeded(state map[string]any) bool {
	terminated := resourceMap(resourceMap(state, "state"), "terminated")
	return terminated != nil && receiverInteger(terminated["exitCode"]) == 0
}
