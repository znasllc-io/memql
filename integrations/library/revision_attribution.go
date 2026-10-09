package library

import (
	"context"
	"sort"

	"github.com/znasllc-io/memql/component/memql"
)

// Snapshot measured model identities into the immutable approval. Model-call
// journals have retention; content authorship must outlive that journal. A
// missing model report stays unknown, never inferred from the routing request.
func (i *Integration) revisionAttribution(ctx context.Context, captured, proposal map[string]any) (map[string]any, error) {
	ids, err := revisionIdentity(ctx, asString(captured["requestId"]))
	if err != nil {
		return nil, err
	}
	calls, err := i.revisionRows(memql.ContextWithFreshRead(ctx), "workModelCallsForOwnerRun", map[string]any{"runId": ids.RunID})
	if err != nil {
		return nil, err
	}
	models := make([]map[string]any, 0, len(calls))
	for _, call := range calls {
		if asString(call["error"]) != "" {
			continue
		}
		models = append(models, map[string]any{"callId": memql.BareShortId(asString(call["id"])), "provider": asString(call["provider"]), "model": asString(call["model"]), "stepKey": asString(call["stepKey"]), "promptRef": asString(call["promptRef"])})
	}
	sort.Slice(models, func(a, b int) bool { return asString(models[a]["callId"]) < asString(models[b]["callId"]) })
	inherited := map[string]any{}
	if amendment := revisionMap(captured["amendment"]); len(amendment) > 0 {
		_, _, _, parent, err := i.revisionRequest(ctx, asString(amendment["requestId"]))
		if err != nil {
			return nil, err
		}
		inherited = revisionMap(revisionMap(revisionMap(parent["subject"])["attribution"])["items"])
	}
	items, err := revisionItems(proposal)
	if err != nil {
		return nil, err
	}
	authored := map[string]any{}
	for _, item := range items {
		if previous := inherited[item.ID]; previous != nil {
			authored[item.ID] = previous
			continue
		}
		authored[item.ID] = map[string]any{"kind": "assistant", "runId": ids.RunID, "models": models, "feedbackIds": item.CommentIDs}
	}
	return map[string]any{"items": authored, "unchanged": "inherited from source version", "sourceRevision": captured["revision"]}, nil
}
