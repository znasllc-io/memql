package memql

// memory_search_world.go -- the owner's memory-search builtins' world over
// Postgres (engineMemorySearchWorld): the binding and its lock, the run rows
// through the index job's codec, the vector tables, the retired rows, the
// owner check and the catalogs. The decisions are memory_search_builtins.go's
// and memory_search_status.go's.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/num"
)

// engineMemorySearchWorld is the builtins' world over Postgres.
type engineMemorySearchWorld struct{ e *MemQLEngine }

func (w engineMemorySearchWorld) binding(ctx context.Context) (EmbedderBinding, error) {
	return w.e.ReadEmbedderBinding(ctx)
}

func (w engineMemorySearchWorld) lockBinding(ctx context.Context) (func(), error) {
	return w.e.lockAskConversationKind(ctx, ActiveEmbedderBindingID, "embedder-binding")
}

func (w engineMemorySearchWorld) readRun(ctx context.Context, kind string) (MemoryIndexRun, time.Time, bool, error) {
	return w.e.readMemoryIndexRun(ctx, kind)
}

func (w engineMemorySearchWorld) writeRun(ctx context.Context, run MemoryIndexRun, version time.Time) (time.Time, error) {
	return w.e.writeMemoryIndexRun(ctx, run, version)
}

func (w engineMemorySearchWorld) writeChoice(ctx context.Context, choice embedderChoice) error {
	return w.e.writeEmbedderChoice(ctx, choice)
}

func (w engineMemorySearchWorld) bind(ctx context.Context, provider string, choice embedderChoice) error {
	_, err := w.e.bindEmbedder(ctx, provider, choice)
	return err
}

func (w engineMemorySearchWorld) probe(ctx context.Context, provider, indexer string) (int, error) {
	return w.e.probeEmbedder(ctx, provider, indexer)
}

func (w engineMemorySearchWorld) ensureTable(ctx context.Context, ref string, dims int) (string, error) {
	db := w.e.database()
	if db == nil {
		return "", fmt.Errorf("memory search: no database")
	}
	return EnsureEmbeddingVectorTable(ctx, db.DB, ref, dims)
}

// vectors counts a vector table's entries. The name comes from
// EmbeddingVectorTable and is checked again before it enters the statement.
func (w engineMemorySearchWorld) vectors(ctx context.Context, table string) (int, error) {
	db, exists, err := w.vectorTable(ctx, table)
	if err != nil || !exists {
		return 0, err
	}
	var n int64
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		return 0, err
	}
	return num.ClampInt64(n), nil
}

// hasVectors reports whether a vector table holds any entry, by EXISTS: it
// reads one row at most, where a count reads them all.
func (w engineMemorySearchWorld) hasVectors(ctx context.Context, table string) (bool, error) {
	db, exists, err := w.vectorTable(ctx, table)
	if err != nil || !exists {
		return false, err
	}
	var held bool
	err = db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM `+table+`)`).Scan(&held)
	return held, err
}

// vectorTable checks a vector table's name again -- it comes from
// EmbeddingVectorTable -- before the name enters a statement, and says whether
// the table exists.
func (w engineMemorySearchWorld) vectorTable(ctx context.Context, table string) (*sql.DB, bool, error) {
	db := w.e.database()
	if db == nil || !embeddingTableName.MatchString(table) {
		return nil, false, fmt.Errorf("memory search: cannot read vector table %q", table)
	}
	var exists bool
	if err := db.DB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_tables WHERE schemaname = current_schema() AND tablename = $1)`, table).Scan(&exists); err != nil {
		return nil, false, err
	}
	return db.DB, exists, nil
}

// retiredEmbedderPayload is a retired model's row as stored.
type retiredEmbedderPayload struct {
	ProviderRef string `json:"providerRef"`
	Dimensions  int    `json:"dimensions"`
	IndexedBy   string `json:"indexedBy"`
	RetireAt    string `json:"retireAt"`
	DroppedAt   string `json:"droppedAt"`
}

// retired reads the newest version of every retired model's row
// (`retired-<16 hex>`, RetiredEmbedderBindingID), straight off the store, so the
// cleanup and the status decide from what is stored now and never from a
// result cache.
//
// staged-data: MUST-NOT-GATE -- the retained list of the cluster's own vector
// tables (a core platform concept, never promoted). Read as staged, a retired
// index would read as absent: the cleanup would never drop it, and Memory search
// would not offer it for switching back.
func (w engineMemorySearchWorld) retired(ctx context.Context) ([]retiredMemoryIndex, error) {
	db := w.e.database()
	if db == nil {
		return nil, fmt.Errorf("memory search: no database")
	}
	var nodes []memorynodes.MemoryNode
	err := db.NewSelect().Model(&nodes).DistinctOn("id").
		Where("concept = ?", EmbedderBindingConceptID).
		Where("id LIKE ?", EmbedderBindingConceptID+":"+retiredEmbedderBindingPrefix+"%").
		OrderExpr(`id ASC, "createdAt" DESC`).Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("memory search: reading the retired indexes: %w", err)
	}
	out := make([]retiredMemoryIndex, 0, len(nodes))
	for _, n := range nodes {
		var p retiredEmbedderPayload
		if err := json.Unmarshal(n.Payload, &p); err != nil {
			w.e.logOrDefault().Warn("memory search: a retired index's row does not decode; it is left as it is", "id", n.ID, "error", err)
			continue
		}
		out = append(out, retiredMemoryIndex{ProviderRef: p.ProviderRef, Dimensions: p.Dimensions, IndexedBy: p.IndexedBy,
			RetireAt: memoryIndexTime(p.RetireAt), DroppedAt: memoryIndexTime(p.DroppedAt)})
	}
	return out, nil
}

