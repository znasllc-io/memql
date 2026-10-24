package harness

import (
	"encoding/json"
	"testing"
)

func TestResultShape(t *testing.T) {
	decoded := func(raw string) any {
		v, ok := decodeValue(json.RawMessage(raw))
		if !ok {
			t.Fatalf("fixture %q is not JSON", raw)
		}
		return v
	}
	cases := []struct {
		name       string
		value      any
		wantType   string
		wantDigest string
	}{
		{"no result at all", nil, "null", Digest([]byte("null"))},
		{"plain text", "notes.txt", "string", Digest([]byte("notes.txt"))},
		{"empty text", "", "string", Digest(nil)},
		// Text that is JSON is typed by what it parses as, and digested as
		// the exact text -- a command's output is the bytes it printed.
		{"text that is an object", `{"b": 1, "a": 2}`, "object", Digest([]byte(`{"b": 1, "a": 2}`))},
		{"text that is a number", "42", "number", Digest([]byte("42"))},
		{"text that is a boolean", "true", "boolean", Digest([]byte("true"))},
		{"text with JSON and more behind it", `{"a":1} trailing`, "string", Digest([]byte(`{"a":1} trailing`))},
		// A decoded value digests as canonical JSON: keys sorted, numbers
		// as written, nothing HTML-escaped.
		{"decoded object", decoded(`{"b":1.50,"a":[true,null],"h":"<&>"}`), "object",
			Digest([]byte(`{"a":[true,null],"b":1.50,"h":"<&>"}`))},
		{"decoded array", decoded(`[{"type":"tool_reference","tool_name":"x"}]`), "array",
			Digest([]byte(`[{"tool_name":"x","type":"tool_reference"}]`))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			typ, digest := resultShape(tc.value)
			if typ != tc.wantType {
				t.Errorf("type = %q, want %q", typ, tc.wantType)
			}
			if digest != tc.wantDigest {
				t.Errorf("digest = %s, want %s", digest, tc.wantDigest)
			}
		})
	}
}

func TestDecodeValueTakesExactlyOneValue(t *testing.T) {
	for raw, ok := range map[string]bool{
		`{"a":1}`: true, ` [1,2] `: true, `"s"`: true, `null`: true,
		``: false, `   `: false, `1 2`: false, `{"a":`: false, `nope`: false,
	} {
		if _, got := decodeValue(json.RawMessage(raw)); got != ok {
			t.Errorf("decodeValue(%q) ok = %v, want %v", raw, got, ok)
		}
	}
	v, _ := decodeValue(json.RawMessage(`12345678901234567890`))
	if n, isNumber := v.(json.Number); !isNumber || n.String() != "12345678901234567890" {
		t.Errorf("a large number came back as %#v; numbers are kept as written", v)
	}
}

func TestDecodeTolerantKeepsTheFieldsThatFit(t *testing.T) {
	var got struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if !decodeTolerant(json.RawMessage(`{"id":"x","status":7}`), &got) {
		t.Fatal("a type change in one field refused the whole object")
	}
	if got.ID != "x" {
		t.Errorf("id = %q, want the field that did decode", got.ID)
	}
	if decodeTolerant(json.RawMessage(`{"id":`), &got) {
		t.Error("malformed JSON was accepted")
	}
}

func TestBlocksValue(t *testing.T) {
	one := []any{map[string]any{"type": "text", "text": "echo: ping"}}
	if got := blocksValue(one); got != "echo: ping" {
		t.Errorf("one text block = %#v, want its text", got)
	}
	two := []any{map[string]any{"type": "text", "text": "a"}, map[string]any{"type": "text", "text": "b"}}
	if got, ok := blocksValue(two).([]any); !ok || len(got) != 2 {
		t.Errorf("two text blocks = %#v, want the blocks: joining makes [a b] and [ab] one result", blocksValue(two))
	}
	image := []any{map[string]any{"type": "image", "data": "..."}}
	if _, ok := blocksValue(image).([]any); !ok {
		t.Error("a non-text block was reduced")
	}
	if got := blocksValue("already text"); got != "already text" {
		t.Errorf("text = %#v", got)
	}
}

func TestMCPResultValuePrefersTheText(t *testing.T) {
	v, _ := decodeValue(json.RawMessage(
		`{"content":[{"type":"text","text":"{\"a\":1}"}],"structuredContent":{"a":1},"_meta":null}`))
	if got := mcpResultValue(v); got != `{"a":1}` {
		t.Errorf("= %#v, want the text block, which is what Claude Code shows the model", got)
	}
	v, _ = decodeValue(json.RawMessage(`{"content":[],"structuredContent":{"a":1}}`))
	if got, ok := mcpResultValue(v).(map[string]any); !ok || got["a"] == nil {
		t.Errorf("= %#v, want the structured content when there is no single text", mcpResultValue(v))
	}
	v, _ = decodeValue(json.RawMessage(`{"content":[{"type":"image"}],"structuredContent":null}`))
	if _, ok := mcpResultValue(v).([]any); !ok {
		t.Errorf("= %#v, want the blocks", mcpResultValue(v))
	}
}

func TestArgsOfAndNulls(t *testing.T) {
	if got := string(argsOf(map[string]any{"b": nil, "a": json.RawMessage(`{"x":1}`)})); got != `{"a":{"x":1},"b":null}` {
		t.Errorf("argsOf = %s", got)
	}
	if !isNull(nil) || !isNull(json.RawMessage(" null ")) || isNull(json.RawMessage("{}")) {
		t.Error("isNull is wrong")
	}
	if string(rawOrNull(nil)) != "null" || string(rawOrNull(json.RawMessage(`{}`))) != "{}" {
		t.Error("rawOrNull is wrong")
	}
}
