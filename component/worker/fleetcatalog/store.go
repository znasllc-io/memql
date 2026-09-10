package fleetcatalog

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/core/num"
)

type Engine interface {
	Execute(context.Context, string) (*memqlengine.ExecuteResult, error)
}

// EngineStore reads only the named worker queries under the owning actor or
// the narrowly scoped synthetic cluster actor. It never reads stream state.
type EngineStore struct{ Engine Engine }

func (s *EngineStore) WorkersForOwner(ctx context.Context, ownerUserId string) ([]Candidate, error) {
	if s == nil || s.Engine == nil {
		return nil, nil
	}
	if strings.TrimSpace(ownerUserId) == "" {
		return nil, fmt.Errorf("agent.worker store: ownerUserId is required")
	}
	res, err := s.Engine.Execute(auth.ContextWithUserActor(ctx, ownerUserId), `query myWorkersWithStatus()`)
	if err != nil {
		return nil, fmt.Errorf("fleet read: %w", err)
	}
	if res == nil {
		return nil, nil
	}
	rows := outputPayloadRows(res.OutputPayload())
	out := make([]Candidate, 0, len(rows))
	for _, row := range rows {
		if row == nil {
			continue
		}
		id := rowString(row, "id")
		if id == "" {
			continue
		}
		out = append(out, Candidate{
			RegistrationId: id,
			Name:           rowString(row, "name"),
			DisplayName:    rowString(row, "displayName"),
			// Stamp the scoped owner so PlanUserModelWithShared recovery and
			// OwnMachineFirst agree with WorkersForOwner even when a later
			// shared-list row is missing ownerUserId.
			OwnerUserId:    ownerUserId,
			Capabilities:   rowStringList(row, "capabilities"),
			// The merge happens HERE, once, on the way out of the store --
			// so no caller can accidentally match on the cockpit's map alone
			// and quietly ignore the labels the owner set.
			Labels: MergeLabels(rowStringMap(row, "labels"), rowStringMap(row, "operatorLabels")),
			// Read from operatorLabels ALONE, deliberately not from the
			// merge one line above (epic memql#4676): the cockpit rewrites
			// `labels` on every reconnect, so an opt-in found there was
			// granted by the machine rather than by its owner.
			SharingMode:     workerservice.SharingFromRow(row["sharing"]).Mode,
			InferenceServe:  capabilityInferenceServe(row["capabilityDescriptor"]),
			Apps:            rowApps(row, "apps"),
			AppDescriptors:  rowAppDescriptors(row, "appDescriptors"),
			Hardware:        workerservice.InventoryFromRow(row["hardware"]),
			Concurrency:     rowUint32Map(row, "concurrency"),
			ActiveCount:     rowInt(row, "activeCount"),
			ConnectedNodeId: rowString(row, "connectedNodeId"),
			LastSelectedAt:  rowTime(row, "lastSelectedAt"),
			LastSeenAt:      rowTime(row, "lastSeenAt"),
			RevokedAt:       rowTime(row, "revokedAt"),
		})
	}
	return out, nil
}
func (s *EngineStore) SharedInferenceWorkers(ctx context.Context) ([]Candidate, error) {
	if s == nil || s.Engine == nil {
		return nil, nil
	}
	res, err := s.Engine.Execute(systemFleetContext(ctx), `query allWorkersWithStatus()`)
	if err != nil {
		return nil, fmt.Errorf("shared fleet read: %w", err)
	}
	if res == nil {
		return nil, nil
	}
	rows := outputPayloadRows(res.OutputPayload())
	out := make([]Candidate, 0, len(rows))
	for _, row := range rows {
		if row == nil {
			continue
		}
		id := rowString(row, "id")
		if id == "" {
			continue
		}
		operator := rowStringMap(row, "operatorLabels")
		out = append(out, Candidate{
			RegistrationId:  id,
			Name:            rowString(row, "name"),
			DisplayName:     rowString(row, "displayName"),
			Capabilities:    rowStringList(row, "capabilities"),
			Labels:          MergeLabels(rowStringMap(row, "labels"), operator),
			SharingMode:     workerservice.SharingFromRow(row["sharing"]).Mode,
			InferenceServe:  capabilityInferenceServe(row["capabilityDescriptor"]),
			Apps:            rowApps(row, "apps"),
			AppDescriptors:  rowAppDescriptors(row, "appDescriptors"),
			Hardware:        workerservice.InventoryFromRow(row["hardware"]),
			Concurrency:     rowUint32Map(row, "concurrency"),
			ActiveCount:     rowInt(row, "activeCount"),
			ConnectedNodeId: rowString(row, "connectedNodeId"),
			LastSelectedAt:  rowTime(row, "lastSelectedAt"),
			LastSeenAt:      rowTime(row, "lastSeenAt"),
			RevokedAt:       rowTime(row, "revokedAt"),
			OwnerUserId:     rowString(row, "ownerUserId"),
		})
	}
	return out, nil
}
func rowString(row map[string]any, key string) string {
	if v, ok := row[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}
func rowInt(row map[string]any, key string) int {
	switch v := row[key].(type) {
	case float64:
		return num.ClampFloat64(v)
	case int:
		return v
	case int64:
		return num.ClampInt64(v)
	}
	return 0
}
func rowStringList(row map[string]any, key string) []string {
	raw, ok := row[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}
func rowStringMap(row map[string]any, key string) map[string]string {
	raw, ok := row[key].(map[string]any)
	if !ok || len(raw) == 0 {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		switch t := v.(type) {
		case string:
			out[k] = t
		case bool:
			out[k] = fmt.Sprintf("%t", t)
		case float64:
			// narrowing: GUARDED -- num.WholeInt64 IS the guard (memql#4779).
			if whole, ok := num.WholeInt64(t); ok {
				out[k] = fmt.Sprintf("%d", whole)
			} else {
				out[k] = fmt.Sprintf("%g", t)
			}
		}
	}
	return out
}
func rowUint32Map(row map[string]any, key string) map[string]uint32 {
	raw, ok := row[key].(map[string]any)
	if !ok || len(raw) == 0 {
		return nil
	}
	out := make(map[string]uint32, len(raw))
	for k, v := range raw {
		if f, ok := v.(float64); ok && f >= 0 {
			out[k] = uint32(f)
		}
	}
	return out
}
func rowTime(row map[string]any, key string) time.Time {
	raw := rowString(row, key)
	if raw == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
func capabilityInferenceServe(v any) string {
	descriptor, ok := v.(map[string]any)
	if !ok {
		return workerservice.InferenceServeOwner
	}
	serve, _ := descriptor["inferenceServe"].(string)
	if strings.TrimSpace(serve) != workerservice.InferenceServeCluster {
		return workerservice.InferenceServeOwner
	}
	return workerservice.InferenceServeCluster
}
func rowApps(row map[string]any, key string) []workerservice.AppInfo {
	raw, ok := row[key].([]any)
	if !ok {
		return nil
	}
	out := make([]workerservice.AppInfo, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id, _ := m["id"].(string)
		if strings.TrimSpace(id) == "" {
			continue
		}
		version, _ := m["version"].(string)
		subscription, _ := m["subscription"].(string)
		signedIn, _ := m["signedIn"].(bool)
		allowed, _ := m["allowed"].(bool)
		out = append(out, workerservice.AppInfo{
			Id:           strings.TrimSpace(id),
			Version:      version,
			SignedIn:     signedIn,
			Subscription: workerservice.NormalizeSubscription(subscription),
			Allowed:      allowed,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
func rowAppDescriptors(row map[string]any, key string) []workerservice.AppDescriptor {
	raw, ok := row[key].([]any)
	if !ok {
		return nil
	}
	out := make([]workerservice.AppDescriptor, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id, _ := m["id"].(string)
		harness, _ := m["harness"].(string)
		structured, _ := m["structuredResult"].(bool)
		followUps, _ := m["followUps"].(bool)
		d := workerservice.AppDescriptor{
			Id:               strings.TrimSpace(id),
			Harness:          strings.TrimSpace(harness),
			StructuredResult: structured,
			FollowUps:        followUps,
		}
		if !d.Valid() {
			continue
		}
		out = append(out, d)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
func systemFleetContext(ctx context.Context) context.Context {
	claims := map[string]any{"sub": systemFleetActor, "role": "owner"}
	ctx = auth.ContextWithClaims(ctx, claims)
	ctx = auth.ContextWithToken(ctx, auth.BuildTokenInfo(claims))
	// Unranked + Synthetic (epic memql#4832, D4): the fleet store acting as
	// the cluster, not as a person. RoleOwner is what buys it the
	// cluster-owner escape; it is not a claim to rank 400, and without the
	// flags a rank-strict concept would read this as an owner writing a PEER
	// owner's row and refuse the sweep.
	return auth.ContextWithAccess(ctx, &auth.AccessContext{
		UserId: systemFleetActor, Role: auth.RoleOwner, Unranked: true, Synthetic: true,
	})
}

const systemFleetActor = "system:fleet-inference"

func outputPayloadRows(payload any) []map[string]any {
	if payload == nil {
		return nil
	}
	switch v := payload.(type) {
	case []map[string]any:
		return v
	case []any:
		out := make([]map[string]any, 0, len(v))
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	case map[string]any:
		return []map[string]any{v}
	}
	return nil
}
