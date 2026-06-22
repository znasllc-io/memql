package knowledge

// Augment-domain pipeline. Powers the chat surface's "Analyze for
// training" action: when a user notices an agent answered from
// general knowledge despite having relevant domains attached, they
// can ask the system to (a) decide whether the topic genuinely fits
// one of the agent's domains and (b) if so, generate a small batch
// of topic-focused chunks to fill the retrieval gap. Future similar
// questions then hit trained content instead of falling through to
// the base model.
//
// Two capabilities exposed:
//
//   augmentDomainAnalyze({userQuestion, agentResponse, domains, retrieved})
//     Synchronous. Calls the augmentDomainAnalyze prompt and returns
//     the structured outcome -- frontend uses this to decide whether
//     to surface a Confirm step, an "alreadyCovered" terminal, or an
//     "outOfScope" terminal.
//
//   augmentDomainGenerate({domainId, topic, sourceUtteranceId, sourceAgentId, spaceId, requestedBy})
//     Synchronous in v1 (~30s). Inserts a Plan row for audit, runs
//     the augmentDomainContent prompt, embeds + writes each chunk
//     with source="augment" + provenance back-pointers, then updates
//     the Plan to succeeded with a summary. Returns {planId, chunksAdded}.
//
// The two capabilities are deliberately separated so the frontend
// can show the analyse result + a Confirm step BEFORE paying for
// the heavier generate call.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/core/id"
)

// augmentDomainAnalyzeSchemaJSON constrains the analyser to a small,
// machine-readable shape the frontend can branch on without parsing
// prose. Three terminal outcomes with required fields per outcome.
const augmentDomainAnalyzeSchemaJSON = `{
  "type": "object",
  "additionalProperties": false,
  "description": "augmentDomainAnalyze.v1",
  "properties": {
    "outcome": {
      "type": "string",
      "enum": ["addable", "alreadyCovered", "outOfScope"],
      "description": "addable = topic fits a domain; default outcome since the agent's uncited reply IS the gap signal regardless of retrieval similarity scores. alreadyCovered = NARROW edge case where the proposed augmentation would duplicate existing chunks AND the agent already drew on trained content; do NOT pick this just because similarity scores look high. outOfScope = topic doesn't fit any of the agent's declared domains."
    },
    "domainId": {
      "type": "string",
      "description": "Best-fit domain id. Required for addable + alreadyCovered. Empty string for outOfScope."
    },
    "topic": {
      "type": "string",
      "description": "Concise noun-phrase naming what to generate chunks ABOUT. ~textbook-chapter-sized. Required for addable; ignored otherwise."
    },
    "reasoning": {
      "type": "string",
      "description": "One short paragraph explaining the decision. Surfaces in the UI so the user understands what the analyser saw."
    },
    "confidence": {
      "type": "number",
      "description": "0..1. How strongly the topic fits the chosen outcome."
    }
  },
  "required": ["outcome", "domainId", "topic", "reasoning", "confidence"]
}`

// augmentDomainAnalyzePayload mirrors the schema above for unmarshal.
type augmentDomainAnalyzePayload struct {
	Outcome    string  `json:"outcome"`
	DomainId   string  `json:"domainId"`
	Topic      string  `json:"topic"`
	Reasoning  string  `json:"reasoning"`
	Confidence float64 `json:"confidence"`
}

// augmentDomainContentSchemaJSON mirrors seedDomainContent's schema --
// same chunk shape so the storage path stays identical.
const augmentDomainContentSchemaJSON = `{
  "type": "object",
  "additionalProperties": false,
  "description": "augmentDomainContent.v1",
  "properties": {
    "chunks": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "properties": {
          "kind": {
            "type": "string",
            "enum": ["principle", "decisionRule", "factExample"]
          },
          "title": {
            "type": "string",
            "description": "Short noun-phrase naming the specific concept / event / framework this chunk covers."
          },
          "body": {
            "type": "string",
            "description": "100-300 words. Dense, declarative, specific. Names + dates + numbers + cited rules. Stand on its own out of context."
          },
          "keyTerms": {
            "type": "array",
            "items": { "type": "string" }
          }
        },
        "required": ["kind", "title", "body", "keyTerms"]
      }
    }
  },
  "required": ["chunks"]
}`

