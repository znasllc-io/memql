package agents

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// ask_specialist_owner_test.go -- memql#3216, at the call site.
//
// The registry-level tests (component/memql/agents_owner_registry_test.go)
// pin that resolution is per owner. These pin that askSpecialist actually
// ASKS per owner -- a correct registry resolved with the wrong argument, or
// with none, is the same defect one layer up.
//
// Every assertion here has a POSITIVE CONTROL for the reason the issue named:
// the registry spent memql#3209 empty, so "user B does not reach user A's
// specialist" was satisfied by a map with nothing in it. Each test proves the
// owning user HITS the expected row first.

func specialistDef(owner, id, roleSlug, name string) *memql.AgentDefinition {
	return &memql.AgentDefinition{
		Id:           id,
		OwnerUserId:  owner,
		Name:         roleSlug,
		RoleSlug:     roleSlug,
		DisplayName:  name,
		Role:         "specialist",
		Kind:         "specialist",
		Description:  name + " description",
		SystemPrompt: name + " system prompt",
	}
}

// registryWith builds a registry holding the given definitions.
func registryWith(t *testing.T, defs ...*memql.AgentDefinition) *memql.AgentRegistry {
	t.Helper()
	r := memql.NewAgentRegistry()
	for _, d := range defs {
		if err := r.Upsert(d); err != nil {
			t.Fatalf("Upsert %s: %v", d.Id, err)
		}
	}
	return r
}

// askArgs is the arg map the agent runtime hands the handler. ownerUserId is
// @autoInjected on the tool, so in production it is server-stamped and an
// LLM-supplied value never reaches here.
func askArgs(owner, role string) map[string]any {
	return map[string]any{
		"role":        role,
		"query":       "what is the parental leave policy",
		"ownerUserId": owner,
	}
}

// The defect, at the call site. Bob asks for a slug only Alice holds and must
// not be handed her specialist's persona.
func TestAskSpecialistDoesNotResolveAnotherOwnersSpecialist(t *testing.T) {
	const alice = "v1:identity:user:alice"
	const bob = "v1:identity:user:bob"

	i := New(registryWith(t, specialistDef(alice, "v1:agents:agent:a1", "human-resources", "Alice HR")), stubEngine{})

	// POSITIVE CONTROL. The owning user must reach her own row -- otherwise
	// the assertion below passes against an empty registry, which is the state
	// that made this defect unreachable rather than absent.
	//
	// The handler goes on to invoke the prompt, which the stub answers, so a
	// successful resolve returns a node rather than an error.
	nodes, err := i.handleAskSpecialist(context.Background(), askArgs(alice, "human-resources"), 0)
	if err != nil {
		t.Fatalf("the OWNING user could not resolve her own specialist: %v\n\n"+
			"The negative assertion below would then pass vacuously.", err)
	}
	if len(nodes) != 1 || !strings.Contains(string(nodes[0].Payload), "human-resources") {
		t.Fatalf("control: unexpected envelope %v", nodes)
	}

	// THE ASSERTION.
	if _, err := i.handleAskSpecialist(context.Background(), askArgs(bob, "human-resources"), 0); err == nil {
		t.Error("bob resolved a specialist only alice owns.\n\n" +
			"handleAskSpecialist feeds def.Name / def.Description / def.SystemPrompt into the " +
			"askSpecialist prompt, so this is one user's assistant being handed another user's " +
			"persona verbatim. memql#3216.")
	}
}

// No owner on the call means the runtime could not say who is asking. Refuse,
// rather than resolve the shared catalog and hope.
func TestAskSpecialistFailsClosedWithNoOwner(t *testing.T) {
	const alice = "v1:identity:user:alice"
	i := New(registryWith(t, specialistDef(alice, "v1:agents:agent:a1", "human-resources", "Alice HR")), stubEngine{})

	// POSITIVE CONTROL.
	if _, err := i.handleAskSpecialist(context.Background(), askArgs(alice, "human-resources"), 0); err != nil {
		t.Fatalf("control: the owning user cannot resolve her own specialist: %v", err)
	}

	_, err := i.handleAskSpecialist(context.Background(), askArgs("", "human-resources"), 0)
	if err == nil {
		t.Fatal("askSpecialist resolved with NO owner on the call")
	}
	if !strings.Contains(err.Error(), "no owner") {
		t.Errorf("error = %q, want it to name the missing owner -- an empty ownerUserId is a "+
			"configuration failure worth seeing, not a reason to fall through to whatever row "+
			"holds the slug", err)
	}
}

// A specialist the asking user actually owns still resolves, and the resolved
// persona is THEIRS. The point of the owner dimension is not to refuse more --
// it is to refuse the right things.
func TestAskSpecialistResolvesTheOwnersOwnSpecialist(t *testing.T) {
	const alice = "v1:identity:user:alice"
	const bob = "v1:identity:user:bob"

	i := New(registryWith(t,
		specialistDef(alice, "v1:agents:agent:a1", "human-resources", "Alice HR"),
		specialistDef(bob, "v1:agents:agent:b1", "human-resources", "Bob HR"),
	), stubEngine{})

	for owner, wantName := range map[string]string{alice: "Alice HR", bob: "Bob HR"} {
		nodes, err := i.handleAskSpecialist(context.Background(), askArgs(owner, "human-resources"), 0)
		if err != nil {
			t.Fatalf("owner %s: %v", owner, err)
		}
		if len(nodes) != 1 {
			t.Fatalf("owner %s: want one envelope, got %d", owner, len(nodes))
		}
		// The stub echoes the specialist's system prompt back, so the envelope
		// proves WHICH persona was fed to the prompt -- not merely that some
		// row resolved.
		if got := string(nodes[0].Payload); !strings.Contains(got, wantName+" system prompt") {
			t.Errorf("owner %s got envelope %s, want it built from %q's persona.\n\n"+
				"Two users hold this slug; each must get their own.", owner, got, wantName)
		}
	}
}

// The tool must declare ownerUserId, and the integration's arg schema must say
// it is auto-stamped. Without the @autoInjected annotation in the DSL the field
// is caller-supplied and the model can name whose specialists to resolve --
// which is the whole defect with an extra step.
func TestAskSpecialistArgSchemaDeclaresTheOwner(t *testing.T) {
	i := New(memql.NewAgentRegistry(), nil)
	for _, c := range i.Capabilities() {
		if c.Name != "askSpecialist" {
			continue
		}
		desc, ok := c.ArgsSchema["ownerUserId"]
		if !ok {
			t.Fatal("askSpecialist's arg schema does not declare ownerUserId -- the owner half " +
				"of the registry key is invisible to anyone reading the capability")
		}
		if !strings.Contains(desc, "auto-stamped") {
			t.Errorf("ownerUserId is described as %q; it must say it is auto-stamped, because a "+
				"model that believes it may choose the owner is a model that will try", desc)
		}
		return
	}
	t.Fatal("no askSpecialist capability")
}

// stubEngine answers InvokeAIStructured by echoing back the specialist's
// system prompt, so the envelope the handler returns proves WHICH persona was
// fed to the prompt rather than merely that some row resolved. Everything else
// is nil-embedded and panics on use, surfacing any accidental new dependency.
type stubEngine struct {
	memql.IntegrationEngineAccess
}

func (stubEngine) InvokeAIStructured(
	_ context.Context,
	_ string,
	data map[string]any,
	_ string,
	_ json.RawMessage,
	_ bool,
) (string, error) {
	prompt, _ := data["specialistSystemPrompt"].(string)
	out, err := json.Marshal(map[string]any{
		"response":   prompt,
		"confidence": 1,
	})
	return string(out), err
}
