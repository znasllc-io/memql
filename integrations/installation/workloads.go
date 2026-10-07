package installation

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/znasllc-io/memql/integrations/argocd"
)

// A workload observation checks controller convergence and the running image
// identities. It is not evidence of database migration compatibility, preserved
// authenticated sessions or application-level behavior. Completion needs those
// independent checks as well as the exact owned Argo operation.
type workloadRequirement struct {
	Key, Kind string
}

type workloadAdmission struct {
	api                               argocd.API
	digest, render, artifacts         string
	configuration, platform, operator string
	created, expires                  time.Time
	requirements                      []workloadRequirement
	objects                           map[string]resourceObject
	runtimeImages                     map[string]string
}

type workloadObservation struct {
	scope, key, reads  string
	started, completed time.Time
}

type workloadEvidence struct {
	digest, scope, render, artifacts, configuration, platform string
	workflow, operator                                        string
	observed, expires                                         time.Time
}

func (*workloadAdmission) String() string    { return "<installation workload scope>" }
func (*workloadAdmission) GoString() string  { return "<installation workload scope>" }
func (workloadObservation) String() string   { return "<installation workload observation>" }
func (workloadObservation) GoString() string { return "<installation workload observation>" }
func (workloadEvidence) String() string      { return "<installation workload evidence>" }
func (workloadEvidence) GoString() string    { return "<installation workload evidence>" }

// require binds the opaque result at its point of use. The completion host
// must still require the owned Argo intent and its other independent proofs.
func (e workloadEvidence) require(rendered argocd.RenderedRevision, artifacts artifactEvidence, workflow, operator string) error {
	now := time.Now().UTC()
	if !internalDigest.MatchString(e.digest) || !internalDigest.MatchString(e.scope) ||
		!internalDigest.MatchString(workflow) || !identifier.MatchString(operator) ||
		e.render != rendered.Digest() || e.artifacts != artifacts.digest ||
		e.configuration != artifacts.configuration || e.platform != artifacts.platform ||
		e.workflow != workflow || e.operator != operator || artifacts.operator != operator ||
		!freshPreservationObservation(e.observed, now) || !e.expires.After(now) || !artifacts.expires.After(now) {
		return errors.New("workload evidence is stale or differs from its native completion scope")
	}
	return nil
}

func newWorkloadAdmission(ctx context.Context, api argocd.API, rendered argocd.RenderedRevision, artifacts artifactEvidence) (*workloadAdmission, error) {
	now := time.Now().UTC()
	if api == nil || rendered.Digest() == "" ||
		(rendered.Digest() != artifacts.before && rendered.Digest() != artifacts.after) ||
		!internalDigest.MatchString(artifacts.digest) || !internalDigest.MatchString(artifacts.configuration) ||
		!internalDigest.MatchString(artifacts.workflow) || !identifier.MatchString(artifacts.operator) ||
		artifacts.observed.IsZero() || artifacts.observed.After(now) || !artifacts.expires.After(now) || len(artifacts.runtimeImages) == 0 {
		return nil, errors.New("workload admission requires a verified render and fresh native artifact evidence")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	inventory, err := resourceInventory(ctx, api, rendered, map[string]map[string]resourceType{})
	if err != nil {
		return nil, err
	}
	images, err := inventoryImages(inventory)
	if err != nil {
		return nil, err
	}
	s := &workloadAdmission{
		api: api, render: rendered.Digest(), artifacts: artifacts.digest,
		configuration: artifacts.configuration, platform: artifacts.platform, operator: artifacts.operator,
		created: now, expires: now.Add(5 * time.Minute),
		objects: map[string]resourceObject{}, runtimeImages: map[string]string{},
	}
	if artifacts.expires.Before(s.expires) {
		s.expires = artifacts.expires
	}
	for slot, image := range images {
		runtime, present := artifacts.runtimeImages[image]
		if _, err := immutableImage(runtime); !present || err != nil {
			return nil, errors.New("rendered workload image has no verified platform manifest")
		}
		s.runtimeImages[image] = runtime
		object := inventory[slot.Resource]
		key := artifactHash("workload-requirement", []any{s.render, s.artifacts, slot.Resource})
		s.objects[key] = object
	}
	if len(s.objects) == 0 || len(s.objects) > 256 {
		return nil, errors.New("workload inventory is empty or exceeds its bound")
	}
	for key, object := range s.objects {
		s.requirements = append(s.requirements, workloadRequirement{key, object.identity.Group + "/" + object.identity.Kind})
	}
	sort.Slice(s.requirements, func(i, j int) bool { return s.requirements[i].Key < s.requirements[j].Key })
	s.digest = artifactHash("workload-scope", []any{s.render, s.artifacts, s.configuration, s.platform, s.operator, s.requirements})
	if ctx.Err() != nil || !s.expires.After(time.Now()) {
		return nil, errors.New("workload admission expired while resolving its inventory")
	}
	return s, nil
}

func (s *workloadAdmission) check(ctx context.Context, key string) (workloadObservation, error) {
	started := time.Now().UTC()
	if s == nil || !s.expires.After(started) {
		return workloadObservation{}, errors.New("workload scope is absent or expired")
	}
	object, found := s.objects[key]
	if !found {
		return workloadObservation{}, errors.New("workload is outside this native scope")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	reads := newReceiverReads(s.api)
	if err := s.observeWorkload(ctx, reads, object); err != nil {
		return workloadObservation{}, err
	}
	digest, err := reads.finish(ctx)
	completed := time.Now().UTC()
	if err != nil || ctx.Err() != nil || !s.expires.After(completed) || !freshPreservationObservation(started, completed) {
		return workloadObservation{}, errors.New("workload changed or observation expired during verification")
	}
	return workloadObservation{s.digest, key, digest, started, completed}, nil
}

func (s *workloadAdmission) seal(observations []workloadObservation) (workloadEvidence, error) {
	now := time.Now().UTC()
	if s == nil || !s.expires.After(now) || len(observations) != len(s.requirements) {
		return workloadEvidence{}, errors.New("workload evidence is incomplete or expired")
	}
	seen := map[string]bool{}
	expires := s.expires
	for _, observation := range observations {
		_, exists := s.objects[observation.key]
		if !exists || seen[observation.key] || observation.scope != s.digest ||
			!internalDigest.MatchString(observation.reads) || observation.started.Before(s.created) ||
			observation.completed.Before(observation.started) || observation.completed.After(now) ||
			!freshPreservationObservation(observation.started, now) {
			return workloadEvidence{}, errors.New("workload observation is missing, repeated, substituted or expired")
		}
		seen[observation.key] = true
		if end := observation.started.Add(time.Minute); end.Before(expires) {
			expires = end
		}
	}
	// The opaque fields remain private; persist only the resulting binding. A
	// recovered host must obtain fresh native observations, never decode proofs.
	sort.Slice(observations, func(i, j int) bool { return observations[i].key < observations[j].key })
	bindings := make([]any, 0, len(observations))
	for _, observation := range observations {
		bindings = append(bindings, []any{observation.key, observation.reads, observation.started, observation.completed})
	}
	return workloadEvidence{
		digest: artifactHash("workload-evidence", []any{s.digest, bindings}), scope: s.digest,
		render: s.render, artifacts: s.artifacts, configuration: s.configuration, platform: s.platform,
		operator: s.operator, observed: now, expires: expires,
	}, nil
}
