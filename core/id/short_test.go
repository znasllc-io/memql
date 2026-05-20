package id

import (
	"testing"

	"github.com/google/uuid"
)

func TestNewShortIdParsesAsUUID(t *testing.T) {
	for i := 0; i < 50; i++ {
		got := NewShortId()
		if _, err := uuid.Parse(got); err != nil {
			t.Fatalf("NewShortId() returned %q which is not a valid UUID: %v", got, err)
		}
	}
}

func TestNewShortIdUnique(t *testing.T) {
	seen := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		got := NewShortId()
		if _, dup := seen[got]; dup {
			t.Fatalf("NewShortId returned duplicate %q at iter %d", got, i)
		}
		seen[got] = struct{}{}
	}
}
