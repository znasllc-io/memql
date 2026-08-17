// Package chat backs the `recentChat` tool: the assistant's read-only
// window into the space's single utterance stream plus space metadata.
//
// Five operations selected via the `operation` discriminator:
//
//	readRecent({count?})                 -- last N utterances
//	readByKeyword({keyword})             -- recent utterances containing a substring
//	readByTime({fromTime, toTime})       -- utterances in an ISO-8601 window
//	getSpaceContext()                    -- title, goal, status
//	listParticipants()                   -- humans + agents currently active
//
// Single-chat architecture: every space carries one v1:cognition:utterance
// stream visible to all participants. This tool reads only that concept;
// there is no per-user private thread.
package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/uptrace/bun"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

const (
	defaultRecentCount = 20
	maxRecentCount     = 100
	recentChatKind     = "chat.recentChat"
)

// Integration exposes the chat read operations as a DSL-callable
// capability. The integration name is "chat" so the FQN matches
// integration.chat.recentChat, which the recentChat builtin in
// dsl/cognition/builtins.memql references via @executor.
type Integration struct {
	bun           func() *bun.DB
	stagedConcept func(conceptId string) bool
}

// NewIntegration constructs the integration. The bun callback is
// invoked lazily so the integration can be wired before the database
// is up; capabilities fail with a clear error when the callback
// returns nil.
//
// stagedConcept is the staged-DATA visibility question (epic memql#3974,
// task memql#3984) and is a REQUIRED constructor parameter rather than a
// SetX injector, because every read below is gated on it and a nil one would
// silently answer "nothing is staged". See withheldAsStaged.
func NewIntegration(bunCallback func() *bun.DB, stagedConcept func(conceptId string) bool) *Integration {
	return &Integration{bun: bunCallback, stagedConcept: stagedConcept}
}

// withheldAsStaged reports whether a read of `concept` must return nothing
// because that concept's DATA is staged (epic memql#3974).
//
// # Why a Go pre-check rather than a SQL conjunct
//
// Every read in this file pins ONE concept with `Where("concept = ?", ...)`,
// and staging is a pure function of the concept (memql#3980), so
// `concept = C AND C not staged` is either `concept = C` or the empty set --
// which is what an early return computes, exactly rather than approximately.
//
// Two of the reads below then fold their rows through a `seen` map to pick the
// latest version per id. That fold is precisely why the check must NOT be a
// SQL conjunct: a conjunct removes rows BEFORE the fold, so the fold would
// pick an OLDER version as "latest" instead of skipping the id -- the quietest
// failure in this whole class, because the caller gets a plausible row rather
// than no row. Deciding before the query cannot produce it.
//
// # Why every read here, when v1:cognition:utterance cannot be staged today
//
// Staging is set only by the authored-promote path, so an embedded engine
// concept never carries it and these gates are structural. They are still
// worth their lines: the alternative is that the day a chat-adjacent concept
// becomes author-promotable, five reads that feed LLM context have to be found
// again by someone who was not here for this issue. readByKeyword in
// particular is an unbounded `ILIKE '%...%'` over conversation text with no
// owner predicate of its own.
//
// Returns an error only when the predicate was never wired. That is a boot
// misconfiguration, and a node that cannot answer "is this staged" must not
// answer it "no".
func (c *Integration) withheldAsStaged(concept string) (bool, error) {
	if c.stagedConcept == nil {
		return false, fmt.Errorf("chat.recentChat: staged-concept predicate not wired; "+
			"refusing to read %s rather than answer as if nothing were staged", concept)
	}
	return c.stagedConcept(concept), nil
}

// IntegrationName implements memql.IntegrationProvider.
func (c *Integration) IntegrationName() string { return "chat" }

// Capabilities implements memql.IntegrationProvider.
func (c *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "recentChat",
			Description: "Read recent utterances + space context. Five operations: readRecent / readByKeyword / readByTime / getSpaceContext / listParticipants. Read-only.",
			Handler:     c.handle,
			ArgsSchema: map[string]string{
				"partitionId":   "string (required)",
				"agentId":   "string (optional, audit only)",
				"operation": "string (required) -- readRecent / readByKeyword / readByTime / getSpaceContext / listParticipants",
				"count":     "int (optional) -- readRecent",
				"keyword":   "string (optional) -- readByKeyword",
				"fromTime":  "string (optional, ISO-8601) -- readByTime",
				"toTime":    "string (optional, ISO-8601) -- readByTime",
			},
		},
	}
}