// augmentDomainAnalyzeHandler runs the preflight analysis. Stateless
// -- the frontend passes everything the prompt needs (avoids a round
// of utterance + agent + domain lookups, which the frontend already
// has loaded in chat state).
func (i *Integration) augmentDomainAnalyzeHandler(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	userQuestion, _ := args["userQuestion"].(string)
	agentResponse, _ := args["agentResponse"].(string)
	if strings.TrimSpace(userQuestion) == "" || strings.TrimSpace(agentResponse) == "" {
		return nil, fmt.Errorf("augmentDomainAnalyze: userQuestion and agentResponse are required")
	}
	domains, _ := args["domains"].([]any)
	if len(domains) == 0 {
		return nil, fmt.Errorf("augmentDomainAnalyze: domains is required (at least one declared domain)")
	}
	retrieved, _ := args["retrieved"].([]any)

	if i.engine == nil {
		return nil, fmt.Errorf("augmentDomainAnalyze: integration not fully wired (engine missing)")
	}

	data := map[string]any{
		"userQuestion":  userQuestion,
		"agentResponse": agentResponse,
		"domains":       domains,
		"retrieved":     retrieved,
	}

	raw, err := i.engine.InvokeAIStructured(
		ctx,
		"augmentDomainAnalyze",
		data,
		"augmentDomainAnalyze",
		json.RawMessage(augmentDomainAnalyzeSchemaJSON),
		true,
	)
	if err != nil {
		return nil, fmt.Errorf("augmentDomainAnalyze AI call: %w", err)
	}

	var payload augmentDomainAnalyzePayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return nil, fmt.Errorf("augmentDomainAnalyze JSON parse: %w", err)
	}

	i.Logger.Info("knowledge.augmentDomainAnalyze: completed",
		"outcome", payload.Outcome,
		"domainId", payload.DomainId,
		"topic", payload.Topic,
		"confidence", payload.Confidence,
	)

	// Return as a single synthetic memory node carrying the analysis
	// payload; the WS bridge unwraps it as a JSON result for the
	// frontend.
	return []memorynodes.MemoryNode{
		newAnalyzeResultNode(payload),
	}, nil
}

