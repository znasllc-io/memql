package events

import (
	"path"
	"strings"
)

// Match checks if a topic matches a glob pattern.
//
// Patterns support two wildcards:
//   - "*" matches exactly one segment (e.g., "graph.node.*" matches "graph.node.created")
//   - "#" matches zero or more segments (e.g., "graph.#" matches "graph.node.created.Skills")
//
// Wildcards can also appear within a segment for intra-segment matching:
//   - "v1:cognition:*" matches "v1:cognition:utterance" (glob within a dot-segment)
//   - "prefix*" matches "prefixSuffix"
//
// Examples:
//   - Match("graph.node.*", "graph.node.created") → true
//   - Match("graph.node.*", "graph.node.created.Skills") → false
//   - Match("graph.#", "graph.node.created.Skills") → true
//   - Match("graph.node.created", "graph.node.created") → true
//   - Match("*", "graph") → true
//   - Match("#", "graph.node.created") → true
//   - Match("graph.node.created.v1:cognition:*", "graph.node.created.v1:cognition:utterance") → true
func Match(pattern, topic string) bool {
	// "#" matches everything including empty
	if pattern == "#" {
		return true
	}

	if pattern == "" || topic == "" {
		return pattern == topic
	}

	// Exact match shortcut
	if pattern == topic {
		return true
	}

	patternParts := strings.Split(pattern, ".")
	topicParts := strings.Split(topic, ".")

	return matchParts(patternParts, topicParts)
}

// matchParts recursively matches pattern parts against topic parts.
func matchParts(pattern, topic []string) bool {
	pi, ti := 0, 0

	for pi < len(pattern) {
		if pi >= len(pattern) {
			return ti >= len(topic)
		}

		part := pattern[pi]

		switch part {
		case "#":
			// "#" at the end matches everything remaining
			if pi == len(pattern)-1 {
				return true
			}

			// Try matching "#" against 0, 1, 2, ... segments
			for skip := 0; skip <= len(topic)-ti; skip++ {
				if matchParts(pattern[pi+1:], topic[ti+skip:]) {
					return true
				}
			}
			return false

		case "*":
			// "*" must match exactly one segment
			if ti >= len(topic) {
				return false
			}
			pi++
			ti++

		default:
			// Literal or intra-segment glob match
			if ti >= len(topic) {
				return false
			}
			if strings.ContainsAny(part, "*?") {
				// Support glob wildcards within a segment (e.g., "v1:cognition:*" matches "v1:cognition:utterance")
				matched, err := path.Match(part, topic[ti])
				if err != nil || !matched {
					return false
				}
			} else if topic[ti] != part {
				return false
			}
			pi++
			ti++
		}
	}

	// Pattern exhausted, topic must also be exhausted
	return ti >= len(topic)
}

// NormalizePattern ensures a pattern is valid and normalized.
// Returns the normalized pattern or the original if already valid.
func NormalizePattern(pattern string) string {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return "#" // Empty pattern matches everything
	}

	// Remove consecutive dots
	for strings.Contains(pattern, "..") {
		pattern = strings.ReplaceAll(pattern, "..", ".")
	}

	// Trim leading/trailing dots
	pattern = strings.Trim(pattern, ".")

	if pattern == "" {
		return "#"
	}

	return pattern
}

// BuildTopicWithConcept creates a topic string with an optional concept suffix.
// Format: baseTopic.{concept}
// Example: "graph.node.created.v1:cognition:participant"
func BuildTopicWithConcept(baseTopic, concept string) string {
	concept = strings.TrimSpace(concept)
	if concept == "" {
		return baseTopic
	}
	return baseTopic + "." + concept
}

// TopicNodeCreated returns the CDC topic for node creation events for the given concept.
// Example: TopicNodeCreated("v1:cognition:agent") returns "graph.node.created.v1:cognition:agent".
func TopicNodeCreated(conceptId string) string {
	return BuildTopicWithConcept(TopicGraphNodeCreated, conceptId)
}

// TopicNodeUpdated returns the CDC topic for node update events for the given concept.
func TopicNodeUpdated(conceptId string) string {
	return BuildTopicWithConcept(TopicGraphNodeUpdated, conceptId)
}

// TopicNodeDeleted returns the CDC topic for node deletion events for the given concept.
func TopicNodeDeleted(conceptId string) string {
	return BuildTopicWithConcept(TopicGraphNodeDeleted, conceptId)
}
