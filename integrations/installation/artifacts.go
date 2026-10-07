package installation

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/argocd"
	"github.com/znasllc-io/memql/integrations/ociregistry"
)

// These are native receiving-configuration inputs, never DSL arguments. The
// host must authenticate and bind the COMPLETE configuration (including trust,
// credential versions and registry transport) before constructing this scope.
// A digest here names that snapshot; this operator cannot authenticate it.
type artifactAdmissionConfig struct {
	ConfigurationDigest string
	Platform            string
	Bindings            []imageBinding
	Dependencies        []pipelines.ReleaseDependency
	Registries          []ociregistry.Target
}

type artifactRequirement struct {
	Key, Image, Platform string
}

type artifactUse struct {
	Role, Image string
	Slot        imageSlot
}

type artifactRegistry struct {
	origin   string
	verifier *ociregistry.AvailabilityVerifier
}

// artifactAdmission only observes immutable inputs. Its caller exposes the
// owned requirements and one check operation to a sealed DSL recipe; the DSL
// chooses ordering/parallelism. No hidden loop performs registry effects.
type artifactAdmission struct {
	digest, configuration, candidate, rollback, resources, before, after, platform string
	created                                                                        time.Time
	requirements                                                                   []artifactRequirement
	registries                                                                     map[string]artifactRegistry
}

type artifactObservation struct {
	scope, key         string
	started, completed time.Time
	image              *ociregistry.AvailableImage
}

type artifactEvidence struct {
	digest, scope, configuration, candidate, rollback, resources, before, after, platform string
	workflow, operator                                                                    string
	observed, expires                                                                     time.Time
}

func (*artifactAdmission) String() string    { return "<installation artifact scope>" }
func (*artifactAdmission) GoString() string  { return "<installation artifact scope>" }
func (artifactObservation) String() string   { return "<installation artifact observation>" }
func (artifactObservation) GoString() string { return "<installation artifact observation>" }
func (artifactEvidence) String() string      { return "<installation artifact evidence>" }
func (artifactEvidence) GoString() string    { return "<installation artifact evidence>" }

func newArtifactAdmission(ctx context.Context, api argocd.API, config artifactAdmissionConfig, rollback, candidate pipelines.VerifiedPublishedRelease, before, after argocd.RenderedRevision) (*artifactAdmission, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if !internalDigest.MatchString(config.ConfigurationDigest) || len(config.Registries) == 0 || len(config.Registries) > 512 {
		return nil, errors.New("artifact admission requires a bounded native configuration")
	}
	if err := pipelines.CheckPublishedReleaseOverlap(rollback, candidate, config.Dependencies); err != nil {
		return nil, err
	}
	resources, err := verifyImagesAndDiff(ctx, api, candidate, before, after, config.Platform, config.Bindings)
	if err != nil {
		return nil, err
	}
	s := &artifactAdmission{configuration: config.ConfigurationDigest, candidate: candidate.Digest(), rollback: rollback.Digest(), resources: resources.digest, before: before.Digest(), after: after.Digest(), platform: config.Platform, created: time.Now().UTC(), registries: map[string]artifactRegistry{}}
	for _, target := range config.Registries {
		verifier, err := ociregistry.NewAvailabilityVerifier(target) // owns/clones mutable trust configuration
		if err != nil {
			return nil, errors.New("invalid installation registry configuration")
		}
		u, _ := url.Parse(target.Origin) // validated by the native registry constructor
		repo := u.Host + "/" + target.Repository
		if _, duplicate := s.registries[repo]; duplicate {
			return nil, errors.New("ambiguous installation registry configuration")
		}
		s.registries[repo] = artifactRegistry{origin: strings.TrimSuffix(target.Origin, "/"), verifier: verifier}
	}
	oldRelease, _ := rollback.Release()
	newRelease, _ := candidate.Release()
	uses := []artifactUse{}
	images := map[string]bool{}
	schemas := map[string]map[string]resourceType{}
	for i, render := range []argocd.RenderedRevision{before, after} {
		inventory, err := resourceInventory(ctx, api, render, schemas)
		if err != nil {
			return nil, err
		}
		refs, err := inventoryImages(inventory)
		if err != nil || len(refs) > 1024 {
			return nil, errors.New("installation image inventory is invalid or exceeds its bound")
		}
		role, release := "rollback", oldRelease
		if i == 1 {
			role, release = "candidate", newRelease
		}
		for _, binding := range config.Bindings {
			ref, found := refs[binding.Slot]
			if !found || !publishedImageMatches(release, binding, config.Platform, ref) {
				return nil, errors.New("installation image differs from its candidate or rollback signed artifact")
			}
			digest, _ := name.NewDigest(ref, name.StrictValidation)
			registry := s.registries[digest.Context().Name()]
			if !publishedOriginMatches(release, binding, config.Platform, ref, registry.origin) {
				return nil, errors.New("signed artifact location differs from configured registry transport")
			}
		}
		for slot, ref := range refs {
			digest, _ := name.NewDigest(ref, name.StrictValidation)
			if s.registries[digest.Context().Name()].verifier == nil {
				return nil, errors.New("rendered image has no explicit receiving registry configuration")
			}
			images[ref] = true
			uses = append(uses, artifactUse{role, ref, slot})
		}
	}
	if len(uses) > 2048 || len(images) == 0 || len(images) > 1024 {
		return nil, errors.New("installation image inventory exceeds its bound")
	}
	sortJSON(uses)
	bindings := append([]imageBinding(nil), config.Bindings...)
	dependencies := append([]pipelines.ReleaseDependency(nil), config.Dependencies...)
	sortJSON(bindings)
	sortJSON(dependencies)
	// ConfigurationDigest binds trust/credential material without persisting it.
	// Public routing is also explicit so origin/protocol changes cannot alias.
	routes := map[string]string{}
	for repo, registry := range s.registries {
		routes[repo] = registry.origin
	}
	s.digest = artifactHash("scope", struct {
		Configuration, Candidate, Rollback, Resources, Before, After, Platform string
		Bindings                                                               []imageBinding
		Dependencies                                                           []pipelines.ReleaseDependency
		Uses                                                                   []artifactUse
		Routes                                                                 map[string]string
	}{s.configuration, s.candidate, s.rollback, s.resources, s.before, s.after, s.platform, bindings, dependencies, uses, routes})
	for ref := range images {
		s.requirements = append(s.requirements, artifactRequirement{artifactHash("requirement", []string{s.digest, ref}), ref, s.platform})
	}
	sort.Slice(s.requirements, func(i, j int) bool { return s.requirements[i].Key < s.requirements[j].Key })
	return s, nil
}

