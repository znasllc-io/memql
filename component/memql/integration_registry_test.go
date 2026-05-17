package memql

import (
	"context"
	"sync"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// mockProvider implements IntegrationProvider for testing.
type mockProvider struct {
	name         string
	capabilities []IntegrationCapability
}

func (m *mockProvider) IntegrationName() string              { return m.name }
func (m *mockProvider) Capabilities() []IntegrationCapability { return m.capabilities }

func TestIntegrationRegistry_Register(t *testing.T) {
	r := newIntegrationRegistry()

	handler := func(_ context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
		return nil, nil
	}

	provider := &mockProvider{
		name: "test",
		capabilities: []IntegrationCapability{
			{Name: "doSomething", Handler: handler},
			{Name: "doOther", Handler: handler},
		},
	}

	if err := r.Register(provider); err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	// Verify capabilities are indexed.
	if h, ok := r.Get("integration.test.doSomething"); !ok || h == nil {
		t.Error("expected doSomething capability to be registered")
	}
	if h, ok := r.Get("integration.test.doOther"); !ok || h == nil {
		t.Error("expected doOther capability to be registered")
	}

	// Verify non-existent capability returns false.
	if _, ok := r.Get("integration.test.notExist"); ok {
		t.Error("expected notExist to not be found")
	}

	// Verify provider names.
	names := r.ProviderNames()
	if len(names) != 1 || names[0] != "test" {
		t.Errorf("expected [test], got %v", names)
	}

	// Verify capability list.
	caps := r.List()
	if len(caps) != 2 {
		t.Errorf("expected 2 capabilities, got %d", len(caps))
	}
}

func TestIntegrationRegistry_DuplicateProvider(t *testing.T) {
	r := newIntegrationRegistry()

	provider := &mockProvider{name: "dup"}
	if err := r.Register(provider); err != nil {
		t.Fatalf("first register failed: %v", err)
	}

	if err := r.Register(provider); err == nil {
		t.Error("expected error on duplicate registration")
	}
}

func TestIntegrationRegistry_NilProvider(t *testing.T) {
	r := newIntegrationRegistry()
	if err := r.Register(nil); err == nil {
		t.Error("expected error on nil provider")
	}
}

func TestIntegrationRegistry_EmptyName(t *testing.T) {
	r := newIntegrationRegistry()
	provider := &mockProvider{name: ""}
	if err := r.Register(provider); err == nil {
		t.Error("expected error on empty name")
	}
}

func TestIntegrationRegistry_EmptyCapabilityName(t *testing.T) {
	r := newIntegrationRegistry()
	provider := &mockProvider{
		name: "test",
		capabilities: []IntegrationCapability{
			{Name: "", Handler: func(_ context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
				return nil, nil
			}},
		},
	}
	if err := r.Register(provider); err == nil {
		t.Error("expected error on empty capability name")
	}
}

func TestIntegrationRegistry_NilCapabilities(t *testing.T) {
	r := newIntegrationRegistry()
	provider := &mockProvider{name: "nocaps", capabilities: nil}
	if err := r.Register(provider); err != nil {
		t.Fatalf("register with nil capabilities should succeed: %v", err)
	}
	names := r.ProviderNames()
	if len(names) != 1 || names[0] != "nocaps" {
		t.Errorf("expected [nocaps], got %v", names)
	}
}

func TestIntegrationRegistry_ConcurrentAccess(t *testing.T) {
	r := newIntegrationRegistry()
	handler := func(_ context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
		return nil, nil
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			name := "provider" + string(rune('a'+idx))
			p := &mockProvider{
				name:         name,
				capabilities: []IntegrationCapability{{Name: "cap", Handler: handler}},
			}
			_ = r.Register(p) // some may fail on race, that's fine
		}(i)
	}

	// Concurrent reads while writing.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.List()
			_, _ = r.Get("integration.providera.cap")
		}()
	}

	wg.Wait()

	// At least some should have registered.
	if len(r.ProviderNames()) == 0 {
		t.Error("expected at least some providers to register")
	}
}
