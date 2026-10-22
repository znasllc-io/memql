package memql

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/core/common"
)

type streamingApps struct {
	stubApps
	call func(context.Context, AppCallRequest) (AppCallResult, error)
}

func (s *streamingApps) Call(ctx context.Context, req AppCallRequest) (AppCallResult, error) {
	return s.call(ctx, req)
}

func TestAppStreamPublishesBeforeCompletionAndCancels(t *testing.T) {
	// Earlier app tests can exhaust the shared guard; this test asserts
	// streaming and cancellation, not the process-wide rate ceiling.
	offTheSharedRateCeiling(t)
	for _, cancelEarly := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "cancel"}[cancelEarly], func(t *testing.T) {
			released := make(chan struct{})
			ended := make(chan struct{})
			apps := &streamingApps{stubApps: stubApps{doors: []AppDoor{runnableDoor(appIdCodex)}}}
			apps.call = func(ctx context.Context, req AppCallRequest) (AppCallResult, error) {
				defer close(ended)
				req.OnContent("\nHello ")
				select {
				case <-released:
					req.OnContent("there\n")
					return AppCallResult{Content: "Hello there"}, nil
				case <-ctx.Done():
					return AppCallResult{}, ctx.Err()
				}
			}
			registry := newProviderRegistry()
			registry.SetAppInference(apps)
			ctx, cancel := context.WithCancel(userCtx("alice"))
			defer cancel()
			entry, _ := registry.EntryForUser(ctx, "alice", AppReferencePrefix+appIdCodex)
			stream, err := entry.Client.(common.ChatStreamProvider).CallChatStream(ctx, []common.ChatMessage{{Role: "user", Content: "hello"}})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case chunk := <-stream:
				if chunk.Content != "Hello" {
					t.Fatalf("first=%+v", chunk)
				}
			case <-time.After(time.Second):
				t.Fatal("waited for completed call before streaming")
			}
			if cancelEarly {
				cancel()
			} else {
				close(released)
			}
			var text strings.Builder
			text.WriteString("Hello")
			done := false
			for chunk := range stream {
				text.WriteString(chunk.Content)
				done = done || chunk.Done
				if chunk.Error != nil && !errors.Is(chunk.Error, context.Canceled) {
					t.Fatal(chunk.Error)
				}
			}
			if !cancelEarly && (text.String() != "Hello there" || !done) {
				t.Fatalf("text=%q done=%v", text.String(), done)
			}
			select {
			case <-ended:
			case <-time.After(time.Second):
				t.Fatal("call outlived cancellation")
			}
		})
	}
}
