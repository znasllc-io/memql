package repositoryauthority

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"unicode/utf8"
)

// TextEdit uses exact, unique source text rather than asking a model to repeat
// a complete file. The ordinary proposal still contains and reviews full bytes.
type TextEdit struct {
	Path         string `json:"path"`
	BeforeSHA256 string `json:"beforeSHA256"`
	Before       string `json:"before"`
	After        string `json:"after"`
}

func applyTextEdits(source RepositorySnapshot, edits []TextEdit) ([]Replacement, error) {
	if len(edits) == 0 || len(edits) > 256 {
		return nil, errors.New("provide between one and 256 bounded text edits")
	}
	originals := map[string]Replacement{}
	for _, file := range source.Files {
		originals[file.Path] = file
	}
	changed := map[string]string{}
	order := []string{}
	total := 0
	for _, edit := range edits {
		total += len(edit.Before) + len(edit.After)
		file, ok := originals[edit.Path]
		if !ok || edit.BeforeSHA256 != file.BeforeSHA256 || total > 1<<20 || !utf8.ValidString(edit.Before) || !utf8.ValidString(edit.After) || strings.ContainsRune(edit.After, 0) {
			return nil, errors.New("text edit does not match its bounded approved source")
		}
		sum := sha256.Sum256([]byte(file.Content))
		if file.BeforeSHA256 != "" && hex.EncodeToString(sum[:]) != file.BeforeSHA256 {
			return nil, errors.New("text edit source hash is invalid")
		}
		content, seen := changed[edit.Path]
		if !seen {
			content = file.Content
			order = append(order, edit.Path)
		}
		if edit.Before == "" {
			if file.BeforeSHA256 != "" || seen || content != "" {
				return nil, errors.New("empty before text is allowed only for a new file")
			}
			content = edit.After
		} else {
			if edit.Before == edit.After || strings.Count(content, edit.Before) != 1 {
				return nil, errors.New("text edit must replace exactly one matching span; include enough surrounding context")
			}
			content = strings.Replace(content, edit.Before, edit.After, 1)
		}
		if len(content) > 1<<20 {
			return nil, errors.New("edited file exceeds its byte bound")
		}
		changed[edit.Path] = content
	}
	result := make([]Replacement, 0, len(order))
	total = 0
	for _, path := range order {
		file := originals[path]
		file.Content = changed[path]
		total += len(file.Content)
		if total > 4<<20 {
			return nil, errors.New("edited files exceed the proposal byte bound")
		}
		result = append(result, file)
	}
	return result, nil
}

// EditFiles reads again inside the actual approved execution and refuses source
// movement. The caller cannot choose a different owner, file scope or source.
func (i *Integration) EditFiles(ctx context.Context, requestID, expectedHead string, edits []TextEdit) ([]Replacement, error) {
	source, err := i.ReadContext(ctx, requestID)
	if err != nil {
		return nil, err
	}
	if !commit.MatchString(expectedHead) || source.ExpectedHead != expectedHead {
		return nil, errors.New("default branch changed while preparing edits; read the source again")
	}
	return applyTextEdits(source, edits)
}