// recordRetired writes a retired model's row WHOLE (recordRetiredEmbedder), at
// the id derived from its model and width, never one taken from input. Its
// version is stamped after the row's newest (ruling R12), read here under the
// binding lock every caller holds, so a replica whose clock lags the last
// writer's still writes the current version.
func (w engineMemorySearchWorld) recordRetired(ctx context.Context, r retiredMemoryIndex) error {
	id := RetiredEmbedderBindingID(r.ProviderRef, r.Dimensions)
	if id == "" {
		return fmt.Errorf("memory search: %q has no index to record", r.ProviderRef)
	}
	version, err := w.e.latestRowVersion(ctx, EmbedderBindingConceptID, id)
	if err != nil {
		return err
	}
	args := map[string]any{"bindingId": id, "providerRef": r.ProviderRef, "dimensions": r.Dimensions}
	if v := strings.TrimSpace(r.IndexedBy); v != "" {
		args["indexedBy"] = v
	}
	memoryIndexTimeArg(args, "retireAt", r.RetireAt)
	memoryIndexTimeArg(args, "droppedAt", r.DroppedAt)
	memoryIndexTimeArg(args, "versionTime", VersionTimeAfter(version, time.Now()))
	return w.e.memoryIndexMutation(auth.ContextWithInternalOrigin(ctx), "platform.recordRetiredEmbedder", args)
}

// memoryIndexModelTable is a model's own vector table, the only kind the cleanup
// may drop: node_vectors_<width>_<16 hex> (EmbeddingVectorTable). The shared
// tables of older releases (node_vectors, node_vectors_<width>) never match.
var memoryIndexModelTable = regexp.MustCompile(`^node_vectors_[0-9]+_[0-9a-f]{16}$`)

// dropTable drops one model's vector table. Only a name of a model's own table
// (memoryIndexModelTable) is accepted, so no other table can reach the
// statement.
func (w engineMemorySearchWorld) dropTable(ctx context.Context, table string) error {
	db := w.e.database()
	if db == nil || !memoryIndexModelTable.MatchString(table) {
		return fmt.Errorf("memory search: refusing to drop table %q", table)
	}
	_, err := db.DB.ExecContext(ctx, `DROP TABLE IF EXISTS `+table)
	return err
}

// isActiveOwner asks the same per-replica owner set the resolver and the index
// job ask (IndexerIsActiveOwner), so an act and the indexing it starts agree,
// and reports an owner check that could not be made rather than reading it as
// "not an owner" (indexerOwnerAnswer).
func (w engineMemorySearchWorld) isActiveOwner(ctx context.Context, userId string) (bool, error) {
	return w.e.indexerOwnerAnswer(ctx, userId)
}

// catalog reads one person's machine models -- their own machines and the ones
// lent to them, what the router reads for a fleet door -- as the cluster, the
// way ownerEmbeddingCandidates reads an owner's: the take-over reads the
// indexer's catalog as well as the caller's, and the answer must not depend on
// whose session asked.
func (w engineMemorySearchWorld) catalog(ctx context.Context, userId string) ([]FleetModel, error) {
	if w.e.providers == nil {
		return nil, nil
	}
	return w.e.providers.FleetCatalog(readinessEvaluateContext(ctx), userId)
}

func (w engineMemorySearchWorld) vendorEmbedders() []vendorEmbedderRecord {
	if w.e.providers == nil {
		return nil
	}
	return w.e.providers.federationEmbedderRecords()
}

// displayName is a person's display name, through the narrow cross-user read
// (userDisplayById: id and displayName, nothing else); empty when unknown.
func (w engineMemorySearchWorld) displayName(ctx context.Context, userId string) string {
	call, err := parser.RenderCall("identity.userDisplayById", map[string]any{"userId": BareShortId(userId)})
	if err != nil {
		return ""
	}
	res, err := w.e.Execute(ctx, "query "+call)
	if err != nil {
		return ""
	}
	rows := MaterializeRows(res.OutputPayload())
	if len(rows) != 1 {
		return ""
	}
	return strings.TrimSpace(stringFromAny(rows[0]["displayName"]))
}

func (w engineMemorySearchWorld) now() time.Time { return time.Now().UTC() }

func (w engineMemorySearchWorld) logger() *slog.Logger { return w.e.logOrDefault() }
