package agents

import (
	"strings"
	"testing"
)

func TestAttachmentsMessageNamesTheFilesAndHowToReadThem(t *testing.T) {
	if got := attachmentsMessage(map[string]any{}); got != "" {
		t.Fatalf("no attachments must add no message, got %q", got)
	}
	if got := attachmentsMessage(map[string]any{"attachments": []any{map[string]any{"name": "no id"}}}); got != "" {
		t.Fatalf("an entry with no artifact id is not a file, got %q", got)
	}
	got := attachmentsMessage(map[string]any{"attachments": []any{
		map[string]any{"artifactId": "artifact-a", "fileId": "file-a", "name": "notes.md"},
	}})
	for _, want := range []string{"untrusted", "libraryReadFile", "fileId", `"artifactId":"artifact-a"`, `"name":"notes.md"`} {
		if !strings.Contains(got, want) {
			t.Errorf("the attachment message does not say %q:\n%s", want, got)
		}
	}
}
