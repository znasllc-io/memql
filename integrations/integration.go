// Package integrations provides external service integrations for MemQL.
// Integrations enable MemQL to communicate with third-party services via
// protocol adapters (HTTP webhooks, gRPC streams, WebSockets, etc.).
package integrations

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/env"
	"github.com/znasllc-io/memql/core/logger"
)

// ComponentName identifies this package in logs and environment variable lookups.
const ComponentName = common.ComponentName("integrations")

type (
	// IntegrationArg is a constructor argument for Integration.
	IntegrationArg common.CtorArg[integrationConfig]

	// Integration is the base struct for all integrations.
	// Specific integrations embed this struct.
	Integration struct {
		sync.Mutex
		componentName common.ComponentName
		Logger        *slog.Logger
		HTTPClient    *http.Client
		config        *integrationConfig
		lifecycle     common.Lifecycle
		isRunning     bool
		readyCh       chan struct{}
	}

	// integrationConfig holds configuration for the base integration.
	integrationConfig struct {
		writer            io.Writer
		level             slog.Level
		httpTimeout       time.Duration
		httpTransport     http.RoundTripper
		tickerIntervalMs  int
		maxRetries        int
		retryIntervalMs   int
		enableHealthCheck bool
	}
)

// NewLogger creates a logger for the integrations component using common.NewLogger.
// This ensures proper color support and component-level enable/disable via environment variables.
func NewLogger() *slog.Logger {
	return logger.New(ComponentName, os.Stdout, resolveLoggerLevel())
}

