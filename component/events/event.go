// Package events provides an in-memory pub/sub event bus for MemQL.
package events

import (
	"time"
)

// Kind represents the type of event being emitted.
type Kind int

const (
	// KindUnspecified is the default/zero value.
	KindUnspecified Kind = iota
	// KindTelemetry is for telemetry/metrics events.
	KindTelemetry
	// KindMessage is for general message events.
	KindMessage
	// KindGraphUpdate is a generic graph update event.
	KindGraphUpdate
	// KindAIEvent is a generic AI event.
	KindAIEvent

	// KindNodeCreated is emitted when a graph node is created.
	KindNodeCreated
	// KindNodeDeleted is emitted when a graph node is deleted.
	KindNodeDeleted
	// KindNodeUpdated is emitted when a graph node is updated.
	KindNodeUpdated

	// KindQueryExecuted is emitted after a query completes.
	KindQueryExecuted

	// KindAICompletionStarted is emitted when an AI completion request starts.
	KindAICompletionStarted
	// KindAICompletionFinished is emitted when an AI completion request succeeds.
	KindAICompletionFinished
	// KindAICompletionError is emitted when an AI completion request fails.
	KindAICompletionError

	// KindSessionOpened is emitted when a streaming session is opened.
	KindSessionOpened
	// KindSessionClosed is emitted when a streaming session is closed.
	KindSessionClosed

	// KindAutomationStarted is emitted when an automation begins execution.
	KindAutomationStarted
	// KindAutomationCompleted is emitted when an automation completes successfully.
	KindAutomationCompleted
	// KindAutomationFailed is emitted when an automation fails.
	KindAutomationFailed
	// KindAutomationStepStarted is emitted when an automation step begins.
	KindAutomationStepStarted
	// KindAutomationStepCompleted is emitted when an automation step completes successfully.
	KindAutomationStepCompleted
	// KindAutomationStepFailed is emitted when an automation step fails.
	KindAutomationStepFailed

	// KindToolCalled is emitted when an MCP tool is invoked.
	KindToolCalled
	// KindToolCompleted is emitted when an MCP tool execution succeeds.
	KindToolCompleted
	// KindToolFailed is emitted when an MCP tool execution fails.
	KindToolFailed

	// Cluster node events
	// KindClusterNodeRegistered is emitted when a node registers in the cluster.
	KindClusterNodeRegistered
	// KindClusterNodeDeregistered is emitted when a node leaves the cluster.
	KindClusterNodeDeregistered
	// KindClusterNodeHealthChanged is emitted when a node's health status changes.
	KindClusterNodeHealthChanged

	// Spawn events
	// KindSpawnRequested is emitted when a spawn request is received.
	KindSpawnRequested
	// KindSpawnCompleted is emitted when a node is successfully spawned.
	KindSpawnCompleted
	// KindSpawnFailed is emitted when a spawn request fails.
	KindSpawnFailed

	// KindSystemStartup is emitted when a node completes its bootstrap.
	KindSystemStartup
	// KindSystemShutdown is emitted when a node begins shutting down.
	KindSystemShutdown
)

// String returns a human-readable name for the event kind.
func (k Kind) String() string {
	switch k {
	case KindTelemetry:
		return "telemetry"
	case KindMessage:
		return "message"
	case KindGraphUpdate:
		return "graph_update"
	case KindAIEvent:
		return "ai_event"
	case KindNodeCreated:
		return "node_created"
	case KindNodeDeleted:
		return "node_deleted"
	case KindNodeUpdated:
		return "node_updated"
	case KindQueryExecuted:
		return "query_executed"
	case KindAICompletionStarted:
		return "ai_completion_started"
	case KindAICompletionFinished:
		return "ai_completion_finished"
	case KindAICompletionError:
		return "ai_completion_error"
	case KindSessionOpened:
		return "session_opened"
	case KindSessionClosed:
		return "session_closed"
	case KindAutomationStarted:
		return "automation_started"
	case KindAutomationCompleted:
		return "automation_completed"
	case KindAutomationFailed:
		return "automation_failed"
	case KindAutomationStepStarted:
		return "automation_step_started"
	case KindAutomationStepCompleted:
		return "automation_step_completed"
	case KindAutomationStepFailed:
		return "automation_step_failed"
	case KindToolCalled:
		return "tool_called"
	case KindToolCompleted:
		return "tool_completed"
	case KindToolFailed:
		return "tool_failed"
	case KindClusterNodeRegistered:
		return "cluster_node_registered"
	case KindClusterNodeDeregistered:
		return "cluster_node_deregistered"
	case KindClusterNodeHealthChanged:
		return "cluster_node_health_changed"
	case KindSpawnRequested:
		return "spawn_requested"
	case KindSpawnCompleted:
		return "spawn_completed"
	case KindSpawnFailed:
		return "spawn_failed"
	default:
		return "unspecified"
	}
}

