package packages

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/znasllc-io/memql/component/deploycontrol"
	"github.com/znasllc-io/memql/integrations/azureblob"
)

type ActiveSetVersion struct {
	Exists  bool              `json:"exists"`
	ETag    string            `json:"etag"`
	Data    []byte            `json:"data"`
	Domains map[string]string `json:"domains"`
}
type conditionalStager interface {
	ReadActiveSetVersion(context.Context) (ActiveSetVersion, error)
	CompareAndSwapActiveSet(context.Context, ActiveSetVersion, map[string]string) (ActiveSetVersion, error)
}
type stablePackageRoller interface {
	PrepareRoll(context.Context, string) ([]deploycontrol.PackageRolloutIntent, error)
	ObserveRoll(context.Context, deploycontrol.PackageRolloutIntent) (deploycontrol.PackageRolloutObservation, error)
	ApplyRoll(context.Context, deploycontrol.PackageRolloutIntent) error
}

func activeSetVersion(s azureblob.PackageActiveSetSnapshot) (ActiveSetVersion, error) {
	v := ActiveSetVersion{Exists: s.Exists, ETag: s.ETag, Data: s.Data, Domains: map[string]string{}}
	if s.Exists {
		if json.Unmarshal(s.Data, &v.Domains) != nil || v.Domains == nil {
			return v, fmt.Errorf("active package set is not a domain map")
		}
	}
	for domain, prefix := range v.Domains {
		d, ok := domainFromPrefix(prefix)
		if !ok || d != domain {
			return v, fmt.Errorf("active package set contains an invalid prefix")
		}
	}
	return v, nil
}
func (s *blobStager) ReadActiveSetVersion(ctx context.Context) (ActiveSetVersion, error) {
	if err := s.resolve(ctx); err != nil {
		return ActiveSetVersion{}, err
	}
	value, err := s.uploader.ReadPackageActiveSet(ctx, s.container)
	if err != nil {
		return ActiveSetVersion{}, err
	}
	return activeSetVersion(value)
}
func (s *blobStager) CompareAndSwapActiveSet(ctx context.Context, before ActiveSetVersion, next map[string]string) (ActiveSetVersion, error) {
	if err := s.resolve(ctx); err != nil {
		return ActiveSetVersion{}, err
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return ActiveSetVersion{}, err
	}
	value, err := s.uploader.CompareAndSwapPackageActiveSet(ctx, s.container, azureblob.PackageActiveSetSnapshot{Exists: before.Exists, ETag: before.ETag, Data: before.Data}, raw)
	if err != nil {
		return ActiveSetVersion{}, err
	}
	return activeSetVersion(value)
}
func (r *deployControlRoller) PrepareRoll(ctx context.Context, marker string) ([]deploycontrol.PackageRolloutIntent, error) {
	targets, ns := rollTargets(), deploycontrol.NamespaceFromEnv()
	if len(targets) == 0 || len(targets) > 32 || ns == "" {
		return nil, fmt.Errorf("package rollout requires a bounded configured target set and namespace")
	}
	api, err := deploycontrol.NewClusterAPI()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := make([]deploycontrol.PackageRolloutIntent, 0, len(targets))
	for _, name := range targets {
		if seen[name] {
			return nil, fmt.Errorf("duplicate configured package rollout target")
		}
		seen[name] = true
		i, err := api.PreparePackageRollout(ctx, ns, name, marker)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, nil
}
func (r *deployControlRoller) ObserveRoll(ctx context.Context, i deploycontrol.PackageRolloutIntent) (deploycontrol.PackageRolloutObservation, error) {
	api, err := deploycontrol.NewClusterAPI()
	if err != nil {
		return deploycontrol.PackageRolloutObservation{}, err
	}
	return api.ObservePackageRollout(ctx, i)
}
func (r *deployControlRoller) ApplyRoll(ctx context.Context, i deploycontrol.PackageRolloutIntent) error {
	api, err := deploycontrol.NewClusterAPI()
	if err != nil {
		return err
	}
	return api.ApplyPackageRollout(ctx, i)
}
