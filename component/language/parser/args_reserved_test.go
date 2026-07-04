package parser

import (
	"strings"
	"testing"
)

// S6 (memql#2361): args fields may not shadow reserved engine names -- the
// documented rejection now has implementing code.
func TestArgsBlockRejectsReservedNames(t *testing.T) {
	for _, name := range []string{"now", "actor", "partition", "config", "trace"} {
		_, err := parseArgsSafe("args { " + name + " string @required }")
		if err == nil || !strings.Contains(err.Error(), "reserved engine name") {
			t.Fatalf("args field %q must be rejected as reserved, got: %v", name, err)
		}
	}
	// Non-reserved names keep parsing.
	if _, err := parseArgsSafe("args { asOf string @required }"); err != nil {
		t.Fatalf("non-reserved field must parse: %v", err)
	}
}
