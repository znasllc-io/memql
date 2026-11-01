//go:build darwin || linux

package modelcall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/znasllc-io/memql/cockpit/internal/worker/capacity"
	"github.com/znasllc-io/memql/cockpit/internal/worker/models"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

type failedDeliveryRecorder struct{ *recorder }

func (*failedDeliveryRecorder) SendModelCallDelta(string, uint64, string, bool) error {
	return errors.New("cluster delivery failed")
}

func terminalTestManager(root string, info models.Info) *Manager {
	limiter := NewLimiter()
	limiter.resources = resourcesForTest(func() (memoryHeadroom, error) {
		return memoryHeadroom{Total: 64 * gib, Available: 40 * gib}, nil
	})
	limiter.resources.leaseRoot = func() (string, error) { return root, nil }
	return NewManager(Options{Inventory: inventoryWith(info), Limiter: limiter})
}

// Exercise the manager with real runtime clients and separate manager-local
// limiters. Only the on-disk witness connects a later manager to the first call.
// A call with a terminal response releases the machine at once, whatever its
// error; one without (a cancel, a truncated or undelivered stream) fences the
// next manager until its own deadline. That the fence then ends is
// TestACancelledCallFencesTheMachineUntilItsDeadline's.
func TestManagerReleasesOnlyCompletedRuntimeErrors(t *testing.T) {
	for _, tc := range []struct {
		name, body, cause                         string
		schema, rejectDelivery, cancel, completed bool
	}{
		{name: "schema validation", body: `{"message":{"content":"invalid JSON"},"done":true}`, schema: true, cause: "does not match", completed: true},
		{name: "terminal delivery", body: `{"message":{"content":"answer"},"done":true}`, rejectDelivery: true, cause: "delivery failed", completed: true},
		{name: "delivery before terminal", body: `{"message":{"content":"partial"},"done":false}` + "\n" + ollamaDoneFrame, rejectDelivery: true, cause: "delivery failed"},
		{name: "truncated response", body: `{"message":{"content":"partial"},"done":false}`, cause: "without a completion"},
		{name: "cancelled response", cancel: true, cause: "cancel"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			var calls atomic.Int32
			started := make(chan struct{})
			var once sync.Once
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/ps" {
					fmt.Fprint(w, `{"models":[]}`)
					return
				}
				if r.URL.Path != "/api/chat" {
					t.Errorf("unexpected request %s", r.URL.Path)
					return
				}
				calls.Add(1)
				if _, err := os.Stat(filepath.Join(root, capacity.InferenceActivityFile)); err != nil {
					t.Error("runtime started without durable activity witness", err)
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				drain(r)
				once.Do(func() { close(started) })
				if tc.cancel {
					<-r.Context().Done()
					return
				}
				if tc.schema && body["format"] != nil {
					w.WriteHeader(http.StatusNotImplemented)
					fmt.Fprint(w, `{"error":"structured output is unavailable"}`)
					return
				}
				fmt.Fprintln(w, tc.body)
			}))
			defer srv.Close()
			info := ollamaModel(srv.URL, "m", models.Attributes{StructuredOutput: true, MaxConcurrent: 1})
			manager := terminalTestManager(root, info)
			t.Cleanup(func() { manager.StopAll("test cleanup") })
			rec := newRecorder()
			var sender Sender = rec
			if tc.rejectDelivery {
				sender = &failedDeliveryRecorder{rec}
			}
			request := start("first", "m", KindChat)
			if tc.schema {
				request.ResponseFormatSchema = []byte(reviewSchema)
			}
			manager.Start(context.Background(), sender, request)
			if tc.cancel {
				awaitRequest(t, started)
				manager.Cancel(&memqlv1.ModelCallCancel{RequestId: "first"})
			}
			end := rec.wait(t)
			if end.ErrorCode == "" || !strings.Contains(strings.ToLower(end.Error), tc.cause) {
				t.Fatalf("original call failure lost: %+v", end)
			}
			_, witnessErr := os.Stat(filepath.Join(root, capacity.InferenceActivityFile))
			if tc.completed != os.IsNotExist(witnessErr) {
				t.Fatalf("completed=%t, witness error=%v", tc.completed, witnessErr)
			}
			before := calls.Load()
			other := terminalTestManager(root, info)
			t.Cleanup(func() { other.StopAll("test cleanup") })
			next := newRecorder()
			other.Start(context.Background(), next, start("next", "m", KindChat))
			nextEnd := next.wait(t)
			if tc.completed {
				if nextEnd.ErrorCode != "" || calls.Load() != before+1 {
					t.Fatalf("completed failure fenced the next manager: %+v calls=%d", nextEnd, calls.Load())
				}
			} else if !strings.Contains(nextEnd.Error, "may still be running") || calls.Load() != before {
				t.Fatalf("uncertain call admitted more runtime work before its deadline: %+v calls=%d", nextEnd, calls.Load())
			}
		})
	}
}

