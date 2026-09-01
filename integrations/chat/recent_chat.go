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
	"github.com/znasllc-io/memql/core/num"
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
	bun            func() *bun.DB
	stagedConcept  func(conceptId string) bool
	admitSourceRow func(ctx context.Context, node memorynodes.MemoryNode) bool
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
//
// admitSourceRow is the PER-ROW AUTHORIZATION question (memql#4029), required
// for the same reason and refused the same way. The two are siblings: one asks
// whether a concept's rows are visible to anyone yet, the other whether THIS
// caller may see this row. See admitted.
func NewIntegration(
	bunCallback func() *bun.DB,
	stagedConcept func(conceptId string) bool,
	admitSourceRow func(ctx context.Context, node memorynodes.MemoryNode) bool,
) *Integration {
	return &Integration{bun: bunCallback, stagedConcept: stagedConcept, admitSourceRow: admitSourceRow}
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

// admitted drops every row this caller may not see, from a slice of rows AS
// FETCHED (memql#4029).
//
// # Why the gate has to run HERE, and cannot run on what this file returns
//
// Every capability in this file is a REPACK: it reads real graph rows and emits
// a synthetic node stamped `chat.recentChat` whose payload carries a summary of
// them -- for wrapUtterances, the utterance text itself. Both of the engine's
// row-authz mechanisms resolve the tier from a CONCEPT (filter injection from
// plan.BoundConcept, the row gate from the row's own concept), so the gate that
// runs on this capability's output is asked about `chat.recentChat`. That
// concept declares no tier, so it admits -- and whatever tier
// v1:cognition:utterance, space or participant declares is never consulted,
// because no row bearing those concepts ever leaves this file.
//
// The rows still carry their real concept, id and payload at the moment they
// come back from bun. That is the last point on the path where the question is
// answerable, so this is where it is asked. Stamping a source concept on the
// summary instead would be worse than useless: the summary has no top-level
// owner field for an `owned` tier to read, no per-row identity for `granted`,
// a synthesized id, and in the general case rows from more than one concept.
//
// # Why this is not redundant with the staged check above
//
// They answer different questions about the same row and neither implies the
// other. `withheldAsStaged` asks whether the concept's data is visible to
// ANYONE yet -- one answer for every caller, decided before the query runs.
// This asks whether THIS caller may see THIS row, which is per row, per caller,
// and decidable only after the fetch. The engine stacks its own two gates in
// exactly this order for exactly this reason (filterStagedSet applied over
// filterRowAuthzSet).
//
// # Why every read, when none of these concepts declares a tier today
//
// v1:cognition:utterance, space and participant declare no @rowAuthz tier, so
// this gate admits everything it is shown right now and the change is
// structural. That is the same argument withheldAsStaged makes for itself two
// functions up, and it holds for the same reason: the alternative is that the
// day one of these concepts declares a tier -- or the day this file grows a
// sixth read -- five reads that feed LLM context have to be found again by
// someone who was not here for this issue.
//
// Refuses when the predicate was never wired, matching withheldAsStaged: a node
// that cannot answer "may this caller see this row" must not answer it "yes".
func (c *Integration) admitted(ctx context.Context, nodes []memorynodes.MemoryNode) ([]memorynodes.MemoryNode, error) {
	if c.admitSourceRow == nil {
		return nil, fmt.Errorf("chat.recentChat: per-row authorization gate not wired; " +
			"refusing the read rather than returning rows that passed no authorization")
	}
	if len(nodes) == 0 {
		return nodes, nil
	}
	out := make([]memorynodes.MemoryNode, 0, len(nodes))
	for _, n := range nodes {
		if c.admitSourceRow(ctx, n) {
			out = append(out, n)
		}
	}
	return out, nil
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
				"partitionId": "string (required)",
				"agentId":     "string (optional, audit only)",
				"operation":   "string (required) -- readRecent / readByKeyword / readByTime / getSpaceContext / listParticipants",
				"count":       "int (optional) -- readRecent",
				"keyword":     "string (optional) -- readByKeyword",
				"fromTime":    "string (optional, ISO-8601) -- readByTime",
				"toTime":      "string (optional, ISO-8601) -- readByTime",
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
	admitted, err := c.admitted(ctx, nodes)
	if err != nil {
		return nil, err
	}
	return wrapUtterances(admitted, "readRecent"), nil
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
	admitted, err := c.admitted(ctx, nodes)
	if err != nil {
		return nil, err
	}
	return wrapUtterances(admitted, "readByKeyword"), nil
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
	admitted, err := c.admitted(ctx, nodes)
	if err != nil {
		return nil, err
	}
	return wrapUtterances(admitted, "readByTime"), nil
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

	// The query already pins the LATEST version (id + createdAt DESC + Limit 1),
	// so the row gated here is the row that decides. Denied means no context
	// node at all rather than an empty one: a summary whose every field is nil
	// still tells the agent the space exists.
	admitted, err := c.admitted(ctx, []memorynodes.MemoryNode{node})
	if err != nil {
		return nil, err
	}
	if len(admitted) == 0 {
		return nil, nil
	}

	var payload map[string]any
	_ = json.Unmarshal(node.Payload, &payload)

	out := map[string]any{
		"operation":    "getSpaceContext",
		"partitionId":  partitionId,
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

	items, err := c.foldActiveParticipants(ctx, nodes)
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"operation":    "listParticipants",
		"partitionId":  partitionId,
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

// foldActiveParticipants collapses the participant rows -- which arrive
// `createdAt DESC`, several versions per id -- down to one entry per active
// participant, applying the per-row authorization gate on the way.
//
// # THE ORDER OF THE THREE STEPS IS THE WHOLE FUNCTION (memql#4029)
//
// dedup -> gate -> status, in that order, and each boundary is load-bearing:
//
//  1. DEDUP BEFORE GATE. The rows are newest-first, so the FIRST row for an id
//     is its latest version and the only one whose verdict is current. Gating
//     the slice first and folding the survivors would let a DENIED latest
//     version fall through to an ADMITTED older one -- the caller is handed a
//     stale row instead of no row, which is the quietest failure available
//     here, because a plausible answer is worse than an empty one. Marking
//     `seen` before the gate is what makes a denial drop the id outright rather
//     than reveal its predecessor. This is the authorization-side twin of the
//     hazard withheldAsStaged records for the staged check.
//  2. GATE BEFORE STATUS. `status` is domain filtering, not authorization.
//     Reading a field off a row this caller may not see is the wrong way round
//     even when nothing is emitted from it.
//
// Extracted from listParticipants so the ordering has a test that does not need
// a database, since it is the part of this file most likely to be "simplified"
// into a pre-filter by someone who reads the gate as a plain slice filter.
//
// Refuses when the gate was never wired, matching admitted.
func (c *Integration) foldActiveParticipants(ctx context.Context, nodes []memorynodes.MemoryNode) ([]map[string]any, error) {
	if c.admitSourceRow == nil {
		return nil, fmt.Errorf("chat.recentChat: per-row authorization gate not wired; " +
			"refusing the read rather than returning rows that passed no authorization")
	}
	seen := make(map[string]struct{}, len(nodes))
	items := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		if _, dup := seen[n.ID]; dup {
			continue
		}
		seen[n.ID] = struct{}{}
		if !c.admitSourceRow(ctx, n) {
			continue
		}
		var p map[string]any
		_ = json.Unmarshal(n.Payload, &p)
		status, _ := p["status"].(string)
		if !strings.EqualFold(status, "active") {
			continue
		}
		typ, _ := p["participantType"].(string)
		dn, _ := p["displayName"].(string)
		items = append(items, map[string]any{
			"participantId":   n.ID,
			"displayName":     dn,
			"participantKind": typ,
		})
	}
	return items, nil
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

// asInt reads a numeric tool arg.
//
// SATURATES out of range (memql#4779). Its one caller is `count`, a page size
// the handler then clamps at both ends itself (`<= 0 -> default`,
// `> max -> max`), so every answer is safe here -- saturation is chosen
// because it is the one that survives that clamp as the value the caller
// actually asked for.
func asInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int32:
		return int(n)
	case int64:
		return num.ClampInt64(n)
	case float64:
		return num.ClampFloat64(n)
	case float32:
		return num.ClampFloat64(float64(n))
	}
	return 0
}