// augmentDomainGenerateHandler runs the topic-focused chunk
// generation + insert flow. v1 is synchronous-in-handler -- the user
// clicks Confirm, waits ~30s for the result. A Plan row is inserted
// for audit + Tasks-panel visibility; the canvas-card lifecycle
// piggybacks on the existing plan.created / plan.completed cards
// fired by the cognition handler when Plan rows transition.
func (i *Integration) augmentDomainGenerateHandler(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	domainId, _ := args["domainId"].(string)
	topic, _ := args["topic"].(string)
	sourceUtteranceId, _ := args["sourceUtteranceId"].(string)
	sourceAgentId, _ := args["sourceAgentId"].(string)
	spaceId, _ := args["spaceId"].(string)
	requestedBy, _ := args["requestedBy"].(string)

	if strings.TrimSpace(domainId) == "" || strings.TrimSpace(topic) == "" {
		return nil, fmt.Errorf("augmentDomainGenerate: domainId and topic are required")
	}
	if strings.TrimSpace(spaceId) == "" {
		return nil, fmt.Errorf("augmentDomainGenerate: spaceId is required (Plan row scopes to a space)")
	}
	if strings.TrimSpace(requestedBy) == "" {
		return nil, fmt.Errorf("augmentDomainGenerate: requestedBy is required (audit + Plan ownership)")
	}

	if i.engine == nil || i.embeddingProvider == nil {
		return nil, fmt.Errorf("augmentDomainGenerate: integration not fully wired")
	}

	// Resolve domain metadata -- need the human-readable name +
	// description for the generate prompt. Read directly from the
	// concept (works for both shipped catalog domains and user-
	// created ones, unlike the seedDomainContent path which only
	// supports shipped catalog).
	domainName, domainDescription, err := i.lookupDomainMeta(ctx, domainId)
	if err != nil {
		return nil, fmt.Errorf("augmentDomainGenerate: lookup domain %q: %w", domainId, err)
	}

	// Plan row -- v1 keeps the Plan in `running` for the whole
	// synchronous body and updates to `succeeded` at the end. The
	// frontend Tasks panel + canvas card flow consumes the Plan
	// transitions automatically.
	planId := augmentPlanId(domainId, topic, sourceUtteranceId)
	planGoal := fmt.Sprintf("Augment %s with %s", domainName, topic)
	startedAt := time.Now().UTC().Format(time.RFC3339)

	planInput, _ := json.Marshal(map[string]any{
		"domainId":          domainId,
		"domainName":        domainName,
		"topic":             topic,
		"sourceUtteranceId": sourceUtteranceId,
		"sourceAgentId":     sourceAgentId,
	})

	createPlanQuery := fmt.Sprintf(
		`mutationCreatePlan({planId: %s, spaceId: %s, kind: %s, goal: %s, requestedBy: %s, triggerSource: %s, input: %s})`,
		quoteString(planId),
		quoteString(spaceId),
		quoteString("augmentDomain"),
		quoteString(planGoal),
		quoteString(requestedBy),
		quoteString("user.explicit"),
		string(planInput),
	)
	if _, err := i.engine.Execute(ctx, createPlanQuery); err != nil {
		return nil, fmt.Errorf("augmentDomainGenerate: create plan: %w", err)
	}

	// Flip to running so the Tasks panel shows it as in-flight while
	// the generation is executing. updatePlanStatus accepts a
	// startedAt for the running transition.
	startQuery := fmt.Sprintf(
		`mutationUpdatePlanStatus({planId: %s, status: %s, startedAt: %s})`,
		quoteString(planId),
		quoteString("running"),
		quoteString(startedAt),
	)
	if _, err := i.engine.Execute(ctx, startQuery); err != nil {
		i.Logger.Warn("augmentDomainGenerate: plan -> running transition failed (continuing)", "planId", planId, "err", err)
	}

	// Run the generation prompt. ~10 chunks targeted at the topic.
	promptData := map[string]any{
		"domainId":          domainId,
		"domainName":        domainName,
		"domainDescription": domainDescription,
		"topic":             topic,
		"targetCount":       10,
	}
	rawChunks, err := i.engine.InvokeAIStructured(
		ctx,
		"augmentDomainContent",
		promptData,
		"augmentDomainContent",
		json.RawMessage(augmentDomainContentSchemaJSON),
		true,
	)
	if err != nil {
		i.failAugmentPlan(ctx, planId, fmt.Sprintf("LLM generation failed: %v", err))
		return nil, fmt.Errorf("augmentDomainGenerate AI call: %w", err)
	}

	var chunkPayload seedChunkPayload
	if err := json.Unmarshal([]byte(rawChunks), &chunkPayload); err != nil {
		i.failAugmentPlan(ctx, planId, fmt.Sprintf("Could not parse generated chunks: %v", err))
		return nil, fmt.Errorf("augmentDomainGenerate JSON parse: %w", err)
	}

	// Embed + insert each chunk. We reuse the same enriched-body +
	// vector-store pattern as seedDomainContent so retrieval works
	// identically for augment chunks.
	provider, err := i.embeddingProvider(defaultProvider)
	if err != nil {
		i.failAugmentPlan(ctx, planId, fmt.Sprintf("Embedding provider %q unavailable: %v", defaultProvider, err))
		return nil, fmt.Errorf("resolve embedding provider %q: %w", defaultProvider, err)
	}
	partition := i.resolvePartition(ctx)

	written := 0
	for idx, c := range chunkPayload.Chunks {
		c.Title = strings.TrimSpace(c.Title)
		c.Body = strings.TrimSpace(c.Body)
		if c.Title == "" || c.Body == "" {
			continue
		}
		if err := i.storeAugmentChunk(ctx, partition, domainId, topic, sourceUtteranceId, sourceAgentId, planId, idx, c, provider); err != nil {
			i.Logger.Warn("augmentDomainGenerate: chunk write failed",
				"planId", planId, "domainId", domainId, "title", c.Title, "err", err)
			continue
		}
		written++
	}

	// Stamp domain freshness so the Knowledge panel's "last updated"
	// reflects the augment. recipeVersion stays "augment-{topicHash}"
	// so it doesn't collide with the seed recipe-version stream.
	completedAt := time.Now().UTC().Format(time.RFC3339)
	if stampErr := i.stampDomainSeeded(ctx, domainId, "augment-"+shortTopicHash(topic)); stampErr != nil {
		i.Logger.Warn("augmentDomainGenerate: stampDomainSeeded failed", "domainId", domainId, "err", stampErr)
	}

	// Update Plan to succeeded with the summary so the Tasks-panel
	// row + plan.completed canvas card carry the result.
	planOutput, _ := json.Marshal(map[string]any{
		"chunksAdded": written,
		"domainId":    domainId,
		"domainName":  domainName,
		"topic":       topic,
	})
	completeQuery := fmt.Sprintf(
		`mutationUpdatePlanStatus({planId: %s, status: %s, completedAt: %s, output: %s})`,
		quoteString(planId),
		quoteString("succeeded"),
		quoteString(completedAt),
		string(planOutput),
	)
	if _, err := i.engine.Execute(ctx, completeQuery); err != nil {
		i.Logger.Warn("augmentDomainGenerate: plan -> succeeded transition failed (chunks were written successfully)", "planId", planId, "err", err)
	}

	i.Logger.Info("knowledge.augmentDomainGenerate: completed",
		"planId", planId,
		"domainId", domainId,
		"topic", topic,
		"chunksAdded", written,
	)

	return []memorynodes.MemoryNode{
		newGenerateResultNode(planId, domainId, domainName, topic, written),
	}, nil
}

