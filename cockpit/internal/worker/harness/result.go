package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// result.go reduces a call's result to the two facts the recording keeps
// about it -- its inferred JSON type and a digest -- and holds the small
// JSON helpers the app translations share.

// resultShape types and digests a result value.
//
// value is the result as the app reported it, reduced to what it carries
// (blocksValue, mcpResultValue); nil means the app reported no result at
// all, which types as null. Text is digested as its exact bytes -- the
// same bytes a file holding that text digests to -- and anything else as
// its canonical JSON, so two results that differ only in key order or
// spacing are one result.
func resultShape(value any) (typ, digest string) {
	if s, ok := value.(string); ok {
		return inferTextType(s), Digest([]byte(s))
	}
	canon, err := canonicalJSON(value)
	if err != nil {
		return "", ""
	}
	return jsonType(value), Digest(canon)
}

// inferTextType types text by what it parses as, and as a string when it
// is not JSON at all.
func inferTextType(s string) string {
	v, ok := decodeValue(json.RawMessage(s))
	if !ok {
		return "string"
	}
	return jsonType(v)
}

// jsonType names a decoded value's JSON type.
func jsonType(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case json.Number, float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return ""
}

// decodeValue decodes exactly one JSON value, numbers kept as written. It
// reports false for anything else, including a value with more text
// behind it -- "1 2" is not JSON, whatever a lenient reader makes of it.
//
// Only JSON's own whitespace is trimmed. bytes.TrimSpace would also strip
// a vertical tab or a form feed, and "0\v" would then be typed a number
// although it is not JSON at all (FuzzResultShapeIsClosed found that one;
// its reproducer is under testdata/fuzz).
func decodeValue(raw json.RawMessage) (any, bool) {
	trimmed := bytes.Trim(raw, jsonWhitespace)
	if len(trimmed) == 0 {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return v, true
}

// jsonWhitespace is the whitespace RFC 8259 allows around a value.
const jsonWhitespace = " \t\n\r"

// decodeTolerant decodes raw into v and keeps every field that decoded
// when one did not. A field whose TYPE changed in a later app release is
// then a fact not reported, rather than a whole call not recorded -- the
// tolerance every decoder in this package owes a binary its owner
// upgrades on their own schedule.
func decodeTolerant(raw json.RawMessage, v any) bool {
	err := json.Unmarshal(raw, v)
	var typeErr *json.UnmarshalTypeError
	return err == nil || errors.As(err, &typeErr)
}

// canonicalJSON is the one encoding a value digests as: keys sorted (as
// encoding/json does for a map), no insignificant space, numbers as
// written, and no HTML escaping -- "<" is a byte the result had, not one
// to rewrite.
func canonicalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// blocksValue reduces MCP-style content blocks to what they carry: ONE
// text block is its text, and anything else stays the blocks.
//
// This is what makes one MCP call digest the same from both apps. Claude
// Code hands the model the server's blocks as the tool result, and the
// Codex app-server hands back the same blocks inside a result object --
// recorded from each on 2026-09-13, [{"type":"text","text":"echo: ping"}]
// both times. Several text blocks are NOT joined: ["a","b"] and ["ab"]
// are different results, and a join would make them one.
func blocksValue(v any) any {
	blocks, ok := v.([]any)
	if !ok || len(blocks) != 1 {
		return v
	}
	block, ok := blocks[0].(map[string]any)
	if !ok || block["type"] != "text" {
		return v
	}
	if text, ok := block["text"].(string); ok {
		return text
	}
	return v
}

// mcpResultValue reduces an MCP CallToolResult to its value.
//
// The single text block wins over structuredContent. The MCP
// specification asks a server that returns structured content to return
// its serialised JSON as a text block too, and Claude Code shows the
// model only the blocks -- so preferring the text is what keeps a call
// through Codex digesting the same as the same call through Claude Code.
// The structured content stands in only when there is no single text.
func mcpResultValue(v any) any {
	obj, ok := v.(map[string]any)
	if !ok {
		return blocksValue(v)
	}
	content, hasContent := obj["content"]
	if hasContent {
		if text, ok := blocksValue(content).(string); ok {
			return text
		}
	}
	if structured, ok := obj["structuredContent"]; ok && structured != nil {
		return structured
	}
	if hasContent {
		return content
	}
	return v
}

// rawOrNull is raw, or JSON null when there is nothing in it: Args is
// always a JSON value on the wire, and null is how "no arguments were
// reported" is spelled.
func rawOrNull(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 {
		return json.RawMessage("null")
	}
	return raw
}

// isNull reports whether raw is empty or JSON null.
func isNull(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

// argsOf encodes arguments this package assembles itself, for a call an
// app reports as an item rather than as arguments.
func argsOf(v any) json.RawMessage {
	body, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return body
}
