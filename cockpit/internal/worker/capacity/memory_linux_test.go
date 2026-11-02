//go:build linux

package capacity

import (
	"context"
	"testing"
)

func TestReadMemoryReadsThisMachine(t *testing.T) {
	mem, err := ReadMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !validKind(mem.Kind) || mem.Total == 0 || mem.Available > mem.Total {
		t.Fatalf("invalid reading: %+v", mem)
	}
}
