package node

import "strings"

// Helpers shared by the delivery paths that sit on the durable substrate.
// They lived in chat_reply_delivery.go, which went with the chat-reply
// substrate (epic memql#4988); plan delivery uses both and is now the only
// caller.

// stringField reads a trimmed string out of a decoded event payload. A
// missing key, a nil map and a non-string value all read as empty, which is
// what every caller already treats as "not set".
func stringField(payload map[string]any, key string) string {
	if payload == nil {
		return ""
	}
	if v, ok := payload[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// deliverableOrigin returns the origin node id to stamp on the re-published
// event, so the local EventBridge treats it as remote (IsRemote() == true)
// and does not loop it back onto the mesh. Falls back to this node's own id
// when the deliverable carries none.
func deliverableOrigin(dl Deliverable, self string) string {
	if dl.OriginNode != "" {
		return dl.OriginNode
	}
	return self
}
