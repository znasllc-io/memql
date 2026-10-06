package knowledge

// train_specialist.go -- training a knowledge domain's corpus, as a builtin a
// work run can call (memql#5051).
//
// # What this replaces
//
// Training used to be a Plan and a dispatcher. `agent_loop_training_mint.go`
// minted a `v1:planner:plan` with `kind: "trainSpecialist"`;
// `train_specialist_dispatch.go` subscribed to
// `graph.node.created/updated.v1:planner:plan`, filtered on that kind, claimed
// the row and ran the Trainer's tool loop; `refresh_cron.go` re-minted on a
// cadence and on a stale signal.
//
// The claim-off-a-graph-event pattern is the part the issue said to reconsider
// rather than port, and reconsidering it removes it: a work run is dispatched
// by the spine already (memql#5054), so the mint sites open a goal naming the
// `trainSpecialist` template and the run carries it. That deletes a
// subscription, a claim and a status machine per dispatcher.
//
// The dispatcher's claim was also an IN-PROCESS map under a mutex, which
// arbitrates within one pod and not at all between two -- so at two planner
// replicas one training Plan ran twice. The run dispatcher's claim is
// Postgres-backed.
//
// # Why it lives here
//
// Training writes knowledge chunks into a knowledge domain, and this package
// already owns the domain, the chunk writers and `embedDomainItems` -- the
// template's sibling. `integrations/planner` registers no capabilities at all
// and is largely retired by memql#5052.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// maxExistingCorpusChunks caps how many existing chunks the Trainer is handed
// in mode='refresh'. It reads summaries to decide what is stale; a full dump
// would blow the context budget on a large domain. Each chunk's text is
// truncated too.
const (
	maxExistingCorpusChunks  = 40
	existingCorpusTextMaxLen = 600
)

// trainerEngine is the bounded-tool-loop seam.
//
// Declared here and resolved by type assertion off the engine rather than
// added to memql.IntegrationEngineAccess, which is the pattern
// work_compile_adapter.go uses for its own optional seams. That is not
// fastidiousness: IntegrationEngineAccess is shared by every integration in
// the tree, and the method it would gain runs a bounded LLM TOOL LOOP. Adding
// it there hands every integration a model-call primitive, which is exactly
// the surface docs/public/ai/llm-cost-control.md exists to keep small.
type trainerEngine interface {
	InvokeAIChatWithFilteredTools(ctx context.Context, templateId string, data map[string]any, toolNames []string) (string, error)
}

// handleTrainSpecialist runs the Trainer Agent's bounded tool loop over one
// knowledge domain.
//
// SYNCHRONOUS, and that is not a downgrade: the asynchrony belongs to the RUN.
// The caller is a work step, and the run it belongs to is already the
// background -- a second layer here would be a run waiting on something it
// could not journal.
//
// The tool loop performs its own writes (writeKnowledgeChunk /
// markChunkSuperseded route to their mutations); the returned text is the
// Trainer's summary.
func (i *Integration) handleTrainSpecialist(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if i == nil || i.engine == nil {
		return nil, fmt.Errorf("trainSpecialist: engine not configured")
	}
	te, ok := i.engine.(trainerEngine)
	if !ok {
		// NAMED rather than silently skipped. A build without the tool loop
		// linked cannot train, and reporting success for a run that trained
		// nothing is the shape of failure this whole epic is about.
		return nil, fmt.Errorf("trainSpecialist: this build has no bounded tool loop, so the Trainer cannot run here")
	}

	domainId := strings.TrimSpace(stringArg(args, "domainId"))
	if domainId == "" {
		return nil, fmt.Errorf("trainSpecialist: domainId is required")
	}
	specialistId := strings.TrimSpace(stringArg(args, "specialistId"))
	topic := strings.TrimSpace(stringArg(args, "topic"))
	mode := strings.TrimSpace(stringArg(args, "mode"))
	if mode == "" {
		mode = "initial"
	}

	result, err := workflowhost.Run(ctx, "knowledgeTrainWorkflow", map[string]any{
		"domainId": domainId, "specialistId": specialistId, "topic": topic, "mode": mode, "partitionId": stringArg(args, "partition"),
	}, workflowhost.Options{Logger: i.Logger, Operations: map[string]workflowhost.Operation{
		"knowledgeTrainingSpecialist": func(ctx context.Context, _ map[string]any) (any, error) {
			return i.loadSpecialist(ctx, specialistId), nil
		},
		"knowledgeTrainingCorpus": func(ctx context.Context, _ map[string]any) (any, error) {
			rows := i.loadExistingCorpus(ctx, domainId)
			if rows == nil {
				rows = []map[string]any{}
			}
			return rows, nil
		},
		"knowledgeTrainingTurn": func(ctx context.Context, a map[string]any) (any, error) {
			data, _ := a["data"].(map[string]any)
			out, err := te.InvokeAIChatWithFilteredTools(ctx, stringArg(a, "prompt"), data, toStringSlice(a["tools"]))
			if err != nil {
				return nil, fmt.Errorf("trainSpecialist: trainerAgent tool loop: %w", err)
			}
			return out, nil
		},
		"knowledgeTrainingReset": func(ctx context.Context, _ map[string]any) (any, error) {
			i.resetStaleAfterRefresh(ctx, domainId)
			return nil, nil
		},
	}})
	if err != nil {
		return nil, err
	}
	summary, _ := result.(string)

	payload, err := json.Marshal(map[string]any{
		"domainId":     domainId,
		"specialistId": specialistId,
		"topic":        topic,
		"mode":         mode,
		"summary":      summary,
	})
	if err != nil {
		return nil, fmt.Errorf("trainSpecialist: marshal result: %w", err)
	}
	return []memorynodes.MemoryNode{{
		ID:        "trainSpecialist-result:" + domainId,
		Concept:   "v1:knowledge:result",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payload,
	}}, nil
}

