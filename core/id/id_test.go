package id

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// AXIOM TESTS
// =============================================================================

func TestAxiom_Deterministic(t *testing.T) {
	engine := New()

	// Same input always produces same output
	id1 := engine.FromString("hello")
	id2 := engine.FromString("hello")
	id3 := engine.FromString("hello")

	assert.Equal(t, id1, id2)
	assert.Equal(t, id2, id3)
}

func TestAxiom_Commutative(t *testing.T) {
	engine := New()

	a := engine.FromString("alpha")
	b := engine.FromString("beta")

	// Order doesn't matter
	ab := engine.Combine(a, b)
	ba := engine.Combine(b, a)

	assert.Equal(t, ab, ba)
}

func TestAxiom_Idempotent(t *testing.T) {
	engine := New()

	a := engine.FromString("content")

	// Combining with self returns self
	aa := engine.Combine(a, a)

	assert.Equal(t, a, aa)
}

func TestAxiom_Idempotent_BaseIds(t *testing.T) {
	engine := New()

	// Base IDs also satisfy idempotency
	assert.Equal(t, ID("0"), engine.Combine("0", "0"))
	assert.Equal(t, ID("1"), engine.Combine("1", "1"))
}

// =============================================================================
// ENGINE TESTS
// =============================================================================

func TestNew(t *testing.T) {
	engine := New()

	assert.True(t, engine.Exists("0"))
	assert.True(t, engine.Exists("1"))
	assert.False(t, engine.Exists("nonexistent"))
}

func TestCombine_ProducesValidHash(t *testing.T) {
	engine := New()

	result := engine.Combine("0", "1")

	// Should be 64-char hex (SHA256)
	assert.Len(t, string(result), 64)
	_, err := hex.DecodeString(string(result))
	assert.NoError(t, err)
}

func TestCombine_MatchesExpectedHash(t *testing.T) {
	engine := New()

	// Manually verify: combine("0", "1") = SHA256("0:1")
	expected := sha256.Sum256([]byte("0:1"))
	expectedHex := hex.EncodeToString(expected[:])

	result := engine.Combine("0", "1")

	assert.Equal(t, ID(expectedHex), result)
}

func TestFromBytes_Empty(t *testing.T) {
	engine := New()

	assert.Equal(t, Root, engine.FromBytes(nil))
	assert.Equal(t, Root, engine.FromBytes([]byte{}))
}

func TestFromBytes_Deterministic(t *testing.T) {
	engine := New()

	data := []byte("test content")

	id1 := engine.FromBytes(data)
	id2 := engine.FromBytes(data)

	assert.Equal(t, id1, id2)
}

func TestFromBytes_Sensitive(t *testing.T) {
	engine := New()

	id1 := engine.FromBytes([]byte("test1"))
	id2 := engine.FromBytes([]byte("test2"))

	assert.NotEqual(t, id1, id2)
}

func TestFromString(t *testing.T) {
	engine := New()

	id := engine.FromString("hello world")

	assert.Len(t, string(id), 64)
}

func TestFromMap(t *testing.T) {
	engine := New()

	m := map[string]any{
		"name":  "test",
		"value": 42,
	}

	id, err := engine.FromMap(m)

	require.NoError(t, err)
	assert.Len(t, string(id), 64)
}

func TestFromMap_KeyOrderIndependent(t *testing.T) {
	engine := New()

	// Different iteration orders, same content
	m1 := map[string]any{"z": 1, "a": 2, "m": 3}
	m2 := map[string]any{"a": 2, "z": 1, "m": 3}
	m3 := map[string]any{"m": 3, "a": 2, "z": 1}

	id1, _ := engine.FromMap(m1)
	id2, _ := engine.FromMap(m2)
	id3, _ := engine.FromMap(m3)

	assert.Equal(t, id1, id2)
	assert.Equal(t, id2, id3)
}

func TestMustFromMap_Panics(t *testing.T) {
	engine := New()

	assert.Panics(t, func() {
		engine.MustFromMap(map[string]any{"bad": make(chan int)})
	})
}

func TestExists(t *testing.T) {
	engine := New()

	id := engine.FromString("content")

	assert.True(t, engine.Exists(id))
	assert.False(t, engine.Exists("unknown"))
}

// =============================================================================
// CONCURRENCY TESTS
// =============================================================================

func TestConcurrency_Combine(t *testing.T) {
	engine := New()

	a := engine.FromString("alpha")
	b := engine.FromString("beta")

	var wg sync.WaitGroup
	results := make([]ID, 100)

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx] = engine.Combine(a, b)
		}(i)
	}

	wg.Wait()

	// All results must be identical
	for i := 1; i < len(results); i++ {
		assert.Equal(t, results[0], results[i])
	}
}

func TestConcurrency_FromBytes(t *testing.T) {
	engine := New()
	data := []byte("concurrent test data")

	var wg sync.WaitGroup
	results := make([]ID, 100)

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx] = engine.FromBytes(data)
		}(i)
	}

	wg.Wait()

	for i := 1; i < len(results); i++ {
		assert.Equal(t, results[0], results[i])
	}
}

// =============================================================================
// CROSS-ENGINE CONSISTENCY
// =============================================================================

func TestCrossEngine_Consistency(t *testing.T) {
	engine1 := New()
	engine2 := New()

	// Independent engines must produce identical results
	data := []byte("cross engine test")

	id1 := engine1.FromBytes(data)
	id2 := engine2.FromBytes(data)

	assert.Equal(t, id1, id2)
}

// =============================================================================
// SENSITIVITY / AVALANCHE TESTS
// =============================================================================

func TestSensitivity_SingleBitFlip(t *testing.T) {
	engine := New()

	original := []byte("test data for sensitivity")
	modified := make([]byte, len(original))
	copy(modified, original)
	modified[10] ^= 0x01

	id1 := engine.FromBytes(original)
	id2 := engine.FromBytes(modified)

	assert.NotEqual(t, id1, id2)

	// Avalanche effect: many characters should differ
	if len(id1) == 64 && len(id2) == 64 {
		differences := 0
		for i := 0; i < 64; i++ {
			if id1[i] != id2[i] {
				differences++
			}
		}
		assert.Greater(t, differences, 20)
	}
}

// =============================================================================
// BENCHMARKS
// =============================================================================

func BenchmarkCombine(b *testing.B) {
	engine := New()
	a := engine.FromString("alpha")
	beta := engine.FromString("beta")
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		engine.Combine(a, beta)
	}
}

func BenchmarkFromBytes_Small(b *testing.B) {
	engine := New()
	data := []byte("small")
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		engine.FromBytes(data)
	}
}

func BenchmarkFromBytes_Large(b *testing.B) {
	engine := New()
	data := make([]byte, 10000)
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		engine.FromBytes(data)
	}
}

func BenchmarkFromMap(b *testing.B) {
	engine := New()
	m := map[string]any{
		"type":    "message",
		"content": "hello world",
		"metadata": map[string]any{
			"timestamp": "2024-01-01",
			"author":    "system",
		},
	}
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		engine.FromMap(m)
	}
}