// failAugmentPlan flips a Plan to failed with an error message so the
// frontend's Tasks-panel row reflects the failure instead of dangling
// in `running` forever.
func (i *Integration) failAugmentPlan(ctx context.Context, planId, errMsg string) {
	now := time.Now().UTC().Format(time.RFC3339)
	q := fmt.Sprintf(
		`mutationUpdatePlanStatus({planId: %s, status: %s, completedAt: %s, errorMessage: %s})`,
		quoteString(planId),
		quoteString("failed"),
		quoteString(now),
		quoteString(errMsg),
	)
	if _, err := i.engine.Execute(ctx, q); err != nil {
		i.Logger.Warn("augmentDomainGenerate: plan -> failed transition failed (best-effort)", "planId", planId, "err", err)
	}
}

// storeAugmentChunk persists one augment chunk: derives a deterministic
// id, embeds the body, writes the chunk row with source="augment" +
// provenance back-pointers, and stores the vector. Same enriched-body
// envelope as seedDomainContent so retrieval reads it identically.
func (i *Integration) storeAugmentChunk(
	ctx context.Context,
	partition string,
	domainId string,
	topic string,
	sourceUtteranceId string,
	sourceAgentId string,
	planId string,
	chunkIndex int,
	c seedChunk,
	provider interface {
		Embed(ctx context.Context, text string) ([]float32, error)
	},
) error {
	chunkId := augmentChunkId(planId, chunkIndex)
	sourceRef := fmt.Sprintf("augment:%s:%d", planId, chunkIndex)

	vec, err := provider.Embed(ctx, c.Body)
	if err != nil {
		return fmt.Errorf("embed: %w", err)
	}

	// Sanitize the title before indexing so role markers / markdown
	// headers in user-supplied content don't ride into the retrieval
	// pool. Defense-in-depth on top of the prompt-render-time
	// framing (bff-copresent PR #25); see SanitizeChunkTitle's
	// doc-comment for the full rule set and bff-copresent#29 for
	// the rationale.
	cleanTitle := SanitizeChunkTitle(c.Title)
	metadata := map[string]any{
		"seedSource": "augment",
		"chunkKind":  c.Kind,
		"chunkTitle": cleanTitle,
		"keyTerms":   c.KeyTerms,
		"topic":      topic,
		"planId":     planId,
	}
	metadataJSON, _ := json.Marshal(metadata)
	enrichedBody := fmt.Sprintf("<!--seed:%s-->\n\n## %s\n\n%s", string(metadataJSON), cleanTitle, c.Body)

	insertQuery := fmt.Sprintf(
		`mutationCreateDocumentChunk({chunkId: %s, domainId: %s, text: %s, sourceRef: %s, seq: %d, tokenCount: %d, source: %s, sourceUtteranceId: %s, sourceAgentId: %s, sourceTopic: %s})`,
		quoteString(chunkId),
		quoteString(domainId),
		quoteString(enrichedBody),
		quoteString(sourceRef),
		chunkIndex,
		approxTokens(enrichedBody),
		quoteString("augment"),
		quoteString(sourceUtteranceId),
		quoteString(sourceAgentId),
		quoteString(topic),
	)
	if _, err := i.engine.Execute(ctx, insertQuery); err != nil {
		return fmt.Errorf("insert chunk: %w", err)
	}
	if err := i.storeVector(ctx, chunkId, "v1:common:documentChunk", vec); err != nil {
		return fmt.Errorf("persist vector: %w", err)
	}
	return nil
}

