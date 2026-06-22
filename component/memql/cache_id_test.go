package memql

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/znasllc-io/memql/core/id"
)

func TestCacheKey_Deterministic(t *testing.T) {
	// This test documents that we're intentionally changing the hash algorithm.
	// The old implementation used direct SHA256, the new uses contentid which
	// also uses SHA256 but with different input construction.
	//
	// This is acceptable because:
	// 1. Cache keys are ephemeral (in-memory cache)
	// 2. A cache miss on upgrade is harmless
	// 3. The new keys are equally deterministic

	engine := id.New()

	// Verify contentid produces deterministic output
	key1 := engine.MustFromMap(map[string]any{"query": "test", "limit": 10})
	key2 := engine.MustFromMap(map[string]any{"query": "test", "limit": 10})

	if key1 != key2 {
		t.Error("id.MustFromMap should be deterministic")
	}
}

func TestCacheKey_DifferentInputs(t *testing.T) {
	engine := id.New()

	key1 := engine.MustFromMap(map[string]any{"query": "test1", "limit": 10})
	key2 := engine.MustFromMap(map[string]any{"query": "test2", "limit": 10})

	if key1 == key2 {
		t.Error("different inputs should produce different keys")
	}
}

func TestAICacheKey_Deterministic(t *testing.T) {
	key1 := buildAICacheKey("template1", "openai", "prompt text")
	key2 := buildAICacheKey("template1", "openai", "prompt text")

	if key1 != key2 {
		t.Error("AI cache key should be deterministic")
	}
}

func TestAICacheKey_DifferentInputs(t *testing.T) {
	key1 := buildAICacheKey("template1", "openai", "prompt text")
	key2 := buildAICacheKey("template1", "anthropic", "prompt text")

	if key1 == key2 {
		t.Error("different providers should produce different keys")
	}
}

func TestShapeTemplateSignature_Deterministic(t *testing.T) {
	// Use a nil template which returns the default "graph-bundle" signature
	sig1 := shapeTemplateSignature(nil)
	sig2 := shapeTemplateSignature(nil)

	if sig1 != sig2 {
		t.Error("shape template signature should be deterministic")
	}

	if sig1 != "graph-bundle" {
		t.Errorf("nil template should return 'graph-bundle', got %q", sig1)
	}
}

func BenchmarkCacheKey_Old(b *testing.B) {
	query := "concept==v1:user;payload.active==true"
	for i := 0; i < b.N; i++ {
		hash := sha256.Sum256([]byte(query + "|ts=latest|limit=100|offset=0|depth=3|sort=createdAt:desc|select=none|shape=graph-bundle"))
		_ = hex.EncodeToString(hash[:])
	}
}

func BenchmarkCacheKey_New(b *testing.B) {
	engine := id.New()
	for i := 0; i < b.N; i++ {
		_ = engine.MustFromMap(map[string]any{
			"query":     "concept==v1:user;payload.active==true",
			"timestamp": "latest",
			"limit":     100,
			"offset":    0,
			"depth":     3,
			"sort":      "createdAt:desc",
			"select":    "none",
			"shape":     "graph-bundle",
		})
	}
}

func BenchmarkAICacheKey_Old(b *testing.B) {
	templateId := "template1"
	provider := "openai"
	prompt := "This is a test prompt with some content"
	for i := 0; i < b.N; i++ {
		input := templateId + "|" + provider + "|" + prompt
		hash := sha256.Sum256([]byte(input))
		_ = hex.EncodeToString(hash[:])
	}
}

func BenchmarkAICacheKey_New(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = buildAICacheKey("template1", "openai", "This is a test prompt with some content")
	}
}
