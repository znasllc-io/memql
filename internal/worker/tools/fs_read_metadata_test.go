//go:build darwin || linux

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql-cockpit/internal/worker/consent"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

func TestFileReadReportsByteSizeAndTruncation(t *testing.T) {
	for _, tc := range []struct {
		name, source, content string
		maxBytes              int
		truncated             bool
	}{
		{name: "empty", maxBytes: 8},
		{name: "below cap", source: "short", content: "short", maxBytes: 8},
		{name: "exact cap", source: "boundary", content: "boundary", maxBytes: 8},
		{name: "over cap", source: "boundary plus omitted bytes", content: "boundary", maxBytes: 8, truncated: true},
		{name: "large prefix", source: strings.Repeat("x", 9000), content: strings.Repeat("x", 8192), maxBytes: 8192, truncated: true},
		{name: "unicode full", source: "é文", content: "é文", maxBytes: 5},
		{name: "unicode byte boundary", source: "é文", content: "é", maxBytes: 2, truncated: true},
		// JSON replaces a partial UTF-8 sequence, as before. Byte accounting
		// must describe the file read, not the encoded or displayed string.
		{name: "unicode partial sequence", source: "aé☀z", content: "aé�", maxBytes: 4, truncated: true},
	} {
		for _, scoped := range []bool{false, true} {
			name := tc.name + "/ordinary"
			if scoped {
				name = tc.name + "/scoped"
			}
			t.Run(name, func(t *testing.T) {
				root := realPermissionDirectory(t)
				path := filepath.Join(root, "source.txt")
				if err := os.WriteFile(path, []byte(tc.source), 0600); err != nil {
					t.Fatal(err)
				}
				args := map[string]any{"path": path, "maxBytes": tc.maxBytes}
				policy := DefaultPolicy()
				var result *memqlv1.Success
				var refusal *memqlv1.Failure
				if scoped {
					dispatcher := NewDispatcher(quietLogger(), policy, &fakeGate{decision: consent.Decision{Allowed: true}})
					result, refusal = dispatcher.Dispatch(context.Background(), permissionDispatch(t, "directory_read", root, "fs_read", args))
				} else {
					result, refusal = runFsRead(context.Background(), args, policy)
				}
				if refusal != nil || result == nil {
					t.Fatalf("read refused: %v", refusal)
				}
				var body struct {
					Path      string `json:"path"`
					Content   string `json:"content"`
					Size      int    `json:"size"`
					FileSize  *int64 `json:"fileSize"`
					Truncated *bool  `json:"truncated"`
				}
				if err := json.Unmarshal(result.GetResultJson(), &body); err != nil {
					t.Fatal(err)
				}
				if body.FileSize == nil || body.Truncated == nil {
					t.Fatalf("read omitted completeness metadata: %s", result.GetResultJson())
				}
				if *body.FileSize != int64(len(tc.source)) || *body.Truncated != tc.truncated {
					t.Fatalf("fileSize=%d truncated=%v, want %d/%v", *body.FileSize, *body.Truncated, len(tc.source), tc.truncated)
				}
				wantBytes := min(len(tc.source), tc.maxBytes)
				if body.Path != path || body.Content != tc.content || body.Size != wantBytes || result.GetBytesOut() != uint64(wantBytes) {
					t.Fatalf("existing read contract changed: body=%+v bytesOut=%d", body, result.GetBytesOut())
				}
				// The full result may be archived. Its bounded preview's tail
				// must still identify whether the file read was incomplete.
				completeness := fmt.Sprintf("[File read: returnedBytes=%d fileSize=%d truncated=%t]", wantBytes, len(tc.source), tc.truncated)
				if preview := result.GetOutputPreview(); len(preview) > 1024 || !strings.HasSuffix(preview, completeness) {
					t.Fatalf("bounded preview lost completeness: %q", preview)
				}
			})
		}
	}
}
