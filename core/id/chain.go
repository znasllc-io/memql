package id

// ChainTracker tracks a chain of content-addressed IDs.
// Each new ID is derived by combining the current head with new content.
// This creates an ordered, verifiable sequence where each state depends on the previous.
//
// Implementations can back the chain with different storage mechanisms
// (memory, database, distributed systems) while maintaining the same interface.
//
// Usage:
//
//	prev := chain.Head()
//	newId, _ := chain.Next(contentId, engine)
//	entity := Entity{ID: newId, PreviousId: prev}
//	chain.Advance(newId)
type ChainTracker interface {
	// Head returns the current chain head. Returns Root for empty chains.
	Head() ID

	// Next computes the next ID without updating state.
	// Formula: nextId = Combine(Head(), content)
	Next(content ID, engine *Engine) ID

	// Advance moves the chain head forward. Call after persisting.
	Advance(newHead ID)
}

// Chain is a simple in-memory ChainTracker implementation.
type Chain struct {
	head ID
}

// NewChain creates a Chain starting at Root.
func NewChain() *Chain {
	return &Chain{head: Root}
}

// NewChainFrom creates a Chain starting at a specific head.
func NewChainFrom(head ID) *Chain {
	return &Chain{head: head}
}

// Head returns the current chain head.
func (c *Chain) Head() ID {
	return c.head
}

// Next computes the next ID by combining current head with content.
func (c *Chain) Next(content ID, engine *Engine) ID {
	return engine.Combine(c.head, content)
}

// Advance moves the chain head to a new position.
func (c *Chain) Advance(newHead ID) {
	c.head = newHead
}

var _ ChainTracker = (*Chain)(nil)
