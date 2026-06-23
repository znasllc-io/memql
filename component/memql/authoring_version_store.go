package memql

// authoring_version_store.go -- the engine-backed ImpactStore (epic memql#954,
// issue #961, increment 4).
//
// Drives the impact-analysis queries: dependentsOfConstruct (#957) for the
// dependent edges, authoringBundleById to filter to ACTIVE dependents
// (dependentsOfConstruct returns every edge regardless of bundle status,
// and the design doc only re-validates ACTIVE dependents -- a draft / retired
// dependent will be re-validated on its own activation), and
// authoringConstructsForBundle to load each dependent's closure for the
// Gate-1 re-compile.

import (
	"context"
	"encoding/json"
	"fmt"
)

// engineImpactStore is the production ImpactStore over a live engine.
type engineImpactStore struct {
	engine *MemQLEngine
}

// ActiveDependentBundleIds finds every edge depending on (toKind, toName), then
// filters to the bundles that are currently ACTIVE -- the impact set the design
// doc re-validates before a shared construct's edit goes live. De-duplicated
// (one bundle can declare several edges to the same construct).
func (s *engineImpactStore) ActiveDependentBundleIds(ctx context.Context, owner, toKind, toName string) ([]string, error) {
	args, err := json.Marshal(map[string]string{"toName": toName, "toKind": toKind})
	if err != nil {
		return nil, err
	}
	res, err := s.engine.Execute(ctx, "dependentsOfConstruct("+string(args)+")")
	if err != nil {
		return nil, err
	}
	if res == nil || res.Bundle == nil {
		return nil, nil
	}

	seen := map[string]struct{}{}
	ordered := make([]string, 0)
	for _, node := range res.Bundle.Nodes {
		payload, err := nodePayloadJSON(node)
		if err != nil {
			return nil, err
		}
		var edge struct {
			BundleId string `json:"bundleId"`
		}
		if len(payload) == 0 {
			continue
		}
		if err := json.Unmarshal(payload, &edge); err != nil || edge.BundleId == "" {
			continue
		}
		if _, ok := seen[edge.BundleId]; ok {
			continue
		}
		seen[edge.BundleId] = struct{}{}
		ordered = append(ordered, edge.BundleId)
	}

	// Filter to ACTIVE bundles.
	active := make([]string, 0, len(ordered))
	for _, id := range ordered {
		bres, err := s.engine.Execute(ctx, fmt.Sprintf(`authoringBundleById({"bundleId":%q})`, id))
		if err != nil {
			return nil, err
		}
		if bres == nil || bres.Bundle == nil || len(bres.Bundle.Nodes) == 0 {
			continue
		}
		payload, err := nodePayloadJSON(bres.Bundle.Nodes[0])
		if err != nil {
			return nil, err
		}
		row, err := parseBundleRow(bres.Bundle.Nodes[0].GetId(), payload)
		if err != nil {
			return nil, err
		}
		if row.Status == BundleActive {
			active = append(active, id)
		}
	}
	return active, nil
}

// ConstructsAsSandbox loads a bundle's member constructs as SandboxConstructs.
func (s *engineImpactStore) ConstructsAsSandbox(ctx context.Context, bundleId string) ([]SandboxConstruct, error) {
	res, err := s.engine.Execute(ctx, fmt.Sprintf(`authoringConstructsForBundle({"bundleId":%q})`, bundleId))
	if err != nil {
		return nil, err
	}
	if res == nil || res.Bundle == nil {
		return nil, nil
	}
	out := make([]SandboxConstruct, 0, len(res.Bundle.Nodes))
	for _, node := range res.Bundle.Nodes {
		payload, err := nodePayloadJSON(node)
		if err != nil {
			return nil, err
		}
		row, err := parseConstructRow(node.GetId(), payload)
		if err != nil {
			return nil, err
		}
		out = append(out, SandboxConstruct{Kind: row.Kind, Name: row.Name, Source: row.Source})
	}
	return out, nil
}