func (c *Integration) handle(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if c.bun == nil || c.bun() == nil {
		return nil, fmt.Errorf("chat.recentChat: database not configured")
	}
	partitionId := strings.TrimSpace(asString(args["partitionId"]))
	if partitionId == "" {
		return nil, fmt.Errorf("chat.recentChat: partitionId required")
	}
	op := strings.TrimSpace(asString(args["operation"]))
	if op == "" {
		return nil, fmt.Errorf("chat.recentChat: operation required")
	}

	switch op {
	case "readRecent":
		count := asInt(args["count"])
		if count <= 0 {
			count = defaultRecentCount
		}
		if count > maxRecentCount {
			count = maxRecentCount
		}
		return c.readRecent(ctx, partitionId, count)
	case "readByKeyword":
		keyword := strings.TrimSpace(asString(args["keyword"]))
		if keyword == "" {
			return nil, fmt.Errorf("chat.recentChat.readByKeyword: keyword required")
		}
		return c.readByKeyword(ctx, partitionId, keyword)
	case "readByTime":
		from := strings.TrimSpace(asString(args["fromTime"]))
		to := strings.TrimSpace(asString(args["toTime"]))
		return c.readByTime(ctx, partitionId, from, to)
	case "getSpaceContext":
		return c.getSpaceContext(ctx, partitionId)
	case "listParticipants":
		return c.listParticipants(ctx, partitionId)
	default:
		return nil, fmt.Errorf("chat.recentChat: unknown operation %q", op)
	}
}

func (c *Integration) readRecent(ctx context.Context, partitionId string, count int) ([]memorynodes.MemoryNode, error) {
	// staged-data: GATE -- utterances go straight into LLM context. See
	// withheldAsStaged for why the check is here and not in the WHERE clause.
	if withheld, err := c.withheldAsStaged(memorynodes.ConceptCognitionUtterance); err != nil || withheld {
		return nil, err
	}
	var nodes []memorynodes.MemoryNode
	err := c.bun().NewSelect().
		Model(&nodes).
		Where("concept = ?", memorynodes.ConceptCognitionUtterance).
		Where("payload->>'partitionId' = ?", partitionId).
		OrderExpr(`"createdAt" DESC`).
		Limit(count).
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("chat.recentChat.readRecent: %w", err)
	}
	return wrapUtterances(nodes, "readRecent"), nil
}

func (c *Integration) readByKeyword(ctx context.Context, partitionId, keyword string) ([]memorynodes.MemoryNode, error) {
	// staged-data: GATE -- free-text `ILIKE '%...%'` over conversation content
	// with no owner predicate of its own; the highest-leverage read in the file
	// for anyone probing what a staged concept holds. See withheldAsStaged.
	if withheld, err := c.withheldAsStaged(memorynodes.ConceptCognitionUtterance); err != nil || withheld {
		return nil, err
	}
	var nodes []memorynodes.MemoryNode
	err := c.bun().NewSelect().
		Model(&nodes).
		Where("concept = ?", memorynodes.ConceptCognitionUtterance).
		Where("payload->>'partitionId' = ?", partitionId).
		Where("payload->>'text' ILIKE ?", "%"+keyword+"%").
		OrderExpr(`"createdAt" DESC`).
		Limit(maxRecentCount).
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("chat.recentChat.readByKeyword: %w", err)
	}
	return wrapUtterances(nodes, "readByKeyword"), nil
}

func (c *Integration) readByTime(ctx context.Context, partitionId, from, to string) ([]memorynodes.MemoryNode, error) {
	// staged-data: GATE -- same stream as readRecent, windowed. See
	// withheldAsStaged.
	if withheld, err := c.withheldAsStaged(memorynodes.ConceptCognitionUtterance); err != nil || withheld {
		return nil, err
	}
	q := c.bun().NewSelect().
		Model((*memorynodes.MemoryNode)(nil)).
		Where("concept = ?", memorynodes.ConceptCognitionUtterance).
		Where("payload->>'partitionId' = ?", partitionId).
		OrderExpr(`"createdAt" DESC`).
		Limit(maxRecentCount)
	if from != "" {
		if t, err := time.Parse(time.RFC3339, from); err == nil {
			q = q.Where(`"createdAt" >= ?`, t)
		}
	}
	if to != "" {
		if t, err := time.Parse(time.RFC3339, to); err == nil {
			q = q.Where(`"createdAt" <= ?`, t)
		}
	}
	var nodes []memorynodes.MemoryNode
	if err := q.Scan(ctx, &nodes); err != nil {
		return nil, fmt.Errorf("chat.recentChat.readByTime: %w", err)
	}
	return wrapUtterances(nodes, "readByTime"), nil
}

