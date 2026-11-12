package storestats

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// check_builtins.go -- the check's three builtins and who may call each
// (check.go, the pass they run).

// ---- the three builtins, and who may call each ----

// Each refusal is the one sentence for every caller its handler does not
// admit; errNoSuchStore is a person's to read.
var (
	errNotTheCheckPrincipal = errors.New("store statistics check requires the engine maintenance principal")
	errRebuildNotAnOwner    = errors.New("store statistics rebuild is for a cluster owner")
	errCheckStateNotAnOwner = errors.New("store statistics check state is for a cluster owner")
)

//nolint:staticcheck // ST1005: person-facing copy, not a log fragment.
var errNoSuchStore = errors.New("Store statistics has no store with that id.")

// admitsTheCheck is the one place handleCheck admits its caller: the check's
// own maintenance principal, with the cluster-owner role it carries, at the
// internal origin the automation runner gives the engine's own automations,
// as handleRollup admits the rollup's. The check reads every store's mirror
// and spends the database's time on a schedule; nobody runs it by hand.
func admitsTheCheck(ctx context.Context) bool {
	actor, ok := auth.AccessFromContext(ctx)
	return ok && actor != nil && actor.UserId == checkPrincipal && actor.IsClusterOwner() && auth.OriginFromContext(ctx).IsInternal()
}

// admitsAPersonWhoIsAClusterOwner is the one place handleRebuild and
// handleCheckState admit their caller: a cluster owner who is a person, not
// the engine acting under a maintenance principal (each of which is a
// cluster owner too). Neither builtin is in the generated clients, and each
// can be called by name by any signed-in user, so this is the gate.
func admitsAPersonWhoIsAClusterOwner(ctx context.Context) bool {
	actor, ok := auth.AccessFromContext(ctx)
	return ok && actor != nil && actor.IsClusterOwner() && !strings.HasPrefix(actor.UserId, maintenancePrefix)
}

// storeArg is the call's storeId, bare: "<id>" and "v1:shopify:store:<id>"
// name one store.
func storeArg(args map[string]any) string {
	s, _ := args["storeId"].(string)
	return memql.BareShortId(strings.TrimSpace(s))
}

func (i *Integration) clock() time.Time {
	if i.now != nil {
		return i.now()
	}
	return time.Now()
}

// answerNode is a builtin's one node.
func answerNode(id, concept string, body any, at time.Time) ([]memorynodes.MemoryNode, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("store statistics: encode the answer: %w", err)
	}
	return []memorynodes.MemoryNode{{ID: id, Concept: concept, Type: memorynodes.NodeTypeObject, CreatedAt: at.UTC(), Payload: payload}}, nil
}

// handleCheck runs the check for its scheduled automation, and for nobody
// else, under checkRunDeadline, and answers one node with its report: counts
// and store ids, since the work journal keeps what a step answers.
func (i *Integration) handleCheck(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if !admitsTheCheck(ctx) {
		return nil, errNotTheCheckPrincipal
	}
	now := i.clock()
	ctx, cancel := context.WithTimeout(ctx, checkRunDeadline)
	defer cancel()
	rep, err := runCheck(ctx, i, now)
	level := slog.LevelDebug
	if rep.Mismatched > 0 || rep.Disagreeing > 0 {
		level = slog.LevelInfo
	}
	i.logger.Log(ctx, level, "Store statistics checked",
		"component", "storestats", "stores", rep.Stores, "compared", rep.Compared, "mismatched", rep.Mismatched,
		"repaired", rep.Repaired, "disagreeing", rep.Disagreeing, "finished", rep.Finished)
	if err != nil {
		return nil, err
	}
	return answerNode("storeStatisticsCheck", checkConcept, rep, now)
}

// handleRebuild marks a store for a rebuild, for a person who is a cluster
// owner: in one transaction, with anyGeneration, rebuildStore raises the
// store's generation, sets rebuilding_since and deletes its watermarks and
// check row. The walk then deletes its facts in batches and refills them, and
// when the rebuild ends a check begins (rollup.go, rollStore). A store the
// statistics hold no row for is refused, and none is made.
func (i *Integration) handleRebuild(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if !admitsAPersonWhoIsAClusterOwner(ctx) {
		return nil, errRebuildNotAnOwner
	}
	id := storeArg(args)
	if id == "" {
		return nil, errNoSuchStore
	}
	var db *bun.DB
	if i.bunDB != nil {
		db = i.bunDB()
	}
	now := micro(i.clock())
	if err := writeTx(ctx, db, id, anyGeneration, func(ctx context.Context, tx bun.Tx) error {
		var held bool
		if err := tx.NewSelect().Table("store_sales_store").ColumnExpr("true").Where("store_id = ?", id).Scan(ctx, &held); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errNoSuchStore
			}
			return err
		}
		return rebuildStore(ctx, tx, id, now)
	}); err != nil {
		if errors.Is(err, errNoSuchStore) {
			return nil, err
		}
		return nil, fmt.Errorf("store %s: mark the store for a rebuild: %w", id, sanitized(err))
	}
	i.logger.Info("Store statistics: a cluster owner asked for a store's rebuild; its facts are deleted and walked again",
		"component", "storestats", "storeId", id)
	return answerNode("storeStatisticsRebuild", rebuildConcept, map[string]any{
		"storeId": id, "rebuildingSince": now.Format(time.RFC3339Nano),
	}, now)
}

// handleCheckState answers, for a person who is a cluster owner, one store's
// check as store_sales_check holds it: when the pass under way began, when
// the last one began and ended, when one last ended clean, and the last
// ended pass's four counts. A time that never was is null. It reads in one
// readTx and nothing else.
func (i *Integration) handleCheckState(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if !admitsAPersonWhoIsAClusterOwner(ctx) {
		return nil, errCheckStateNotAnOwner
	}
	id := storeArg(args)
	if id == "" {
		return nil, errNoSuchStore
	}
	var db *bun.DB
	if i.bunDB != nil {
		db = i.bunDB()
	}
	var row checkRow
	if err := readTx(ctx, db, func(ctx context.Context, tx bun.Tx) error {
		var held bool
		if err := tx.NewSelect().Table("store_sales_store").ColumnExpr("true").Where("store_id = ?", id).Scan(ctx, &held); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errNoSuchStore
			}
			return err
		}
		var err error
		row, _, err = readCheckRow(ctx, tx, id)
		return err
	}); err != nil {
		if errors.Is(err, errNoSuchStore) {
			return nil, err
		}
		return nil, fmt.Errorf("store %s: read the store's check: %w", id, sanitized(err))
	}
	at := func(t sql.NullTime) any {
		if !t.Valid {
			return nil
		}
		return t.Time.UTC().Format(time.RFC3339Nano)
	}
	return answerNode("checkState", checkStateConcept, map[string]any{
		"storeId":           id,
		"passUnderWaySince": at(row.PassStartedAt),
		"lastStartedAt":     at(row.LastStartedAt),
		"lastFinishedAt":    at(row.LastFinishedAt),
		"cleanAt":           at(row.CleanAt),
		"compared":          row.LastCompared,
		"mismatched":        row.LastMismatched,
		"repaired":          row.LastRepaired,
		"disagreeing":       row.LastDisagreeing,
	}, i.clock())
}
