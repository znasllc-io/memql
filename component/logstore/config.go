// Package logstore is the log store (epic memql#4893, design record
// docs/superpowers/specs/2026-09-03-logs-design.md): every node's log lines
// persisted beside the observability rows in a dedicated log_line hypertable,
// thirty days of retention with an archive before the sweep, and the Go-served
// reads the v1:observability:logLine builtins are executed by.
//
// Four halves, one package:
//
//   - sink.go -- the batching Sink installed by app/database.go on every node
//     type (the component/observe TimescaleSink precedent). It implements
//     core/logger.Sink; the handler chain logger.New builds hands it every
//     line at or above the store floor.
//   - reader.go -- Search / Tail / Sources / Status over bun.
//   - sweep.go -- the nightly retention sweep (archive, then delete, never
//     the second without the first) and the restore.
//   - client.go + plugin.go -- the OS write (logsRecordClient) and the
//     `logs` plug-in that binds all eight builtins to their executors.
//
// The concept declares no row tier and no row of it ever passes graph
// admission: access is a ROLE FLOOR enforced in each handler (design L3), the
// integrationConfigure precedent, because a builtin carries no @requiresRank.
package logstore

import (
	"os"
	"strconv"
	"strings"

	"github.com/znasllc-io/memql/component/envregistry"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/core/logger"
)

// The store's environment. All four are registered `component: observability`,
// `scope: node`, `optional: true` in scripts/secrets/manifest.yaml.
const (
	// EnvRetentionDays is how many UTC days of lines the store keeps before
	// the nightly sweep archives and deletes them. Default 30, clamped 1..365.
	EnvRetentionDays = "MEMQL_LOGS_RETENTION_DAYS"

	// EnvLevel is the store's own floor on this node (debug / info / warn /
	// error / off), independent of the console level. Read once by
	// core/logger; reported here through LevelName.
	EnvLevel = "MEMQL_LOGS_LEVEL"

	// EnvMaxLinesPerSecond bounds the per-node token bucket in front of the
	// queue. Default 2000, clamped 10..100000. Above it lines are DROPPED and
	// counted (memql_logs_dropped_total{reason="rate"}), never queued.
	EnvMaxLinesPerSecond = "MEMQL_LOGS_MAX_LINES_PER_SECOND"

	// EnvArchiveContainer is the blob container the sweep archives expired
	// days into. Defaults to the cluster's own container,
	// MEMQL_AZURE_BLOB_CONTAINER. With neither set the sweep REFUSES to
	// delete anything (design L7: no archive, no delete).
	EnvArchiveContainer = "MEMQL_LOGS_ARCHIVE_CONTAINER"

	// envBlobContainer is the cluster's blob container, the archive default.
	envBlobContainer = "MEMQL_AZURE_BLOB_CONTAINER"
)

// The documented defaults and bounds.
const (
	DefaultRetentionDays = 30
	MinRetentionDays     = 1
	MaxRetentionDays     = 365

	DefaultMaxLinesPerSecond = 2000
	MinMaxLinesPerSecond     = 10
	MaxMaxLinesPerSecond     = 100000
)

// RetentionDays returns MEMQL_LOGS_RETENTION_DAYS clamped to 1..365, or the
// default 30 when unset or unparseable. Never unbounded: a value past the
// bound lands ON the bound rather than being honoured, because "keep logs for
// 10000 days" is a disk decision nobody made on purpose.
func RetentionDays() int {
	return clampedInt(os.Getenv(EnvRetentionDays), DefaultRetentionDays, MinRetentionDays, MaxRetentionDays)
}

// MaxLinesPerSecond returns MEMQL_LOGS_MAX_LINES_PER_SECOND clamped to
// 10..100000, or the default 2000 when unset or unparseable.
func MaxLinesPerSecond() int {
	return clampedInt(os.Getenv(EnvMaxLinesPerSecond), DefaultMaxLinesPerSecond, MinMaxLinesPerSecond, MaxMaxLinesPerSecond)
}

// ArchiveContainer returns the archive container: MEMQL_LOGS_ARCHIVE_CONTAINER,
// else MEMQL_AZURE_BLOB_CONTAINER, else "" -- which the sweep reads as "no
// archive configured" and refuses to delete.
func ArchiveContainer() string {
	if c := strings.TrimSpace(os.Getenv(EnvArchiveContainer)); c != "" {
		return c
	}
	return strings.TrimSpace(os.Getenv(envBlobContainer))
}

// LevelName returns the store floor this node runs with (debug / info / warn /
// error / off), the one parse of MEMQL_LOGS_LEVEL that core/logger made.
func LevelName() string {
	return logger.StoreLevelName()
}

// clampedInt parses an integer env value with a default and inclusive
// bounds. Blank or unparseable is the default; out of range is the nearer
// bound. It takes the VALUE rather than the variable name so every read of
// a registered variable is a literal os.Getenv(EnvX) the registry scanner
// resolves.
func clampedInt(raw string, def, lo, hi int) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

// resolveNodeType is the node type every line this node writes is stamped
// with: MEMQL_NODE_TYPE, defaulting to bff exactly as boot validation does.
func resolveNodeType() string {
	return envregistry.ResolveNodeType()
}

// resolveNodeId is the replica id every line this node writes is stamped
// with: MEMQL_NODE_ID, then the hostname (the per-pod fallback
// component/node/identity.go uses under `fieldRef: metadata.name`), then a
// fresh short id when even the hostname is unavailable -- which it never is
// under Kubernetes.
func resolveNodeId() string {
	if v := strings.TrimSpace(os.Getenv("MEMQL_NODE_ID")); v != "" {
		return v
	}
	if host, err := os.Hostname(); err == nil && strings.TrimSpace(host) != "" {
		return strings.TrimSpace(host)
	}
	return id.NewShortId()
}
