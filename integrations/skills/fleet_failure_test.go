package skills

import (
	"context"
	"errors"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

func TestFleetCompositionOnlyConsumesExplicitFailedReceipts(t *testing.T) {
	failed := errors.New("worker operation failed")
	for _, tc := range []struct {
		name, concept, body string
		result              bool
	}{
		{"failed effect", "integration:worker:result", `{"ok":false,"errorCode":"fs_read_failed","errorMessage":"no such file"}`, true},
		{"success with error", "integration:worker:result", `{"ok":true,"errorCode":"fs_read_failed"}`, false},
		{"missing outcome", "integration:worker:result", `{"errorCode":"fs_read_failed"}`, false},
		{"unclassified failure", "integration:worker:result", `{"ok":false}`, false},
		{"malformed", "integration:worker:result", `{`, false},
		{"unrelated receipt", "other", `{"ok":false,"errorCode":"fs_read_failed"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			surface := NewFleetSurface(func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error) {
				return []memorynodes.MemoryNode{{Concept: tc.concept, Payload: []byte(tc.body)}}, failed
			})
			result, err := surface.Call(context.Background(), Request{}, "fs_read", nil)
			if tc.result {
				if err != nil || result.OK || result.ErrorCode != "fs_read_failed" {
					t.Fatalf("failed receipt lost: %+v %v", result, err)
				}
			} else if !errors.Is(err, failed) {
				t.Fatalf("uncertain failure was swallowed: %+v %v", result, err)
			}
		})
	}
}
