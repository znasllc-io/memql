package bus

import (
	"crypto/rand"
	"encoding/hex"
	"sync/atomic"

	busv1 "github.com/visionarys-io/memql/component/bus/gen"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var msgCounter atomic.Uint64

// generateId returns a short unique message ID.
// Uses a monotonic counter combined with random bytes for uniqueness
// across process restarts.
func generateId() string {
	seq := msgCounter.Add(1)
	var buf [4]byte
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:]) + "-" + hex.EncodeToString([]byte{
		byte(seq >> 24), byte(seq >> 16), byte(seq >> 8), byte(seq),
	})
}

// NewMessage creates an InternalMessage with a unique message_id and timestamp.
// The correlation_id is set to the message_id, starting a new trace.
func NewMessage() *busv1.InternalMessage {
	msgId := generateId()
	return &busv1.InternalMessage{
		MessageId:     msgId,
		CorrelationId: msgId,
		CreatedAt:     timestamppb.Now(),
		Metadata:      make(map[string]string),
	}
}

// NewCorrelatedMessage creates an InternalMessage that carries forward
// the correlation_id from a parent message for distributed tracing.
func NewCorrelatedMessage(parent *busv1.InternalMessage) *busv1.InternalMessage {
	msg := NewMessage()
	if parent != nil {
		msg.CorrelationId = parent.CorrelationId
		msg.CorrelateTo = parent.MessageId
	}
	return msg
}
