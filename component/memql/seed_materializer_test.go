package memql

import (
	"strings"
	"testing"
)

// renderArgsObject is the materializer's mutation-arg renderer.
// MemQL mutation calls require bare-identifier keys, not JSON-style
// quoted keys; an earlier raw-JSON implementation looked fine in
// Go but the engine's mutation parser rejected the calls silently
// (the materializer logged success but no rows landed). These tests
// pin the bare-key format + nested-block handling.

func TestRenderArgsObject_BareKeysScalarValues(t *testing.T) {
	got, err := renderArgsObject(map[string]any{
		"agentId":     "assistant-user-abc",
		"name":        "Assistant",
		"active":      true,
		"deleted":     false,
		"description": "Has a \"quote\" in it",
	})
	if err != nil {
		t.Fatalf("renderArgsObject: %v", err)
	}
	// Keys are sorted alphabetically for log-diff stability.
	want := `{active: true, agentId: "assistant-user-abc", deleted: false, description: "Has a \"quote\" in it", name: "Assistant"}`
	if got != want {
		t.Errorf("got:  %s\nwant: %s", got, want)
	}
}

func TestRenderArgsObject_NestedBlocks(t *testing.T) {
	got, err := renderArgsObject(map[string]any{
		"agentId": "ga-jose",
		"capabilities": map[string]any{
			"avatar":  true,
			"claw":    false,
			"domains": []any{"general", "copresent-ui"},
			"tools":   []any{},
		},
		"providerConfig": map[string]any{
			"llm": map[string]any{
				"policyName":  "balancedChat",
				"temperature": 0.7,
				"maxTokens":   int64(4000),
			},
		},
	})
	if err != nil {
		t.Fatalf("renderArgsObject: %v", err)
	}
	// Verify the key invariants rather than the whole string (sort
	// order pins it but the assertion is more readable this way).
	mustContain(t, got, `agentId: "ga-jose"`)
	mustContain(t, got, `capabilities: {`)
	mustContain(t, got, `avatar: true`)
	mustContain(t, got, `claw: false`)
	mustContain(t, got, `domains: ["general", "copresent-ui"]`)
	mustContain(t, got, `tools: []`)
	mustContain(t, got, `providerConfig: {`)
	mustContain(t, got, `llm: {`)
	mustContain(t, got, `policyName: "balancedChat"`)
	mustContain(t, got, `temperature: 0.7`)
	mustContain(t, got, `maxTokens: 4000`)

	// Bare identifier keys ONLY -- no quoted keys allowed (this is
	// the bug the rewrite fixes: json.Marshal produced `"agentId":`
	// which the mutation parser rejected).
	for _, badKey := range []string{`"agentId"`, `"capabilities"`, `"llm"`, `"policyName"`} {
		if strings.Contains(got, badKey) {
			t.Errorf("output contains JSON-style quoted key %q -- MemQL mutation parser rejects this:\n%s", badKey, got)
		}
	}
}

func TestRenderArgsObject_EmptyMap(t *testing.T) {
	got, err := renderArgsObject(map[string]any{})
	if err != nil {
		t.Fatalf("renderArgsObject: %v", err)
	}
	if got != "{}" {
		t.Errorf("empty map should render as `{}`, got %q", got)
	}
}

func TestRenderArgsObject_NilValue(t *testing.T) {
	got, err := renderArgsObject(map[string]any{
		"id":      "x",
		"missing": nil,
	})
	if err != nil {
		t.Fatalf("renderArgsObject: %v", err)
	}
	mustContain(t, got, "missing: null")
	mustContain(t, got, `id: "x"`)
}

func TestBuildArgsFromBody_PerUserStampsConceptIdAndOwner(t *testing.T) {
	body := seedBlock{
		fields: map[string]seedValue{
			"name": {kind: seedString, str: "Assistant"},
			"role": {kind: seedString, str: "assistant"},
		},
	}
	body.keys = []string{"name", "role"}

	args := buildArgsFromBody(body, "agent", "assistant-user-jose", "user-jose")

	// The synthetic id field uses the conceptName+Id convention.
	if args["agentId"] != "assistant-user-jose" {
		t.Errorf("agentId = %v, want assistant-user-jose", args["agentId"])
	}
	// ownerUserId is stamped from the user context.
	if args["ownerUserId"] != "user-jose" {
		t.Errorf("ownerUserId = %v, want user-jose", args["ownerUserId"])
	}
	// Body fields pass through by name.
	if args["name"] != "Assistant" {
		t.Errorf("name = %v, want Assistant", args["name"])
	}
	if args["role"] != "assistant" {
		t.Errorf("role = %v, want assistant", args["role"])
	}
}

