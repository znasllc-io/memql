package memql

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/envregistry"
	"github.com/znasllc-io/memql/component/memql/readiness"
)

// sentinelValue is a value no report may ever carry. TestReportsCarryNoValues
// resolves every slot to it and then greps the marshalled report: a report
// that leaked a resolved value would put a secret into a broadcast row.
const sentinelValue = "SECRET-VALUE-THAT-MUST-NEVER-APPEAR"

var errSlotNotFound = errors.New("not found")

func fakeResolvers(env map[string]string, vars map[string]string, secrets map[string]string) readinessResolvers {
	return readinessResolvers{
		Env: func(name string) (string, bool) { v, ok := env[name]; return v, ok },
		Variable: func(_ context.Context, name string) (string, error) {
			if v, ok := vars[name]; ok {
				return v, nil
			}
			return "", errSlotNotFound
		},
		Secret: func(_ context.Context, name string) (string, error) {
			if v, ok := secrets[name]; ok {
				return v, nil
			}
			return "", errSlotNotFound
		},
		IsSecret: func(name string) bool { return strings.HasSuffix(name, "_SECRET") },
		Hosted:   func(envregistry.Module) bool { return true },
		// No machines and no federation: the fresh-cluster answer, which is
		// what every case here that is not about inference wants underneath it.
		Registrations:        func(context.Context) ([]readiness.RegistrationFacts, error) { return nil, nil },
		FederationConfigured: func() bool { return false },
		IntegrationState: func(_ context.Context, name string) (string, bool, bool, error) {
			return "", false, false, nil
		},
	}
}

var twoSlotModule = envregistry.Module{
	Name: "storage", Core: true, Description: "d",
	Lanes: []envregistry.Lane{{
		Name: "azure-blob", ConfigurableFrom: envregistry.ConfigurableFromDeployment,
		Slots:         []string{"MEMQL_A", "MEMQL_B_SECRET"},
		OptionalSlots: []string{"MEMQL_C"},
	}},
}

func evalOne(t *testing.T, r readinessResolvers, mod envregistry.Module) readiness.NodeReport {
	t.Helper()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	reports := evaluateModules(context.Background(), r, []envregistry.Module{mod}, "node-a", "bff", now)
	if len(reports) != 1 {
		t.Fatalf("want one report, got %d", len(reports))
	}
	return reports[0]
}

func TestLaneStates(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		secrets map[string]string
		want    readiness.State
	}{
		{"complete lane is configured", map[string]string{"MEMQL_A": "x"}, map[string]string{"MEMQL_B_SECRET": sentinelValue}, readiness.Configured},
		{"some required slots is partial", map[string]string{"MEMQL_A": "x"}, nil, readiness.Partial},
		{"nothing is unconfigured", nil, nil, readiness.Unconfigured},
		{"an optional slot alone is unconfigured", map[string]string{"MEMQL_C": "x"}, nil, readiness.Unconfigured},
		{"blank counts as absent", map[string]string{"MEMQL_A": "  "}, nil, readiness.Unconfigured},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := evalOne(t, fakeResolvers(c.env, nil, c.secrets), twoSlotModule)
			if got.State != c.want {
				t.Fatalf("got %s want %s (%+v)", got.State, c.want, got.Lanes)
			}
		})
	}
}

func TestSlotSourceFollowsTheLadder(t *testing.T) {
	r := fakeResolvers(map[string]string{"MEMQL_A": "x"}, nil, map[string]string{"MEMQL_B_SECRET": sentinelValue})
	got := evalOne(t, r, twoSlotModule)
	slots := map[string]readiness.SlotReport{}
	for _, s := range got.Lanes[0].Slots {
		slots[s.Name] = s
	}
	if slots["MEMQL_A"].Source != readinessSourceEnv || slots["MEMQL_B_SECRET"].Source != readinessSourceSecret {
		t.Fatalf("sources: %+v", slots)
	}
	if !slots["MEMQL_C"].Optional || slots["MEMQL_C"].Present {
		t.Fatalf("optional absent slot: %+v", slots["MEMQL_C"])
	}
}

// A slot that is not a secret and is absent from the environment falls to the
// variable tier -- the third rung of the ladder, and the one the OS writes to.
func TestSlotFallsToTheVariableTier(t *testing.T) {
	r := fakeResolvers(nil, map[string]string{"MEMQL_A": "x"}, map[string]string{"MEMQL_B_SECRET": "y"})
	got := evalOne(t, r, twoSlotModule)
	for _, s := range got.Lanes[0].Slots {
		if s.Name == "MEMQL_A" && (!s.Present || s.Source != readinessSourceVariable) {
			t.Fatalf("MEMQL_A: %+v", s)
		}
	}
	if got.State != readiness.Configured {
		t.Fatalf("got %s", got.State)
	}
}

// A secret slot is NEVER read from the variable tier: a value written as a
// plaintext variable under a secret's name must not satisfy the slot, or the
// report would say configured while the decrypting reader finds nothing.
func TestSecretSlotIgnoresTheVariableTier(t *testing.T) {
	r := fakeResolvers(map[string]string{"MEMQL_A": "x"}, map[string]string{"MEMQL_B_SECRET": "plaintext"}, nil)
	got := evalOne(t, r, twoSlotModule)
	if got.State != readiness.Partial {
		t.Fatalf("got %s want partial (%+v)", got.State, got.Lanes)
	}
}

