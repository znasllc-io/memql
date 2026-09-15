package conformance

// Loop scenarios are isolated DSL fixtures, unlike the shipped-product suites.
// Each shape proves strict loading, then follows the real events published by
// mutations and publish steps until convergence or a journaled refusal. The
// test-only opaque builtins model a Go integration whose writes the graph
// cannot inspect; all its database writes still use the production engine.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	automationSteps "github.com/znasllc-io/memql/component/automations/steps"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/metrics"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

type loopScenarioSuite struct {
	Description string            `json:"description"`
	Domain      string            `json:"domain"`
	Files       map[string]string `json:"files"`
	Scenarios   []loopScenario    `json:"scenarios"`
}
type loopScenario struct {
	Name              string             `json:"name"`
	Automations       string             `json:"automations"`
	Seed              []scenarioSeed     `json:"seed"`
	Event             *scenarioEvent     `json:"event"`
	OpaqueMutation    string             `json:"opaqueMutation"`
	OpaqueTopic       string             `json:"opaqueTopic"`
	RemoveLoopRefuses bool               `json:"removeLoopRefuses"`
	Expect            loopScenarioExpect `json:"expect"`
}
type loopScenarioExpect struct {
	LoadError       string   `json:"loadError"`
	MessageContains []string `json:"messageContains"`
	Runs            int      `json:"runs"`
	Row             *struct {
		Concept string         `json:"concept"`
		Payload map[string]any `json:"payload"`
	} `json:"row"`
	Counters []loopScenarioCounter `json:"counters"`
	Stop     *struct {
		Automation string `json:"automation"`
		Reason     string `json:"reason"`
		Depth      int    `json:"depth"`
		Cap        int    `json:"cap"`
	} `json:"stop"`
}
type loopScenarioCounter struct {
	Automation string  `json:"automation"`
	Reason     string  `json:"reason"`
	Delta      float64 `json:"delta"`
}

func loadLoopScenarios(t *testing.T) map[string]loopScenarioSuite {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(scenarioRoot, "loops", "*", "scenario.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 3 {
		t.Fatalf("loop corpus has %d shapes, want self-update, mutual-pair and self-publish", len(paths))
	}
	suites := map[string]loopScenarioSuite{}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var s loopScenarioSuite
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&s); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if s.Domain == "" || len(s.Scenarios) < 2 {
			t.Fatalf("%s is empty", path)
		}
		suites[filepath.Base(filepath.Dir(path))] = s
	}
	return suites
}

func loopScenarioTree(s loopScenarioSuite, source string) fstest.MapFS {
	tree := fstest.MapFS{}
	for name, src := range s.Files {
		tree[s.Domain+"/"+name] = &fstest.MapFile{Data: []byte(src)}
	}
	if source != "" {
		tree[s.Domain+"/automations.memql"] = &fstest.MapFile{Data: []byte(source)}
	}
	return tree
}

