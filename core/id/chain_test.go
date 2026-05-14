package id

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

// =============================================================================
// CHAIN TESTS
// =============================================================================

func TestNewChain_StartsAtRoot(t *testing.T) {
	chain := NewChain()

	assert.Equal(t, Root, chain.Head())
}

func TestNewChainFrom(t *testing.T) {
	chain := NewChainFrom(ID("custom-head"))

	assert.Equal(t, ID("custom-head"), chain.Head())
}

func TestChain_Next_Deterministic(t *testing.T) {
	engine := New()
	chain := NewChain()

	content := engine.FromString("action")

	id1 := chain.Next(content, engine)
	id2 := chain.Next(content, engine)
	id3 := chain.Next(content, engine)

	assert.Equal(t, id1, id2)
	assert.Equal(t, id2, id3)
}

func TestChain_Next_DoesNotAdvance(t *testing.T) {
	engine := New()
	chain := NewChain()

	content := engine.FromString("action")
	originalHead := chain.Head()

	chain.Next(content, engine)

	assert.Equal(t, originalHead, chain.Head())
}

func TestChain_Advance(t *testing.T) {
	engine := New()
	chain := NewChain()

	content := engine.FromString("action")
	newId := chain.Next(content, engine)

	chain.Advance(newId)

	assert.Equal(t, newId, chain.Head())
	assert.NotEqual(t, Root, chain.Head())
}

func TestChain_Sequence(t *testing.T) {
	engine := New()
	chain := NewChain()

	history := []ID{chain.Head()}

	actions := []string{"first", "second", "third"}
	for _, action := range actions {
		content := engine.FromString(action)
		newId := chain.Next(content, engine)
		chain.Advance(newId)
		history = append(history, newId)
	}

	// All states must be unique
	seen := make(map[ID]bool)
	for _, id := range history {
		assert.False(t, seen[id], "Duplicate ID in chain")
		seen[id] = true
	}
}

func TestChain_OrderMatters(t *testing.T) {
	engine := New()
	chain1 := NewChain()
	chain2 := NewChain()

	a := engine.FromString("A")
	b := engine.FromString("B")

	// Chain1: A then B
	id := chain1.Next(a, engine)
	chain1.Advance(id)
	id = chain1.Next(b, engine)
	chain1.Advance(id)

	// Chain2: B then A
	id = chain2.Next(b, engine)
	chain2.Advance(id)
	id = chain2.Next(a, engine)
	chain2.Advance(id)

	assert.NotEqual(t, chain1.Head(), chain2.Head())
}

func TestChain_Reproducible(t *testing.T) {
	engine1 := New()
	engine2 := New()
	chain1 := NewChain()
	chain2 := NewChain()

	actions := []string{"first", "second", "third"}

	for _, action := range actions {
		c1 := engine1.FromString(action)
		c2 := engine2.FromString(action)

		id1 := chain1.Next(c1, engine1)
		id2 := chain2.Next(c2, engine2)

		assert.Equal(t, id1, id2)

		chain1.Advance(id1)
		chain2.Advance(id2)
	}

	assert.Equal(t, chain1.Head(), chain2.Head())
}

func TestChain_DifferentContent_DifferentIds(t *testing.T) {
	engine := New()
	chain1 := NewChain()
	chain2 := NewChain()

	id1 := chain1.Next(engine.FromString("hello"), engine)
	id2 := chain2.Next(engine.FromString("goodbye"), engine)

	assert.NotEqual(t, id1, id2)
}

// =============================================================================
// CHAIN LINKAGE TESTS
// =============================================================================

type Message struct {
	ID         ID
	Content    string
	PreviousId ID
}

func TestChain_Linkage(t *testing.T) {
	engine := New()
	chain := NewChain()

	var messages []Message
	contents := []string{"first", "second", "third"}

	for _, content := range contents {
		prevId := chain.Head()
		newId := chain.Next(engine.FromString(content), engine)

		messages = append(messages, Message{
			ID:         newId,
			Content:    content,
			PreviousId: prevId,
		})

		chain.Advance(newId)
	}

	// First message links to root
	assert.Equal(t, Root, messages[0].PreviousId)

	// Each subsequent message links to previous
	for i := 1; i < len(messages); i++ {
		assert.Equal(t, messages[i-1].ID, messages[i].PreviousId)
	}
}

func TestChain_Reconstruction(t *testing.T) {
	engine := New()
	chain := NewChain()

	// Build original chain
	var messages []Message
	contents := []string{"first", "second", "third"}

	for _, content := range contents {
		prevId := chain.Head()
		newId := chain.Next(engine.FromString(content), engine)
		messages = append(messages, Message{
			ID:         newId,
			Content:    content,
			PreviousId: prevId,
		})
		chain.Advance(newId)
	}

	// Reconstruct and verify
	for _, msg := range messages {
		verifyChain := NewChainFrom(msg.PreviousId)
		reconstructedId := verifyChain.Next(engine.FromString(msg.Content), engine)
		assert.Equal(t, msg.ID, reconstructedId)
	}
}

// =============================================================================
// CUSTOM CHAIN TRACKER IMPLEMENTATION
// =============================================================================

// threadSafeChain demonstrates a custom ChainTracker implementation
type threadSafeChain struct {
	mu   sync.RWMutex
	head ID
}

func newThreadSafeChain() *threadSafeChain {
	return &threadSafeChain{head: Root}
}

func (c *threadSafeChain) Head() ID {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.head
}

func (c *threadSafeChain) Next(content ID, engine *Engine) ID {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return engine.Combine(c.head, content)
}

func (c *threadSafeChain) Advance(newHead ID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.head = newHead
}

var _ ChainTracker = (*threadSafeChain)(nil)

func TestChainTracker_CustomImplementation(t *testing.T) {
	engine := New()
	chain := newThreadSafeChain()

	content := engine.FromString("test")
	newId := chain.Next(content, engine)
	chain.Advance(newId)

	assert.Equal(t, newId, chain.Head())
}

func TestChainTracker_ConcurrentAccess(t *testing.T) {
	engine := New()
	chain := newThreadSafeChain()

	content := engine.FromString("concurrent")

	var wg sync.WaitGroup
	results := make([]ID, 100)

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx] = chain.Next(content, engine)
		}(i)
	}

	wg.Wait()

	// All should be identical (same head, same content)
	for i := 1; i < len(results); i++ {
		assert.Equal(t, results[0], results[i])
	}
}

// =============================================================================
// BENCHMARKS
// =============================================================================

func BenchmarkChain_Next(b *testing.B) {
	engine := New()
	chain := NewChain()
	content := engine.FromString("action")
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		chain.Next(content, engine)
	}
}

func BenchmarkChain_Sequence(b *testing.B) {
	engine := New()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		chain := NewChain()
		for j := 0; j < 10; j++ {
			content := engine.FromString("action")
			id := chain.Next(content, engine)
			chain.Advance(id)
		}
	}
}
