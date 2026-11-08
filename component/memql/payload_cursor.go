package memql

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// enablePayloadKeyset admits only schema-declared text scalars. Open objects,
// unions and unbound runtime sorts retain their existing in-memory ordering;
// guessing a type from the first page would make later pages incorrect.
// Datetimes retain the existing string ordering. SQL and Go both use bytewise
// string order, and absent/blank values sort last in either direction.
func (c *compiledSort) enablePayloadKeyset(concept *memorynodes.Concept) error {
	fields, err := flattenConceptFields(concept)
	if err != nil {
		return err
	}
	for _, field := range c.fields {
		switch field.kind {
		case sortFieldCreatedAt, sortFieldId:
		case sortFieldPayload:
			typ := fields[strings.Join(field.payloadPath, ".")].Type
			if typ != "string" && typ != "datetime" && typ != "enum" {
				return nil
			}
		default:
			return nil
		}
	}
	for i := range c.fields {
		c.fields[i].keysetText = c.fields[i].kind != sortFieldCreatedAt
	}
	c.requiresFullScan = false
	c.payloadKeyset = true
	// The guard distinguishes this normalized SQL comparator from old cursors.
	c.signature += ":text-keyset-v1"
	return nil
}

// The legacy intrinsic codec assumes createdAt then id ASC. Explicit id DESC
// and id-leading orderings need the same value-key codec as text payloads;
// reusing the legacy predicate would skip or duplicate timestamp ties.
func (c *compiledSort) needsIntrinsicValueKeyset() bool {
	if c == nil || c.requiresFullScan || len(c.fields) == 0 {
		return false
	}
	if c.fields[0].kind == sortFieldId {
		return true
	}
	for _, field := range c.fields {
		if field.kind == sortFieldId {
			return field.direction == SortDirectionDesc
		}
	}
	return false
}

// trimSpaceCharacters is unicode.IsSpace's set, matching strings.TrimSpace.
// Keep the original nonblank value: whitespace is ignored only when deciding
// absence, not when comparing two present strings.
const trimSpaceCharacters = "\t\n\v\f\r \u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000"

func (f compiledSortField) keysetExpression() string {
	if f.kind == sortFieldCreatedAt {
		return `"createdAt"`
	}
	if f.kind == sortFieldId {
		return `(id COLLATE "C")`
	}
	text, _ := buildJSONPathExpression(f.payloadPath) // validated at compilation
	value, _ := buildJSONBPathExpression(f.payloadPath)
	return fmt.Sprintf(`((CASE WHEN jsonb_typeof(%s) = 'string' AND btrim(%s, '%s') <> '' THEN %s END) COLLATE "C")`, value, text, trimSpaceCharacters, text)
}

type payloadCursor struct {
	Version int       `json:"v"`
	Sig     string    `json:"s"`
	Values  []*string `json:"k"`
}

func encodePageCursor(last memorynodes.MemoryNode, sorter *compiledSort) (string, error) {
	if sorter == nil || !sorter.payloadKeyset {
		return encodeCursor(last, sorter.signatureValue())
	}
	key, err := sorter.buildKey(last)
	if err != nil {
		return "", err
	}
	values := make([]*string, len(key))
	for i, value := range key {
		if !value.present {
			continue
		}
		text := value.str
		if value.kind == sortValueTime {
			text = value.timeVal.UTC().Format(time.RFC3339Nano)
		}
		values[i] = &text
	}
	raw, err := json.Marshal(payloadCursor{Version: 2, Sig: sortSignatureHash(sorter.signatureValue()), Values: values})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodePageCursor(token string, sorter *compiledSort) (keysetPosition, error) {
	if sorter == nil || !sorter.payloadKeyset {
		return decodeCursor(token, sorter.signatureValue())
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(token))
	if err != nil {
		return keysetPosition{}, fmt.Errorf("%w: %v", ErrCursorMalformed, err)
	}
	var cursor payloadCursor
	if err := json.Unmarshal(raw, &cursor); err != nil {
		return keysetPosition{}, fmt.Errorf("%w: %v", ErrCursorMalformed, err)
	}
	if cursor.Version != 2 || len(cursor.Values) != len(sorter.fields) {
		return keysetPosition{}, ErrCursorMalformed
	}
	if cursor.Sig != sortSignatureHash(sorter.signatureValue()) {
		return keysetPosition{}, ErrCursorSortMismatch
	}
	for i, field := range sorter.fields {
		value := cursor.Values[i]
		if field.kind == sortFieldCreatedAt {
			if value == nil {
				return keysetPosition{}, ErrCursorMalformed
			}
			if _, err := time.Parse(time.RFC3339Nano, *value); err != nil {
				return keysetPosition{}, ErrCursorMalformed
			}
		}
		if field.kind == sortFieldId && (value == nil || strings.TrimSpace(*value) == "") {
			return keysetPosition{}, ErrCursorMalformed
		}
	}
	return keysetPosition{values: cursor.Values}, nil
}

// Lexicographic continuation over exactly the expressions used by ORDER BY.
// NULLS LAST means a present value continues through both smaller/larger
// values and NULL; an absent value continues only through equal NULL prefixes.
func payloadKeysetWhere(pos keysetPosition, sorter *compiledSort) (string, []any) {
	var alternatives, equal []string
	var args, equalArgs []any
	for i, field := range sorter.fields {
		expr := field.keysetExpression()
		value := pos.values[i]
		if value == nil {
			equal = append(equal, expr+" IS NULL")
			continue
		}
		var bound any = *value
		if field.kind == sortFieldCreatedAt {
			bound, _ = time.Parse(time.RFC3339Nano, *value)
		}
		op := ">"
		if field.direction == SortDirectionDesc {
			op = "<"
		}
		term := fmt.Sprintf("(%s %s ? OR %s IS NULL)", expr, op, expr)
		if len(equal) > 0 {
			term = "(" + strings.Join(equal, " AND ") + " AND " + term + ")"
		}
		alternatives = append(alternatives, term)
		args = append(args, equalArgs...)
		args = append(args, bound)
		equal = append(equal, expr+" = ?")
		equalArgs = append(equalArgs, bound)
	}
	return "(" + strings.Join(alternatives, " OR ") + ")", args
}
