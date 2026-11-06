package retention

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/num"
	"github.com/znasllc-io/memql/integrations/azureblob"
	"github.com/znasllc-io/memql/integrations/storagehealth"
)

// Use the instance's existing archive destination; no per-node policy copy.
const archiveContainerEnv = "MEMQL_AZURE_BLOB_CONTAINER"

// compactionVolumeEnv is the data volume's size in bytes, for an installation
// whose database-health monitor publishes no report (judgeRoom). Unset, such an
// installation never rewrites a chunk: the pass answers room_unknown.
const compactionVolumeEnv = "MEMQL_CHUNK_COMPACTION_VOLUME_BYTES"

// compactionEnabledEnv is the operator's stop switch for the compaction's
// rewrites. On by default; "false" (any strconv.ParseBool false) stops them, and
// so does a value that is not a boolean at all: somebody set it, and the
// cautious reading of a switch nobody can parse is the one that rewrites
// nothing. Dry runs measure and report either way.
const compactionEnabledEnv = "MEMQL_CHUNK_COMPACTION_ENABLED"

// maintenanceAutomation is the maintenance principal the pass runs as: the one the
// hourly automation of the same name already runs under.
const maintenanceAutomation = "retireDeclaredConceptVersions"

// The compaction's policy defaults and floors. The automation passes its
// thresholds explicitly (dsl/platform/automations.memql); a caller that leaves
// one out gets the default, and none may go below the floor -- a pass that
// rewrote chunks for a few megabytes would spend a night's I/O and a chunk's
// worth of WAL on nothing.
const (
	defaultMinReclaimBytes   = 256 << 20
	minReclaimBytesFloor     = 16 << 20
	defaultMinReclaimPercent = 30
	minReclaimPercentFloor   = 5
	defaultMeasureChunks     = 4
	maxMeasureChunks         = 16
)

type Integration struct {
	context   memql.PluginContext
	archive   Archive
	container string
	// storage reads the database-health report for the compaction's room check.
	storage func(context.Context) (StorageReading, error)
}

func init() {
	memql.RegisterPlugin("retention", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		if pctx.Concepts == nil || pctx.AdmitSourceRow == nil || pctx.InvalidateConcept == nil {
			return nil, fmt.Errorf("retention requires live concepts, row admission and cache invalidation")
		}
		i := &Integration{context: pctx, container: strings.TrimSpace(os.Getenv(archiveContainerEnv)), storage: storageReader(storagehealth.New())}
		if i.container != "" {
			archive, err := azureblob.New(context.Background())
			if err == nil {
				i.archive = archive
			} else if pctx.Logger != nil {
				pctx.Logger.Warn("concept retention archive unavailable; archived policies keep their records", "error", err)
			}
		}
		return i, nil
	})
}

// storageReader adapts the database-health report to the room check's reading.
// The report's figures are floats; a value past int64 saturates (core/num)
// rather than wrapping.
func storageReader(r *storagehealth.Integration) func(context.Context) (StorageReading, error) {
	return func(ctx context.Context) (StorageReading, error) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		got, err := r.Read(ctx)
		if err != nil {
			return StorageReading{}, err
		}
		return StorageReading{
			ObservedAt:        got.ObservedAt,
			DataBytes:         num.ClampFloat64ToInt64(got.DataBytes),
			DataCapacityBytes: num.ClampFloat64ToInt64(got.DataCapacityBytes),
			WALBytes:          num.ClampFloat64ToInt64(got.WALBytes),
			WALCapacityBytes:  num.ClampFloat64ToInt64(got.WALCapacityBytes),
			Issues:            got.Issues,
		}, nil
	}
}

func (*Integration) IntegrationName() string { return "retention" }
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{Name: "sweep", Description: "Archive and expire versions according to each concept's retention declaration. Cluster owner only.", ArgsSchema: map[string]string{"dryRun": "bool -- report the first page per concept without changing data or progress"}, Handler: i.sweep},
		{Name: "compact", Description: "Give back the space retention deletes leave inside CLOSED MemoryNodes and SecretMemoryNodes chunks: measure changed chunks, then rewrite at most one with TimescaleDB reorder_chunk. Cluster owner only.", ArgsSchema: map[string]string{
			"dryRun":            "bool -- measure and report what a pass would do, without rewriting a chunk or recording progress",
			"minReclaimBytes":   "int -- the least a chunk must give back (default 268435456; at least 16777216)",
			"minReclaimPercent": "int -- the least share of the chunk's size it must give back (default 30; 5 to 100)",
			"measureChunks":     "int -- how many changed or unmeasured chunks a pass may scan (default 4; 1 to 16)",
		}, Handler: i.compact},
	}
}
func (i *Integration) sweep(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	actor, ok := auth.AccessFromContext(ctx)
	if !ok || actor == nil || !actor.IsClusterOwner() {
		return nil, fmt.Errorf("concept retention requires a cluster owner")
	}
	dry, _ := args["dryRun"].(bool)
	if i.context.BunDB == nil {
		return nil, fmt.Errorf("concept retention database unavailable")
	}
	// THE PASS RUNS AS THE CLUSTER, WHOEVER ASKED. Each row is admitted before it is
	// archived or deleted (AdmitSourceRow, the gate a read passes), and some concepts
	// are unreadable to a client by any path -- the access codes and their sessions
	// (clientUnreadableConcepts, component/memql) -- so a person at client origin,
	// however senior, would have the first expired session of those refuse the whole
	// batch, and the documented manual run would fail where the hourly one succeeds.
	// The cluster-owner check above is what makes this elevation safe: only a caller
	// who could already ask for the pass gets it, it is the same pass the hourly
	// automation runs, and it reads nothing back to the caller beyond the report.
	maintenance := auth.MaintenanceActor(maintenanceAutomation)
	if maintenance == nil {
		return nil, fmt.Errorf("concept retention has no maintenance principal")
	}
	if actor.UserId != maintenance.UserId && i.context.Logger != nil {
		i.context.Logger.Info("concept retention sweep run by a person, as the maintenance principal", "caller", actor.UserId, "dryRun", dry)
	}
	ctx = auth.ContextWithAccess(ctx, maintenance)
	s := Sweeper{DB: i.context.BunDB(), Concepts: i.context.Concepts, Admit: i.context.AdmitSourceRow, Invalidate: i.context.InvalidateConcept, Archive: i.archive, Container: i.container}
	report, err := s.Run(ctx, dry)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(report)
	if err != nil {
		return nil, err
	}
	return []memorynodes.MemoryNode{{ID: "conceptRetentionSweep", Concept: "integration:retention:sweep", Type: "object", CreatedAt: time.Now().UTC(), Payload: payload}}, nil
}

