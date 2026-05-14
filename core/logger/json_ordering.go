package logger

import (
	"bytes"
	"encoding/json"
	"io"
)

var orderedJSONKeys = []string{"time", "level", "component", "msg"}

// NewOrderedJSONWriter wraps the provided writer and ensures that JSON log objects
// emit keys in a consistent order: time, level, component, msg, followed by the
// remaining fields in their original order.
func NewOrderedJSONWriter(dst io.Writer) io.Writer {
	if dst == nil {
		return nil
	}
	return &orderedJSONWriter{dst: dst}
}

type orderedJSONWriter struct {
	dst io.Writer
}

func (w *orderedJSONWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	var out bytes.Buffer
	remaining := p
	for len(remaining) > 0 {
		idx := bytes.IndexByte(remaining, '\n')
		if idx == -1 {
			line := remaining
			if len(line) > 0 {
				out.Write(reorderJSONLine(line))
			}
			break
		}

		line := remaining[:idx]
		if len(line) > 0 {
			out.Write(reorderJSONLine(line))
		}
		out.WriteByte('\n')
		remaining = remaining[idx+1:]
	}

	if out.Len() == 0 {
		// If we didn't rewrite anything (e.g. empty lines), forward the original bytes.
		_, err := w.dst.Write(p)
		if err != nil {
			return 0, err
		}
		return len(p), nil
	}

	_, err := w.dst.Write(out.Bytes())
	if err != nil {
		return 0, err
	}

	return len(p), nil
}

func reorderJSONLine(line []byte) []byte {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return line
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return line
	}

	keys := extractTopLevelKeys(trimmed)
	seen := make(map[string]struct{}, len(raw))
	orderedKeys := make([]string, 0, len(raw))

	for _, key := range orderedJSONKeys {
		if _, ok := raw[key]; ok {
			orderedKeys = append(orderedKeys, key)
			seen[key] = struct{}{}
		}
	}

	for _, key := range keys {
		if _, ok := seen[key]; ok {
			continue
		}
		if _, present := raw[key]; present {
			orderedKeys = append(orderedKeys, key)
			seen[key] = struct{}{}
		}
	}

	for key := range raw {
		if _, ok := seen[key]; ok {
			continue
		}
		orderedKeys = append(orderedKeys, key)
		seen[key] = struct{}{}
	}

	var buf bytes.Buffer
	buf.WriteByte('{')
	for idx, key := range orderedKeys {
		if idx > 0 {
			buf.WriteByte(',')
		}
		buf.WriteByte('"')
		buf.WriteString(key)
		buf.WriteString(`":`)
		buf.Write(raw[key])
	}
	buf.WriteByte('}')

	// Preserve leading/trailing whitespace if present.
	if !bytes.Equal(trimmed, line) {
		prefixLen := len(line) - len(bytes.TrimLeftFunc(line, isSpace))
		suffixLen := len(line) - len(bytes.TrimRightFunc(line, isSpace))
		var final bytes.Buffer
		if prefixLen > 0 {
			final.Write(line[:prefixLen])
		}
		final.Write(buf.Bytes())
		if suffixLen > 0 {
			final.Write(line[len(line)-suffixLen:])
		}
		return final.Bytes()
	}

	return buf.Bytes()
}

func extractTopLevelKeys(line []byte) []string {
	var keys []string
	depth := 0
	inString := false
	escaped := false
	expectKey := false
	var keyStart int

	for i, b := range line {
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if b == '\\' {
				escaped = true
				continue
			}
			if b == '"' {
				inString = false
				if depth == 1 && expectKey {
					keys = append(keys, string(line[keyStart:i]))
				}
			}
			continue
		}

		switch b {
		case '{':
			depth++
			if depth == 1 {
				expectKey = true
			}
		case '}':
			depth--
		case '"':
			if depth == 1 && expectKey {
				inString = true
				keyStart = i + 1
			} else {
				inString = true
			}
		case ':':
			if depth == 1 && expectKey {
				expectKey = false
			}
		case ',':
			if depth == 1 {
				expectKey = true
			}
		}
	}

	return keys
}

func isSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\r' || r == '\n'
}
