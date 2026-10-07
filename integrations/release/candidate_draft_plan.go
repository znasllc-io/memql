package release

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strings"

	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/githubrelease"
)

// One remote release is shared by its candidate's exact asset set. Native
// identity owns that relationship; caller-provided receipt maps cannot add or
// omit assets, change notes, or widen trust while reusing an approval.
type candidateDraftPlan struct {
	ResourceKey string                `json:"resourceKey"`
	Target      candidateFileTarget   `json:"target"`
	Assets      []candidateDraftAsset `json:"assets"`
}

type candidateDraftAsset struct {
	Destination pl.ReleaseDestination          `json:"destination"`
	Expected    githubrelease.AssetExpectation `json:"expected"`
}

func draftResourceKey(t candidateFileTarget) string {
	origin := strings.ToLower(strings.TrimSuffix(t.APIOrigin, "/"))
	if u, err := url.Parse(origin); err == nil {
		if (u.Scheme == "https" && u.Port() == "443") || (u.Scheme == "http" && u.Port() == "80") {
			u.Host = strings.TrimSuffix(u.Host, ":"+u.Port())
			origin = u.String()
		}
	}
	return string(id.NewUntracked().FromString("release-resource-v1:" + origin + "/" + strings.ToLower(t.Repository) + "/" + t.Tag))
}

func draftAuthority(t candidateFileTarget) candidateFileTarget {
	t.ID, t.Component, t.Artifact, t.AssetName = "", "", "", ""
	return t
}

func (r candidateTargetReader) draftPlan(ctx context.Context, record candidateRecord, targetID string) (candidateDraftPlan, error) {
	if _, err := candidateOwner(ctx); err != nil {
		return candidateDraftPlan{}, err
	}
	var chosen pl.ReleaseDestination
	for _, d := range record.Manifest.Destinations {
		if d.TargetID == targetID {
			chosen = d
		}
	}
	t, _, err := r.resolveFile(ctx, chosen)
	if err != nil {
		return candidateDraftPlan{}, err
	}
	plan := candidateDraftPlan{ResourceKey: draftResourceKey(t), Target: t, Assets: []candidateDraftAsset{}}
	authority, _ := json.Marshal(draftAuthority(t))
	names := map[string]bool{}
	total := int64(0)
	for _, d := range record.Manifest.Destinations {
		other, ok := r.files[d.TargetID]
		if !ok || draftResourceKey(other) != plan.ResourceKey {
			continue
		}
		other, _, err = r.resolveFile(ctx, d)
		if err != nil {
			return candidateDraftPlan{}, err
		}
		otherAuthority, _ := json.Marshal(draftAuthority(other))
		if string(authority) != string(otherAuthority) {
			return candidateDraftPlan{}, errors.New("candidate release assets disagree on draft identity, metadata or trust")
		}
		pub, err := candidatePublicationFor(record, d.TargetID, d.Component, d.Artifact)
		if err != nil {
			return candidateDraftPlan{}, err
		}
		a := pub.Artifact
		if a.Kind != "file" || a.Size < 0 || a.Size >= 2<<30 || names[other.AssetName] {
			return candidateDraftPlan{}, errors.New("release requires unique bounded file assets")
		}
		names[other.AssetName] = true
		total += a.Size
		plan.Assets = append(plan.Assets, candidateDraftAsset{Destination: d, Expected: githubrelease.AssetExpectation{Name: other.AssetName, Size: a.Size, SHA256: a.Digest}})
	}
	if len(plan.Assets) == 0 || len(plan.Assets) > 64 || total > 8<<30 {
		return candidateDraftPlan{}, errors.New("release asset set exceeds its verification bound")
	}
	slices.SortFunc(plan.Assets, func(a, b candidateDraftAsset) int {
		return strings.Compare(a.Destination.TargetID, b.Destination.TargetID)
	})
	// The first declared target supplies transport construction, independent of
	// which asset the owner selected to address this one release.
	plan.Target = r.files[plan.Assets[0].Destination.TargetID]
	return plan, nil
}

func (p candidateDraftPlan) identity() (string, []byte, error) {
	body, err := json.Marshal(p)
	if err != nil || len(body) > 1<<20 {
		return "", nil, errors.New("draft plan exceeds its encoding bound")
	}
	return string(id.NewUntracked().FromBytes(body)), body, nil
}

func (p candidateDraftPlan) expected() []githubrelease.AssetExpectation {
	out := make([]githubrelease.AssetExpectation, 0, len(p.Assets))
	for _, a := range p.Assets {
		out = append(out, a.Expected)
	}
	return out
}