// compact runs one closed-chunk compaction pass (compact.go).
//
// THE OWNER GATE IS THE WHOLE AUTHORIZATION, and it holds for a dry run too: the
// pass reads no row's content -- it aggregates column sizes and rewrites storage
// as it stands -- so there is no row admission to apply, and what it needs is the
// authority to spend the database's I/O and WAL on the installation's behalf.
// The nightly reclaimClosedChunkSpace automation reaches it as its maintenance
// principal (component/auth/maintenance_actor.go); a person reaches it only as a
// cluster owner. Unlike the sweep above it needs no elevation: it reads nothing
// through the graph.
func (i *Integration) compact(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	actor, ok := auth.AccessFromContext(ctx)
	if !ok || actor == nil || !actor.IsClusterOwner() {
		return nil, fmt.Errorf("chunk compaction requires a cluster owner")
	}
	if i.context.OpenMaintenanceSession == nil {
		return nil, fmt.Errorf("chunk compaction database unavailable")
	}
	opts := compactOptionsFrom(args)
	if !actor.Synthetic && i.context.Logger != nil {
		i.context.Logger.Info("chunk compaction run by a person", "caller", actor.UserId, "dryRun", opts.DryRun)
	}
	disabled, why := compactionDisabled()
	c := Compactor{
		Session:        i.context.OpenMaintenanceSession,
		Storage:        i.storage,
		VolumeBytes:    volumeBytesFromEnv(),
		Tables:         []string{memorynodes.MemoryNodesTable, memorynodes.SecretMemoryNodesTable},
		Logger:         i.context.Logger,
		Disabled:       disabled,
		DisabledReason: why,
	}
	report, err := c.Run(ctx, opts)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(report)
	if err != nil {
		return nil, err
	}
	return []memorynodes.MemoryNode{{ID: "closedChunkCompaction", Concept: "integration:retention:compact", Type: "object", CreatedAt: time.Now().UTC(), Payload: payload}}, nil
}

// compactOptionsFrom reads the builtin's arguments: each threshold the caller's
// value when it gave a usable one, the default otherwise, and never below its
// floor.
func compactOptionsFrom(args map[string]any) CompactOptions {
	dry, _ := args["dryRun"].(bool)
	return CompactOptions{
		DryRun:            dry,
		MinReclaimBytes:   max(argInt64(args, "minReclaimBytes", defaultMinReclaimBytes), minReclaimBytesFloor),
		MinReclaimPercent: min(max(argInt64(args, "minReclaimPercent", defaultMinReclaimPercent), minReclaimPercentFloor), 100),
		MeasureChunks:     num.ClampInt64(min(argInt64(args, "measureChunks", defaultMeasureChunks), maxMeasureChunks)),
	}
}

// argInt64 reads an integer argument with core/num's CALLER-DEFAULT answer: a
// value that is absent, not a whole number, out of range or not positive takes
// def, since none of them is a threshold anybody meant.
func argInt64(args map[string]any, key string, def int64) int64 {
	var n int64
	switch v := args[key].(type) {
	case int:
		n = int64(v)
	case int64:
		n = v
	case float64:
		whole, ok := num.WholeInt64(v)
		if !ok {
			return def
		}
		n = whole
	case json.Number:
		parsed, err := v.Int64()
		if err != nil {
			return def
		}
		n = parsed
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return def
		}
		n = parsed
	default:
		return def
	}
	if n <= 0 {
		return def
	}
	return n
}

// compactionDisabled reads MEMQL_CHUNK_COMPACTION_ENABLED, answering whether
// rewrites are stopped and the sentence a stopped pass reports.
func compactionDisabled() (bool, string) {
	raw := strings.TrimSpace(os.Getenv(compactionEnabledEnv))
	if raw == "" {
		return false, ""
	}
	enabled, err := strconv.ParseBool(raw)
	switch {
	case err != nil:
		return true, fmt.Sprintf("%s=%q is not a boolean, so no chunk is rewritten until it reads true", compactionEnabledEnv, raw)
	case !enabled:
		return true, compactionEnabledEnv + " is false: the operator stopped chunk rewrites"
	}
	return false, ""
}

// volumeBytesFromEnv reads MEMQL_CHUNK_COMPACTION_VOLUME_BYTES: a positive whole
// number of bytes, or 0 (unset, or a value that is not one -- a mistyped size
// must not pass for a configured volume).
func volumeBytesFromEnv() int64 {
	raw := strings.TrimSpace(os.Getenv(compactionVolumeEnv))
	if raw == "" {
		return 0
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}
