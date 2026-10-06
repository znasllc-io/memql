package steps

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

type parallelLoopProbe func(context.Context, string, map[string]any) error

func (p parallelLoopProbe) Execute(ctx context.Context, step *automations.Step, stepCtx *Context) (*automations.StepResult, error) {
	args, err := stepCtx.Evaluator.ResolveV1Map(ctx, step.Function.Args)
	if err == nil {
		err = p(ctx, step.Function.Name, args)
	}
	status := "success"
	if err != nil {
		status = "failed"
	}
	return &automations.StepResult{StepId: step.ID, Status: status}, err
}

func executeParallelLoop(t *testing.T, ctx context.Context, body string, probe parallelLoopProbe) (*automations.AutomationExecution, error) {
	t.Helper()
	a, err := automations.NewLoader(automations.LoaderOptions{}).CompileSource("@trigger(event=\"probe.fired\")\nautomation boundedLoop {\n"+body+"\n}", "parallel-loop.memql")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	reg := NewRegistry()
	reg.Register(automations.StepTypeFunction, probe)
	ev := events.NewEvent("probe.fired", events.KindMessage, nil)
	return automations.NewExecutor(automations.ExecutorOptions{StepRegistry: reg}).ExecuteWithEvent(ctx, a, "test", &ev)
}

func TestParallelForBoundsActiveWorkAndKeepsItemScopes(t *testing.T) {
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	started := make(chan struct{}, 6)
	release := make(chan struct{})
	var active, peak atomic.Int32
	var mu sync.Mutex
	seen := map[string]int{}
	keys := map[string]string{}
	after := false
	done := make(chan error, 1)
	go func() {
		_, err := executeParallelLoop(t, ctx, `
  values := [0, 1, 2, 3, 4, 5]
  for item in values if item != 4 parallel(2) {
    copied := item * 10
    builtin process(value: copied)
  }
  builtin after()
`, func(ctx context.Context, name string, args map[string]any) error {
			if name == "after" {
				if active.Load() != 0 {
					return fmt.Errorf("children still active at the next stage")
				}
				after = true
				return nil
			}
			n := active.Add(1)
			defer active.Add(-1)
			for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
			}
			started <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			mu.Lock()
			seen[fmt.Sprint(args["value"])]++
			run, _ := common.RunFromContext(ctx)
			keys[fmt.Sprint(args["value"])] = run.StepKey
			mu.Unlock()
			return nil
		})
		done <- err
	}()
	for range 2 {
		select {
		case <-started:
		case err := <-done:
			t.Fatalf("ended before filling both slots: %v", err)
		case <-ctx.Done():
			t.Fatal("parallel loop did not fill both slots")
		}
	}
	if n := active.Load(); n != 2 {
		t.Fatalf("active = %d", n)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if peak.Load() != 2 || !after || len(seen) != 5 {
		t.Fatalf("peak=%d after=%v seen=%v", peak.Load(), after, seen)
	}
	for _, value := range []string{"0", "10", "20", "30", "50"} {
		if seen[value] != 1 {
			t.Errorf("item %s executed %d times", value, seen[value])
		}
	}
	if !strings.HasSuffix(keys["50"], "for_item/5/process") {
		t.Fatalf("filtering changed the original item's execution key: %v", keys)
	}
}

func TestParallelForCancellationJoinsChildrenAndStopsNestedEffects(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := make(chan struct{}, 2)
	var active, forbidden atomic.Int32
	done := make(chan error, 1)
	go func() {
		_, err := executeParallelLoop(t, ctx, `
  for item in [1, 2, 3, 4] parallel(2) {
    builtin waiting(item: item) on error continue
    builtin forbidden()
  } on error continue
  builtin forbidden()
`, func(ctx context.Context, name string, args map[string]any) error {
			if name != "waiting" {
				forbidden.Add(1)
				return nil
			}
			active.Add(1)
			defer active.Add(-1)
			started <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		})
		done <- err
	}()
	for range 2 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("parallel loop did not start")
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	if active.Load() != 0 || forbidden.Load() != 0 {
		t.Fatalf("active=%d later effects=%d", active.Load(), forbidden.Load())
	}
}

func TestParallelForContinueHandlesOrdinaryFailuresOnly(t *testing.T) {
	for _, failure := range []error{fmt.Errorf("ordinary failure"), automations.ErrJournalRequired, &work.HumanWait{}} {
		t.Run(fmt.Sprintf("%T-%s", failure, failure.Error()), func(t *testing.T) {
			var calls, after atomic.Int32
			_, err := executeParallelLoop(t, context.Background(), `
  for item in [1, 2, 3, 4] parallel(2) {
    builtin process(item: item)
  } on error continue
  builtin after()
`, func(ctx context.Context, name string, args map[string]any) error {
				if name == "after" {
					after.Add(1)
					return nil
				}
				calls.Add(1)
				return failure
			})
			if strings.Contains(failure.Error(), "ordinary") {
				if err != nil || calls.Load() != 4 || after.Load() != 1 {
					t.Fatalf("ordinary failures: calls=%d after=%d err=%v", calls.Load(), after.Load(), err)
				}
			} else if after.Load() != 0 || calls.Load() > 2 {
				t.Fatalf("fatal failure continued: calls=%d after=%d err=%v", calls.Load(), after.Load(), err)
			}
		})
	}
}

func TestParallelForAuthoredLimitsAndReturnRefusals(t *testing.T) {
	for _, limit := range []string{"0", "1", "65", "-2", "2.5", "\"2\"", "count", "2, 3"} {
		t.Run(limit, func(t *testing.T) {
			_, err := automations.NewLoader(automations.LoaderOptions{}).CompileSource("automation bad {\n  for item in [1] parallel("+limit+") {\n    builtin process(item: item)\n  }\n}", "bad.memql")
			if err == nil {
				t.Fatal("invalid limit loaded")
			}
		})
	}
	for _, body := range []string{"return item", "if item > 0 {\n return item\n}"} {
		_, err := automations.NewLoader(automations.LoaderOptions{}).CompileSource("automation bad {\n  for item in [1] parallel(2) {\n"+body+"\n  }\n}", "bad.memql")
		if err == nil || !strings.Contains(err.Error(), "body_return_in_parallel") {
			t.Fatalf("parallel return loaded: %v", err)
		}
	}
}
