package pipelinerun

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// IntegrationName is the plug-in's name: the `pipelines` in every
// `@executor("integration.pipelines.<capability>")` dsl/pipelines declares.
//
// Spelled as a STRING LITERAL in RegisterPlugin below rather than as this
// constant, and that is not an oversight: the taxonomy gate
// (module_taxonomy_test.go) finds every registration by scanning source for
// the literal. TestTheRegistrationNameIsTheLiteral holds the two together.
const IntegrationName = "pipelines"

func init() {
	memql.RegisterPlugin("pipelines", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		// What the plug-in context can give: the DSL store over the engine
		// and the secret resolver. The GitHub port, the gate, the node id,
		// the OS origin and the journal are app/'s to wire
		// (app/integrations_pipelines.go): only app/ can see the packages
		// integration and the direct database. Until it does, every act
		// that needs one refuses by name rather than half-working.
		d := Deps{Logger: pctx.Logger, Secrets: pctx.ResolveSystemSecret}
		if pctx.Engine != nil {
			d.Store = NewDSLStore(pctx.Engine)
		}
		return New(d), nil
	})
}

// IntegrationName implements memql.IntegrationProvider.
func (i *Integration) IntegrationName() string { return IntegrationName }

// Capabilities implements memql.IntegrationProvider: the six executors
// dsl/pipelines/builtins.memql declares, plus `status`, which no builtin
// declares -- the readiness evaluator reads it directly by its executor name.
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "trigger",
			Description: "Open the pipeline runs a staged GitHub delivery asks for (design record D4-D6): classify the signed body's shape, then for every active pipeline of the repository whose installation matches, open one run keyed on (repository, SHA, mode, event) with a queued check run -- a fork's pull request refused with a failing one, a re-requested check run or suite as the next attempt of the original. A redelivery or a poll for the same head opens nothing.",
			Handler:     i.handleTrigger,
			ArgsSchema: map[string]string{
				"inboundRequestId": "string (required) -- the staged v1:platform:inboundRequest row",
				"source":           "string -- the allowlisted source segment; anything but github is a no-op",
				"body":             "string -- the verified raw body",
				"headersJson":      "string -- the allowlisted delivery headers as a JSON object",
			},
		},
		{
			Name:        "poll",
			Description: "The polling delivery (D4, D11): for every active pipeline whose delivery is poll, read the default branch's head and the open pull requests' heads through the owner's grant; the first poll records a baseline and opens nothing, a later one opens a push run for a moved default head and a pull_request run (fork-aware) for a new or moved pull request head. Then, on a node that drives runs, recover the runs nobody claimed or whose driver went silent.",
			Handler:     i.handlePoll,
			ArgsSchema:  map[string]string{},
		},
		{
			Name:        "connect",
			Description: "Connect a pipeline to one of the caller's sources (D12, D14): prove the grant reaches the repository by minting a token through it, read memql-package.yaml at the default branch's head, refuse a source with no pipeline block (pipeline_not_declared) or one that does not validate, and create or reconnect the source's one pipeline. Answers {pipelineId, repository, delivery, compute, stages}.",
			Handler:     i.handleConnect,
			ArgsSchema: map[string]string{
				"packageId":   "string (required) -- the caller's v1:platform:package source",
				"delivery":    "string (required) -- webhook or poll",
				"compute":     "string -- cluster (default) or cluster_and_fleet",
				"secretNames": "[]string -- the globalSecret NAMES steps may resolve; MEMQL_* is refused",
			},
		},
		{
			Name:        "disconnect",
			Description: "Disconnect one of the caller's pipelines: it opens no more runs, and a run no agent has started concludes pipeline_disconnected. The row and its runs stay as history. Answers {pipelineId, status}.",
			Handler:     i.handleDisconnect,
			ArgsSchema:  map[string]string{"pipelineId": "string (required) -- the caller's v1:pipelines:pipeline"},
		},
		{
			Name:        "rerun",
			Description: "Re-run one of the caller's runs: the next attempt of its run key, with the original's mode and event, trigger rerun and rerunOf naming the original, and a new check run. Refused while the key's newest attempt is still going. Answers {runId, attempt, rerunOf, status}.",
			Handler:     i.handleRerun,
			ArgsSchema:  map[string]string{"runId": "string (required) -- the caller's v1:pipelines:run"},
		},
		{
			Name:        "cancel",
			Description: "Ask one of the caller's runs to stop: flags it for its driver, which cancels what is executing and concludes it cancelled. A queued run no agent has claimed is concluded cancelled on the spot, check run included. Answers {runId, status, conclusion, cancelRequested}.",
			Handler:     i.handleCancel,
			ArgsSchema:  map[string]string{"runId": "string (required) -- the caller's v1:pipelines:run"},
		},
		{
			Name:        "status",
			Description: "The pipelines readiness self-report: whether this cluster has a GitHub App and whether this node has a step runner registered, in the integration-status envelope the readiness evaluator reads. No credential value and no network call.",
			Handler:     i.handleStatus,
			ArgsSchema:  map[string]string{"probe": "boolean -- accepted for the envelope's contract; this report never reaches out"},
		},
	}
}

// resultConcept is the ephemeral node kind a capability answers on. Never
// persisted -- the wire envelope for a return value.
const resultConcept = "integration:pipelines:result"

// resultNode wraps a capability's answer.
func resultNode(payload map[string]any) []memorynodes.MemoryNode {
	raw, err := json.Marshal(payload)
	if err != nil {
		raw = []byte("{}")
	}
	now := time.Now().UTC()
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("pipelines:%d", now.UnixNano()),
		Concept:   resultConcept,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: now,
		Payload:   raw,
	}}
}

func stringArg(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	switch v := args[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case fmt.Stringer:
		return strings.TrimSpace(v.String())
	}
	return ""
}

func boolArg(args map[string]any, key string) bool {
	if args == nil {
		return false
	}
	switch v := args[key].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "true")
	}
	return false
}

// stringListArg reads a []string argument in either spelling the engine
// hands back -- a real []string, or the []any a decoded object carries.
// present reports whether the argument was given at all.
func stringListArg(args map[string]any, key string) (values []string, present bool) {
	if args == nil {
		return nil, false
	}
	raw, ok := args[key]
	if !ok || raw == nil {
		return nil, false
	}
	switch v := raw.(type) {
	case []string:
		for _, s := range v {
			if s = strings.TrimSpace(s); s != "" {
				values = append(values, s)
			}
		}
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				values = append(values, strings.TrimSpace(s))
			}
		}
	case string:
		// A single name passed as a string is one name, not a list of its
		// characters.
		if s := strings.TrimSpace(v); s != "" {
			values = append(values, s)
		}
	}
	return values, true
}

// handlerContext is the context a capability runs its work under: the
// caller's, unchanged. Named so every handler says which context it passes
// on, and so no handler can reach a stamped one.
func handlerContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}
