// Package liveknowledge exposes Live Knowledge dispatch as a DSL-callable
// capability (Phase 5 of the planner-redesign work). Prompt-context
// builders + the Planner Agent's between-Task hook call
// `integration.liveknowledge.query` with a source name + args; the
// integration routes through component/memql/liveknowledge to the
// registered connector and returns the result.
package liveknowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	memorynodes "github.com/visionarys-io/memql/component/database/memory-nodes"
	"github.com/visionarys-io/memql/component/memql"
	lk "github.com/visionarys-io/memql/component/memql/liveknowledge"
)

// LiveKnowledgeIntegration exposes Live Knowledge query dispatch to
// the DSL via the integration plug-in system.
type LiveKnowledgeIntegration struct {
	dispatcher *lk.Dispatcher
}

// New constructs the integration. Caller supplies a Dispatcher already
// wired to an engine adapter + connector registry; the integration
// itself is stateless.
func New(dispatcher *lk.Dispatcher) *LiveKnowledgeIntegration {
	return &LiveKnowledgeIntegration{dispatcher: dispatcher}
}

// IntegrationName returns the stable identifier registered with the
// engine. Capabilities ultimately resolve as
// "integration.liveknowledge.<name>".
func (i *LiveKnowledgeIntegration) IntegrationName() string {
	return "liveknowledge"
}

// Capabilities exposes the single Phase 5 surface: query. Future
// capabilities (subscribe-to-changes, invalidate-snapshot, etc.)
// register here.
func (i *LiveKnowledgeIntegration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "query",
			Description: "Dispatch a Live Knowledge query against a registered v1:knowledge:liveSource by name. Args carry the source's named-query inputs; result returns the connector's response (rows for memql/SQL kinds, body for rest/graphql).",
			Handler:     i.handleQuery,
			ArgsSchema: map[string]string{
				"sourceName": "string",
				"args":       "object",
			},
		},
	}
}

// handleQuery is the dispatch entry point. Expects:
//
//	args.sourceName : string  (the v1:knowledge:liveSource.name to read)
//	args.args       : object  (the source's named-query inputs)
func (i *LiveKnowledgeIntegration) handleQuery(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if i.dispatcher == nil {
		return nil, fmt.Errorf("liveknowledge: dispatcher not configured")
	}
	sourceName, _ := args["sourceName"].(string)
	if sourceName == "" {
		return nil, fmt.Errorf("liveknowledge.query: sourceName is required")
	}
	queryArgs, _ := args["args"].(map[string]any)
	if queryArgs == nil {
		queryArgs = map[string]any{}
	}

	result, err := i.dispatcher.Query(ctx, sourceName, queryArgs)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("liveknowledge: marshal result: %w", err)
	}
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("lk-query:%s:%d", sourceName, time.Now().UnixNano()),
		Concept:   "integration:liveknowledge:query",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payload,
	}}, nil
}
