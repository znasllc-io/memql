package component

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/visionarys-io/memql/core/common"
	"github.com/visionarys-io/memql/core/env"
	"github.com/visionarys-io/memql/core/logger"
)

type (
	Arg common.CtorArg[config]

	Component struct {
		name      common.ComponentName
		Logger    *slog.Logger
		config    *config
		hooks     common.LifecycleHooks
		isRunning bool
		lifecycle common.Lifecycle
		mu        sync.RWMutex
		readyCh   chan struct{}
	}

	config struct {
		writer io.Writer
		level  slog.Level
	}
)

const (
	engineDefaultOrder = 60
)

func OptionalCtorArg(name string, fn func(*config)) Arg {
	return common.NewCtorArg(name, true, fn)
}

func RequiredCtorArg(name string, fn func(*config)) Arg {
	return common.NewCtorArg(name, false, fn)
}

func New(componentName common.ComponentName, args ...Arg) (*Component, error) {
	if strings.TrimSpace(string(componentName)) == "" {
		return nil, fmt.Errorf("component name is required")
	}

	cfg := defaultConfig()

	for _, arg := range args {
		if arg == nil {
			continue
		}
		arg.Apply(cfg)
	}

	cfg.level = resolveLoggerLevel(componentName, cfg.level)

	engine := &Component{
		name:      componentName,
		Logger:    logger.New(componentName, cfg.writer, cfg.level),
		config:    cfg,
		hooks:     common.LifecycleHooks{},
		lifecycle: common.Lifecycle{},
		readyCh:   make(chan struct{}),
	}

	engine.Logger.Info("component instance created")

	return engine, nil
}

func (e *Component) WithLoggerWriter(w io.Writer) Arg {
	return OptionalCtorArg("logger_writer", func(c *config) {
		if w != nil {
			c.writer = w
		}
	})
}

func (e *Component) WithLoggerLevel(level slog.Level) Arg {
	return OptionalCtorArg("logger_level", func(c *config) {
		c.level = level
	})
}

func (e *Component) Start(ctx context.Context) {
	hooks := e.copyHooks()

	runFn := hooks.Run
	if runFn == nil {
		runFn = func(runCtx context.Context, markStarted func()) error {
			markStarted()
			<-common.EnsureContext(runCtx).Done()
			return nil
		}
	}

	opts := common.LifecycleHooks{
		Prepare: hooks.Prepare,
		Run: func(runCtx context.Context, markStarted func()) error {
			return runFn(runCtx, markStarted)
		},
		OnStarted: func() {
			e.setRunning(true)
			e.closeReady()
			if hooks.OnStarted != nil {
				hooks.OnStarted()
			}
		},
		OnStop: func() {
			if hooks.OnStop != nil {
				hooks.OnStop()
			}
			e.setRunning(false)
		},
	}

	if err := e.lifecycle.Start(ctx, e.Logger, opts); err != nil && !errors.Is(err, common.ErrLifecycleAlreadyRunning) && !errors.Is(err, context.Canceled) {
		e.Logger.Error("failed to start", "error", err)
	}
}

func (e *Component) IsRunning() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.isRunning
}

func (e *Component) Order() int {
	return engineDefaultOrder
}

func (e *Component) ComponentName() common.ComponentName {
	return e.name
}

func (e *Component) Stop(ctx context.Context) {
	if err := e.lifecycle.Stop(ctx, e.Logger); err != nil && !errors.Is(err, common.ErrLifecycleNotRunning) && !errors.Is(err, context.Canceled) {
		e.Logger.Error("failed to stop", "error", err)
	}
}

func (e *Component) ConfigureLifecycle(opts ...common.LifecycleOption) error {
	if e.lifecycle.IsRunning() {
		return common.ErrLifecycleAlreadyRunning
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	for _, opt := range opts {
		if opt == nil {
			continue
		}
		opt(&e.hooks)
	}

	return nil
}

func WithPrepareHook(fn func(context.Context) (context.Context, context.CancelFunc, error)) common.LifecycleOption {
	return func(h *common.LifecycleHooks) {
		h.Prepare = fn
	}
}

func WithRunHook(fn func(context.Context, func()) error) common.LifecycleOption {
	return func(h *common.LifecycleHooks) {
		h.Run = fn
	}
}

func WithOnStartedHook(fn func()) common.LifecycleOption {
	return func(h *common.LifecycleHooks) {
		h.OnStarted = fn
	}
}

func WithOnStopHook(fn func()) common.LifecycleOption {
	return func(h *common.LifecycleHooks) {
		h.OnStop = fn
	}
}

// Ready returns a channel that is closed when the component has finished
// initialization and is ready to accept work.
func (e *Component) Ready() <-chan struct{} {
	return e.readyCh
}

// closeReady closes the readyCh exactly once. Safe to call multiple times.
func (e *Component) closeReady() {
	select {
	case <-e.readyCh:
		// Already closed
	default:
		close(e.readyCh)
	}
}

func (e *Component) setRunning(running bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.isRunning = running
}

func (e *Component) copyHooks() common.LifecycleHooks {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.hooks
}

func defaultConfig() *config {
	return &config{
		writer: os.Stdout,
		level:  slog.LevelInfo,
	}
}

func resolveLoggerLevel(component common.ComponentName, fallback slog.Level) slog.Level {
	prefix := env.EnvPrefixForComponent(component)
	if prefix == "" {
		return fallback
	}

	reader := env.NewEnvReader(prefix)
	for _, key := range loggerLevelKeys() {
		value, ok := reader.String(key)
		if !ok || strings.TrimSpace(value) == "" {
			continue
		}

		var level slog.Level
		if err := level.UnmarshalText([]byte(strings.ToLower(value))); err == nil {
			return level
		}
	}

	return fallback
}

func loggerLevelKeys() []string {
	return []string{
		"CAPABILITIES_LOGGING_LOG_LEVEL",
		"LOGGER_LEVEL",
		"LOG_LEVEL",
	}
}
