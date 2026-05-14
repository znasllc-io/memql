package id

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// MARSHAL TESTS
// =============================================================================

func TestMarshal_Empty(t *testing.T) {
	result, err := Marshal(map[string]any{})
	require.NoError(t, err)
	assert.Equal(t, "{}", string(result))
}

func TestMarshal_Nil(t *testing.T) {
	var m map[string]any
	result, err := Marshal(m)
	require.NoError(t, err)
	assert.Equal(t, "null", string(result))
}

func TestMarshal_Simple(t *testing.T) {
	m := map[string]any{
		"name": "test",
		"age":  30,
	}
	result, err := Marshal(m)
	require.NoError(t, err)
	assert.Equal(t, `{"age":30,"name":"test"}`, string(result))
}

func TestMarshal_SortedKeys(t *testing.T) {
	m1 := map[string]any{"z": 1, "a": 2, "m": 3}
	m2 := map[string]any{"a": 2, "z": 1, "m": 3}
	m3 := map[string]any{"m": 3, "z": 1, "a": 2}

	r1, _ := Marshal(m1)
	r2, _ := Marshal(m2)
	r3, _ := Marshal(m3)

	assert.Equal(t, string(r1), string(r2))
	assert.Equal(t, string(r2), string(r3))
	assert.Equal(t, `{"a":2,"m":3,"z":1}`, string(r1))
}

func TestMarshal_Nested(t *testing.T) {
	m := map[string]any{
		"outer": map[string]any{
			"b": 2,
			"a": 1,
		},
		"simple": "value",
	}

	result, err := Marshal(m)
	require.NoError(t, err)
	assert.Equal(t, `{"outer":{"a":1,"b":2},"simple":"value"}`, string(result))
}

func TestMarshal_DeeplyNested(t *testing.T) {
	m := map[string]any{
		"level1": map[string]any{
			"level2": map[string]any{
				"level3": map[string]any{
					"z": 1,
					"a": 2,
				},
			},
		},
	}

	result, err := Marshal(m)
	require.NoError(t, err)
	assert.Equal(t, `{"level1":{"level2":{"level3":{"a":2,"z":1}}}}`, string(result))
}

func TestMarshal_ArrayOfMaps(t *testing.T) {
	arr := []map[string]any{
		{"z": 1, "a": 2},
		{"b": 3, "a": 4},
	}

	result, err := Marshal(arr)
	require.NoError(t, err)
	assert.Equal(t, `[{"a":2,"z":1},{"a":4,"b":3}]`, string(result))
}

func TestMarshal_MapWithArrays(t *testing.T) {
	m := map[string]any{
		"items": []any{1, 2, 3},
		"name":  "test",
	}

	result, err := Marshal(m)
	require.NoError(t, err)
	assert.Equal(t, `{"items":[1,2,3],"name":"test"}`, string(result))
}

func TestMarshal_Deterministic(t *testing.T) {
	m := map[string]any{
		"type":    "message",
		"content": "hello world",
		"metadata": map[string]any{
			"timestamp": "2024-01-01T00:00:00Z",
			"author":    "system",
		},
	}

	var results []string
	for i := 0; i < 100; i++ {
		result, err := Marshal(m)
		require.NoError(t, err)
		results = append(results, string(result))
	}

	for i := 1; i < len(results); i++ {
		assert.Equal(t, results[0], results[i])
	}
}

func TestMarshal_SpecialCharacters(t *testing.T) {
	m := map[string]any{
		"emoji":   "😀",
		"unicode": "日本語",
		"escape":  "line1\nline2\ttab",
	}

	result, err := Marshal(m)
	require.NoError(t, err)

	var parsed map[string]any
	err = json.Unmarshal(result, &parsed)
	require.NoError(t, err)
	assert.Equal(t, m["emoji"], parsed["emoji"])
	assert.Equal(t, m["unicode"], parsed["unicode"])
	assert.Equal(t, m["escape"], parsed["escape"])
}

func TestMarshal_Numbers(t *testing.T) {
	m := map[string]any{
		"float":    3.14159,
		"int":      42,
		"negative": -100,
		"zero":     0,
	}

	result, err := Marshal(m)
	require.NoError(t, err)
	assert.Equal(t, `{"float":3.14159,"int":42,"negative":-100,"zero":0}`, string(result))
}

func TestMarshal_BoolAndNull(t *testing.T) {
	m := map[string]any{
		"true":  true,
		"false": false,
		"null":  nil,
	}

	result, err := Marshal(m)
	require.NoError(t, err)
	assert.Equal(t, `{"false":false,"null":null,"true":true}`, string(result))
}

func TestMarshal_Struct(t *testing.T) {
	type testStruct struct {
		Name  string `json:"name"`
		Value int    `json:"value"`
	}

	s := testStruct{Name: "test", Value: 42}
	result, err := Marshal(s)
	require.NoError(t, err)
	assert.Equal(t, `{"name":"test","value":42}`, string(result))
}

func TestMustMarshal_Success(t *testing.T) {
	m := map[string]any{"key": "value"}
	result := MustMarshal(m)
	assert.Equal(t, `{"key":"value"}`, string(result))
}

func TestMustMarshal_Panics(t *testing.T) {
	assert.Panics(t, func() {
		MustMarshal(make(chan int))
	})
}

// =============================================================================
// BENCHMARKS
// =============================================================================

func BenchmarkMarshal_Small(b *testing.B) {
	m := map[string]any{
		"id":   "12345",
		"type": "message",
	}

	for i := 0; i < b.N; i++ {
		Marshal(m)
	}
}

func BenchmarkMarshal_Nested(b *testing.B) {
	m := map[string]any{
		"level1": map[string]any{
			"level2": map[string]any{
				"level3": map[string]any{
					"data": "value",
				},
			},
		},
	}

	for i := 0; i < b.N; i++ {
		Marshal(m)
	}
}