func TestReportsCarryNoValues(t *testing.T) {
	r := fakeResolvers(map[string]string{"MEMQL_A": sentinelValue}, nil, map[string]string{"MEMQL_B_SECRET": sentinelValue})
	got := evalOne(t, r, twoSlotModule)
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), sentinelValue) {
		t.Fatalf("a report carried a resolved value: %s", raw)
	}
}

func TestNotHostedIsNotApplicable(t *testing.T) {
	r := fakeResolvers(map[string]string{"MEMQL_A": "x"}, nil, map[string]string{"MEMQL_B_SECRET": "y"})
	r.Hosted = func(envregistry.Module) bool { return false }
	if got := evalOne(t, r, twoSlotModule); got.State != readiness.NotApplicable {
		t.Fatalf("got %s", got.State)
	}
}

func TestInferenceEvaluator(t *testing.T) {
	mod := envregistry.Module{Name: "ai", Core: true, Description: "d", Evaluator: envregistry.EvaluatorInferenceStatus}
	r := fakeResolvers(nil, nil, nil)
	if got := evalOne(t, r, mod); got.State != readiness.Unconfigured {
		t.Fatalf("no door open must be unconfigured, got %s", got.State)
	}
	// A DOOR IS A ROW NOW, not a live seam. What opens it here is a machine
	// whose label meets the capability floor -- and it opens on EVERY node
	// type, which is the defect this replaced (see
	// TestEveryNodeTypeProducesTheSameInferenceReport).
	r.Registrations = func(context.Context) ([]readiness.RegistrationFacts, error) {
		return []readiness.RegistrationFacts{{
			Labels:     map[string]string{"model:llama3.1:8b": "ctx=8192,structured=true"},
			LastSeenAt: time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC),
		}}, nil
	}
	got := evalOne(t, r, mod)
	if got.State != readiness.Configured {
		t.Fatalf("a door open must be configured, got %s", got.State)
	}
	// The three lanes ride on the row, so the wizard and the Set up group can
	// say WHICH door is open without a second read.
	if len(got.Lanes) != 3 {
		t.Fatalf("the ai report carries %d lanes, want the three doors: %+v", len(got.Lanes), got.Lanes)
	}
}

func TestIntegrationEvaluator(t *testing.T) {
	mod := envregistry.Module{Name: "email", Core: true, Description: "d", Evaluator: "integration:email"}
	cases := []struct {
		state      string
		touched    bool
		registered bool
		want       readiness.State
	}{
		{"configured", true, true, readiness.Configured},
		// unhealthy is CONFIGURED: somebody did the setup and the send is
		// failing for another reason. Reporting it unconfigured would send a
		// person back to a form they already filled in correctly.
		{"unhealthy", true, true, readiness.Configured},
		{"needs_configuration", true, true, readiness.Partial},
		{"needs_configuration", false, true, readiness.Unconfigured},
		{"", false, false, readiness.NotApplicable},
	}
	for _, c := range cases {
		r := fakeResolvers(nil, nil, nil)
		r.IntegrationState = func(context.Context, string) (string, bool, bool, error) {
			return c.state, c.touched, c.registered, nil
		}
		if got := evalOne(t, r, mod); got.State != c.want {
			t.Errorf("%+v: got %s", c, got.State)
		}
	}
}

// An integration that answers with an ERROR is unknown: a probe that failed
// says nothing about whether a person did the setup, so it is neither
// unconfigured (which would send them to a form they may not need) nor
// notApplicable -- the word for "not hosted here", behind which a failed
// probe on a node that DOES host the integration used to hide. An integration
// that is not registered (no error) stays notApplicable; that case is in
// TestIntegrationEvaluator.
func TestIntegrationEvaluatorErrorIsUnknown(t *testing.T) {
	mod := envregistry.Module{Name: "email", Core: true, Description: "d", Evaluator: "integration:email"}
	r := fakeResolvers(nil, nil, nil)
	r.IntegrationState = func(context.Context, string) (string, bool, bool, error) {
		return "", false, true, errors.New("probe failed")
	}
	got := evalOne(t, r, mod)
	if got.State != readiness.Unknown {
		t.Fatalf("got %s, want unknown", got.State)
	}
	if got.Reason != readiness.ReasonIntegrationProbeFailed {
		t.Fatalf("reason %q, want %q", got.Reason, readiness.ReasonIntegrationProbeFailed)
	}
}

// Every report carries the node that made it and the module it is about, so
// the fold can key on them and the row id can be derived from them.
func TestReportsCarryTheirNodeAndModule(t *testing.T) {
	got := evalOne(t, fakeResolvers(nil, nil, nil), twoSlotModule)
	if got.Module != "storage" || got.NodeId != "node-a" || got.NodeType != "bff" || !got.Core {
		t.Fatalf("%+v", got)
	}
	if got.ReportedAt.IsZero() {
		t.Fatalf("reportedAt is zero")
	}
}
