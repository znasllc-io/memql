//go:build darwin

package capacity

import (
	"context"
	"testing"
)

func TestReadMemoryReadsThisMac(t *testing.T) {
	mem, err := ReadMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if mem.Kind != KindUnified || mem.Total == 0 || mem.Available > mem.Total || mem.ModelLimit == 0 || mem.ModelLimit > mem.Total {
		t.Fatalf("invalid reading: %+v", mem)
	}
}
