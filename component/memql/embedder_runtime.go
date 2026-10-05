package memql

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/airoute"
)

func (e *MemQLEngine) readEmbedderPlan(ctx context.Context) (EmbedderBinding, error) {
	result, err := e.Execute(ContextWithFreshRead(ctx), "query platform.activeEmbedderBinding()")
	if err != nil {
		return EmbedderBinding{}, err
	}
	rows := MaterializeRows(result.OutputPayload())
	if len(rows) != 1 {
		return EmbedderBinding{}, ErrNoEmbedderBound
	}
	row := rows[0]
	var b EmbedderBinding
	raw, _ := json.Marshal(row)
	if err = json.Unmarshal(raw, &b); err != nil {
		return b, err
	}
	if !b.Valid() {
		return EmbedderBinding{}, ErrNoEmbedderBound
	}
	return b, nil
}

func (e *MemQLEngine) ReadEmbedderBinding(ctx context.Context) (EmbedderBinding, error) {
	b, err := e.readEmbedderPlan(ctx)
	if err == nil && b.ActivatedAt == "" {
		return EmbedderBinding{}, ErrNoEmbedderBound
	}
	return b, err
}

// Vector dimensions belong to the bound model, including a fleet model whose
// runtime reports an unknown width. Explicit providers keep their own space.
func EmbeddingDimensions(ctx context.Context, provider string, declared int) int {
	if b, err := CurrentEmbedderBinding(ctx); err == nil && b.ProviderRef == provider {
		return b.Dimensions
	}
	return declared
}

func ValidateEmbeddingVector(ctx context.Context, provider string, declared int, vector []float32) error {
	expected := EmbeddingDimensions(ctx, provider, declared)
	if len(vector) == 0 || (expected > 0 && len(vector) != expected) {
		return fmt.Errorf("embedding model returned %d dimensions, expected %d", len(vector), expected)
	}
	nonzero := false
	for _, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return fmt.Errorf("embedding model returned a non-finite vector")
		}
		nonzero = nonzero || value != 0
	}
	if !nonzero {
		return fmt.Errorf("embedding model returned a zero vector")
	}
	return nil
}

var embeddingTableName = regexp.MustCompile(`^node_vectors(?:_[0-9]+(?:_[0-9a-f]{16})?)?$`)

// Retention removes a deleted source from every historical model space. Only
// validated identifiers from PostgreSQL's table catalog enter generated SQL.
func EmbeddingVectorTables(ctx context.Context, db interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT tablename FROM pg_tables WHERE schemaname=current_schema() AND tablename LIKE 'node_vectors%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			return nil, err
		}
		if embeddingTableName.MatchString(table) {
			tables = append(tables, table)
		}
	}
	return tables, rows.Err()
}

// Model identity as well as width defines a vector space. Two 1024-wide models
// must never search one another's vectors. The identifier is derived entirely
// from a digest and a validated integer, not SQL supplied by a caller.
func EmbeddingVectorTable(provider string, dimensions int) (string, error) {
	if strings.TrimSpace(provider) == "" || dimensions < 1 || dimensions > 2000 {
		return "", fmt.Errorf("embedding index requires a provider and 1–2000 dimensions")
	}
	hash := sha256.Sum256([]byte(provider))
	return fmt.Sprintf("node_vectors_%d_%x", dimensions, hash[:8]), nil
}

func EnsureEmbeddingVectorTable(ctx context.Context, db *sql.DB, provider string, dimensions int) (string, error) {
	table, err := EmbeddingVectorTable(provider, dimensions)
	if err != nil {
		return "", err
	}
	if db == nil {
		return "", fmt.Errorf("embedding storage is unavailable")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, table); err != nil {
		return "", err
	}
	for _, statement := range createVectorTableSQL(dimensions) {
		statement = strings.ReplaceAll(statement, VectorTableFor(dimensions), table)
		if _, err = tx.ExecContext(ctx, statement); err != nil {
			return "", err
		}
	}
	return table, tx.Commit()
}

// Initial activation is deliberately distinct from switching an existing
// corpus. Switching needs a complete, source-validated reindex and remains
// refused until that workflow is available; no partial space becomes active.
func (e *MemQLEngine) bindEmbedderBuiltin(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ac, _ := auth.AccessFromContext(ctx)
	if ac == nil || ac.Role != auth.RoleOwner {
		return nil, fmt.Errorf("only the cluster owner can bind its embedder")
	}
	provider := strings.TrimSpace(stringArg(args, "provider"))
	if provider == "" {
		return nil, fmt.Errorf("choose an embedding model")
	}
	unlock, err := e.lockAskConversationKind(ctx, "active", "embedder-binding")
	if err != nil {
		return nil, err
	}
	defer unlock()
	current, readErr := e.readEmbedderPlan(ctx)
	if readErr == nil {
		if current.ProviderRef != provider {
			return nil, fmt.Errorf("changing the active embedder requires reindexing the existing corpus; that workflow is not available yet")
		}
		if current.ActivatedAt != "" {
			raw, _ := json.Marshal(map[string]any{"provider": provider, "dimensions": current.Dimensions, "active": true})
			return []memorynodes.MemoryNode{{ID: "active", Payload: raw}}, nil
		}
	}
	if readErr != nil && readErr != ErrNoEmbedderBound {
		return nil, readErr
	}
	model, _, err := ResolveAITyped[EmbeddingAIProvider](ctx, e, airoute.ResolveRequest{Level: airoute.LevelEmbeddings, Modality: airoute.ModalityEmbedding, ExplicitProvider: provider})
	if err != nil {
		return nil, err
	}
	vector, err := model.Embed(ctx, "MemQL embedding readiness check")
	if err != nil {
		return nil, err
	}
	if err = ValidateEmbeddingVector(ctx, provider, model.Dimensions(), vector); err != nil {
		return nil, err
	}
	if current.Valid() && current.Dimensions != len(vector) {
		return nil, fmt.Errorf("the pending binding has a different vector width")
	}
	if e.database() == nil {
		return nil, fmt.Errorf("embedding storage is unavailable")
	}
	if _, err = EnsureEmbeddingVectorTable(ctx, e.database().DB, provider, len(vector)); err != nil {
		return nil, err
	}
	internal := auth.ContextWithInternalOrigin(ctx)
	for _, write := range []struct {
		name string
		args map[string]any
	}{
		{"platform.recordEmbedderBindingPlan", map[string]any{"bindingId": "active", "providerRef": provider, "dimensions": len(vector)}},
		{"platform.activateEmbedderBinding", map[string]any{"bindingId": "active", "activatedAt": time.Now().UTC().Format(time.RFC3339Nano)}},
	} {
		call, _ := parser.RenderCall(write.name, write.args)
		if _, err = e.Execute(internal, "mutation "+call); err != nil {
			return nil, err
		}
	}
	bound, err := e.ReadEmbedderBinding(ctx)
	if err != nil {
		return nil, err
	}
	if bound.ProviderRef != provider || bound.Dimensions != len(vector) {
		return nil, fmt.Errorf("the binding changed during activation")
	}
	raw, _ := json.Marshal(map[string]any{"provider": provider, "dimensions": len(vector), "active": true})
	return []memorynodes.MemoryNode{{ID: "active", Payload: raw}}, nil
}