func publishedOriginMatches(release pipelines.PublishedRelease, binding imageBinding, platform, image, origin string) bool {
	for _, c := range release.Components {
		if c.Name != binding.Component {
			continue
		}
		for _, a := range c.Artifacts {
			if a.Name != binding.Artifact || a.Kind != "oci" || a.Platform != platform {
				continue
			}
			for _, loc := range a.Locations {
				u, _ := url.Parse(loc.Origin)
				ref, err := immutableImage(u.Host + "/" + loc.Repository + "@" + a.ImageDigest)
				if err == nil && ref == image && strings.TrimSuffix(loc.Origin, "/") == origin {
					return true
				}
			}
		}
	}
	return false
}

func (s *artifactAdmission) requirementsForWorkflow() []artifactRequirement {
	return append([]artifactRequirement(nil), s.requirements...)
}

// check performs a fresh bounded read for one native-selected immutable image.
// Recovery constructs a new scope and rereads bytes; serialized receipts never
// stand in for AvailableImage. Checks may run concurrently without shared state.
func (s *artifactAdmission) check(ctx context.Context, key string) (artifactObservation, error) {
	if s == nil || time.Since(s.created) > time.Hour {
		return artifactObservation{}, errors.New("artifact scope is absent or expired")
	}
	for _, want := range s.requirements {
		if want.Key != key {
			continue
		}
		ref, _ := name.NewDigest(want.Image, name.StrictValidation)
		started := time.Now().UTC()
		proof, err := s.registries[ref.Context().Name()].verifier.Check(ctx, ref.DigestStr(), want.Platform, ociregistry.AvailabilityLimits{})
		if err != nil {
			return artifactObservation{}, err
		}
		return artifactObservation{s.digest, key, started, time.Now().UTC(), proof}, nil
	}
	return artifactObservation{}, errors.New("image is outside this installation artifact scope")
}

// seal is a completeness gate, not installation approval. Thirty minutes is a
// native maximum observation age; a workflow may require a fresher read. It
// makes no registry-retention, unpackability or migration-safety promise.
func (s *artifactAdmission) seal(observations []artifactObservation) (artifactEvidence, error) {
	now := time.Now().UTC()
	if s == nil || now.Sub(s.created) > time.Hour || len(observations) != len(s.requirements) {
		return artifactEvidence{}, errors.New("artifact evidence is incomplete or expired")
	}
	byKey := map[string]artifactObservation{}
	for _, obs := range observations {
		if _, exists := byKey[obs.key]; exists {
			return artifactEvidence{}, errors.New("duplicate artifact observation")
		}
		if obs.scope != s.digest || obs.started.Before(s.created) || obs.completed.Before(obs.started) || obs.completed.After(now) || now.Sub(obs.started) > 30*time.Minute {
			return artifactEvidence{}, errors.New("artifact observation has changed scope or expired")
		}
		byKey[obs.key] = obs
	}
	type receipt struct {
		Key                string
		Started, Completed time.Time
		Image              ociregistry.AvailabilityReceipt
	}
	receipts := make([]receipt, 0, len(s.requirements))
	expires := now.Add(30 * time.Minute)
	for _, want := range s.requirements {
		obs, exists := byKey[want.Key]
		got, err := obs.image.Receipt()
		if !exists || err != nil || got.Repository+"@"+got.ImageDigest != want.Image || got.Platform != want.Platform {
			return artifactEvidence{}, errors.New("artifact observation differs from required image or platform")
		}
		receipts = append(receipts, receipt{want.Key, obs.started, obs.completed, got})
		if end := obs.started.Add(30 * time.Minute); end.Before(expires) {
			expires = end
		}
	}
	digest := artifactHash("evidence", struct {
		Scope    string
		Receipts []receipt
	}{s.digest, receipts})
	return artifactEvidence{digest: digest, scope: s.digest, configuration: s.configuration, candidate: s.candidate, rollback: s.rollback, resources: s.resources, before: s.before, after: s.after, platform: s.platform, observed: now, expires: expires}, nil
}

func artifactHash(kind string, value any) string {
	body, _ := json.Marshal(value) // only closed native types, no untrusted codec hooks
	return "memql-id:" + string(id.NewUntracked().FromString("installation-artifact-"+kind+"-v1:"+string(body)))
}

func sortJSON[T any](values []T) {
	sort.Slice(values, func(i, j int) bool {
		a, _ := json.Marshal(values[i])
		b, _ := json.Marshal(values[j])
		return string(a) < string(b)
	})
}
