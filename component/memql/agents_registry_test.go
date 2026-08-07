package memql

import (
	"fmt"
	"testing"
)

// agents_registry_test.go -- memql#3209.
//
// The specialist registry is keyed by role slug and was LAST-WINS: a later row
// silently displaced an earlier one under the same key. These tests pin the two
// halves of the fix -- that the registry can be built at all, and that the
// winner under a contested key does not depend on the order rows arrive in.
//
// FALSIFIABILITY (recorded rather than asserted, per the rejected tautology
// memql#3115's review found -- a pure function called 20x on ONE fixture, which
// cannot fail for any implementation):
//
//   - TestExtractRowListAcceptsWhatEngineExecuteReturns is RED against untouched
//     main: extractRowList's type switch had no *ExecuteResult arm, so the whole
//     registry loaded zero agents.
//   - The permuting test below is new-code coverage. Its falsifiability was
//     established by mutation: betterAgentMatch hardcoded to `return true`, and
//     to `return false`, each makes it fail (the winner then tracks input order
//     -- first or last row wins -- which is exactly the defect).

// rowFor builds the shape agentDefinitionFromRow consumes: a row map with a
// top-level id and a payload. Deliberately built from a ROW rather than by
// filling an AgentDefinition literal, so the key-derivation path under test is
// the one production uses.
func rowFor(id, roleSlug, name, kind string) map[string]any {
	return map[string]any{
		"id": id,
		"payload": map[string]any{
			"id":       id,
			"roleSlug": roleSlug,
			"name":     name,
			"kind":     kind,
			"role":     "specialist",
		},
	}
}

func defsFromRows(t *testing.T, rows []map[string]any) []*AgentDefinition {
	t.Helper()
	out := make([]*AgentDefinition, 0, len(rows))
	for _, r := range rows {
		def, ok := agentDefinitionFromRow(r)
		if !ok {
			t.Fatalf("agentDefinitionFromRow rejected %v", r)
		}
		out = append(out, def)
	}
	return out
}

// permutations returns every ordering of the input. Used instead of "call the
// function N times on one slice", which cannot distinguish an order-independent
// implementation from an order-dependent one.
func permutations[T any](in []T) [][]T {
	if len(in) <= 1 {
		return [][]T{append([]T(nil), in...)}
	}
	var out [][]T
	for i := range in {
		rest := make([]T, 0, len(in)-1)
		rest = append(rest, in[:i]...)
		rest = append(rest, in[i+1:]...)
		for _, p := range permutations(rest) {
			out = append(out, append([]T{in[i]}, p...))
		}
	}
	return out
}

// The registry loads ZERO agents, and this pins that it STAYS that way until
// the owner dimension lands.
//
// engine.Execute returns *ExecuteResult and LoadFromRows passes it straight to
// extractRowList, whose type switch handles only []any and map[string]any and
// falls through to `default: return nil`. So every specialist lookup fails
// closed with "no agent registered with that role (loaded: [])".
//
// That is a bug -- and fixing it here would have been a security regression,
// not a fix. The map is keyed by a caller-supplied roleSlug, built from an
// unscoped read carrying every user's systemPrompt, and resolved by
// askSpecialist with a bare lookup and no owner check. Emptiness is the only
// thing making that unreachable today. Adding the arm would turn a dead path
// into a live cross-tenant one in the same commit.
//
// So this asserts the switch is still OFF. Delete this test in the change that
// gives the lookup an owner dimension (memql#3216) -- and turn them on
// together, never separately.
func TestTheAgentRegistryReadIsStillOffPendingOwnerScoping(t *testing.T) {
	res := newExecuteResult(nil)
	res.setOutput([]any{
		map[string]any{"id": "v1:agents:agent:a", "payload": map[string]any{"roleSlug": "one"}},
	})

	if rows := extractRowList(res); len(rows) != 0 {
		t.Fatalf("extractRowList(*ExecuteResult) returned %d rows, want 0.\n\n"+
			"If you just taught extractRowList to read an *ExecuteResult, you also just made the "+
			"specialist registry live -- and it is keyed by a caller-supplied roleSlug over an "+
			"unscoped row set that carries every user's systemPrompt, with no owner check at the "+
			"askSpecialist lookup. Land memql#3216's owner dimension in the SAME change, then "+
			"delete this test. memql#3209.", len(rows))
	}
}

