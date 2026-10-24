package tools

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// StartPipelineMaintenance reconciles recorded resources even when no new
// build arrives, and even while the cluster connection is unavailable. It
// never enumerates or prunes unowned Docker resources. The caller stops and
// joins this loop when the worker exits.
func StartPipelineMaintenance(ctx context.Context, logger *slog.Logger) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		maintainPipelineResources(ctx, logger, time.Minute, reconcileIdlePipeline)
	}()
	return func() { cancel(); <-done }
}

func maintainPipelineResources(ctx context.Context, logger *slog.Logger, every time.Duration, reconcile func(context.Context) (bool, error)) {
	if logger == nil {
		logger = slog.Default()
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	lastError := ""
	for {
		if ctx.Err() != nil {
			return
		}
		probe, cancel := context.WithTimeout(ctx, 30*time.Second)
		cleaned, err := reconcile(probe)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if message := err.Error(); message != lastError {
				logger.Warn("pipeline cleanup remains pending; build capacity is reserved", "error", message)
				lastError = message
			}
		} else {
			if cleaned {
				logger.Info("pipeline resources reconciled; build capacity is available")
			}
			lastError = ""
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func reconcileIdlePipeline(ctx context.Context) (bool, error) {
	pending, err := hasPipelineAttempt()
	if err != nil || !pending {
		return false, err
	}
	reservation, err := acquirePipelineReservation()
	if errors.Is(err, errPipelineBusy) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer reservation.close()
	if !reservation.dirty {
		return false, nil
	}
	if err := reconcilePipelineReservation(ctx, reservation); err != nil {
		return false, err
	}
	return true, nil
}