func TestLoopScenarios(t *testing.T) {
	t.Setenv("MEMQL_AUTOMATION_MAX_CHAIN_DEPTH", "16")
	t.Setenv(memql.AllowSkipsEnvVar, "")
	suites := loadLoopScenarios(t)
	names := make([]string, 0, len(suites))
	for name := range suites {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, shape := range names {
		t.Run(shape, func(t *testing.T) {
			suite := suites[shape]
			_, _, unmount := memqldsl.MountOverlayDomains(corpusQuiet, loopScenarioTree(suite, ""))
			t.Cleanup(func() { unmount(); memoryNodes.ReplaceAll(nil); _, _ = memql.LoadUnifiedConcepts(corpusQuiet) })
			if _, skips, err := memql.LoadUnifiedConceptsWithSkips(corpusQuiet); err != nil || len(skips) > 0 {
				t.Fatalf("fixture concept load: %v, skips: %+v", err, skips)
			}
			env := newEnv(t)
			for _, name := range []string{"loopScenarioOpaqueWrite", "loopScenarioOpaquePublish"} {
				if err := env.Eng.Functions().Upsert(&memql.Function{Name: name, FunctionKind: "builtin", Type: "builtin", Executor: "loopScenario", Origin: "scenario:loops"}); err != nil {
					t.Fatal(err)
				}
			}
			for _, sc := range suite.Scenarios {
				t.Run(sc.Name, func(t *testing.T) {
					loader := automations.NewLoader(automations.LoaderOptions{Logger: corpusQuiet, Registry: env.Registry, Functions: env.Eng.Functions()})
					autos, err := loader.LoadFromTree(loopScenarioTree(suite, sc.Automations))
					if sc.Expect.LoadError != "" {
						if err == nil || !strings.Contains(err.Error(), "["+sc.Expect.LoadError+"]") {
							t.Fatalf("load = %v, want [%s]", err, sc.Expect.LoadError)
						}
						for _, name := range sc.Expect.MessageContains {
							if !strings.Contains(err.Error(), name) {
								t.Fatalf("refusal does not name %s: %v", name, err)
							}
						}
						return
					}
					if err != nil {
						t.Fatalf("load: %v", err)
					}
					if len(autos) == 0 || sc.Expect.Runs < 1 || len(sc.Expect.Counters) == 0 {
						t.Fatal("live scenario must name runs and explicit counter deltas")
					}
					if sc.RemoveLoopRefuses {
						control := regexp.MustCompile(`(?m)^@loop\([^\n]*\)\n`).ReplaceAllString(sc.Automations, "")
						if control == sc.Automations {
							t.Fatal("negative control removed no @loop")
						}
						_, err := loader.LoadFromTree(loopScenarioTree(suite, control))
						if err == nil || !strings.Contains(err.Error(), "[loop_cycle]") {
							t.Fatalf("remove @loop negative control: %v", err)
						}
					}
					t.Run("live", func(t *testing.T) {
						if !env.HasDB {
							if os.Getenv("MEMQL_REQUIRE_DB") == "1" {
								t.Fatal("loop scenarios require Postgres")
							}
							t.Skip("loop scenarios require Postgres")
						}
						runLoopScenario(t, env, sc, autos)
					})
				})
			}
		})
	}
}

// loopScenarioRegistry supplies two deliberately opaque Go builtin handlers.
// The writer delegates to the real mutation executor, preserving ctx's cause;
// the publisher uses the same event envelope contract as an integration.
type loopScenarioRegistry struct {
	inner           automations.StepExecutorRegistry
	mutation, topic string
}

func (r *loopScenarioRegistry) Execute(ctx context.Context, s *automations.Step, scope *automations.StepContext) (*automations.StepResult, error) {
	if s.Function == nil {
		return r.inner.Execute(ctx, s, scope)
	}
	switch s.Function.Name {
	case "loopScenarioOpaqueWrite":
		if r.mutation == "" {
			return nil, fmt.Errorf("opaque writer has no mutation")
		}
		copyStep := *s
		copyFn := *s.Function
		copyFn.Name = r.mutation
		copyFn.Kind = "mutation"
		copyStep.Function = &copyFn
		return r.inner.Execute(ctx, &copyStep, scope)
	case "loopScenarioOpaquePublish":
		payload, err := scope.Evaluator.ResolveV1Map(ctx, s.Function.Args)
		if err != nil {
			return nil, err
		}
		if r.topic == "" {
			return nil, fmt.Errorf("opaque publisher has no topic")
		}
		ev := events.NewEvent(r.topic, events.KindMessage, payload)
		if cause, ok := events.CauseFromContext(ctx); ok {
			ev = ev.WithCause(cause)
		}
		scope.EventBus.Publish(ev)
		return &automations.StepResult{StepId: s.ID, Status: "completed", Result: payload}, nil
	}
	return r.inner.Execute(ctx, s, scope)
}

func runLoopScenario(t *testing.T, env *Env, sc loopScenario, autos []*automations.Automation) {
	t.Helper()
	ctx := auth.ContextWithInternalOrigin(env.Ctx)
	id := fmt.Sprintf("loop-%d", time.Now().UnixNano())
	queue := make(chan events.Event, 256)
	unsub := env.Eng.EventBus().Subscribe("#", func(ev events.Event) {
		for _, a := range autos {
			if events.Match(a.Trigger.Event, ev.Topic) {
				key, _ := ev.Payload["key"].(string)
				rawID, _ := ev.Payload["id"].(string)
				if key == id || rawID == id || strings.HasSuffix(rawID, ":"+id) {
					queue <- ev
				}
				return
			}
		}
	})
	defer unsub()
	counters := map[loopScenarioCounter]float64{}
	for _, counter := range sc.Expect.Counters {
		counters[counter] = metrics.AutomationLoopsStoppedValue(counter.Automation, counter.Reason)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := &loopScenarioRegistry{inner: automationSteps.NewRegistry(), mutation: sc.OpaqueMutation, topic: sc.OpaqueTopic}
	executor := automations.NewExecutor(automations.ExecutorOptions{Logger: quiet, Engine: env.Eng, EventBus: env.Eng.EventBus(), StepRegistry: reg, ChainTrackingEnabled: true, DedupEnabled: true})
	defer executor.Close()
	var runs []string
	defer func() {
		for _, runID := range runs {
			_, _ = env.DB.NewDelete().Model((*memoryNodes.MemoryNode)(nil)).Where("id IN (?)", bun.In([]string{runID, "v1:work:run:" + runID})).Exec(context.Background())
			_, _ = env.DB.NewDelete().Model((*memoryNodes.MemoryNode)(nil)).Where("payload ->> 'runId' = ?", runID).Exec(context.Background())
		}
		_, _ = env.DB.NewDelete().Model((*memoryNodes.MemoryNode)(nil)).Where("payload ->> 'key' = ?", id).Exec(context.Background())
	}()
	for _, seed := range sc.Seed {
		args := map[string]any{}
		for key, value := range seed.Args {
			if value == "{id}" {
				value = id
			}
			args[key] = value
		}
		query := seed.Mutation + "(" + loopCallArgs(t, args) + ")"
		if _, err := env.Eng.Execute(ctx, query); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if sc.Event != nil {
		payload := map[string]any{}
		for key, value := range sc.Event.Payload {
			if value == "{id}" {
				value = id
			}
			payload[key] = value
		}
		env.Eng.EventBus().Publish(events.NewEvent(sc.Event.Topic, events.KindMessage, payload))
	}
	completed, filtered, stopped := 0, 0, 0
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case ev := <-queue:
			for _, a := range autos {
				if !events.Match(a.Trigger.Event, ev.Topic) {
					continue
				}
				if lam := a.Trigger.FilterLambda; lam != nil {
					matches, err := memql.EvalCondition(ctx, lam.Body, memql.MapScope{lam.Params[0]: automations.TriggerRow(&ev), "args": ev.Payload}, memql.EvalOptions{})
					if err != nil {
						t.Fatalf("trigger filter: %v", err)
					}
					if !matches {
						filtered++
						continue
					}
				}
				run, err := executor.ExecuteWithEvent(ctx, a, "event:"+ev.Topic, &ev)
				if run == nil {
					t.Fatalf("fire returned no run: %v", err)
				}
				runs = append(runs, run.ID)
				if len(runs) > 40 {
					t.Fatal("causal loop did not stop within 40 attempts")
				}
				if err != nil {
					var refusal *automations.LoopRefusal
					if !errors.As(err, &refusal) || sc.Expect.Stop == nil {
						t.Fatalf("unexpected fire failure: %v", err)
					}
					stopped++
					want := sc.Expect.Stop
					if refusal.Reason != want.Reason || refusal.Depth != want.Depth || refusal.Cap != want.Cap || refusal.Automation != want.Automation {
						t.Fatalf("refusal=%+v want=%+v", refusal, want)
					}
					assertLoopScenarioJournal(t, env, run.ID, refusal)
				} else if run.Status != "completed" {
					t.Fatalf("unexpected run status %s: %s", run.Status, run.Error)
				} else {
					completed++
				}
			}
		case <-time.After(500 * time.Millisecond):
			if completed != sc.Expect.Runs {
				t.Fatalf("completed %d runs, want %d", completed, sc.Expect.Runs)
			}
			if sc.Expect.Stop != nil && stopped != 1 {
				t.Fatalf("stopped %d runs, want exactly 1", stopped)
			}
			if sc.Expect.Stop == nil && filtered == 0 {
				t.Fatal("converging chain never reached its false filter")
			}
			for counter, before := range counters {
				if delta := metrics.AutomationLoopsStoppedValue(counter.Automation, counter.Reason) - before; delta != counter.Delta {
					t.Errorf("%s reason=%s counter delta=%v want=%v", counter.Automation, counter.Reason, delta, counter.Delta)
				}
			}
			if want := sc.Expect.Row; want != nil {
				payload := loopScenarioRow(t, env, want.Concept, id)
				if !holds(payload, want.Payload) {
					t.Errorf("final row=%v want fields=%v", payload, want.Payload)
				}
			}
			return
		case <-deadline.C:
			t.Fatal("loop scenario timed out")
		}
	}
}

func loopCallArgs(t *testing.T, args map[string]any) string {
	t.Helper()
	keys := make([]string, 0, len(args))
	for key := range args {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		raw, err := json.Marshal(args[key])
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, key+": "+string(raw))
	}
	return strings.Join(out, ", ")
}
func loopScenarioRow(t *testing.T, env *Env, concept, id string) map[string]any {
	t.Helper()
	var rows []memoryNodes.MemoryNode
	if err := env.DB.NewSelect().Model(&rows).Where("concept = ?", concept).Where("id IN (?)", bun.In([]string{id, concept + ":" + id})).OrderExpr(`"createdAt" DESC`).Limit(1).Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("%s %s: found %d rows", concept, id, len(rows))
	}
	var payload map[string]any
	if err := json.Unmarshal(rows[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}
func assertLoopScenarioJournal(t *testing.T, env *Env, runID string, refusal *automations.LoopRefusal) {
	t.Helper()
	row := loopScenarioRow(t, env, "v1:work:run", runID)
	if row["status"] != "failed" || row["errorCode"] != "loop_depth_exceeded" || row["errorMessage"] != refusal.Error() {
		t.Fatalf("refused journal row: %v", row)
	}
	outcome, _ := row["outcome"].(map[string]any)
	loop, _ := outcome["loop"].(map[string]any)
	chain, _ := loop["chain"].([]any)
	if loop["reason"] != refusal.Reason || loop["correlationId"] != refusal.Cause.CorrelationId || len(chain) != len(refusal.PriorRuns()) {
		t.Fatalf("journal loop ancestry=%v refusal=%+v", loop, refusal)
	}
	for i, prior := range refusal.PriorRuns() {
		entry, _ := chain[i].(map[string]any)
		if entry["automation"] != prior.Automation || entry["runId"] != prior.RunId {
			t.Fatalf("journal chain entry %d = %v, want %+v", i, entry, prior)
		}
	}
	if loop["depth"] != float64(refusal.Depth) || loop["cap"] != float64(refusal.Cap) {
		t.Fatalf("journal depth/cap=%v", loop)
	}
	trigger, _ := row["triggerEvent"].(map[string]any)
	parent, _ := trigger["cause"].(map[string]any)
	if parent["depth"] != float64(refusal.Depth-1) {
		t.Fatalf("journal parent depth=%v", parent)
	}
}
