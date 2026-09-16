package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
)

type testPolicyStore struct {
	mu   sync.Mutex
	doc  PolicyDocument
	fail bool
}

func (s *testPolicyStore) Read(context.Context) (PolicyDocument, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return PolicyDocument{}, fmt.Errorf("database offline")
	}
	raw, _ := json.Marshal(s.doc)
	var doc PolicyDocument
	_ = json.Unmarshal(raw, &doc)
	return doc, nil
}
func (s *testPolicyStore) CompareAndSwap(_ context.Context, expected int64, doc PolicyDocument) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if expected != s.doc.Revision {
		return fmt.Errorf("stale revision")
	}
	s.doc = doc
	return nil
}
func policyOwner() context.Context {
	return auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "owner", Role: auth.RoleOwner})
}
func policyFixture(store PolicyStore) *PolicyRegistry {
	r := newPolicyRegistry()
	r.byName = map[string]*PolicyConfig{"localFirst": {Name: "localFirst", Primary: "fleet:strongest", Fallbacks: []string{"app:*", "federation:cheapest"}}, "localOnly": {Name: "localOnly", Primary: "fleet:strongest"}, "embeddingsBinding": {Name: "embeddingsBinding", Primary: "embedder:active"}}
	r.AttachStore(store)
	return r
}
func TestPolicyCustomizationPersistsAcrossNodesAndReset(t *testing.T) {
	store := &testPolicyStore{doc: PolicyDocument{Overrides: map[string]PolicyConfig{}}}
	a := policyFixture(store)
	b := policyFixture(store)
	ctx := policyOwner()
	require.NoError(t, a.Save(ctx, 0, PolicyConfig{Name: "localFirst", Primary: "app:codex", Fallbacks: []string{"fleet:fastest"}}))
	snapshot, err := b.Snapshot(ctx)
	require.NoError(t, err)
	p, ok := snapshot.Lookup("localFirst")
	require.True(t, ok)
	require.Equal(t, []string{"app:codex", "fleet:fastest"}, p.ProviderChain())
	restarted := policyFixture(store)
	rows, err := restarted.Catalog(ctx)
	require.NoError(t, err)
	var changed PolicyRecord
	for _, row := range rows {
		if row.Name == "localFirst" {
			changed = row
		}
	}
	require.True(t, changed.Customized)
	require.Equal(t, []string{"fleet:strongest", "app:*", "federation:cheapest"}, changed.DefaultChain)
	require.Error(t, b.Save(ctx, 0, PolicyConfig{Name: "localFirst", Primary: "fleet:strongest"}))
	require.NoError(t, b.Reset(ctx, 1, "localFirst"))
	snapshot, err = a.Snapshot(ctx)
	require.NoError(t, err)
	p, _ = snapshot.Lookup("localFirst")
	require.Equal(t, changed.DefaultChain, p.ProviderChain())
	require.NoError(t, a.Save(ctx, 2, PolicyConfig{Name: "myPolicy", Primary: "policy:localOnly", Fallbacks: []string{"app:codex"}}))
	snapshot, err = b.Snapshot(ctx)
	require.NoError(t, err)
	p, _ = snapshot.Lookup("myPolicy")
	require.Equal(t, []string{"fleet:strongest", "app:codex"}, p.ProviderChain())
	require.Error(t, b.Reset(ctx, 3, "myPolicy"))
	require.EqualValues(t, 3, store.doc.Revision)
}
func TestPolicyCustomizationValidationAndAuthority(t *testing.T) {
	store := &testPolicyStore{doc: PolicyDocument{Overrides: map[string]PolicyConfig{}}}
	r := policyFixture(store)
	for _, role := range []auth.Role{auth.RoleReader, auth.RoleWriter, auth.RoleAdmin} {
		ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "other", Role: role})
		require.Error(t, r.Save(ctx, 0, PolicyConfig{Name: "mine", Primary: "app:*"}))
		require.Error(t, r.Reset(ctx, 0, "localFirst"))
	}
	require.Error(t, r.Save(context.Background(), 0, PolicyConfig{Name: "mine", Primary: "app:*"}))
	for _, p := range []PolicyConfig{{Name: "bad name", Primary: "app:*"}, {Name: "mine", Primary: "image:codex"}, {Name: "mine", Primary: "fleet:*"}, {Name: "mine", Primary: "app:*", Fallbacks: []string{"app:*"}}, {Name: "mine", Primary: "policy:missing"}, {Name: "localOnly", Primary: "policy:localOnly"}, {Name: "embeddingsBinding", Primary: "fleet:strongest"}, {Name: "embeddingsBinding", Primary: "embedder:active", Fallbacks: []string{"app:*"}}} {
		require.Error(t, r.Save(policyOwner(), 0, p))
		require.EqualValues(t, 0, store.doc.Revision)
		require.Empty(t, store.doc.Overrides)
	}
	require.NoError(t, r.Save(policyOwner(), 0, PolicyConfig{Name: "one", Primary: "policy:localOnly"}))
	require.Error(t, r.Save(policyOwner(), 1, PolicyConfig{Name: "localOnly", Primary: "policy:one"}))
	require.EqualValues(t, 1, store.doc.Revision)
	store.fail = true
	_, err := r.Snapshot(policyOwner())
	require.ErrorContains(t, err, "database offline")
}
func TestPolicyConcurrentWritesHaveOneWinner(t *testing.T) {
	store := &testPolicyStore{doc: PolicyDocument{Overrides: map[string]PolicyConfig{}}}
	r := policyFixture(store)
	results := make(chan error, 2)
	for _, name := range []string{"one", "two"} {
		go func(name string) { results <- r.Save(policyOwner(), 0, PolicyConfig{Name: name, Primary: "app:*"}) }(name)
	}
	first, second := <-results, <-results
	require.NotEqual(t, first == nil, second == nil)
	require.EqualValues(t, 1, store.doc.Revision)
	require.Len(t, store.doc.Overrides, 1)
}

