package emaildoc

import (
	"encoding/json"
	"errors"
	"unicode"
)

// controlplace.go -- telling a person where a control character is.
//
// A control character is invisible: a NUL pasted in from a PDF or a terminal
// looks like nothing, and "this email contains a character that can't be
// saved" does not say which of the forty paragraphs it is in. So the refusal
// names the place, the way the file's own shape gives it: the field (the
// subject, the plain-text body) or, in a layout, the block by its id and the
// field of the block.
//
// The file is not known to be JSON yet, or to be a document at all, and it is
// as large as the limit allows, so the place is read from its bytes by the
// shape of the brackets and strings, without reading the file: a stack of the
// objects and lists open at the character (never more than maxPlaceDepth, which
// the format does not come near), and one pass over the block around it to find
// its id wherever the block writes it. Nothing is allocated in proportion to
// the file. A file whose shape does not say where the character is gets the
// sentence without a place.

// controlRefusal is the refusal for the control character at offset off of the
// file: the sentence, with the place the character is in when the file says it.
func controlRefusal(data []byte, off int) error {
	where := controlPlace(data, off)
	if where == "" {
		return errControlCharacter
	}
	return errors.New("This email contains a character that can't be saved, " + where + ". Remove it and try again.")
}

const (
	// maxPlaceDepth is how deeply a file may nest before its places are not told.
	// A block in a column of a block in a section is six open brackets down.
	maxPlaceDepth = 32
	// maxFieldBytes is the longest field name that is named; maxIDBytes the
	// longest block id read, before it is clipped to what a sentence holds.
	maxFieldBytes = 40
	maxIDBytes    = 16 << 10
)

// frame is an object or a list that is open at the character.
type frame struct {
	object    bool
	key       string // the field of the object around it that this is the value of
	start     int    // where it opens
	member    string // an object: the field being read, when it has a plain name
	expectKey bool   // an object: the next string is a field name
}

// topFields are the places a field of the file itself is called by.
var topFields = map[string]string{
	"subject": "the subject", "preheader": "the preheader", "layout": "the layout name",
	"textBody": "the plain-text body", "htmlBody": "the HTML body", "theme": "the theme", "blocks": "the blocks",
}

// controlPlace says where, in the file, the string that begins at offset off
// is: "in the subject", `in block "intro" (text)`, "in a field name"; or "" when
// the shape of the file does not say.
func controlPlace(data []byte, off int) string {
	var stack []frame
	inString, isKey, stringStart := false, false, 0
	for i := 0; i < off && i < len(data); i++ {
		c := data[i]
		if inString {
			switch c {
			case '\\':
				i++
			case '"':
				inString = false
				if isKey {
					stack[len(stack)-1].member = fieldName(data[stringStart:i])
				}
			}
			continue
		}
		switch c {
		case '"':
			inString, stringStart = true, i+1
			isKey = len(stack) > 0 && stack[len(stack)-1].object && stack[len(stack)-1].expectKey
		case '{', '[':
			if len(stack) == maxPlaceDepth {
				return ""
			}
			key := ""
			if len(stack) > 0 && stack[len(stack)-1].object {
				key = stack[len(stack)-1].member
			}
			stack = append(stack, frame{object: c == '{', key: key, start: i, expectKey: c == '{'})
		case '}', ']':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		case ':':
			if n := len(stack); n > 0 && stack[n-1].object {
				stack[n-1].expectKey = false
			}
		case ',':
			if n := len(stack); n > 0 && stack[n-1].object {
				stack[n-1].expectKey = true
			}
		}
	}
	if !inString || len(stack) == 0 {
		return ""
	}
	if isKey {
		return "in a field name"
	}
	// A block is an object in a list called blocks, at the top level or in a
	// section or a column. The nearest one is the one the character is in.
	for j := len(stack) - 1; j >= 1; j-- {
		if stack[j].object && !stack[j-1].object && stack[j-1].key == "blocks" {
			return blockPlace(data, stack[j], j == len(stack)-1)
		}
	}
	if !stack[0].object || stack[0].member == "" {
		return ""
	}
	if name, ok := topFields[stack[0].member]; ok {
		return "in " + name
	}
	return "in the field " + quote(stack[0].member)
}

// blockPlace names the block and the field of it. itself is whether the
// character is directly in a field of the block, as opposed to in a list or an
// object the block holds.
func blockPlace(data []byte, block frame, itself bool) string {
	if itself && block.member == "id" {
		return "in the id of a block"
	}
	where := "in a block"
	if id, ok := blockID(data, block.start); ok {
		where = "in block " + quote(id)
	}
	if block.member != "" {
		where += " (" + block.member + ")"
	}
	return where
}

// blockID reads the id of the block whose object opens at start, wherever in
// the object the id is written: the first field called id, when its value is
// text.
func blockID(data []byte, start int) (string, bool) {
	depth, wantKey, key := 0, false, ""
	inString, isKey, stringStart := false, false, 0
	for i := start; i < len(data); i++ {
		c := data[i]
		if inString {
			switch c {
			case '\\':
				i++
			case '"':
				inString = false
				raw := data[stringStart:i]
				if isKey {
					key = fieldName(raw)
				} else if depth == 1 && key == "id" {
					return decodeText(raw, maxIDBytes)
				}
			}
			continue
		}
		switch c {
		case '"':
			inString, stringStart = true, i+1
			isKey = depth == 1 && wantKey
		case '{', '[':
			if depth++; depth == 1 {
				wantKey = true
			}
		case '}', ']':
			if depth--; depth <= 0 {
				return "", false
			}
		case ':':
			if depth == 1 {
				wantKey = false
			}
		case ',':
			if depth == 1 {
				wantKey = true
			}
		}
	}
	return "", false
}

// fieldName is a field name as text when it is a plain one, letters and
// digits, as every field of the format is; otherwise "".
func fieldName(raw []byte) string {
	name, ok := decodeText(raw, maxFieldBytes)
	if !ok || name == "" {
		return ""
	}
	for _, r := range name {
		if r > unicode.MaxASCII || !(unicode.IsLetter(r) || unicode.IsDigit(r)) {
			return ""
		}
	}
	return name
}

// decodeText reads the bytes between the quotes of a JSON string as the text
// they write, when there are at most limit of them.
func decodeText(raw []byte, limit int) (string, bool) {
	if len(raw) > limit {
		return "", false
	}
	quoted := make([]byte, 0, len(raw)+2)
	quoted = append(append(append(quoted, '"'), raw...), '"')
	var text string
	if err := json.Unmarshal(quoted, &text); err != nil {
		return "", false
	}
	return text, true
}