// lookupDomainMeta resolves the human-readable name + description for
// any domain id (catalog or user-created) by reading the
// v1:common:knowledgeDomain row directly. Falls back to the bare id
// + an empty description if the row is missing -- the prompt still
// runs, just less context-rich.
func (i *Integration) lookupDomainMeta(ctx context.Context, domainId string) (string, string, error) {
	q := fmt.Sprintf(`queryKnowledgeDomainById({domainId: %s})`, quoteString(domainId))
	res, err := i.engine.Execute(ctx, q)
	if err != nil {
		return domainId, "", fmt.Errorf("query knowledge domain: %w", err)
	}
	if res == nil || res.Bundle == nil || len(res.Bundle.Nodes) == 0 {
		return domainId, "", nil
	}
	node := res.Bundle.Nodes[0]
	if node == nil || node.Payload == nil {
		return domainId, "", nil
	}
	payloadJSON, err := node.Payload.MarshalJSON()
	if err != nil {
		return domainId, "", nil
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		return domainId, "", nil
	}
	name, _ := payload["name"].(string)
	desc, _ := payload["description"].(string)
	if name == "" {
		name = domainId
	}
	return name, desc, nil
}

// augmentPlanId derives a deterministic Plan id so two simultaneous
// clicks on the same {domainId, topic, sourceUtteranceId} collapse
// into one Plan rather than racing against each other. The
// augment-vs-seed origin is captured in row provenance, not in the
// id string.
func augmentPlanId(domainId, topic, sourceUtteranceId string) string {
	return string(id.New().MustFromMap(map[string]any{
		"kind":               "augment-plan",
		"domainId":           domainId,
		"topic":              topic,
		"sourceUtteranceId":  sourceUtteranceId,
	}))
}

// augmentChunkId derives a deterministic chunk id under a Plan.
// Same hashing pattern as seedChunkId; origin is captured in
// provenance.
func augmentChunkId(planId string, chunkIndex int) string {
	return string(id.New().MustFromMap(map[string]any{
		"kind":       "augment-chunk",
		"planId":     planId,
		"chunkIndex": chunkIndex,
	}))
}

// shortTopicHash reduces a topic string to a hash for use in the
// recipeVersion field on the domain freshness stamp. The recipeVersion
// namespace stays clean -- "augment-{hash}" never collides with seed
// recipe versions like "v1" / "v2" regardless of the hash length.
func shortTopicHash(topic string) string {
	return string(id.New().FromString(topic))
}

// newAnalyzeResultNode wraps the analyse payload as a synthetic memory
// node so the WS bridge can deliver it as a structured result. The
// integration system unwraps single-node returns into the result
// payload the frontend reads. Concept name doesn't need a real
// schema row -- the engine treats integration-returned nodes as
// transient.
func newAnalyzeResultNode(payload augmentDomainAnalyzePayload) memorynodes.MemoryNode {
	body, _ := json.Marshal(map[string]any{
		"outcome":    payload.Outcome,
		"domainId":   payload.DomainId,
		"topic":      payload.Topic,
		"reasoning":  payload.Reasoning,
		"confidence": payload.Confidence,
	})
	return memorynodes.MemoryNode{
		ID:        "augment-analyze-result",
		Concept:   "integration:knowledge:augmentAnalyze",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   body,
	}
}

// newGenerateResultNode wraps the generate summary the same way.
func newGenerateResultNode(planId, domainId, domainName, topic string, chunksAdded int) memorynodes.MemoryNode {
	body, _ := json.Marshal(map[string]any{
		"planId":      planId,
		"domainId":    domainId,
		"domainName":  domainName,
		"topic":       topic,
		"chunksAdded": chunksAdded,
	})
	return memorynodes.MemoryNode{
		ID:        "augment-generate-result",
		Concept:   "integration:knowledge:augmentGenerate",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   body,
	}
}
