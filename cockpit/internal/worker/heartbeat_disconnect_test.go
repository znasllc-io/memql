package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// A failed outbound heartbeat need not make Recv return. Keeping that read
// alive leaves a worker permanently silent instead of reaching reconnect.
type failedHeartbeatStream struct{ *scriptStream }

func (s *failedHeartbeatStream) Send(m *memqlv1.WorkerClientMessage) error {
	if m.GetHeartbeat() != nil {
		return errors.New("outbound heartbeat failed")
	}
	return s.scriptStream.Send(m)
}

func TestHeartbeatFailureEndsAReadThatStillWaits(t *testing.T) {
	r, err := NewRunner(Options{Config: testHomeConfig(t, "heartbeat-failure"), Heartbeat: time.Millisecond, Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &failedHeartbeatStream{newScriptStream(0)}
	conn := &Connection{conn: s, cancel: cancel}
	defer conn.Close()
	done := make(chan error, 1)
	go func() {
		_, err := r.runStream(ctx, conn)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("failed heartbeat must end this stream before reconnect")
		}
	case <-time.After(time.Second):
		t.Fatal("heartbeat failed but the worker stayed in Recv without reconnecting")
	}
}