// Topic constants for common event topics.
const (
	// Graph node events
	TopicGraphNodeCreated = "graph.node.created"
	TopicGraphNodeDeleted = "graph.node.deleted"
	TopicGraphNodeUpdated = "graph.node.updated"

	// Query events
	TopicQueryExecuted = "query.executed"

	// AI events
	TopicAICompletionStarted  = "ai.completion.started"
	TopicAICompletionFinished = "ai.completion.finished"
	TopicAICompletionError    = "ai.completion.error"

	// Session events
	TopicSessionOpened = "session.opened"
	TopicSessionClosed = "session.closed"

	// Automation events
	TopicAutomationStarted       = "automation.started"
	TopicAutomationCompleted     = "automation.completed"
	TopicAutomationFailed        = "automation.failed"
	TopicAutomationStepStarted   = "automation.step.started"
	TopicAutomationStepCompleted = "automation.step.completed"
	TopicAutomationStepFailed    = "automation.step.failed"

	// MCP Tool events
	TopicToolCalled    = "tool.called"
	TopicToolCompleted = "tool.completed"
	TopicToolFailed    = "tool.failed"

	// Cluster node events
	TopicClusterNodeRegistered    = "cluster.node.registered"
	TopicClusterNodeDeregistered  = "cluster.node.deregistered"
	TopicClusterNodeHealthChanged = "cluster.node.health_changed"

	// Spawn events
	TopicSpawnRequested = "cluster.spawn.requested"
	TopicSpawnCompleted = "cluster.spawn.completed"
	TopicSpawnFailed    = "cluster.spawn.failed"

	// System lifecycle events
	TopicSystemStartup  = "system.startup"
	TopicSystemShutdown = "system.shutdown"
)

// Event represents an event that can be published to the event bus.
type Event struct {
	// Topic is the hierarchical topic string (e.g., "graph.node.created.Skills").
	Topic string

	// Kind is the type of event.
	Kind Kind

	// Timestamp is when the event occurred.
	Timestamp time.Time

	// Payload contains event-specific data.
	Payload map[string]any

	// Metadata contains additional context (e.g., actor).
	Metadata map[string]string

	// OriginNodeId is the ID of the node where this event was originally
	// published. Empty string means the event originated locally.
	// Set by the distributed EventBridge when forwarding events from peers.
	OriginNodeId string

	// Partition is the data isolation boundary this event belongs to.
	Partition string
}

// NewEvent creates a new event with the current timestamp.
func NewEvent(topic string, kind Kind, payload map[string]any) Event {
	return Event{
		Topic:     topic,
		Kind:      kind,
		Timestamp: time.Now().UTC(),
		Payload:   payload,
		Metadata:  make(map[string]string),
	}
}

// WithMetadata returns a copy of the event with additional metadata.
func (e Event) WithMetadata(key, value string) Event {
	if e.Metadata == nil {
		e.Metadata = make(map[string]string)
	}
	e.Metadata[key] = value
	return e
}

// IsRemote returns true if this event was forwarded from another node.
func (e Event) IsRemote() bool {
	return e.OriginNodeId != ""
}

// Clone returns a deep copy of the event.
// WithPartition returns a copy of the event with the partition set.
func (e Event) WithPartition(partition string) Event {
	e.Partition = partition
	return e
}

func (e Event) Clone() Event {
	clone := Event{
		Topic:        e.Topic,
		Kind:         e.Kind,
		Timestamp:    e.Timestamp,
		OriginNodeId: e.OriginNodeId,
		Partition:    e.Partition,
	}

	if e.Payload != nil {
		clone.Payload = make(map[string]any, len(e.Payload))
		for k, v := range e.Payload {
			clone.Payload[k] = v
		}
	}

	if e.Metadata != nil {
		clone.Metadata = make(map[string]string, len(e.Metadata))
		for k, v := range e.Metadata {
			clone.Metadata[k] = v
		}
	}

	return clone
}
