package memql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql/readiness"
)

// readModuleReadinessRows reads every node's latest row under the caller's
// own context. The concept is public, requiresIdentity, so any signed-in
// caller sees all of them and an anonymous one sees none.
//
// It reads result.Bundle even though moduleReadinessAll declares a shape, and
// that is deliberate rather than an oversight. A shape sets result.output and
// LEAVES Bundle standing; maybeClearBundle nils it only inside ToAPIResult,
// the API-response path a client crosses. An in-process Go caller is on the
// other side of that boundary and still sees the nodes. The trap runs the
// other way for a CLIENT -- there, adding a shape drops the bundle and a
// caller reading rawNodes() silently gets an empty list -- which is why this
// is written down here rather than assumed either way.
func (e *MemQLEngine) readModuleReadinessRows(ctx context.Context) ([]readiness.NodeReport, error) {
	result, err := e.Execute(ctx, "query moduleReadinessAll()")
	if err != nil {
		return nil, fmt.Errorf("module readiness: rows: %w", err)
	}
	var out []readiness.NodeReport
	if result == nil || result.Bundle == nil {
		return out, nil
	}
	for _, n := range result.Bundle.Nodes {
		raw, err := protojson.Marshal(n.GetPayload())
		if err != nil {
			continue
		}
		var r readiness.NodeReport
		if err := json.Unmarshal(raw, &r); err != nil || r.Module == "" {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// readClusterNodeLiveness reads the latest non-stopped cluster nodes, the same
// set the reconciler prunes over.
func (e *MemQLEngine) readClusterNodeLiveness(ctx context.Context) ([]readiness.NodeLiveness, error) {
	result, err := e.Execute(ctx, "query staleClusterNodes()")
	if err != nil {
		return nil, fmt.Errorf("module readiness: cluster nodes: %w", err)
	}
	var out []readiness.NodeLiveness
	if result == nil || result.Bundle == nil {
		return out, nil
	}
	for _, n := range result.Bundle.Nodes {
		f := n.GetPayload().GetFields()
		id := strings.TrimPrefix(n.GetId(), "v1:cluster:node:")
		liveness := readiness.NodeLiveness{
			NodeId: id,
			Health: strings.ToLower(strings.TrimSpace(f["health"].GetStringValue())),
		}
		if ls := strings.TrimSpace(f["lastSeen"].GetStringValue()); ls != "" {
			if t, perr := time.Parse(time.RFC3339, ls); perr == nil {
				liveness.LastSeen = t
			}
		}
		out = append(out, liveness)
	}
	return out, nil
}

// evaluateModuleReadinessExpression is the moduleReadiness builtin: the fold,
// one row per module.
//
// The row id is the MODULE NAME. A builtin's reply is one id-keyed map, so
// two rows sharing an id would silently collapse to one -- a module quietly
// missing from the verdict list, which a reader renders as a module that does
// not exist rather than as an error.
func (e *MemQLEngine) evaluateModuleReadinessExpression(ctx context.Context) ([]memorynodes.MemoryNode, error) {
	reports, err := e.readModuleReadinessRows(ctx)
	if err != nil {
		return nil, err
	}
	nodes, err := e.readClusterNodeLiveness(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	verdicts := readiness.Fold(reports, nodes, now)
	out := make([]memorynodes.MemoryNode, 0, len(verdicts))
	for _, v := range verdicts {
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		out = append(out, memorynodes.MemoryNode{
			ID:        v.Module,
			Concept:   ModuleVerdictConcept,
			Type:      memorynodes.NodeTypeObject,
			Payload:   raw,
			CreatedAt: now,
		})
	}
	return out, nil
}

// evaluateReadinessRecomputeExpression is the readinessRecompute builtin. Not
// on the SDK wire, and refused unless the caller is internal or a cluster
// owner: a recompute is harmless in itself, but a surface any signed-in
// person can hit in a loop is a way to make every node rewrite rows all day.
func (e *MemQLEngine) evaluateReadinessRecomputeExpression(ctx context.Context) ([]memorynodes.MemoryNode, error) {
	if !auth.OriginFromContext(ctx).IsInternal() && !rowAuthzIsClusterOwner(ctx) {
		return nil, fmt.Errorf("readinessRecompute is internal or owner-only")
	}
	written, err := e.WriteModuleReadiness(ctx)
	// A PASS THAT COULD NOT EVALUATE A MODULE STILL ANSWERS. Its known rows
	// were written, and refusing the whole call would tell the caller the
	// recompute did nothing when it did most of it; the modules it could not
	// evaluate are named instead, each with its closed-vocabulary reason, and
	// this node's recompute loop keeps retrying them on its own.
	unknown := []string{}
	var u *ReadinessUnknownError
	switch {
	case err == nil:
	case errors.As(err, &u):
		unknown = u.pairs()
	default:
		return nil, err
	}
	nodeId, _ := e.readinessIdentity()
	raw, err := json.Marshal(map[string]any{"nodeId": nodeId, "written": written, "unknown": unknown})
	if err != nil {
		return nil, err
	}
	return []memorynodes.MemoryNode{{
		// One row answering one question about one node, so its id is a
		// constant.
		ID:        "current",
		Concept:   ReadinessRecomputeResultConcept,
		Type:      memorynodes.NodeTypeObject,
		Payload:   raw,
		CreatedAt: time.Now().UTC(),
	}}, nil
}