func TestCompletedTranscriptionDeliveryFailureReleasesCapacity(t *testing.T) {
	root := t.TempDir()
	srv, _ := serveRaw(t, "data: "+chunk("transcript")+"\n\ndata: [DONE]\n\n")
	withPayload(t, Payload{Audio: []byte("RIFF"), AudioMediaType: "audio/wav"})
	info := models.Info{ID: "m", Kind: models.KindOpenAICompatible, BaseURL: srv.URL, Allowed: true,
		Attributes: models.Attributes{AudioIn: true, MaxConcurrent: 1}}
	manager := terminalTestManager(root, info)
	t.Cleanup(func() { manager.StopAll("test cleanup") })
	rec := newRecorder()
	manager.Start(context.Background(), &failedDeliveryRecorder{rec}, start("transcribe", "m", KindTranscribe))
	end := rec.wait(t)
	if !strings.Contains(end.Error, "delivery failed") {
		t.Fatalf("delivery failure lost: %+v", end)
	}
	if _, err := os.Stat(filepath.Join(root, capacity.InferenceActivityFile)); !os.IsNotExist(err) {
		t.Fatalf("completed transcription retained activity witness: %v", err)
	}
}

// A cancelled call leaves its fence, because the runtime may still be
// generating, and the fence names the call's own deadline. Before that time the
// next manager's call is refused without reaching the runtime; after it the
// fence is absent, the stale record is removed, and the next call runs.
func TestACancelledCallFencesTheMachineUntilItsDeadline(t *testing.T) {
	root := t.TempDir()
	var calls atomic.Int32
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ps" {
			fmt.Fprint(w, `{"models":[]}`)
			return
		}
		drain(r)
		if calls.Add(1) == 1 {
			// The first call is still generating when the cluster cancels it.
			close(started)
			<-r.Context().Done()
			return
		}
		fmt.Fprintln(w, `{"message":{"content":"answer"},"done":true}`)
	}))
	defer srv.Close()
	info := ollamaModel(srv.URL, "m", models.Attributes{MaxConcurrent: 1})
	path := filepath.Join(root, capacity.InferenceActivityFile)

	first := terminalTestManager(root, info)
	t.Cleanup(func() { first.StopAll("test cleanup") })
	rec := newRecorder()
	request := start("first", "m", KindChat)
	request.Limits.TimeoutSeconds = 2
	began := time.Now()
	first.Start(context.Background(), rec, request)
	awaitRequest(t, started)
	first.Cancel(&memqlv1.ModelCallCancel{RequestId: "first"})
	if end := rec.wait(t); end.ErrorCode != CodeCancelled {
		t.Fatalf("the first call ended %+v, want cancelled", end)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("a cancelled call cleared its fence", err)
	}
	var witness struct {
		Until string `json:"until"`
	}
	if err := json.Unmarshal(data, &witness); err != nil {
		t.Fatalf("the fence %s: %v", data, err)
	}
	until, err := time.Parse(time.RFC3339, witness.Until)
	if err != nil || until.Before(began.Add(2*time.Second)) || until.After(time.Now().Add(2*time.Second)) {
		t.Fatalf("the fence ends %q, want the call's own deadline, 2s after it began", witness.Until)
	}

	before := calls.Load()
	second := terminalTestManager(root, info)
	t.Cleanup(func() { second.StopAll("test cleanup") })
	next := newRecorder()
	second.Start(context.Background(), next, start("second", "m", KindChat))
	if end := next.wait(t); !strings.Contains(end.Error, "may still be running") || calls.Load() != before {
		t.Fatalf("before its deadline the fence admitted more runtime work: %+v calls=%d", end, calls.Load())
	}
	if after, err := os.ReadFile(path); err != nil || string(after) != string(data) {
		t.Fatal("a fence still in force was changed", err)
	}

	time.Sleep(time.Until(until) + 100*time.Millisecond)
	third := terminalTestManager(root, info)
	t.Cleanup(func() { third.StopAll("test cleanup") })
	last := newRecorder()
	third.Start(context.Background(), last, start("third", "m", KindChat))
	if end := last.wait(t); end.ErrorCode != "" || calls.Load() != before+1 {
		t.Fatalf("after its deadline the fence still refused: %+v calls=%d", end, calls.Load())
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("the completed call left a fence: %v", err)
	}
}

// A record that names no end of its own (an older Cockpit's) ends
// capacity.InferenceFenceBound after it was written. That bound must outlast
// the deadline a call is given when its envelope names none, or an upgrade
// could clear the fence of a call still inside its default deadline.
func TestTheInferenceFenceBoundOutlastsTheDefaultCall(t *testing.T) {
	if capacity.InferenceFenceBound <= DefaultTimeout {
		t.Fatalf("InferenceFenceBound = %s, want it longer than a call's default deadline, %s", capacity.InferenceFenceBound, DefaultTimeout)
	}
}