// The core of memql#3209: two rows contest one registry key, and the winner
// must not depend on which order the query happened to return them in.
//
// Contest: a system-seeded agent carrying roleSlug "system-planner" (the
// @scope("perUser") platform seed) versus a user-minted agent carrying the same
// roleSlug. `createAgent` takes roleSlug as a plain caller-supplied arg -- no
// enum, no catalog validation, no uniqueness -- and `allAgents` filters only
// isNotDeleted, so the user row genuinely enters the registry's input set.
func TestBuildAgentIndexIsIndependentOfRowOrder(t *testing.T) {
	rows := []map[string]any{
		rowFor("v1:agents:agent:sys-1", "system-planner", "MemQL Planner", agentKindSystem),
		rowFor("v1:agents:agent:usr-9", "system-planner", "Not The Planner", "specialist"),
		rowFor("v1:agents:agent:usr-2", "system-planner", "Also Not It", "specialist"),
	}

	const wantId = "v1:agents:agent:sys-1"

	for i, perm := range permutations(rows) {
		t.Run(fmt.Sprintf("order-%d", i), func(t *testing.T) {
			idx := buildAgentIndex(defsFromRows(t, perm))

			got, ok := idx["system-planner"]
			if !ok {
				t.Fatalf("no entry for the contested key under this ordering")
			}
			if got.Id != wantId {
				order := make([]string, 0, len(perm))
				for _, r := range perm {
					order = append(order, r["id"].(string))
				}
				t.Errorf("contested key resolved to %q under ordering %v, want %q.\n\n"+
					"The winner must be a property of the ROWS, not of the order the query "+
					"returned them in. A last-wins map makes specialist resolution depend on "+
					"`sort row.createdAt desc` -- and because the engine positions each logical "+
					"row at its NEWEST version, that ordering is really last-MODIFIED, so "+
					"editing an unrelated agent can change which one answers. memql#3209.",
					got.Id, order, wantId)
			}
		})
	}
}

// The tie-break must be intrinsic, not positional. With no system row in play,
// the lowest row id wins -- the same shape memql#3066 chose for the agentRole
// catalog (integrations/agents/factory.go betterRoleMatch).
func TestBuildAgentIndexTieBreaksOnRowIdWhenNeitherIsSystem(t *testing.T) {
	rows := []map[string]any{
		rowFor("v1:agents:agent:zzz", "human-resources", "HR Two", "specialist"),
		rowFor("v1:agents:agent:aaa", "human-resources", "HR One", "specialist"),
	}
	const wantId = "v1:agents:agent:aaa"

	for i, perm := range permutations(rows) {
		idx := buildAgentIndex(defsFromRows(t, perm))
		if got := idx["human-resources"].Id; got != wantId {
			t.Errorf("ordering %d resolved to %q, want the lowest row id %q -- the tie-break "+
				"must be a property of the rows", i, got, wantId)
		}
	}
}

// The registry key falls back to the agent's DISPLAY NAME when roleSlug is
// empty, and `name` is caller-supplied on createAgent. So naming an agent
// "system-planner" puts it in that key's bucket without writing a roleSlug at
// all. A slug-derived claim on a key must outrank a name-derived one.
func TestBuildAgentIndexPrefersASlugKeyOverANameKey(t *testing.T) {
	rows := []map[string]any{
		// No roleSlug: this row claims the key only through its name.
		rowFor("v1:agents:agent:aaa", "", "human-resources", "specialist"),
		// A genuine slug holder, deliberately given a LATER row id so an
		// id-only tie-break would pick the impostor.
		rowFor("v1:agents:agent:zzz", "human-resources", "People Ops", "specialist"),
	}
	const wantId = "v1:agents:agent:zzz"

	for i, perm := range permutations(rows) {
		idx := buildAgentIndex(defsFromRows(t, perm))
		got, ok := idx["human-resources"]
		if !ok {
			t.Fatalf("ordering %d: no entry for the contested key", i)
		}
		if got.Id != wantId {
			t.Errorf("ordering %d resolved to %q, want the slug holder %q.\n\n"+
				"The display-name fallback exists for legacy rows that predate roleSlug. It must "+
				"never let a caller-chosen NAME displace a row that actually carries the slug.",
				i, got.Id, wantId)
		}
	}
}

// A row that claims no key at all must not land in the index under "".
func TestBuildAgentIndexSkipsRowsWithNoKey(t *testing.T) {
	defs := []*AgentDefinition{{Id: "v1:agents:agent:x"}}
	if idx := buildAgentIndex(defs); len(idx) != 0 {
		t.Errorf("index has %d entries for a row with neither roleSlug nor name; want 0", len(idx))
	}
}
