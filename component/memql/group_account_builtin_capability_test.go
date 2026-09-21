package memql

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// group_account_builtin_capability_test.go -- the caller gate on the two
// account-lifecycle group builtins (epic memql#5165, D5).
//
// `groupEnsureForAccount` mints the one group an account's whole membership
// hangs on. `groupArchiveForAccount` archives every group tied to an account
// and removes every membership on them, both kinds. Both write through
// integrations/groups' store, which stamps a synthetic cluster-owner actor and
// INTERNAL ORIGIN so an unowned row can be written at all (store.go) -- so per
// row authorization narrows neither of them, and the caller gate is the whole
// of what decides who may drive them.
//
// THE CALL UNDER TEST IS THE ONE THE AUTOMATIONS MAKE, verbatim:
// `builtin groupEnsureForAccount(accountId: ...)` is what
// dsl/accounts/automations.memql and the seed materializer's boot sweep
// (account_group_backfill.go) hand the engine. It runs through Execute rather
// than through evaluateBuiltinFunctionExpression, so the parse, the top-level
// builtin branch and the gate are all the production path and only the handler
// is a fixture.
//
// THE TREE IS THE CONTROL. Each requirement is read back off the loaded
// builtin before it is exercised, so a builtin that lost its annotation fails
// this file rather than passing it: an engine with no declaration admits every
// role below, and asserting against a locally-written requirement would call
// that a pass.

// groupAccountBuiltins is the pair, with the requirement each declares.
var groupAccountBuiltins = []struct {
	name     string
	verb     string
	resource string
}{
	{"groupEnsureForAccount", auth.VerbCreate, auth.ResourceGroup},
	{"groupArchiveForAccount", auth.VerbUpdate, auth.ResourceGroup},
}

// groupsHandlerProbe stands in for integrations/groups, registered under the
// SAME integration name so the tree's `@executor("integration.groups.…")`
// resolves to it. Only the answer is a fixture; the registration and the
// dispatch are the production path.
type groupsHandlerProbe struct {
	mu      sync.Mutex
	reached map[string]int
}

func (p *groupsHandlerProbe) IntegrationName() string { return "groups" }

func (p *groupsHandlerProbe) Capabilities() []IntegrationCapability {
	out := make([]IntegrationCapability, 0, len(groupAccountBuiltins))
	for _, b := range groupAccountBuiltins {
		name := b.name
		out = append(out, IntegrationCapability{
			Name:        name,
			Description: "records that the handler was reached",
			Handler: func(_ context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
				p.mu.Lock()
				defer p.mu.Unlock()
				p.reached[name]++
				return nil, nil
			},
		})
	}
	return out
}

func (p *groupsHandlerProbe) count(name string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reached[name]
}

// groupBuiltinProbe builds a DB-free engine holding the embedded tree's
// builtins, with the groups executors answered by a recorder -- so "was the
// gate crossed" is observed rather than inferred from an error that could have
// come from anywhere.
func groupBuiltinProbe(t *testing.T) (*MemQLEngine, *groupsHandlerProbe) {
	t.Helper()
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	fns := newFunctionRegistry()
	if _, err := LoadUnifiedBuiltins(slog.New(slog.NewTextHandler(io.Discard, nil)), fns); err != nil {
		t.Fatalf("LoadUnifiedBuiltins: %v", err)
	}
	e := &MemQLEngine{
		initialized:  true,
		functions:    fns,
		integrations: newIntegrationRegistry(),
		db:           neverDialledDB(t),
	}
	probe := &groupsHandlerProbe{reached: map[string]int{}}
	if err := e.RegisterIntegration(probe); err != nil {
		t.Fatalf("RegisterIntegration: %v", err)
	}
	return e, probe
}

// groupBuiltinCall renders the statement an automation emits.
func groupBuiltinCall(name, accountID string) string {
	return fmt.Sprintf("builtin %s(accountId: %s)", name, langparser.QuoteString(accountID))
}

// requirementFromTree reads back what the loaded builtin declares, and fails
// when it is not the requirement this file is about.
func requirementFromTree(t *testing.T, e *MemQLEngine, name, verb, resource string) {
	t.Helper()
	fn, err := e.functions.Get(name)
	if err != nil || fn == nil {
		t.Fatalf("%s is not in the loaded tree: %v", name, err)
	}
	if fn.RequiresCapability != (CapabilityRequirement{Verb: verb, Resource: resource}) {
		t.Fatalf("%s declares %+v, want %s on %s", name, fn.RequiresCapability, verb, resource)
	}
}

