//go:build darwin || linux

package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/znasllc-io/memql-cockpit/internal/worker/consent"
)

func TestBoundedFileInspectionBothPermissionLanes(t *testing.T) {
	root := realPermissionDirectory(t)
	path := filepath.Join(root, "source.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta needle\ngamma\nneedle final\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, scoped := range []bool{false, true} {
		for _, tc := range []struct {
			name     string
			args     map[string]any
			want     string
			complete bool
		}{
			{"range", map[string]any{"startLine": 2, "lineCount": 2}, "beta needle\ngamma\n", true},
			{"offset", map[string]any{"offsetBytes": 6, "maxBytes": 4}, "beta", false},
			{"search", map[string]any{"search": "needle"}, "beta needle\nneedle final\n", true},
			{"search cap", map[string]any{"search": "needle", "maxMatches": 1}, "beta needle\n", false},
			{"absent in prefix", map[string]any{"search": "absent", "maxScanBytes": 10}, "", false},
			{"absence", map[string]any{"search": "absent"}, "", true},
			{"past EOF", map[string]any{"startLine": 100, "lineCount": 2}, "", true},
			{"byte cap", map[string]any{"startLine": 2, "lineCount": 2, "maxBytes": 4}, "beta", false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				tc.args["path"] = path
				dispatch := permissionDispatch(t, "directory_read", root, "fs_read", tc.args)
				if !scoped {
					dispatch.ComputerPermission = nil
				}
				d := NewDispatcher(quietLogger(), DefaultPolicy(), &fakeGate{decision: consent.Decision{Allowed: true}})
				result, refusal := d.Dispatch(context.Background(), dispatch)
				if refusal != nil {
					t.Fatal(refusal)
				}
				var got map[string]any
				if err := json.Unmarshal(result.GetResultJson(), &got); err != nil {
					t.Fatal(err)
				}
				if got["content"] != tc.want || got["complete"] != tc.complete || got["truncated"] == tc.complete || got["sha256"] == "" {
					t.Fatalf("scoped=%v got %v", scoped, got)
				}
				if matches, ok := got["matches"].([]any); ok {
					for _, match := range matches {
						span := match.(map[string]any)
						start, length := int(span["contentOffsetBytes"].(float64)), int(span["lengthBytes"].(float64))
						if start < 0 || length < 1 || start+length > len(tc.want) {
							t.Fatalf("match outside returned content: %v", span)
						}
					}
				}
			})
		}
	}
}

func TestFileInspectionDoesNotEscapeApprovedDirectory(t *testing.T) {
	root := realPermissionDirectory(t)
	outside := realPermissionDirectory(t)
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("private needle"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	d := NewDispatcher(quietLogger(), DefaultPolicy(), &fakeGate{decision: consent.Decision{Allowed: true}})
	for _, p := range []string{secret, link} {
		dispatch := permissionDispatch(t, "directory_read", root, "fs_read", map[string]any{"path": p, "search": "needle"})
		result, refusal := d.Dispatch(context.Background(), dispatch)
		if result != nil || refusal == nil {
			t.Fatalf("escaped directory through %s", p)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	allowed := filepath.Join(root, "allowed")
	if err := os.WriteFile(allowed, []byte("needle"), 0600); err != nil {
		t.Fatal(err)
	}
	dispatch := permissionDispatch(t, "directory_read", root, "fs_read", map[string]any{"path": allowed, "search": "needle"})
	if result, refusal := d.Dispatch(ctx, dispatch); result != nil || refusal == nil || refusal.GetErrorCode() != "cancelled" {
		t.Fatal("cancelled inspection succeeded")
	}
}