func (c *Integration) getSpaceContext(ctx context.Context, partitionId string) ([]memorynodes.MemoryNode, error) {
	// staged-data: GATE -- the space row's title / goal / architecture are
	// rendered into the agent's prompt. See withheldAsStaged.
	if withheld, err := c.withheldAsStaged(memorynodes.ConceptCognitionSpace); err != nil || withheld {
		return nil, err
	}
	var node memorynodes.MemoryNode
	err := c.bun().NewSelect().
		Model(&node).
		Where("concept = ?", memorynodes.ConceptCognitionSpace).
		Where("id = ?", partitionId).
		OrderExpr(`"createdAt" DESC`).
		Limit(1).
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("chat.recentChat.getSpaceContext: %w", err)
	}

	var payload map[string]any
	_ = json.Unmarshal(node.Payload, &payload)

	out := map[string]any{
		"operation":    "getSpaceContext",
		"partitionId":      partitionId,
		"title":        payload["title"],
		"goal":         payload["goal"],
		"status":       payload["status"],
		"architecture": payload["architecture"],
	}
	bytes, _ := json.Marshal(out)
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("recentChat:context:%s", partitionId),
		Concept:   recentChatKind,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   bytes,
	}}, nil
}

func (c *Integration) listParticipants(ctx context.Context, partitionId string) ([]memorynodes.MemoryNode, error) {
	// staged-data: GATE -- the roster the agent is told it is talking to. This
	// is one of the two reads whose Go-side `seen` fold makes a SQL conjunct
	// actively wrong rather than merely redundant; see withheldAsStaged.
	if withheld, err := c.withheldAsStaged(memorynodes.ConceptCognitionParticipant); err != nil || withheld {
		return nil, err
	}
	var nodes []memorynodes.MemoryNode
	err := c.bun().NewSelect().
		Model(&nodes).
		Where("concept = ?", memorynodes.ConceptCognitionParticipant).
		Where("payload->>'partitionId' = ?", partitionId).
		OrderExpr(`"createdAt" DESC`).
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("chat.recentChat.listParticipants: %w", err)
	}

	seen := make(map[string]struct{})
	type entry struct {
		id          string
		displayName string
		kind        string
	}
	var entries []entry
	for _, n := range nodes {
		if _, dup := seen[n.ID]; dup {
			continue
		}
		seen[n.ID] = struct{}{}
		var p map[string]any
		_ = json.Unmarshal(n.Payload, &p)
		status, _ := p["status"].(string)
		if !strings.EqualFold(status, "active") {
			continue
		}
		typ, _ := p["participantType"].(string)
		dn, _ := p["displayName"].(string)
		entries = append(entries, entry{id: n.ID, displayName: dn, kind: typ})
	}

	items := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		items = append(items, map[string]any{
			"participantId":   e.id,
			"displayName":     e.displayName,
			"participantKind": e.kind,
		})
	}
	out := map[string]any{
		"operation":    "listParticipants",
		"partitionId":      partitionId,
		"participants": items,
		"count":        len(items),
	}
	bytes, _ := json.Marshal(out)
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("recentChat:participants:%s", partitionId),
		Concept:   recentChatKind,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   bytes,
	}}, nil
}

func wrapUtterances(nodes []memorynodes.MemoryNode, op string) []memorynodes.MemoryNode {
	items := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		var p map[string]any
		_ = json.Unmarshal(n.Payload, &p)
		speakerId, _ := p["participantId"].(string)
		speakerKind, _ := p["participantType"].(string)
		text, _ := p["text"].(string)
		items = append(items, map[string]any{
			"utteranceId": n.ID,
			"speakerId":   speakerId,
			"speakerKind": speakerKind,
			"timestamp":   n.CreatedAt.Format(time.RFC3339),
			"content":     text,
		})
	}
	out := map[string]any{
		"operation":  op,
		"count":      len(items),
		"utterances": items,
	}
	bytes, _ := json.Marshal(out)
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("recentChat:%s:%d", op, time.Now().UnixNano()),
		Concept:   recentChatKind,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   bytes,
	}}
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func asInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int32:
		return int(n)
	case int64:
		return int(n)
	case float64:
		return int(n)
	case float32:
		return int(n)
	}
	return 0
}
