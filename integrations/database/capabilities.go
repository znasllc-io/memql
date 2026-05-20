// Package database provides a database management IntegrationProvider.
// The database connection itself remains a core component; this integration
// exposes management operations (health, stats) to the MemQL DSL.
package database

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/uptrace/bun"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// DatabaseIntegration exposes database management operations as DSL-callable capabilities.
type DatabaseIntegration struct {
	dbGetter func() *bun.DB
}

// NewDatabaseIntegration creates a database integration.
// The dbGetter returns the current bun.DB handle (supports reconnection).
func NewDatabaseIntegration(dbGetter func() *bun.DB) *DatabaseIntegration {
	return &DatabaseIntegration{dbGetter: dbGetter}
}

// IntegrationName returns the stable identifier.
func (d *DatabaseIntegration) IntegrationName() string {
	return "database"
}

// Capabilities returns DSL-callable database management operations.
func (d *DatabaseIntegration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "healthCheck",
			Description: "Check database connectivity. Returns healthy/unhealthy status and response time.",
			Handler:     d.handleHealthCheck,
			ArgsSchema:  map[string]string{},
		},
		{
			Name:        "stats",
			Description: "Return database connection pool statistics (open, idle, in use, wait count).",
			Handler:     d.handleStats,
			ArgsSchema:  map[string]string{},
		},
	}
}

func (d *DatabaseIntegration) handleHealthCheck(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	db := d.dbGetter()
	if db == nil {
		return nil, fmt.Errorf("database not available")
	}

	start := time.Now()
	err := db.PingContext(ctx)
	elapsed := time.Since(start)

	status := "healthy"
	errMsg := ""
	if err != nil {
		status = "unhealthy"
		errMsg = err.Error()
	}

	payloadBytes, _ := json.Marshal(map[string]any{
		"status":     status,
		"responseMs": elapsed.Milliseconds(),
		"error":      errMsg,
		"checkedAt":  time.Now().UTC().Format(time.RFC3339),
	})

	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("db-health:%d", time.Now().UnixNano()),
		Concept:   "integration:database:health",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payloadBytes,
	}}, nil
}

func (d *DatabaseIntegration) handleStats(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	db := d.dbGetter()
	if db == nil {
		return nil, fmt.Errorf("database not available")
	}

	sqlDB := db.DB
	stats := sqlDB.Stats()

	payloadBytes, _ := json.Marshal(map[string]any{
		"maxOpen":      stats.MaxOpenConnections,
		"open":         stats.OpenConnections,
		"inUse":        stats.InUse,
		"idle":         stats.Idle,
		"waitCount":    stats.WaitCount,
		"waitDuration": stats.WaitDuration.String(),
		"queriedAt":    time.Now().UTC().Format(time.RFC3339),
	})

	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("db-stats:%d", time.Now().UnixNano()),
		Concept:   "integration:database:stats",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payloadBytes,
	}}, nil
}
