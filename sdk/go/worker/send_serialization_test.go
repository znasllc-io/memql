package worker

import (
	"errors"
	"io"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// overlapDetectingStream stands where grpc-go's clientStream would be and
// enforces the rule this file exists for, in grpc-go's own words
// (google.golang.org/grpc/stream.go, ClientStream): "it is not safe to
// call SendMsg on the same stream in different goroutines. It is also not
// safe to call CloseSend concurrently with SendMsg."
//
// Every Send and CloseSend bumps an in-flight counter on entry, yields so
// a racing writer gets its turn, and drops the counter on exit. An entry
// that finds the counter already non-zero is an OVERLAP, which is the
// defect, and it fails the test with or without -race. The frame log is
// appended WITHOUT a lock on purpose: under -race an unserialized pair of
// writers is reported as a data race on it, which is the same fact stated
// by the detector instead of the counter. A CloseSend lands in the same
// log as a nil frame, so a CloseSend racing a Send is a race on the same
// memory.
type overlapDetectingStream struct {
	// The embedded interface is nil. Header / Trailer / Context / SendMsg /
	// RecvMsg are never reached: Connection calls Send, Recv and CloseSend
	// and nothing else.
	grpc.ClientStream

	inFlight atomic.Int32
	overlaps atomic.Int32
	// sendErr, when set, is what Send answers instead of recording.
	sendErr error

	frames []*memqlv1.WorkerClientMessage // a nil entry is a CloseSend
}

func (s *overlapDetectingStream) enter() {
	if s.inFlight.Add(1) != 1 {
		s.overlaps.Add(1)
	}
	runtime.Gosched()
}

func (s *overlapDetectingStream) leave() { s.inFlight.Add(-1) }

func (s *overlapDetectingStream) Send(m *memqlv1.WorkerClientMessage) error {
	s.enter()
	defer s.leave()
	if s.sendErr != nil {
		return s.sendErr
	}
	s.frames = append(s.frames, m)
	return nil
}

func (s *overlapDetectingStream) CloseSend() error {
	s.enter()
	defer s.leave()
	s.frames = append(s.frames, nil)
	return nil
}

func (s *overlapDetectingStream) Recv() (*memqlv1.WorkerServerMessage, error) {
	return nil, io.EOF
}

func (s *overlapDetectingStream) closes() int {
	n := 0
	for _, f := range s.frames {
		if f == nil {
			n++
		}
	}
	return n
}

// The three writers the cockpit runs on one stream at once: the heartbeat
// goroutine, the recv goroutine answering a Ping, and a model call's delta
// stream (memql-cockpit internal/worker/loop.go and modelcall/session.go).
func heartbeatFrame(i int) *memqlv1.WorkerClientMessage {
	return &memqlv1.WorkerClientMessage{Payload: &memqlv1.WorkerClientMessage_Heartbeat{
		Heartbeat: &memqlv1.Heartbeat{Ts: timestamppb.Now(), ActiveCallsTotal: uint32(i)},
	}}
}

func pongFrame(i int) *memqlv1.WorkerClientMessage {
	return &memqlv1.WorkerClientMessage{Payload: &memqlv1.WorkerClientMessage_Pong{
		Pong: &memqlv1.Pong{RequestId: "ping-" + strconv.Itoa(i), SentAt: timestamppb.Now(), ReceivedAt: timestamppb.Now()},
	}}
}

func deltaFrame(i int) *memqlv1.WorkerClientMessage {
	return &memqlv1.WorkerClientMessage{Payload: &memqlv1.WorkerClientMessage_ModelCallDelta{
		ModelCallDelta: &memqlv1.ModelCallDelta{RequestId: "call-1", Seq: uint64(i), Content: "tok", Keepalive: i%7 == 0},
	}}
}

const framesPerWriter = 500

func TestSendIsSerializedAcrossWriters(t *testing.T) {
	stream := &overlapDetectingStream{}
	conn := &Connection{stream: stream}

	var wg sync.WaitGroup
	writer := func(frame func(int) *memqlv1.WorkerClientMessage) {
		defer wg.Done()
		for i := 0; i < framesPerWriter; i++ {
			if err := conn.Send(frame(i)); err != nil {
				t.Errorf("Send: %v", err)
				return
			}
		}
	}
	wg.Add(3)
	go writer(heartbeatFrame)
	go writer(pongFrame)
	go writer(deltaFrame)
	wg.Wait()

	if n := stream.overlaps.Load(); n != 0 {
		t.Fatalf("%d Send calls entered the stream while another was in flight; grpc-go forbids concurrent SendMsg on one stream", n)
	}
	if got, want := len(stream.frames), 3*framesPerWriter; got != want {
		t.Fatalf("stream recorded %d frames, want %d", got, want)
	}
}

func TestCloseSendIsSerializedWithSend(t *testing.T) {
	stream := &overlapDetectingStream{}
	conn := &Connection{stream: stream}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < framesPerWriter; i++ {
			if err := conn.Send(heartbeatFrame(i)); err != nil {
				// The connection was closed under this writer: the
				// expected end.
				return
			}
		}
	}()
	runtime.Gosched()
	conn.Close()
	wg.Wait()

	if n := stream.overlaps.Load(); n != 0 {
		t.Fatalf("CloseSend overlapped a Send %d time(s); grpc-go forbids CloseSend concurrently with SendMsg", n)
	}
	if got := stream.closes(); got != 1 {
		t.Fatalf("CloseSend was called %d times, want exactly 1", got)
	}
	if err := conn.Send(heartbeatFrame(0)); err == nil {
		t.Fatal("Send after Close succeeded; want an error, and no frame on a half-closed stream")
	}
	// Close is idempotent: a second one must not half-close the stream
	// again (the cockpit closes from its runner, its re-advertise path and
	// its shutdown path, and any of them can be second).
	conn.Close()
	if got := stream.closes(); got != 1 {
		t.Fatalf("CloseSend was called %d times after a second Close, want exactly 1", got)
	}
}

func TestAFailedSendIsStickyAndNamesTheFirstError(t *testing.T) {
	boom := errors.New("transport is closing")
	stream := &overlapDetectingStream{}
	conn := &Connection{stream: stream}

	if err := conn.Send(heartbeatFrame(0)); err != nil {
		t.Fatalf("first Send: %v", err)
	}
	stream.sendErr = boom
	if err := conn.Send(heartbeatFrame(1)); !errors.Is(err, boom) {
		t.Fatalf("failing Send = %v, want %v", err, boom)
	}
	stream.sendErr = nil
	if err := conn.Send(heartbeatFrame(2)); !errors.Is(err, boom) {
		t.Fatalf("Send after a failure = %v, want the first error %v, sticky: on error SendMsg has aborted the stream", err, boom)
	}
	if got := len(stream.frames); got != 1 {
		t.Fatalf("stream recorded %d frames after the failure, want 1: a dead stream is not written again", got)
	}
}
