package id

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// Marshal produces deterministic JSON bytes.
// Keys are sorted alphabetically, whitespace is minimized.
//
// Unlike json.Marshal, this guarantees identical output for identical data,
// regardless of map iteration order.
func Marshal(v any) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}

	var generic any
	if err := json.Unmarshal(data, &generic); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}

	sorted := sortValue(generic)

	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(sorted); err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}

	result := buf.Bytes()
	if len(result) > 0 && result[len(result)-1] == '\n' {
		result = result[:len(result)-1]
	}

	return result, nil
}

// MustMarshal is like Marshal but panics on error.
func MustMarshal(v any) []byte {
	data, err := Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}

func sortValue(v any) any {
	switch val := v.(type) {
	case map[string]any:
		return sortMap(val)
	case []any:
		return sortSlice(val)
	default:
		return v
	}
}

func sortMap(m map[string]any) any {
	if m == nil {
		return nil
	}

	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	ordered := make(orderedMap, 0, len(keys))
	for _, k := range keys {
		ordered = append(ordered, orderedEntry{
			Key:   k,
			Value: sortValue(m[k]),
		})
	}

	return ordered
}

func sortSlice(s []any) []any {
	if s == nil {
		return nil
	}

	result := make([]any, len(s))
	for i, v := range s {
		result[i] = sortValue(v)
	}
	return result
}

type orderedEntry struct {
	Key   string
	Value any
}

type orderedMap []orderedEntry

func (o orderedMap) MarshalJSON() ([]byte, error) {
	if o == nil {
		return []byte("null"), nil
	}

	var buf bytes.Buffer
	buf.WriteByte('{')

	for i, entry := range o {
		if i > 0 {
			buf.WriteByte(',')
		}

		key, err := json.Marshal(entry.Key)
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')

		value, err := json.Marshal(entry.Value)
		if err != nil {
			return nil, err
		}
		buf.Write(value)
	}

	buf.WriteByte('}')
	return buf.Bytes(), nil
}
