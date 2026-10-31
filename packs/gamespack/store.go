package gamespack

import (
	"context"
	"fmt"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// store.go -- how the Go half reads and writes rows: through the engine, under
// the CALLER'S actor, by rendering the pack's own constructs.
//
// A builtin's returned nodes are the expression's VALUE and are never
// persisted, so every row this pack writes goes through one of its mutations,
// which stamps ownerUserId from the actor. A row written any other way would
// be owned by nobody.

// rowStore is the read and write surface the decisions run over.
type rowStore interface {
	// query runs one of the pack's queries (or a core read the pack is
	// allowed, ladderPolicyCurrent) and returns its shaped rows.
	query(ctx context.Context, name string, args map[string]any) ([]map[string]any, error)
	// write runs one of the pack's client-reachable mutations.
	write(ctx context.Context, name string, args map[string]any) error
	// embedStore embeds text and stores it as nodeID's vector through the one
	// embedding write path (builtin gamesEmbedStore).
	embedStore(ctx context.Context, nodeID, concept, text string) error
	// createTrace and createSkill run two of the three @serverOnly writers
	// (store_internal.go).
	createTrace(ctx context.Context, args map[string]any) error
	createSkill(ctx context.Context, args map[string]any) error
	// recordLadder runs the third, which writes a skill's whole ladder state
	// (store_internal.go).
	recordLadder(ctx context.Context, args map[string]any) error
	// walk runs a paginated read of the pack's from its first page to its last and
	// hands each page's rows to visit, in order, so a caller that must see every row
	// does not stop at the page the query declares. A walk that cannot reach the end
	// is an error, never a short list.
	walk(ctx context.Context, name string, args map[string]any, visit func(rows []map[string]any) error) error
}

// engineStore is the production rowStore.
type engineStore struct {
	engine memql.IntegrationEngineAccess
}

func (s engineStore) query(ctx context.Context, name string, args map[string]any) ([]map[string]any, error) {
	call, err := langparser.RenderCall(name, compact(args))
	if err != nil {
		return nil, err
	}
	res, err := s.exec(ctx, "query "+call)
	if err != nil {
		return nil, fmt.Errorf("games: query %s: %w", name, err)
	}
	return memql.MaterializeRows(res), nil
}

func (s engineStore) write(ctx context.Context, name string, args map[string]any) error {
	call, err := langparser.RenderCall(name, compact(args))
	if err != nil {
		return err
	}
	if _, err := s.exec(ctx, "mutation "+call); err != nil {
		return fmt.Errorf("games: mutation %s: %w", name, err)
	}
	return nil
}

// maxWalkPages bounds a walk. A page is at most 200 rows (the pack's queries declare
// that), so this is a million rows: far past anything a cluster holds, and a walk
// that reaches it is stuck, which is an error and not a reason to read on forever.
const maxWalkPages = 5000

// walk follows the engine's keyset cursor (memql.WalkQueryPages), which reads every
// page fresh under the caller's actor and refuses a page that claims more rows
// after it and gives no way to reach them. The cursor is the engine's: a query it
// can continue is one ordered by the row's own write time (a payload field cannot
// carry a position).
func (s engineStore) walk(ctx context.Context, name string, args map[string]any, visit func(rows []map[string]any) error) error {
	call, err := langparser.RenderCall(name, compact(args))
	if err != nil {
		return err
	}
	err = memql.WalkQueryPages(ctx, s.exec, "query "+call, maxWalkPages, func(res *memql.ExecuteResult) error {
		return visit(memql.MaterializeRows(res))
	})
	if err != nil {
		return fmt.Errorf("games: query %s: %w", name, err)
	}
	return nil
}

func (s engineStore) embedStore(ctx context.Context, nodeID, concept, text string) error {
	call, err := langparser.RenderCall("gamesEmbedStore", map[string]any{
		"nodeId":      nodeID,
		"text":        text,
		"concept":     concept,
		"vectorField": "content",
	})
	if err != nil {
		return err
	}
	if _, err := s.exec(ctx, "builtin "+call); err != nil {
		return fmt.Errorf("games: builtin gamesEmbedStore: %w", err)
	}
	return nil
}

func (s engineStore) exec(ctx context.Context, statement string) (*memql.ExecuteResult, error) {
	if s.engine == nil {
		return nil, fmt.Errorf("games: the pack has no engine handle on this node")
	}
	return s.engine.Execute(ctx, statement)
}

// compact drops nil values, so an omitted optional argument is absent from
// the call rather than rendered as a null the language does not spell.
func compact(args map[string]any) map[string]any {
	out := make(map[string]any, len(args))
	for k, v := range args {
		if v != nil {
			out[k] = v
		}
	}
	return out
}
