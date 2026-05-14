// Package contentid provides deterministic, content-addressable ID generation.
//
// IDs are derived from content using SHA256, making them reproducible and verifiable.
// The core operations satisfy three axioms:
//
//   - Deterministic: Same content always produces the same ID
//   - Commutative: Combine(a, b) == Combine(b, a)
//   - Idempotent: Combine(a, a) == a
//
// # Basic Usage
//
//	engine := contentid.New()
//	id := engine.FromString("hello world")
//	combined := engine.Combine(id1, id2)
//
// # Chaining
//
// For ordered sequences where each state depends on the previous:
//
//	chain := contentid.NewChain()
//	id, _ := chain.Next(engine.FromString("first action"), engine)
//	chain.Advance(id)
package id

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
)

// ID is a content-addressed identifier.
// IDs are 64-character hex strings (SHA256), except for base IDs "0" and "1".
type ID string

// Root is the starting point for chains.
const Root ID = "0"

// Engine generates content-addressed IDs. Safe for concurrent use.
type Engine struct {
	cache     sync.Map
	byteNodes [256]ID
	byteOnce  sync.Once
}

// New creates an Engine.
func New() *Engine {
	e := &Engine{}
	e.cache.Store(ID("0"), true)
	e.cache.Store(ID("1"), true)
	return e
}

// Combine merges two IDs into one deterministically.
// Order doesn't matter: Combine(a, b) == Combine(b, a).
// Idempotent: Combine(a, a) == a.
func (e *Engine) Combine(a, b ID) ID {
	if a == b {
		return a
	}

	first, second := a, b
	if first > second {
		first, second = second, first
	}

	input := string(first) + ":" + string(second)
	hash := sha256.Sum256([]byte(input))
	id := ID(hex.EncodeToString(hash[:]))

	e.cache.Store(id, true)
	return id
}

// FromBytes generates an ID from raw bytes.
func (e *Engine) FromBytes(data []byte) ID {
	if len(data) == 0 {
		return Root
	}

	e.initByteNodes()

	current := Root
	for _, b := range data {
		current = e.Combine(current, e.byteNodes[b])
	}
	return current
}

// FromString generates an ID from a string.
func (e *Engine) FromString(s string) ID {
	return e.FromBytes([]byte(s))
}

// FromMap generates an ID from a map. Keys are sorted for determinism.
func (e *Engine) FromMap(m map[string]any) (ID, error) {
	data, err := Marshal(m)
	if err != nil {
		return "", err
	}
	return e.FromBytes(data), nil
}

// MustFromMap is like FromMap but panics on error.
func (e *Engine) MustFromMap(m map[string]any) ID {
	id, err := e.FromMap(m)
	if err != nil {
		panic(err)
	}
	return id
}

// Exists returns true if an ID has been seen by this engine.
func (e *Engine) Exists(id ID) bool {
	_, ok := e.cache.Load(id)
	return ok
}

// initByteNodes precomputes IDs for all 256 byte values.
func (e *Engine) initByteNodes() {
	e.byteOnce.Do(func() {
		for b := 0; b <= 255; b++ {
			e.byteNodes[b] = e.computeByteId(byte(b))
		}
	})
}

// computeByteId computes the ID for a byte by walking its bits MSB to LSB.
func (e *Engine) computeByteId(b byte) ID {
	current := Root
	for i := 7; i >= 0; i-- {
		bit := (b >> i) & 1
		if bit == 1 {
			current = e.Combine(current, "1")
		} else {
			current = e.Combine(current, "0")
		}
	}
	return current
}
