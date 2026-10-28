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
			} else if !strings.Contains(nextEnd.Error, "reconciliation") || calls.Load() != before {
				t.Fatalf("uncertain call admitted more runtime work: %+v calls=%d", nextEnd, calls.Load())
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