func TestRoutingConfigurationRulesConsumeCustomPolicies(t *testing.T) {
	ctx := policyOwner()
	store := &testPolicyStore{doc: PolicyDocument{Overrides: map[string]PolicyConfig{}}}
	policies := policyFixture(store)
	base := NewRuleRegistry()
	require.NoError(t, base.Register(&RuleConfig{Name: "default", Policy: "localFirst", Precedence: 0, Locked: true}))
	require.NoError(t, base.Register(&RuleConfig{Name: "embeddingsBound", Policy: "embeddingsBinding", Precedence: 20, Locked: true, When: RuleWhen{Level: "embeddings", Present: map[string]bool{"level": true}}}))
	require.NoError(t, base.Finalize())
	engine := &MemQLEngine{policies: policies, rules: base}
	require.NoError(t, policies.Save(ctx, 0, PolicyConfig{Name: "myPolicy", Primary: "app:codex"}))
	args := map[string]any{"name": "myChat", "conditions": map[string]any{"modality": "chat", "prompt": ""}, "policy": "myPolicy", "precedence": 10, "expectedRevision": 1, "onUnavailable": "park"}
	_, err := engine.routingRuleValidateBuiltin(ctx, args, 0)
	require.NoError(t, err)
	require.Empty(t, store.doc.Rules)
	_, err = engine.routingRuleSaveBuiltin(ctx, args, 0)
	require.NoError(t, err)
	peer := policyFixture(store)
	effective, rules, err := peer.SnapshotRouting(ctx, base)
	require.NoError(t, err)
	p, ok := effective.Lookup("myPolicy")
	require.True(t, ok)
	require.Equal(t, "app:codex", p.Primary)
	require.Equal(t, []string{"embeddingsBound", "myChat", "default"}, []string{rules.Ordered()[0].Name, rules.Ordered()[1].Name, rules.Ordered()[2].Name})
	saved, _ := rules.Lookup("myChat")
	require.True(t, saved.When.Has("prompt"))
	require.Equal(t, "", saved.When.Prompt)
	require.False(t, saved.When.Has("role"))
	_, err = engine.routingRuleSaveBuiltin(ctx, args, 0)
	require.Error(t, err)
	args["name"] = "default"
	args["expectedRevision"] = 2
	_, err = engine.routingRuleSaveBuiltin(ctx, args, 0)
	require.Error(t, err)
	_, err = engine.routingRuleRemoveBuiltin(ctx, map[string]any{"name": "embeddingsBound", "expectedRevision": 2}, 0)
	require.Error(t, err)
	_, err = engine.routingRuleRemoveBuiltin(ctx, map[string]any{"name": "myChat", "expectedRevision": 2}, 0)
	require.NoError(t, err)
	_, rules, err = peer.SnapshotRouting(ctx, base)
	require.NoError(t, err)
	_, ok = rules.Lookup("myChat")
	require.False(t, ok)
}
