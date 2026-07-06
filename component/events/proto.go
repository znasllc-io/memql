package events

import (
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// ToProtoEventKind converts an events.Kind to the proto EventKind.
func ToProtoEventKind(k Kind) memqlv1.EventKind {
	switch k {
	case KindTelemetry:
		return memqlv1.EventKind_EVENT_KIND_TELEMETRY
	case KindMessage:
		return memqlv1.EventKind_EVENT_KIND_MESSAGE
	case KindGraphUpdate:
		return memqlv1.EventKind_EVENT_KIND_GRAPH_UPDATE
	case KindAIEvent:
		return memqlv1.EventKind_EVENT_KIND_AI_EVENT
	case KindNodeCreated:
		return memqlv1.EventKind_EVENT_KIND_NODE_CREATED
	case KindNodeDeleted:
		return memqlv1.EventKind_EVENT_KIND_NODE_DELETED
	case KindNodeUpdated:
		return memqlv1.EventKind_EVENT_KIND_NODE_UPDATED
	case KindQueryExecuted:
		return memqlv1.EventKind_EVENT_KIND_QUERY_EXECUTED
	case KindAICompletionStarted:
		return memqlv1.EventKind_EVENT_KIND_AI_COMPLETION_STARTED
	case KindAICompletionFinished:
		return memqlv1.EventKind_EVENT_KIND_AI_COMPLETION_FINISHED
	case KindAICompletionError:
		return memqlv1.EventKind_EVENT_KIND_AI_COMPLETION_ERROR
	case KindSessionOpened:
		return memqlv1.EventKind_EVENT_KIND_SESSION_OPENED
	case KindSessionClosed:
		return memqlv1.EventKind_EVENT_KIND_SESSION_CLOSED
	case KindAutomationStarted:
		return memqlv1.EventKind_EVENT_KIND_AUTOMATION_STARTED
	case KindAutomationCompleted:
		return memqlv1.EventKind_EVENT_KIND_AUTOMATION_COMPLETED
	case KindAutomationFailed:
		return memqlv1.EventKind_EVENT_KIND_AUTOMATION_FAILED
	case KindAutomationStepStarted:
		return memqlv1.EventKind_EVENT_KIND_AUTOMATION_STEP_STARTED
	case KindAutomationStepCompleted:
		return memqlv1.EventKind_EVENT_KIND_AUTOMATION_STEP_COMPLETED
	case KindAutomationStepFailed:
		return memqlv1.EventKind_EVENT_KIND_AUTOMATION_STEP_FAILED
	case KindToolCalled:
		return memqlv1.EventKind_EVENT_KIND_TOOL_CALLED
	case KindToolCompleted:
		return memqlv1.EventKind_EVENT_KIND_TOOL_COMPLETED
	case KindToolFailed:
		return memqlv1.EventKind_EVENT_KIND_TOOL_FAILED
	default:
		return memqlv1.EventKind_EVENT_KIND_UNSPECIFIED
	}
}

// FromProtoEventKind converts a proto EventKind to events.Kind.
func FromProtoEventKind(k memqlv1.EventKind) Kind {
	switch k {
	case memqlv1.EventKind_EVENT_KIND_TELEMETRY:
		return KindTelemetry
	case memqlv1.EventKind_EVENT_KIND_MESSAGE:
		return KindMessage
	case memqlv1.EventKind_EVENT_KIND_GRAPH_UPDATE:
		return KindGraphUpdate
	case memqlv1.EventKind_EVENT_KIND_AI_EVENT:
		return KindAIEvent
	case memqlv1.EventKind_EVENT_KIND_NODE_CREATED:
		return KindNodeCreated
	case memqlv1.EventKind_EVENT_KIND_NODE_DELETED:
		return KindNodeDeleted
	case memqlv1.EventKind_EVENT_KIND_NODE_UPDATED:
		return KindNodeUpdated
	case memqlv1.EventKind_EVENT_KIND_QUERY_EXECUTED:
		return KindQueryExecuted
	case memqlv1.EventKind_EVENT_KIND_AI_COMPLETION_STARTED:
		return KindAICompletionStarted
	case memqlv1.EventKind_EVENT_KIND_AI_COMPLETION_FINISHED:
		return KindAICompletionFinished
	case memqlv1.EventKind_EVENT_KIND_AI_COMPLETION_ERROR:
		return KindAICompletionError
	case memqlv1.EventKind_EVENT_KIND_SESSION_OPENED:
		return KindSessionOpened
	case memqlv1.EventKind_EVENT_KIND_SESSION_CLOSED:
		return KindSessionClosed
	case memqlv1.EventKind_EVENT_KIND_AUTOMATION_STARTED:
		return KindAutomationStarted
	case memqlv1.EventKind_EVENT_KIND_AUTOMATION_COMPLETED:
		return KindAutomationCompleted
	case memqlv1.EventKind_EVENT_KIND_AUTOMATION_FAILED:
		return KindAutomationFailed
	case memqlv1.EventKind_EVENT_KIND_AUTOMATION_STEP_STARTED:
		return KindAutomationStepStarted
	case memqlv1.EventKind_EVENT_KIND_AUTOMATION_STEP_COMPLETED:
		return KindAutomationStepCompleted
	case memqlv1.EventKind_EVENT_KIND_AUTOMATION_STEP_FAILED:
		return KindAutomationStepFailed
	case memqlv1.EventKind_EVENT_KIND_TOOL_CALLED:
		return KindToolCalled
	case memqlv1.EventKind_EVENT_KIND_TOOL_COMPLETED:
		return KindToolCompleted
	case memqlv1.EventKind_EVENT_KIND_TOOL_FAILED:
		return KindToolFailed
	default:
		return KindUnspecified
	}
}

// GraphNodeActionVerb maps a proto GraphNodeAction to its CDC topic verb
// ("created" / "updated" / "deleted"). UNSPECIFIED returns "" + false so
// the caller can reject it inside a populated actions list (an empty list
// is the "all actions" signal and is handled before this is called).
func GraphNodeActionVerb(a memqlv1.GraphNodeAction) (string, bool) {
	switch a {
	case memqlv1.GraphNodeAction_GRAPH_NODE_ACTION_CREATED:
		return "created", true
	case memqlv1.GraphNodeAction_GRAPH_NODE_ACTION_UPDATED:
		return "updated", true
	case memqlv1.GraphNodeAction_GRAPH_NODE_ACTION_DELETED:
		return "deleted", true
	default:
		return "", false
	}
}

// TopicPatternFromSubscriptionKind returns a glob pattern for the given
// subscription kind. The filter is prefixed with the kind's namespace
// (telemetry., message., graph., ai., query., automation.) to produce the
// canonical bus pattern.
//
// Graph topics are 4-segment graph.node.{action}.{concept} (the partition
// segment was retired with #56; see BuildTopicWithConcept in pattern.go),
// so a graph filter is e.g. "node.created.v1:cognition:text:chunk". The
// bus matcher requires exact segment count, so a filter written against
// the retired 5-segment partitioned format never matches any real event.
func TopicPatternFromSubscriptionKind(kind memqlv1.SubscriptionKind, filter string) string {
	filter = NormalizePattern(filter)

	switch kind {
	case memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_TELEMETRY:
		if filter == "#" {
			return "telemetry.#"
		}
		return "telemetry." + filter
	case memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_MESSAGE:
		if filter == "#" {
			return "message.#"
		}
		return "message." + filter
	case memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_GRAPH_EVENTS:
		if filter == "#" {
			return "graph.#"
		}
		return "graph." + filter
	case memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_AI_STREAM:
		if filter == "#" {
			return "ai.#"
		}
		return "ai." + filter
	case memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_QUERY_SPEC:
		if filter == "#" {
			return "query.#"
		}
		return "query." + filter
	case memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_DOMAIN_EVENTS:
		// Domain events use the filter directly (e.g., "hr.checkin.completed", "hr.flag.created")
		// No prefix is added since domain events already include their domain prefix
		if filter == "#" {
			return "#"
		}
		return filter
	case memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_AUTOMATION_EVENTS:
		if filter == "#" {
			return "automation.#"
		}
		return "automation." + filter
	case memqlv1.SubscriptionKind_SUBSCRIPTION_KIND_ALL:
		return "#"
	default:
		// Use the filter as the pattern directly
		return filter
	}
}
