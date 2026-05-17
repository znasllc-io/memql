package seeder

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"

	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/env"
	"github.com/znasllc-io/memql/core/logger"
)

// ComponentName identifies the concept seeder in logs and environment variable lookups.
const ComponentName = common.ComponentName("conceptSeeder")

type dependency struct {
	runner  *Runner
	logger  *slog.Logger
	running atomic.Bool
	readyCh chan struct{}
}

// NewDependency wraps a Runner in a common.Dependency so it can participate in the service lifecycle.
// If logger is nil, a new logger is created using common.NewLogger with the component's
// configured color and level from environment variables.
func NewDependency(runner *Runner, slogger *slog.Logger) common.Dependency {
	var depLogger *slog.Logger
	if slogger != nil {
		depLogger = slogger
	} else {
		depLogger = logger.New(ComponentName, os.Stdout, resolveLoggerLevel())
	}

	return &dependency{
		runner:  runner,
		logger:  depLogger,
		readyCh: make(chan struct{}),
	}
}

// resolveLoggerLevel reads the log level from environment variables.
func resolveLoggerLevel() slog.Level {
	reader := env.NewEnvReader("CONCEPT_SEEDER")
	for _, key := range loggerLevelKeys() {
		if value, ok := reader.String(key); ok {
			if strings.TrimSpace(value) == "" {
				continue
			}
			var level slog.Level
			if err := level.UnmarshalText([]byte(strings.ToLower(value))); err == nil {
				return level
			}
		}
	}
	return slog.LevelInfo
}

func loggerLevelKeys() []string {
	return []string{
		"CAPABILITIES_LOGGING_LOG_LEVEL",
		"LOGGER_LEVEL",
		"LOG_LEVEL",
	}
}

func (d *dependency) Start(ctx context.Context) {
	if d.runner == nil {
		return
	}

	if d.logger != nil {
		d.logger.Info("concept seeder starting")
	}

	if err := d.runner.Run(ctx); err != nil {
		if d.logger != nil {
			d.logger.Error("concept seeder failed", "error", err)
		}
		d.running.Store(false)
		return
	}

	// Keep running=true after successful completion
	// This is a one-time initialization task - once it completes successfully,
	// the service is healthy and concepts are seeded
	d.running.Store(true)
	select {
	case <-d.readyCh:
	default:
		close(d.readyCh)
	}

	if d.logger != nil {
		d.logger.Info("concept seeder completed")
	}
}

func (d *dependency) Stop(ctx context.Context) {
	// No-op: seeding runs once during Start.
	if d.logger != nil {
		d.logger.Info("concept seeder stop requested")
	}
}

func (d *dependency) IsRunning() bool {
	return d.running.Load()
}

func (d *dependency) Order() int {
	return 60
}

func (d *dependency) ComponentName() common.ComponentName {
	return common.ComponentName("conceptSeeder")
}

// Ready returns a channel that is closed when seeding is complete.
func (d *dependency) Ready() <-chan struct{} {
	return d.readyCh
}
