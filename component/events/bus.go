package events

import (
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/visionarys-io/memql/core/common"
	"github.com/visionarys-io/memql/core/env"
	"github.com/visionarys-io/memql/core/logger"
)

// ComponentName identifies the event bus in logs and environment variable lookups.
const ComponentName = common.ComponentName("eventBus")

// NewLogger creates a logger for the event bus using core.NewLogger.
// This ensures proper color support and component-level enable/disable via environment variables.
func NewLogger() *slog.Logger {
	return logger.New(ComponentName, os.Stdout, resolveLoggerLevel())
}

// resolveLoggerLevel reads the log level from environment variables.
func resolveLoggerLevel() slog.Level {
	reader := env.NewEnvReader("EVENT_BUS")
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

// Handler is called when an event matches a subscription pattern.
type Handler func(event Event)

// subscription represents a registered subscription.
type subscription struct {
	id      string
	name    string // Human-readable name for logging (e.g., "automation:bootstrapUser")
	pattern string
	handler Handler
	closed  atomic.Bool
}

// SubscribeOption configures a subscription.
type SubscribeOption func(*subscribeConfig)

type subscribeConfig struct {
	name string
}

// WithSubscriberName sets a human-readable name for the subscription.
// This name appears in logs to help identify what component created the subscription.
// Example: events.WithSubscriberName("automation:bootstrapUser")
func WithSubscriberName(name string) SubscribeOption {
	return func(cfg *subscribeConfig) {
		cfg.name = name
	}
}

// Bus is an in-memory pub/sub event bus with glob pattern matching.
type Bus struct {
	mu            sync.RWMutex
	subscriptions map[string]*subscription
	nextId        uint64
	logger        *slog.Logger
	closed        atomic.Bool
}

// BusOption configures the event bus.
type BusOption func(*busConfig)

type busConfig struct {
	logger *slog.Logger
}

// WithLogger sets the logger for the event bus.
func WithLogger(logger *slog.Logger) BusOption {
	return func(cfg *busConfig) {
		cfg.logger = logger
	}
}

// NewBus creates a new event bus.
// If no logger is provided via WithLogger option, a new logger is created using core.NewLogger
// with the component's configured color and level from environment variables.
func NewBus(opts ...BusOption) *Bus {
	cfg := &busConfig{}
	for _, opt := range opts {
		if opt != nil {
			opt(cfg)
		}
	}

	logger := cfg.logger
	if logger == nil {
		logger = NewLogger()
	}

	return &Bus{
		subscriptions: make(map[string]*subscription),
		logger:        logger,
	}
}

// Subscribe registers a handler for events matching the given pattern.
// Returns an unsubscribe function that should be called to clean up.
//
// Pattern examples:
//   - "graph.node.created" - exact match
//   - "graph.node.*" - matches graph.node.created, graph.node.deleted, etc.
//   - "graph.#" - matches all graph events
//   - "#" - matches all events
//
// Options:
//   - WithSubscriberName("automation:myAutomation") - sets a human-readable name for logging
func (b *Bus) Subscribe(pattern string, handler Handler, opts ...SubscribeOption) func() {
	if b == nil || handler == nil || b.closed.Load() {
		return func() {}
	}

	cfg := &subscribeConfig{}
	for _, opt := range opts {
		if opt != nil {
			opt(cfg)
		}
	}

	pattern = NormalizePattern(pattern)

	id := b.generateId()
	sub := &subscription{
		id:      id,
		name:    cfg.name,
		pattern: pattern,
		handler: handler,
	}

	b.mu.Lock()
	b.subscriptions[id] = sub
	b.mu.Unlock()

	if b.logger != nil {
		logArgs := []any{"id", id, "pattern", pattern}
		if cfg.name != "" {
			logArgs = append(logArgs, "subscriber", cfg.name)
		}
		b.logger.Debug("subscription created", logArgs...)
	}

	return func() {
		b.unsubscribe(id, cfg.name)
	}
}

// Publish sends an event to all matching subscribers asynchronously.
// This method is non-blocking; events are delivered in goroutines.
func (b *Bus) Publish(event Event) {
	if b == nil || b.closed.Load() {
		return
	}

	b.mu.RLock()
	subs := make([]*subscription, 0, len(b.subscriptions))
	for _, sub := range b.subscriptions {
		if sub != nil && !sub.closed.Load() && Match(sub.pattern, event.Topic) {
			subs = append(subs, sub)
		}
	}
	b.mu.RUnlock()

	if len(subs) == 0 {
		return
	}

	if b.logger != nil {
		b.logger.Debug("publishing event",
			"topic", event.Topic,
			"kind", event.Kind.String(),
			"subscribers", len(subs),
		)
	}

	// Clone event per subscriber to prevent concurrent map access
	for _, sub := range subs {
		go b.deliverEvent(sub, event.Clone())
	}
}

// PublishSync sends an event to all matching subscribers synchronously.
// This method blocks until all handlers have completed.
// Useful for testing or when delivery order matters.
func (b *Bus) PublishSync(event Event) {
	if b == nil || b.closed.Load() {
		return
	}

	b.mu.RLock()
	subs := make([]*subscription, 0, len(b.subscriptions))
	for _, sub := range b.subscriptions {
		if sub != nil && !sub.closed.Load() && Match(sub.pattern, event.Topic) {
			subs = append(subs, sub)
		}
	}
	b.mu.RUnlock()

	if len(subs) == 0 {
		return
	}

	cloned := event.Clone()

	var wg sync.WaitGroup
	for _, sub := range subs {
		wg.Add(1)
		go func(s *subscription) {
			defer wg.Done()
			b.deliverEvent(s, cloned)
		}(sub)
	}
	wg.Wait()
}

// Close shuts down the event bus and cleans up all subscriptions.
func (b *Bus) Close() {
	if b == nil || b.closed.Swap(true) {
		return
	}

	b.mu.Lock()
	for id, sub := range b.subscriptions {
		if sub != nil {
			sub.closed.Store(true)
		}
		delete(b.subscriptions, id)
	}
	b.mu.Unlock()

	if b.logger != nil {
		b.logger.Info("event bus closed")
	}
}

// SubscriberCount returns the current number of active subscriptions.
func (b *Bus) SubscriberCount() int {
	if b == nil {
		return 0
	}
	b.mu.RLock()
	count := len(b.subscriptions)
	b.mu.RUnlock()
	return count
}

// unsubscribe removes a subscription by ID.
func (b *Bus) unsubscribe(id string, name string) {
	if b == nil || id == "" {
		return
	}

	b.mu.Lock()
	if sub, exists := b.subscriptions[id]; exists {
		if sub != nil {
			sub.closed.Store(true)
		}
		delete(b.subscriptions, id)
	}
	b.mu.Unlock()

	if b.logger != nil {
		logArgs := []any{"id", id}
		if name != "" {
			logArgs = append(logArgs, "subscriber", name)
		}
		b.logger.Debug("subscription removed", logArgs...)
	}
}

// deliverEvent safely calls the subscription handler.
func (b *Bus) deliverEvent(sub *subscription, event Event) {
	if sub == nil || sub.closed.Load() || sub.handler == nil {
		return
	}

	defer func() {
		if r := recover(); r != nil && b.logger != nil {
			logArgs := []any{
				"subscription_id", sub.id,
				"pattern", sub.pattern,
				"topic", event.Topic,
				"panic", r,
			}
			if sub.name != "" {
				logArgs = append(logArgs, "subscriber", sub.name)
			}
			b.logger.Error("event handler panicked", logArgs...)
		}
	}()

	sub.handler(event)
}

// generateId creates a unique subscription ID.
func (b *Bus) generateId() string {
	id := atomic.AddUint64(&b.nextId, 1)
	return string(rune('a'+id%26)) + string(rune('0'+id/26%10)) + string(rune('a'+id/260%26))
}
