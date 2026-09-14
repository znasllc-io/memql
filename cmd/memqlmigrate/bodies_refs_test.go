package main

import (
	"strings"
	"testing"
)

func TestBodiesRefs(t *testing.T) {
	auto := func() *refContext {
		return &refContext{
			construct:   "automation",
			steps:       map[string]string{"decide": "logic", "gate": "action", "rows": "query", "created": "mutation"},
			args:        map[string]bool{"id": true, "provider": true, "node": true},
			loopVars:    map[string]bool{},
			eventFields: map[string]bool{},
		}
	}
	logic := func() *refContext {
		return &refContext{
			construct:   "logic",
			steps:       map[string]string{"rows": "query", "result": "builtin"},
			args:        map[string]bool{},
			loopVars:    map[string]bool{},
			eventFields: map[string]bool{},
		}
	}
	cases := []struct {
		name string
		rc   *refContext
		loop string
		in   string
		want string
		err  string
		// fields are the args an automation now reads from event.payload.
		fields []string
	}{
		{name: "steps result", rc: auto(), in: `steps.decide.result == "queued"`, want: `decide == "queued"`},
		{name: "steps result field", rc: auto(), in: `steps.decide.result.outcome`, want: `decide.outcome`},
		{name: "bare result", rc: auto(), in: `decide.result`, want: `decide`},
		{name: "bare result field", rc: auto(), in: `decide.result.deploymentId`, want: `decide.deploymentId`},
		{name: "action climbs", rc: auto(), in: `steps.gate.result.result.result.passed`, want: `gate.passed`},
		{name: "action partial climb", rc: auto(), in: `steps.gate.result.result.passed`, err: "reads an action's envelope"},
		{name: "status has no successor", rc: auto(), in: `steps.decide.status == "skipped"`, err: "has no statement spelling"},
		{name: "unknown step", rc: auto(), in: `steps.nope.result`, err: "names no step of this body"},
		{name: "accessor lowered", rc: logic(), in: `result.First().payload.effect`, want: `result.first().effect`},
		{name: "first then payload", rc: logic(), in: `rows.first().payload.value ?? "30"`, want: `rows.first().value ?? "30"`},
		{name: "Len is count", rc: logic(), in: `rows.Len() > 0`, want: `rows.count() > 0`},
		{name: "first function", rc: logic(), in: `first(rows).id`, want: `rows.first().id`},
		{name: "bare args automation", rc: auto(), in: `provider == "docker-local"`, want: `args.provider == "docker-local"`},
		{name: "args path", rc: auto(), in: `node.version ?? ""`, want: `args.node.version ?? ""`},
		{name: "event payload", rc: auto(), in: `event.payload.naturalKeyValue != nil`, want: `args.naturalKeyValue != nil`, fields: []string{"naturalKeyValue"}},
		{name: "implicit payload", rc: auto(), in: `exists(payload.identityProvider)`, want: `exists(args.identityProvider)`, fields: []string{"identityProvider"}},
		{name: "event whole", rc: auto(), in: `event`, want: `event`},
		{name: "logic args stay", rc: logic(), in: `args.event.payload.id ?? ""`, want: `args.event.payload.id ?? ""`},
		{name: "loop row payload", rc: auto(), loop: "item", in: `item.payload.status == "provisioned"`, want: `item.status == "provisioned"`},
		{name: "loop row id", rc: auto(), loop: "item", in: `item.id`, want: `item.id`},
		{name: "map key untouched", rc: auto(), in: `{ provider: provider, id: id }`, want: `{ provider: args.provider, id: args.id }`},
		{name: "named arg untouched", rc: auto(), in: `concat("x", id)`, want: `concat("x", args.id)`},
		{name: "ternary branch not a key", rc: auto(), in: `provider == "a" ? id : node`, want: `args.provider == "a" ? args.id : args.node`},
		{name: "lambda parameter", rc: logic(), in: `rows.where(r => r.active).count()`, want: `rows.where(r => r.active).count()`},
		{name: "string untouched", rc: auto(), in: `"steps.decide.result"`, want: `"steps.decide.result"`},
		{name: "comment untouched", rc: auto(), in: "id // steps.decide.result", want: "args.id // steps.decide.result"},
		{name: "shorthand entry", rc: &refContext{construct: "automation", steps: map[string]string{}, args: map[string]bool{},
			loopVars: map[string]bool{}, eventFields: map[string]bool{}, logicEvent: true},
			in:   "{ delegationId: args.event.payload.id, args.event.payload.identityId, timestamp: now }",
			want: "{ delegationId: args.id, identityId: args.identityId, timestamp: now }", fields: []string{"id", "identityId"}},
		{name: "logic event whole", rc: &refContext{construct: "automation", steps: map[string]string{}, args: map[string]bool{},
			loopVars: map[string]bool{}, eventFields: map[string]bool{}, logicEvent: true},
			in: "args.event", want: "event"},
		{name: "logic other arg", rc: &refContext{construct: "automation", steps: map[string]string{}, args: map[string]bool{},
			loopVars: map[string]bool{}, eventFields: map[string]bool{}, logicEvent: true},
			in: "args.limit", err: "does not pass"},
		{name: "construct call args", rc: auto(), in: `query activeUsers(status: provider)`, want: `query activeUsers(status: args.provider)`},
		{name: "keyword words untouched", rc: auto(), in: `x in ["a", "b"] && true`, want: `x in ["a", "b"] && true`},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			rc := c.rc
			if c.loop != "" {
				rc = rc.withLoopVar(c.loop)
			}
			got, err := rewriteRefs(c.in, rc)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("rewriteRefs(%q) error = %v, want one containing %q", c.in, err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("rewriteRefs(%q): %v", c.in, err)
			}
			if got != c.want {
				t.Fatalf("rewriteRefs(%q) = %q, want %q", c.in, got, c.want)
			}
			for _, f := range c.fields {
				if !rc.eventFields[f] {
					t.Errorf("%s was read from the event payload but is not collected for the args block", f)
				}
			}
		})
	}
}

func TestTokenizeExprRoundTrips(t *testing.T) {
	for _, s := range []string{
		`steps.gate.result.result.result.passed`,
		`coalesce(args.event.payload.status, "") // note`,
		"{ a: 1,\n  b: \"x\\\"y\" }",
		`rows.where(r => r.a != nil && r.b ?? "") .count()`,
		`/* block */ x .? y`,
	} {
		var b strings.Builder
		for _, tok := range tokenizeExpr(s) {
			b.WriteString(tok.text)
		}
		if b.String() != s {
			t.Errorf("tokens of %q reassemble to %q", s, b.String())
		}
	}
}
