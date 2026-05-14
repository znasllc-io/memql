package node

import (
	"os"
	"testing"
)

func TestNewIdentity_Defaults(t *testing.T) {
	// Ensure env vars are clean
	os.Unsetenv("MEMQL_NODE_TYPE")
	os.Unsetenv("MEMQL_NODE_ID")
	os.Unsetenv("MEMQL_NODE_ADDRESS")
	os.Unsetenv("MEMQL_PARENT_ADDRESS")
	os.Unsetenv("MEMQL_NODE_LABELS")

	id := NewIdentity("1.0.0")

	// Tagged binaries force their compiled type; standalone respects env var
	expected := CompiledNodeType()
	if id.Type != expected {
		t.Errorf("expected %s, got %s", expected, id.Type)
	}
	if id.ID == "" {
		t.Error("expected generated UUID, got empty")
	}
	if id.Version != "1.0.0" {
		t.Errorf("expected version 1.0.0, got %s", id.Version)
	}
	if id.HasParent() {
		t.Error("expected HasParent() to be false")
	}
}

func TestNewIdentity_EnvVars(t *testing.T) {
	t.Setenv("MEMQL_NODE_TYPE", "bff")
	t.Setenv("MEMQL_NODE_ID", "test-node-123")
	t.Setenv("MEMQL_NODE_ADDRESS", "localhost:50052")
	t.Setenv("MEMQL_PARENT_ADDRESS", "parent:50052")
	t.Setenv("MEMQL_NODE_LABELS", "region=us-central1,env=staging")

	id := NewIdentity("2.0.0")

	// Compiled node type always takes precedence. Default builds compile as BFF.
	compiled := CompiledNodeType()
	if id.Type != compiled {
		t.Errorf("expected %s (compiled type), got %s", compiled, id.Type)
	}

	if id.ID != "test-node-123" {
		t.Errorf("expected test-node-123, got %s", id.ID)
	}
	if id.Address != "localhost:50052" {
		t.Errorf("expected localhost:50052, got %s", id.Address)
	}
	if id.ParentAddress != "parent:50052" {
		t.Errorf("expected parent:50052, got %s", id.ParentAddress)
	}
	if !id.HasParent() {
		t.Error("expected HasParent() to be true")
	}
	if id.Labels["region"] != "us-central1" {
		t.Errorf("expected label region=us-central1, got %s", id.Labels["region"])
	}
	if id.Labels["env"] != "staging" {
		t.Errorf("expected label env=staging, got %s", id.Labels["env"])
	}
}

func TestNewIdentity_InvalidType(t *testing.T) {
	t.Setenv("MEMQL_NODE_TYPE", "invalid")

	id := NewIdentity("1.0.0")

	// Tagged binaries use compiled type; standalone falls back to standalone
	expected := CompiledNodeType()
	if id.Type != expected {
		t.Errorf("expected %s for invalid type, got %s", expected, id.Type)
	}
}

func TestCompiledNodeType(t *testing.T) {
	compiled := CompiledNodeType()
	if !ValidNodeTypes[compiled] {
		t.Errorf("CompiledNodeType() returned invalid type: %s", compiled)
	}
}

func TestParseLabels(t *testing.T) {
	tests := []struct {
		input    string
		expected map[string]string
	}{
		{"", map[string]string{}},
		{"key=val", map[string]string{"key": "val"}},
		{"a=1,b=2,c=3", map[string]string{"a": "1", "b": "2", "c": "3"}},
		{" a = 1 , b = 2 ", map[string]string{"a": "1", "b": "2"}},
		{"noequals", map[string]string{}},
	}

	for _, tt := range tests {
		result := parseLabels(tt.input)
		if len(result) != len(tt.expected) {
			t.Errorf("parseLabels(%q): got %d entries, want %d", tt.input, len(result), len(tt.expected))
			continue
		}
		for k, v := range tt.expected {
			if result[k] != v {
				t.Errorf("parseLabels(%q)[%s] = %q, want %q", tt.input, k, result[k], v)
			}
		}
	}
}
