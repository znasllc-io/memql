package memql

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/id"
)

// ask_attachments.go -- the Library files a person attached to one Ask message.
//
// THE IDS ARE RESOLVED HERE, ON THE REPLICA THAT TOOK THE TURN, AND UNDER THE
// CALLER. A client names artifacts by bare id; each one is read through the
// owner-gated libraryArtifactById, so an id somebody else owns resolves to no
// row and refuses the turn before anything is written. What the person may
// open is the whole of what a turn may carry.
//
// THEN THEY RIDE THE GOAL, NEVER THE CONTEXT. The turn is compiled on a planner
// and its steps run on an agent, neither sharing memory with this replica, so
// the resolved files are written into the goal's conversation input (the run
// row every executing node rehydrates) and onto the transcript turn (what the
// conversation shows). integrations/agents' workTurnHistory reads them back
// from the row.

// MaxAskAttachments bounds one message. The OS composer holds the same number.
const MaxAskAttachments = 10

// Refusal codes for an attachment, carried as gRPC metadata `code` with
// field "attachments".
const (
	AskAttachmentInvalid  = "attachment_invalid"
	AskAttachmentTooMany  = "attachment_too_many"
	AskAttachmentNotFound = "attachment_not_found"
)

// AskAttachmentError refuses a turn over one of its attachments.
type AskAttachmentError struct {
	Code   string
	Value  string
	Reason string
}

func (e *AskAttachmentError) Error() string {
	if e == nil {
		return AskAttachmentInvalid
	}
	return e.Code + ": " + e.Reason
}

// AskAttachment is one Library file attached to an Ask message, as it was
// when the turn opened.
type AskAttachment struct {
	ArtifactID string `json:"artifactId"`
	// FileID is the v1:library:file behind the artifact, the id the Library's
	// read capability takes. Empty when the artifact is not a file.
	FileID   string `json:"fileId,omitempty"`
	Name     string `json:"name"`
	MimeType string `json:"mimeType,omitempty"`
	Kind     string `json:"kind,omitempty"`
}

// normalizeAskAttachmentIDs trims, drops blanks and duplicates (keeping the
// first occurrence's order), and refuses a malformed id or more than
// MaxAskAttachments distinct ids.
func normalizeAskAttachmentIDs(ids []string) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, raw := range ids {
		value := BareShortId(strings.TrimSpace(raw))
		if value == "" || seen[value] {
			continue
		}
		if err := id.ValidateShortId("v1:library:artifact", value); err != nil || len(value) > 160 || strings.ContainsAny(value, " \t\r\n") {
			return nil, &AskAttachmentError{Code: AskAttachmentInvalid, Value: raw,
				Reason: fmt.Sprintf("attachment %q is not a Library artifact id", raw)}
		}
		seen[value] = true
		out = append(out, value)
	}
	if len(out) > MaxAskAttachments {
		return nil, &AskAttachmentError{Code: AskAttachmentTooMany, Value: fmt.Sprint(len(out)),
			Reason: fmt.Sprintf("a message can carry at most %d attachments", MaxAskAttachments)}
	}
	return out, nil
}

// resolveAskAttachments reads each attached artifact under the caller. An id
// the caller cannot read, or an archived artifact, refuses the turn.
func (e *MemQLEngine) resolveAskAttachments(ctx context.Context, ids []string) ([]AskAttachment, error) {
	ids, err := normalizeAskAttachmentIDs(ids)
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	out := make([]AskAttachment, 0, len(ids))
	for _, artifactID := range ids {
		call, err := parser.RenderCall("libraryArtifactById", map[string]any{"artifactId": artifactID})
		if err != nil {
			return nil, err
		}
		result, err := e.Execute(ContextWithFreshRead(ctx), "query "+call)
		if err != nil {
			return nil, err
		}
		rows := MaterializeRows(result)
		if len(rows) != 1 || rows[0]["archived"] == true {
			return nil, &AskAttachmentError{Code: AskAttachmentNotFound, Value: artifactID,
				Reason: "an attached file is not in your Library; remove it and attach it again"}
		}
		out = append(out, askAttachmentFrom(artifactID, rows[0]))
	}
	return out, nil
}

// askAttachmentFrom projects an artifactFull row onto what a turn carries.
func askAttachmentFrom(artifactID string, row map[string]any) AskAttachment {
	text := func(key string) string { value, _ := row[key].(string); return strings.TrimSpace(value) }
	attachment := AskAttachment{ArtifactID: artifactID, Name: text("title"), MimeType: text("mimeType"), Kind: text("kind")}
	if ref := text("sourceConceptRef"); strings.HasPrefix(ref, "v1:library:file:") {
		attachment.FileID = BareShortId(ref)
	}
	if attachment.Name == "" {
		attachment.Name = "Untitled file"
	}
	return attachment
}

// askAttachmentsInput is the goal-input form: plain maps, so the run row's
// JSON reads the same on every node whatever Go type wrote it.
func askAttachmentsInput(attachments []AskAttachment) []any {
	out := make([]any, 0, len(attachments))
	for _, a := range attachments {
		entry := map[string]any{"artifactId": a.ArtifactID, "name": a.Name}
		if a.FileID != "" {
			entry["fileId"] = a.FileID
		}
		if a.MimeType != "" {
			entry["mimeType"] = a.MimeType
		}
		if a.Kind != "" {
			entry["kind"] = a.Kind
		}
		out = append(out, entry)
	}
	return out
}