// resolveLoggerLevel reads the log level from environment variables.
func resolveLoggerLevel() slog.Level {
	reader := env.NewEnvReader("INTEGRATIONS")
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

// defaultIntegrationConfig returns the default configuration for Integration.
func defaultIntegrationConfig() *integrationConfig {
	return &integrationConfig{
		writer:            os.Stdout,
		level:             slog.LevelInfo,
		httpTimeout:       30 * time.Second,
		httpTransport:     nil, // Use default transport
		tickerIntervalMs:  30000,
		maxRetries:        3,
		retryIntervalMs:   1000,
		enableHealthCheck: true,
	}
}

// OptionalCtorArg creates an optional constructor argument.
func OptionalCtorArg(name string, fn func(*integrationConfig)) IntegrationArg {
	return newCtorArg(name, true, fn)
}

// RequiredCtorArg creates a required constructor argument.
func RequiredCtorArg(name string, fn func(*integrationConfig)) IntegrationArg {
	return newCtorArg(name, false, fn)
}

func newCtorArg(name string, optional bool, fn func(*integrationConfig)) IntegrationArg {
	return common.NewCtorArg(name, optional, fn)
}

// WithLoggerWriter sets the writer for the logger.
func WithLoggerWriter(w io.Writer) IntegrationArg {
	return OptionalCtorArg("logger_writer", func(c *integrationConfig) {
		if w != nil {
			c.writer = w
		}
	})
}

// WithLoggerLevel sets the log level.
func WithLoggerLevel(level slog.Level) IntegrationArg {
	return OptionalCtorArg("logger_level", func(c *integrationConfig) {
		c.level = level
	})
}

// WithHTTPTimeout sets the HTTP client timeout.
func WithHTTPTimeout(timeout time.Duration) IntegrationArg {
	return OptionalCtorArg("http_timeout", func(c *integrationConfig) {
		if timeout > 0 {
			c.httpTimeout = timeout
		}
	})
}

// WithHTTPTransport sets a custom HTTP transport.
func WithHTTPTransport(transport http.RoundTripper) IntegrationArg {
	return OptionalCtorArg("http_transport", func(c *integrationConfig) {
		c.httpTransport = transport
	})
}

// WithTickerInterval sets the health check ticker interval in milliseconds.
func WithTickerInterval(intervalMs int) IntegrationArg {
	return OptionalCtorArg("ticker_interval_ms", func(c *integrationConfig) {
		if intervalMs > 0 {
			c.tickerIntervalMs = intervalMs
		}
	})
}

// WithMaxRetries sets the maximum number of retries for failed requests.
func WithMaxRetries(retries int) IntegrationArg {
	return OptionalCtorArg("max_retries", func(c *integrationConfig) {
		if retries >= 0 {
			c.maxRetries = retries
		}
	})
}

// WithRetryInterval sets the retry interval in milliseconds.
func WithRetryInterval(intervalMs int) IntegrationArg {
	return OptionalCtorArg("retry_interval_ms", func(c *integrationConfig) {
		if intervalMs > 0 {
			c.retryIntervalMs = intervalMs
		}
	})
}

// WithHealthCheck enables or disables health checks.
func WithHealthCheck(enabled bool) IntegrationArg {
	return OptionalCtorArg("enable_health_check", func(c *integrationConfig) {
		c.enableHealthCheck = enabled
	})
}

// NewIntegration creates a new base Integration instance.
func NewIntegration(componentName common.ComponentName, args ...IntegrationArg) (*Integration, error) {
	cfg := defaultIntegrationConfig()

	for _, arg := range args {
		if arg == nil {
			continue
		}
		arg.Apply(cfg)
	}

	// Create HTTP client
	transport := cfg.httpTransport
	if transport == nil {
		transport = http.DefaultTransport
	}

	httpClient := &http.Client{
		Timeout:   cfg.httpTimeout,
		Transport: transport,
	}

	i := &Integration{
		componentName: componentName,
		Logger:        logger.New(componentName, cfg.writer, cfg.level),
		HTTPClient:    httpClient,
		config:        cfg,
		isRunning:     false,
		readyCh:       make(chan struct{}),
	}

	i.Logger.Info("instance created")

	return i, nil
}

// Start begins the integration lifecycle.
func (i *Integration) Start(ctx context.Context) {
	if i == nil {
		return
	}

	if err := i.lifecycle.Start(ctx, i.Logger, common.LifecycleHooks{
		Run: func(runCtx context.Context, markStarted func()) error {
			return i.run(runCtx, markStarted)
		},
		OnStarted: func() {
			select {
			case <-i.readyCh:
			default:
				close(i.readyCh)
			}
		},
		OnStop: func() {
			i.Lock()
			i.isRunning = false
			i.Unlock()
		},
	}); err != nil && err != common.ErrLifecycleAlreadyRunning {
		i.Logger.Error("failed to start", "error", err)
	}
}

// Stop halts the integration lifecycle.
func (i *Integration) Stop(ctx context.Context) {
	if i == nil {
		return
	}

	if err := i.lifecycle.Stop(ctx, i.Logger); err != nil && err != common.ErrLifecycleNotRunning {
		i.Logger.Error("failed to stop", "error", err)
	}
}

// IsRunning reports whether the integration is active.
func (i *Integration) IsRunning() bool {
	if i == nil {
		return false
	}

	i.Lock()
	defer i.Unlock()
	return i.isRunning
}

// Order returns the startup order for this component.
// Integrations start after automations (150).
func (i *Integration) Order() int {
	return 160
}

// ComponentName returns the component identifier.
func (i *Integration) ComponentName() common.ComponentName {
	if i == nil {
		return ""
	}
	return i.componentName
}

// Ready returns a channel that is closed when the integration is ready.
func (i *Integration) Ready() <-chan struct{} {
	return i.readyCh
}

// run is the main lifecycle loop.
func (i *Integration) run(ctx context.Context, markStarted func()) error {
	ctx = common.EnsureContext(ctx)

	i.Lock()
	i.isRunning = true
	i.Unlock()

	markStarted()

	if !i.config.enableHealthCheck {
		// If health check is disabled, just wait for context cancellation
		<-ctx.Done()
		return nil
	}

	ticker := time.NewTicker(time.Duration(i.config.tickerIntervalMs) * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			// Health check tick - can be overridden by embedded integrations
		}
	}
}

// DoRequest executes an HTTP request with retry logic.
func (i *Integration) DoRequest(ctx context.Context, req *http.Request) (*http.Response, error) {
	if i == nil || i.HTTPClient == nil {
		return nil, ErrIntegrationNotConfigured
	}

	var lastErr error
	for attempt := 0; attempt <= i.config.maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(i.config.retryIntervalMs) * time.Millisecond):
			}
			i.Logger.Debug("retrying request", "attempt", attempt)
		}

		resp, err := i.HTTPClient.Do(req.WithContext(ctx))
		if err != nil {
			lastErr = err
			continue
		}

		// Check for retryable status codes
		if resp.StatusCode >= 500 || resp.StatusCode == 429 {
			lastErr = &HTTPError{StatusCode: resp.StatusCode}
			resp.Body.Close()
			continue
		}

		return resp, nil
	}

	return nil, lastErr
}
