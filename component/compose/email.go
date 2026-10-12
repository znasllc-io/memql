package compose

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// maxEmailFileBytes is the largest email file: the size the email document's
// own parser reads (emaildoc.MaxBytes, which this package may not import).
const maxEmailFileBytes = 2 << 20

var errEmailFileTooLarge = errors.New("email file exceeds 2 MiB")

// RenderEmailFile writes an email document as its file: the JSON object it is
// given, compacted and laid out again with two-space indentation and one
// trailing newline. That is the canonical form the email document writes
// itself in, so a document's own bytes come back unchanged, and the file a
// recipe rebuilds is the file that was delivered.
//
// It checks only that the body is one JSON object and that it and the file are
// at most 2 MiB. What the object says is checked before a recipe is saved
// (integrations/compose parses it as an email document): this package rebuilds
// files standalone and carries no email parser.
func RenderEmailFile(body string) (Result, error) {
	if len(body) > maxEmailFileBytes {
		return Result{}, errEmailFileTooLarge
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(body)); err != nil {
		return Result{}, fmt.Errorf("email file is not valid JSON: %w", err)
	}
	if compact.Len() == 0 || compact.Bytes()[0] != '{' {
		return Result{}, errors.New("an email file must be a JSON object")
	}
	var file bytes.Buffer
	if err := json.Indent(&file, compact.Bytes(), "", "  "); err != nil {
		return Result{}, fmt.Errorf("email file is not valid JSON: %w", err)
	}
	file.WriteByte('\n')
	if file.Len() > maxEmailFileBytes {
		return Result{}, errEmailFileTooLarge
	}
	return Result{Bytes: file.Bytes(), Embedded: false, Note: "An email file carries no provenance of its own; its history is kept on its composition, not in the file."}, nil
}