// TestTheAccountGroupBuiltinsRequireAGroupCapability is the probe this gate was
// built from: an ordinary authenticated person naming either builtin.
//
// `user` and `viewer` hold `read` on group and nothing else. `developer` RANKS
// 300, above admin's 200, and holds no group grant at all -- which is the pair
// that makes this a capability question rather than a rank one, since a floor
// phrased "admin and above" would admit the developer.
func TestTheAccountGroupBuiltinsRequireAGroupCapability(t *testing.T) {
	installSeededCatalog(t)
	e, probe := groupBuiltinProbe(t)

	for _, b := range groupAccountBuiltins {
		requirementFromTree(t, e, b.name, b.verb, b.resource)
		for _, role := range []string{"user", "viewer", "developer"} {
			_, err := e.Execute(asCaller(role), groupBuiltinCall(b.name, "acme"))
			if err == nil {
				t.Fatalf("%s admitted a %s: an account's groups are not every signed-in person's to place or empty", b.name, role)
			}
			// The CODE is what the OS reads a refusal out of; a refusal for
			// another reason would pass a looser check while leaving the gate
			// open.
			if !strings.Contains(err.Error(), CodeCapabilityNotHeld+": ") {
				t.Fatalf("%s refused a %s with %q, want the %s refusal", b.name, role, err, CodeCapabilityNotHeld)
			}
			if !strings.Contains(err.Error(), b.verb) || !strings.Contains(err.Error(), auth.ResourceGroup) {
				t.Fatalf("%s refused a %s with %q, which does not name the %s-on-%s capability the caller needs",
					b.name, role, err, b.verb, auth.ResourceGroup)
			}
			if probe.count(b.name) != 0 {
				t.Fatalf("%s ran its handler for a refused %s: the gate sits after the work", b.name, role)
			}
		}
	}
}

// TestTheAccountGroupBuiltinsAdmitTheRolesThatHoldTheCapability is the other
// half, and without it the test above passes on a builtin nobody can call.
//
// owner and admin are the two roles the seeds give create and update on group
// -- the same pair that may reach groupCreate and groupArchive by hand.
func TestTheAccountGroupBuiltinsAdmitTheRolesThatHoldTheCapability(t *testing.T) {
	installSeededCatalog(t)
	e, probe := groupBuiltinProbe(t)

	for _, b := range groupAccountBuiltins {
		requirementFromTree(t, e, b.name, b.verb, b.resource)
		for _, role := range []string{"owner", "admin"} {
			before := probe.count(b.name)
			if _, err := e.Execute(asCaller(role), groupBuiltinCall(b.name, "acme")); err != nil {
				t.Fatalf("%s refused a %s, who holds %s on %s: %v", b.name, role, b.verb, b.resource, err)
			}
			if probe.count(b.name) != before+1 {
				t.Fatalf("%s did not reach its handler for an admitted %s", b.name, role)
			}
		}
	}
}

// TestTheAccountGroupBuiltinsStillRunForTheAutomations.
//
// Every caller in the tree is the engine rather than a person: the
// ensureAccountGroup and archiveAccountGroup automations
// (dsl/accounts/automations.memql) and the boot sweep in
// account_group_backfill.go. An automation whose source came from the
// registered tree reaches the engine with INTERNAL ORIGIN
// (component/automations' originForSource) and the sweep stamps it by hand, so
// the gate passes them for the reason it passes @serverOnly: trusted
// server-side Go stamped for one call is not a principal.
//
// THIS IS THE TEST THAT SAYS THE GATE IS ON THE RIGHT SIDE. A requirement that
// refused the cascade would leave an archived account's people still holding
// its groups -- the failure the cascade exists to prevent.
func TestTheAccountGroupBuiltinsStillRunForTheAutomations(t *testing.T) {
	installSeededCatalog(t)
	e, probe := groupBuiltinProbe(t)

	for _, b := range groupAccountBuiltins {
		// A `user` deliberately: an automation's own actor holds no group
		// capability, so if the origin stamp were not what carries it, this is
		// the call that would fail.
		ctx := auth.ContextWithInternalOrigin(asCaller("user"))
		if _, err := e.Execute(ctx, groupBuiltinCall(b.name, "acme")); err != nil {
			t.Fatalf("%s refused a trusted automation; the account lifecycle cascade cannot run: %v", b.name, err)
		}
		if probe.count(b.name) != 1 {
			t.Fatalf("%s did not reach its handler under internal origin", b.name)
		}
	}
}

