package automations

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestModeSingleParallelAndDefault(t *testing.T) {
	for _, tc := range []struct {
		kind       string
		max, admit int
	}{{"single", 0, 1}, {"parallel", 2, 2}, {"", 0, 5}} {
		t.Run(tc.kind, func(t *testing.T) {
			g := &modeGate{}
			a := &Automation{Name: "a"}
			if tc.kind != "" {
				a.Mode = &ModeConfig{Kind: tc.kind, Max: tc.max}
			}
			for i := 0; i < tc.admit; i++ {
				_, release, err := g.acquire(context.Background(), a, string(rune('a'+i)))
				if err != nil {
					t.Fatal(err)
				}
				defer release()
			}
			if tc.kind != "" {
				_, _, err := g.acquire(context.Background(), a, "refused")
				var refusal *ModeRefusal
				if !errors.As(err, &refusal) {
					t.Fatal(err)
				}
			}
		})
	}
}
func TestModeRestartCancelsPrevious(t *testing.T) {
	g := &modeGate{}
	a := &Automation{Name: "restart", Mode: &ModeConfig{Kind: "restart"}}
	first, release, err := g.acquire(context.Background(), a, "first")
	if err != nil {
		t.Fatal(err)
	}
	second, release2, err := g.acquire(context.Background(), a, "second")
	if err != nil {
		t.Fatal(err)
	}
	defer release2()
	release()
	if first.Err() != context.Canceled || second.Err() != nil {
		t.Fatal("restart failed")
	}
}
func TestModeQueuedFIFOAndCancellation(t *testing.T) {
	g := &modeGate{}
	a := &Automation{Name: "queue", Mode: &ModeConfig{Kind: "queued", Max: 2}}
	_, release, err := g.acquire(context.Background(), a, "first")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan string, 2)
	releases := make(chan func(), 2)
	queue := func(ctx context.Context, id string) {
		go func() {
			_, r, err := g.acquire(ctx, a, id)
			if err != nil {
				done <- "cancelled"
				return
			}
			done <- id
			releases <- r
		}()
	}
	waitCount := func(n int) {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			g.mu.Lock()
			count := len(g.byName[a.Name].waiting)
			g.mu.Unlock()
			if count == n {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("queue did not arrive")
	}
	queue(context.Background(), "second")
	waitCount(1)
	ctx, cancel := context.WithCancel(context.Background())
	queue(ctx, "cancel")
	waitCount(2)
	if _, _, err := g.acquire(context.Background(), a, "overflow"); err == nil {
		t.Fatal("queue overflow admitted")
	}
	cancel()
	if got := <-done; got != "cancelled" {
		t.Fatal(got)
	}
	waitCount(1)
	queue(context.Background(), "third")
	waitCount(2)
	release()
	if got := <-done; got != "second" {
		t.Fatal(got)
	}
	(<-releases)()
	if got := <-done; got != "third" {
		t.Fatal(got)
	}
	(<-releases)()
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.byName) != 0 {
		t.Fatal("idle states leaked")
	}
}
