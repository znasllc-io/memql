package memoryNodes

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/lib/pq"
	"github.com/uptrace/bun"
)

type (
	// QueryOrder specifies the order of results when querying memory nodes.
	QueryOrder int

	// CreateParams describes the payload persisted when creating a new concept record.
	CreateParams struct {
		Actor    string
		Payload  map[string]any
		ID       string
		Concept  string
		Clock    func() time.Time
		Metadata map[string]string
	}

	// QueryParams configures filters applied when querying concept records.
	QueryParams struct {
		IDs            []string
		Concept        string
		IncludeDeleted bool
		CreatedBy      string
		Limit          int
		Order          QueryOrder
	}

	// DeleteParams describes the deletion tombstone persisted for a concept record.
	DeleteParams struct {
		Actor  string
		ID     string
		Reason string
		Clock  func() time.Time
	}
)

const (
	// QueryOrderCreatedAtDesc specifies descending order by createdAt.
	QueryOrderCreatedAtDesc QueryOrder = iota
	// QueryOrderCreatedAtAsc specifies ascending order by createdAt.
	QueryOrderCreatedAtAsc
)

var (
	// ErrBunNotConfigured is returned when the bun database is not configured.
	ErrBunNotConfigured = errors.New("bun database not configured")

	// ErrStoreUnavailable is returned when the underlying store is not configured.
	ErrStoreUnavailable = ErrBunNotConfigured

	// ErrDuplicateNode is returned when an insert violates a unique constraint.
	// For example, inserting a second active AI participant for the same (spaceId, agentId).
	ErrDuplicateNode = errors.New("duplicate node: record already exists")
)

func (db *MemoryNodesDatabase) CreateMemoryNode(ctx context.Context, node *MemoryNode) error {
	if node == nil {
		return errors.New("memory node is nil")
	}

	if strings.TrimSpace(node.ID) == "" {
		return errors.New("memory node id is required")
	}

	if strings.TrimSpace(node.Concept) == "" {
		return errors.New("memory node concept is required")
	}

	bunDB := db.bun()
	if bunDB == nil {
		return ErrBunNotConfigured
	}

	// ON CONFLICT DO NOTHING on the (id, createdAt) PK dedups
	// same-microsecond duplicate inserts (React StrictMode double-fire,
	// concurrent ensure flows). The post-exec error switch below still
	// catches OTHER 23505 violations (the
	// idx_active_participant_space_agent index, etc) -- only the PK
	// race is silenced. See InsertMemoryNode in component/memql/
	// executor.go for the same fix at the engine layer.
	_, err := bunDB.NewInsert().
		Model(node).
		On("CONFLICT (id, \"createdAt\") DO NOTHING").
		Exec(ctx)
	if err != nil {
		// Detect PostgreSQL unique constraint violations (error code 23505).
		// This catches the idx_active_participant_space_agent constraint and any
		// other unique indexes, wrapping them in ErrDuplicateNode for callers.
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == "23505" {
			detail := pqErr.Detail
			if detail == "" {
				detail = pqErr.Message
			}
			return fmt.Errorf("%w: %s", ErrDuplicateNode, detail)
		}
		return err
	}
	return nil
}

func (db *MemoryNodesDatabase) ListRecentMemoryNodes(ctx context.Context, createdBy string, limit int) ([]MemoryNode, error) {
	bunDB := db.bun()
	if bunDB == nil {
		return nil, ErrBunNotConfigured
	}

	var nodes []MemoryNode
	query := bunDB.NewSelect().Model(&nodes).OrderExpr(`"createdAt" DESC`)
	if createdBy != "" {
		query = query.Where(`"createdBy" = ?`, createdBy)
	}
	if limit > 0 {
		query = query.Limit(limit)
	}

	if err := query.Scan(ctx); err != nil {
		return nil, err
	}

	return nodes, nil
}

func (db *MemoryNodesDatabase) LoadMemoryNode(ctx context.Context, id string) (*MemoryNode, error) {
	trimmedId := strings.TrimSpace(id)
	if trimmedId == "" {
		return nil, errors.New("memory node id is required")
	}

	bunDB := db.bun()
	if bunDB == nil {
		return nil, ErrBunNotConfigured
	}

	var node MemoryNode
	err := bunDB.NewSelect().Model(&node).
		Where("id = ?", trimmedId).
		OrderExpr(`"createdAt" DESC`).
		Limit(1).
		Scan(ctx)

	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	return &node, nil
}

func (db *MemoryNodesDatabase) FindMemoryNodes(ctx context.Context, params QueryParams) ([]MemoryNode, error) {
	bunDB := db.bun()
	if bunDB == nil {
		return nil, ErrBunNotConfigured
	}

	var nodes []MemoryNode
	query := bunDB.NewSelect().Model(&nodes)

	if trimmed := sanitizeIdentifiers(params.IDs); len(trimmed) > 0 {
		query = query.Where("id IN (?)", bun.In(trimmed))
	}

	if concept := strings.TrimSpace(params.Concept); concept != "" {
		query = query.Where("concept = ?", concept)
	}

	if createdBy := strings.TrimSpace(params.CreatedBy); createdBy != "" {
		query = query.Where(`"createdBy" = ?`, createdBy)
	}

	switch params.Order {
	case QueryOrderCreatedAtAsc:
		query = query.OrderExpr(`"createdAt" ASC`)
	default:
		query = query.OrderExpr(`"createdAt" DESC`)
	}

	if params.Limit > 0 {
		query = query.Limit(params.Limit)
	}

	if err := query.Scan(ctx); err != nil {
		return nil, err
	}

	return nodes, nil
}

// InsertMemoryNode satisfies the memorynodes.Store interface by delegating to CreateMemoryNode.
func (db *MemoryNodesDatabase) InsertMemoryNode(ctx context.Context, node *MemoryNode) error {
	return db.CreateMemoryNode(ctx, node)
}

// QueryMemoryNodes satisfies the memorynodes.Store interface by delegating to FindMemoryNodes.
func (db *MemoryNodesDatabase) QueryMemoryNodes(ctx context.Context, params QueryParams) ([]MemoryNode, error) {
	return db.FindMemoryNodes(ctx, params)
}

func (db *MemoryNodesDatabase) bun() *bun.DB {
	if db == nil || db.TimescaleDBDatabase == nil || db.TimescaleDBDatabase.Database == nil {
		return nil
	}
	// Use thread-safe method to avoid race condition during reconnection
	return db.TimescaleDBDatabase.Database.BunDB()
}

// BunDB exposes the underlying Bun database handle. It returns nil until the database dependency has started.
func (db *MemoryNodesDatabase) BunDB() *bun.DB {
	return db.bun()
}

func sanitizeIdentifiers(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	result := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if trimmed := strings.TrimSpace(id); trimmed != "" {
			key := strings.ToLower(trimmed)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			result = append(result, trimmed)
		}
	}
	if len(result) == 0 {
		return nil
	}
	slices.Sort(result)
	return result
}