// TestDeterministicPerUserSeedId_MatchesDocumentedContract is the
// #273 acceptance test: the materializer's docstring (and the seed
// file at memql/dsl/agents/assistant.memql) promises that perUser
// seeds materialize at `<seedName>-<userId>`. The implementation
// previously broke that promise by minting a fresh UUID in
// `lookupOrMintPerUserId` when no prior row existed, which made
// cluster boot produce N distinct rows per user (one per node
// racing the startup sweep). This test pins the contract.
func TestDeterministicPerUserSeedId_MatchesDocumentedContract(t *testing.T) {
	def := &SeedDefinition{
		Name:         "assistant",
		UseNamespace: "agents",
		UseConcept:   "agent",
	}

	cases := []struct {
		name   string
		userId string
		want   string
	}{
		{
			name:   "canonical-prefixed userId (the form Execute returns)",
			userId: "v1:identity:user:395e4e72-3097-4371-b9be-18da56eb8d5a",
			want:   "assistant-395e4e72-3097-4371-b9be-18da56eb8d5a",
		},
		{
			name:   "bare userId (some event payloads carry the shortId only)",
			userId: "395e4e72-3097-4371-b9be-18da56eb8d5a",
			want:   "assistant-395e4e72-3097-4371-b9be-18da56eb8d5a",
		},
		{
			name:   "short alias used in adjacent existing tests",
			userId: "user-jose",
			want:   "assistant-user-jose",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := deterministicPerUserSeedId(def, tc.userId)
			if got != tc.want {
				t.Errorf("deterministicPerUserSeedId = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDeterministicPerUserSeedId_ConvergesUnderConcurrentRacers
// asserts the load-bearing cluster-boot invariant: N callers
// running the deterministic-id path concurrently for the same
// (seed, user) pair all land on the SAME id. Without this, every
// node's startup sweep wrote a separate row and the user saw N
// duplicate assistants. The deterministic-id function itself is
// pure string manipulation so this is over-specified -- the test
// exists to prevent a future refactor from re-introducing random-
// id generation in this path.
func TestDeterministicPerUserSeedId_ConvergesUnderConcurrentRacers(t *testing.T) {
	def := &SeedDefinition{
		Name:         "assistant",
		UseNamespace: "agents",
		UseConcept:   "agent",
	}
	userId := "v1:identity:user:racer-test-user"

	const racers = 16
	out := make(chan string, racers)
	for i := 0; i < racers; i++ {
		go func() { out <- deterministicPerUserSeedId(def, userId) }()
	}

	want := "assistant-racer-test-user"
	for i := 0; i < racers; i++ {
		got := <-out
		if got != want {
			t.Fatalf("racer %d: got %q, want %q", i, got, want)
		}
	}
}

func TestBuildArgsFromBody_GlobalUsesBodyIdNotOwner(t *testing.T) {
	body := seedBlock{
		fields: map[string]seedValue{
			"partitionType": {kind: seedString, str: "standard"},
			"displayName":   {kind: seedString, str: "Default"},
		},
	}
	body.keys = []string{"partitionType", "displayName"}

	// For @scope("global"), the materializer passes idVal from
	// body.fields["id"] and ownerUserId="" (no user context).
	args := buildArgsFromBody(body, "partition", "default", "")

	if args["partitionId"] != "default" {
		t.Errorf("partitionId = %v, want default", args["partitionId"])
	}
	if _, hasOwner := args["ownerUserId"]; hasOwner {
		t.Errorf("global seed must not stamp ownerUserId, got %v", args["ownerUserId"])
	}
}

func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("expected output to contain %q\nfull output:\n%s", needle, haystack)
	}
}
