package modelcall

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRuntimeCompletionRequiresObservedStreamTerminator(t *testing.T) {
	deliveryErr := errors.New("cluster output stream closed")
	for _, tc := range []struct {
		name, body                                   string
		openAI, rejectDelivery, completed, wantError bool
	}{
		{name: "ollama terminal", body: ollamaDoneFrame, completed: true},
		{name: "ollama terminal delivery error", body: `{"message":{"content":"answer"},"done":true}`, rejectDelivery: true, completed: true, wantError: true},
		{name: "ollama terminal runtime error", body: `{"error":"generation failed","done":true}`, completed: true, wantError: true},
		{name: "ollama truncated", body: `{"message":{"content":"partial"},"done":false}`, wantError: true},
		{name: "ollama undelivered terminal", body: `{"message":{"content":"partial"},"done":false}` + "\n" + ollamaDoneFrame, rejectDelivery: true, wantError: true},
		{name: "ollama malformed before terminal", body: "{broken}\n" + ollamaDoneFrame, wantError: true},
		{name: "openai terminal", openAI: true, body: openAIDoneStream, completed: true},
		{name: "openai finish reason without terminal", openAI: true, body: strings.ReplaceAll(openAIDoneStream, "data: [DONE]", ""), wantError: true},
		{name: "openai undelivered terminal", openAI: true, body: "data: {\"choices\":[{\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", rejectDelivery: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := serveRaw(t, tc.body)
			var client Client = &ollamaClient{baseURL: srv.URL, http: srv.Client()}
			if tc.openAI {
				client = &openAIClient{baseURL: srv.URL, http: srv.Client()}
			}
			result, err := client.Chat(context.Background(), ChatRequest{Model: "m"}, func(string) error {
				if tc.rejectDelivery {
					return deliveryErr
				}
				return nil
			})
			if result.RuntimeCompleted != tc.completed || (err != nil) != tc.wantError {
				t.Fatalf("completion=%t, error=%v; want completion=%t, error=%t", result.RuntimeCompleted, err, tc.completed, tc.wantError)
			}
			if tc.rejectDelivery && !errors.Is(err, deliveryErr) {
				t.Fatalf("delivery error was lost: %v", err)
			}
		})
	}
}

func TestRuntimeCompletionSurvivesStructuredValidationAndDeliveryErrors(t *testing.T) {
	for _, tc := range []struct {
		name, answer, want string
		rejectDelivery     bool
	}{
		{name: "invalid structured answer", answer: `{"other":"value"}`, want: "does not match"},
		{name: "validated answer delivery failed", answer: `{"reply":"answer"}`, want: "delivery failed", rejectDelivery: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &noFormatRunner{answer: tc.answer}
			srv := runner.serve(t)
			client := &ollamaClient{baseURL: srv.URL, http: srv.Client()}
			result, err := client.Chat(context.Background(), reviewRequest(), func(string) error {
				if tc.rejectDelivery {
					return errors.New("delivery failed")
				}
				t.Error("invalid structured answer was delivered")
				return nil
			})
			if !result.RuntimeCompleted || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("terminal evidence or validation cause lost: result=%+v error=%v", result, err)
			}
			if len(runner.sent()) != 2 {
				t.Fatal("test did not exercise real schema fallback")
			}
		})
	}
	srv, _ := serveRaw(t, `{"message":{"thinking":"private reasoning"},"done":true}`)
	result, err := (&ollamaClient{baseURL: srv.URL, http: srv.Client()}).Chat(context.Background(), reviewRequest(), discardEmit)
	if !result.RuntimeCompleted || err == nil || !strings.Contains(err.Error(), "gave no answer") {
		t.Fatalf("thought-only terminal output: completed=%t error=%v", result.RuntimeCompleted, err)
	}
}

func TestRuntimeCancellationWithoutTerminalIsNotCompletion(t *testing.T) {
	for _, openAI := range []bool{false, true} {
		name := "ollama"
		if openAI {
			name = "openai"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				drain(r)
				body := `{"message":{"content":"partial"},"done":false}` + "\n"
				if openAI {
					body = "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"
				}
				_, _ = io.WriteString(w, body)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer srv.Close()
			var client Client = &ollamaClient{baseURL: srv.URL, http: srv.Client()}
			if openAI {
				client = &openAIClient{baseURL: srv.URL, http: srv.Client()}
			}
			result, err := client.Chat(ctx, ChatRequest{Model: "m"}, func(string) error { cancel(); return nil })
			if result.RuntimeCompleted || !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled transport counted as completion: %+v, %v", result, err)
			}
		})
	}
}