// loadSpecialist resolves the target specialist's agent row. Returns an empty
// map (not nil) on any failure so the prompt's required-field schema sees an
// object rather than null.
func (i *Integration) loadSpecialist(ctx context.Context, specialistId string) map[string]any {
	if specialistId == "" {
		return map[string]any{}
	}
	res, err := i.engine.Execute(ctx, fmt.Sprintf(`query agentById(agentId:%s)`, langparser.QuoteString(specialistId)))
	if err != nil {
		i.Logger.Warn("trainSpecialist: loadSpecialist failed",
			"component", "knowledge.train", "specialistId", specialistId, "error", err)
		return map[string]any{}
	}
	rows := memql.MaterializeRows(res)
	if len(rows) == 0 {
		return map[string]any{}
	}
	return rows[0]
}

// loadExistingCorpus pulls the domain's current chunks (capped and
// text-truncated) for the Trainer's refresh decision. Best-effort: a query
// failure yields an empty corpus, which the prompt tolerates.
func (i *Integration) loadExistingCorpus(ctx context.Context, domainId string) []map[string]any {
	res, err := i.engine.Execute(ctx, fmt.Sprintf(`query documentChunksForDomain(domainId:%s)`, langparser.QuoteString(domainId)))
	if err != nil {
		i.Logger.Warn("trainSpecialist: loadExistingCorpus failed",
			"component", "knowledge.train", "domainId", domainId, "error", err)
		return nil
	}
	rows := memql.MaterializeRows(res)
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		// Skip already-superseded chunks -- the Trainer only needs to reason
		// about live content.
		if b, ok := row["superseded"].(bool); ok && b {
			continue
		}
		entry := map[string]any{
			"chunkId":   rowStringField(row, "id"),
			"sourceRef": rowStringField(row, "sourceRef"),
			"text":      truncateText(rowStringField(row, "text"), existingCorpusTextMaxLen),
		}
		if ca := rowStringField(row, "createdAt"); ca != "" {
			entry["createdAt"] = ca
		}
		out = append(out, entry)
		if len(out) >= maxExistingCorpusChunks {
			break
		}
	}
	return out
}

// resetStaleAfterRefresh zeroes staleSignalCount and stamps lastSeededAt after
// a successful refresh. Best-effort.
func (i *Integration) resetStaleAfterRefresh(ctx context.Context, domainId string) {
	q := fmt.Sprintf(`mutation mutationResetStaleAfterRefresh(domainId:%s, lastSeededAt:%s)`,
		langparser.QuoteString(domainId),
		langparser.QuoteString(time.Now().UTC().Format(time.RFC3339)))
	if _, err := i.engine.Execute(ctx, q); err != nil {
		i.Logger.Warn("trainSpecialist: resetStaleAfterRefresh failed",
			"component", "knowledge.train", "domainId", domainId, "error", err)
	}
}

func rowStringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

func truncateText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// stringArg reads a string capability argument, tolerating absence.
func stringArg(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	s, _ := args[key].(string)
	return s
}