// TestTheAccountGroupBuiltinsRefuseACallWithNoIdentity. An unauthenticated
// stream carries no caller, and the gate fails closed on it rather than
// reading an absent role as one holding nothing in particular.
func TestTheAccountGroupBuiltinsRefuseACallWithNoIdentity(t *testing.T) {
	installSeededCatalog(t)
	e, probe := groupBuiltinProbe(t)

	for _, b := range groupAccountBuiltins {
		if _, err := e.Execute(context.Background(), groupBuiltinCall(b.name, "acme")); err == nil {
			t.Fatalf("%s admitted a call carrying no caller identity", b.name)
		}
		if probe.count(b.name) != 0 {
			t.Fatalf("%s ran its handler for a call with no identity", b.name)
		}
	}
}

// TestTheAccountGroupRefusalTellsARefusedCallerNothingAboutTheAccount.
//
// The gate runs BEFORE the handler, so the same sentence comes back for an
// account that exists, one that does not, and one that is not named at all.
// That is the property rather than a side effect: a refusal that varied would
// turn either builtin into an account enumerator for anybody who may not call
// it. `group_account_not_found` stays the answer a caller who MAY call it gets
// for a stranger.
func TestTheAccountGroupRefusalTellsARefusedCallerNothingAboutTheAccount(t *testing.T) {
	installSeededCatalog(t)
	e, probe := groupBuiltinProbe(t)

	for _, b := range groupAccountBuiltins {
		var answers []string
		for _, accountID := range []string{"acme", "no-such-account", ""} {
			_, err := e.Execute(asCaller("viewer"), groupBuiltinCall(b.name, accountID))
			if err == nil {
				t.Fatalf("%s admitted a viewer naming %q", b.name, accountID)
			}
			answers = append(answers, err.Error())
		}
		for _, got := range answers[1:] {
			if got != answers[0] {
				t.Fatalf("%s answers a refused caller differently depending on the account:\n  %q\n  %q\n"+
					"The difference is a read of the account registry by somebody refused it", b.name, answers[0], got)
			}
		}
		if probe.count(b.name) != 0 {
			t.Fatalf("%s reached its handler while refusing a viewer", b.name)
		}
	}
}

// TestSdkIsAGeneratorMarkerNotAGate pins the premise the gate rests on.
//
// Neither builtin carries `@sdk`, and both say so in their own descriptions as
// part of their safety argument. `@sdk` decides what the generators emit and
// nothing else -- the builtin converter reads it into no field at all -- so its
// absence keeps a name out of sdk/go and sdk/ts and leaves it resolvable by
// anyone who types it. The capability, and not the missing marker, is what
// refuses.
//
// The experiment is the pair: read the declaration and see no `@sdk`, then
// execute the same name and watch it run.
func TestSdkIsAGeneratorMarkerNotAGate(t *testing.T) {
	installSeededCatalog(t)
	e, probe := groupBuiltinProbe(t)

	source, err := os.ReadFile(groupBuiltinsSourcePath)
	if err != nil {
		t.Fatalf("the identity builtins are unreadable at %s: %v", groupBuiltinsSourcePath, err)
	}
	for _, b := range groupAccountBuiltins {
		if annotationsAbove(string(source), b.name, "@sdk") {
			t.Fatalf("%s now carries @sdk; this file's reasoning assumes it does not", b.name)
		}
		if _, err := e.Execute(asCaller("owner"), groupBuiltinCall(b.name, "acme")); err != nil {
			t.Fatalf("%s is unreachable even for an owner, so @sdk cannot be what gates it: %v", b.name, err)
		}
		if probe.count(b.name) != 1 {
			t.Fatalf("%s did not dispatch for an owner, so this file proves nothing about @sdk", b.name)
		}
	}
}

// groupBuiltinsSourcePath is where the two declarations live, read as text so
// the annotation block above each can be inspected for a marker the engine
// keeps no record of.
const groupBuiltinsSourcePath = "../../dsl/identity/builtins.memql"

// annotationsAbove reports whether `marker` opens a line in the contiguous
// annotation block directly above `builtin <name> {`.
func annotationsAbove(source, name, marker string) bool {
	i := strings.Index(source, "\nbuiltin "+name+" {")
	if i < 0 {
		return false
	}
	lines := strings.Split(source[:i], "\n")
	for j := len(lines) - 1; j >= 0; j-- {
		line := strings.TrimSpace(lines[j])
		if !strings.HasPrefix(line, "@") {
			return false
		}
		if strings.HasPrefix(line, marker) {
			return true
		}
	}
	return false
}
