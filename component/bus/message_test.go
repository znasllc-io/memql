package bus

import (
	"testing"
)

func TestNewMessage(t *testing.T) {
	msg := NewMessage()

	if msg.MessageId == "" {
		t.Error("expected non-empty message_id")
	}
	if msg.CorrelationId == "" {
		t.Error("expected non-empty correlation_id")
	}
	if msg.CorrelationId != msg.MessageId {
		t.Error("expected correlation_id to equal message_id for new messages")
	}
	if msg.CreatedAt == nil {
		t.Error("expected non-nil created_at")
	}
	if msg.Metadata == nil {
		t.Error("expected non-nil metadata map")
	}
}

func TestNewMessageUniqueIds(t *testing.T) {
	msg1 := NewMessage()
	msg2 := NewMessage()

	if msg1.MessageId == msg2.MessageId {
		t.Error("expected unique message IDs")
	}
}

func TestNewCorrelatedMessage(t *testing.T) {
	parent := NewMessage()
	parent.CorrelationId = "trace-123"

	child := NewCorrelatedMessage(parent)

	if child.CorrelationId != "trace-123" {
		t.Errorf("expected correlation_id=%q, got %q", "trace-123", child.CorrelationId)
	}
	if child.CorrelateTo != parent.MessageId {
		t.Errorf("expected correlate_to=%q, got %q", parent.MessageId, child.CorrelateTo)
	}
	if child.MessageId == parent.MessageId {
		t.Error("expected child to have its own message_id")
	}
}

func TestNewCorrelatedMessageNilParent(t *testing.T) {
	child := NewCorrelatedMessage(nil)

	if child.MessageId == "" {
		t.Error("expected non-empty message_id even with nil parent")
	}
	// With nil parent, correlation_id should be own message_id
	if child.CorrelationId != child.MessageId {
		t.Error("expected correlation_id to equal own message_id with nil parent")
	}
}
