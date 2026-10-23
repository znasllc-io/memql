//go:build darwin || linux

package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/cockpit/internal/worker/consent"
)

func TestFileReadDispatcherRefusesUnsupportedArgumentsBeforeOpening(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		lane := "ordinary"
		if scoped {
			lane = "scoped"
		}
		t.Run(lane, func(t *testing.T) {
			root := realPermissionDirectory(t)
			path := filepath.Join(root, "source.txt")
			if err := os.WriteFile(path, []byte(strings.Repeat("source line\n", 320)), 0600); err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				name string
				args map[string]any
			}{
				{"range 30-60", map[string]any{"startLine": 30, "endLine": 60}},
				{"range 290-310", map[string]any{"startLine": 290, "endLine": 310}},
				{"offset", map[string]any{"offset": 32}},
				{"invalid search", map[string]any{"search": ""}},
				{"misspelled byte limit", map[string]any{"maxByets": 8}},
			} {
				for _, missing := range []bool{false, true} {
					name := tc.name
					if missing {
						name += "/missing file"
					}
					t.Run(name, func(t *testing.T) {
						args := map[string]any{"path": path}
						if missing {
							args["path"] = filepath.Join(root, "missing.txt")
						}
						for k, v := range tc.args {
							args[k] = v
						}
						dispatch := permissionDispatch(t, "directory_read", root, "fs_read", args)
						if !scoped {
							dispatch.ComputerPermission = nil
						}
						d := NewDispatcher(quietLogger(), DefaultPolicy(), &fakeGate{decision: consent.Decision{Allowed: true}})
						result, refusal := d.Dispatch(context.Background(), dispatch)
						if result != nil || refusal.GetErrorCode() != "bad_request" {
							t.Fatalf("unsupported read must refuse before opening, got success=%t refusal=%v", result != nil, refusal)
						}
						if !strings.Contains(refusal.GetErrorMessage(), "path") || !strings.Contains(refusal.GetErrorMessage(), "maxBytes") {
							t.Fatalf("refusal must explain supported inputs: %s", refusal.GetErrorMessage())
						}
					})
				}
			}
		})
	}
}

func TestFileReadDispatcherKeepsDocumentedInputsAndGates(t *testing.T) {
	root := realPermissionDirectory(t)
	path := filepath.Join(root, "source.txt")
	if err := os.WriteFile(path, []byte("é文 source"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, scoped := range []bool{false, true} {
		for _, capped := range []bool{false, true} {
			args := map[string]any{"path": path}
			want := "é文 source"
			if capped {
				args["maxBytes"] = 5
				want = "é文"
			}
			dispatch := permissionDispatch(t, "directory_read", root, "fs_read", args)
			if !scoped {
				dispatch.ComputerPermission = nil
			}
			d := NewDispatcher(quietLogger(), DefaultPolicy(), &fakeGate{decision: consent.Decision{Allowed: true}})
			result, refusal := d.Dispatch(context.Background(), dispatch)
			if refusal != nil || result == nil {
				t.Fatalf("documented read refused (scoped=%t capped=%t): %v", scoped, capped, refusal)
			}
			var out struct {
				Content   string `json:"content"`
				Size      int    `json:"size"`
				Truncated bool   `json:"truncated"`
			}
			if err := json.Unmarshal(result.GetResultJson(), &out); err != nil {
				t.Fatal(err)
			}
			if out.Content != want || out.Size != len(want) || out.Truncated != capped {
				t.Fatalf("documented read changed (scoped=%t capped=%t): %+v", scoped, capped, out)
			}
		}
	}

	// Malformed read inputs must not bypass or replace consent/receipt gates.
	args := map[string]any{"path": path, "startLine": 1}
	dispatch := permissionDispatch(t, "directory_read", root, "fs_read", args)
	d := NewDispatcher(quietLogger(), DefaultPolicy(), &fakeGate{decision: consent.Decision{Allowed: false}})
	if result, refusal := d.Dispatch(context.Background(), dispatch); result != nil || refusal.GetErrorCode() != "consent_required" {
		t.Fatalf("consent gate changed: result=%v refusal=%v", result, refusal)
	}
	d = NewDispatcher(quietLogger(), DefaultPolicy(), &fakeGate{decision: consent.Decision{Allowed: true}})
	dispatch.ComputerPermission.ArgsHash = strings.Repeat("0", 64)
	if result, refusal := d.Dispatch(context.Background(), dispatch); result != nil || refusal.GetErrorCode() != "computer_permission_refused" {
		t.Fatalf("receipt gate changed: result=%v refusal=%v", result, refusal)
	}
}
