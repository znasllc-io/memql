package id

import (
	"testing"
)

func TestParseNodeId(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		partition string
		concept   string
		shortId   string
		wantErr   bool
	}{
		{
			name:      "full ID with partition",
			input:     "acme:v1:cognition:participant:abc123",
			partition: "acme",
			concept:   "v1:cognition:participant",
			shortId:   "abc123",
		},
		{
			name:      "full ID without partition (legacy)",
			input:     "v1:cognition:participant:abc123",
			partition: "",
			concept:   "v1:cognition:participant",
			shortId:   "abc123",
		},
		{
			name:      "partition with long content hash",
			input:     "steam-prowess:v1:copresent:agent:a9f3b7c2e1d04f8a9b3c5d7e2f1a0b4c6d8e0f2a4b6c8d0e2f4a6b8c0d2e4f6",
			partition: "steam-prowess",
			concept:   "v1:copresent:agent",
			shortId:   "a9f3b7c2e1d04f8a9b3c5d7e2f1a0b4c6d8e0f2a4b6c8d0e2f4a6b8c0d2e4f6",
		},
		{
			name:      "default partition explicit",
			input:     "default:v1:cognition:space:lab-1",
			partition: "default",
			concept:   "v1:cognition:space",
			shortId:   "lab-1",
		},
		{
			name:      "v2 concept",
			input:     "mypartition:v2:data:record:xyz",
			partition: "mypartition",
			concept:   "v2:data:record",
			shortId:   "xyz",
		},
		{
			name:      "short ID only (no colons)",
			input:     "abc123",
			partition: "",
			concept:   "",
			shortId:   "abc123",
		},
		{
			name:    "empty string",
			input:   "",
			wantErr: true,
		},
		{
			name:      "concept without short ID",
			input:     "acme:v1:cognition:space",
			partition: "acme",
			concept:   "v1:cognition:space",
			shortId:   "",
		},
		{
			name:      "concept without partition or short ID",
			input:     "v1:cognition:space",
			partition: "",
			concept:   "v1:cognition:space",
			shortId:   "",
		},
		{
			name:      "short ID with colons in content hash",
			input:     "acme:v1:cognition:participant:hash:with:colons",
			partition: "acme",
			concept:   "v1:cognition:participant",
			shortId:   "hash:with:colons",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			partition, concept, shortId, err := ParseNodeId(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if partition != tt.partition {
				t.Errorf("partition: got %q, want %q", partition, tt.partition)
			}
			if concept != tt.concept {
				t.Errorf("concept: got %q, want %q", concept, tt.concept)
			}
			if shortId != tt.shortId {
				t.Errorf("shortId: got %q, want %q", shortId, tt.shortId)
			}
		})
	}
}

func TestBuildNodeId(t *testing.T) {
	tests := []struct {
		name      string
		partition string
		concept   string
		shortId   string
		want      string
	}{
		{
			name:      "full build",
			partition: "acme",
			concept:   "v1:cognition:participant",
			shortId:   "abc123",
			want:      "acme:v1:cognition:participant:abc123",
		},
		{
			name:      "empty partition defaults",
			partition: "",
			concept:   "v1:cognition:participant",
			shortId:   "abc123",
			want:      "default:v1:cognition:participant:abc123",
		},
		{
			name:      "no short ID",
			partition: "acme",
			concept:   "v1:cognition:space",
			shortId:   "",
			want:      "acme:v1:cognition:space",
		},
		{
			name:      "partition only",
			partition: "acme",
			concept:   "",
			shortId:   "",
			want:      "acme",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildNodeId(tt.partition, tt.concept, tt.shortId)
			if got != tt.want {
				t.Errorf("BuildNodeId(%q, %q, %q) = %q, want %q",
					tt.partition, tt.concept, tt.shortId, got, tt.want)
			}
		})
	}
}

func TestHasPartition(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"acme:v1:cognition:participant:abc", true},
		{"v1:cognition:participant:abc", false},
		{"abc123", false},
		{"", false},
		{"default:v1:copresent:agent:xyz", true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := HasPartition(tt.input); got != tt.want {
				t.Errorf("HasPartition(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestPrependPartition(t *testing.T) {
	tests := []struct {
		name      string
		partition string
		existing  string
		want      string
	}{
		{
			name:      "add partition to bare concept ID",
			partition: "acme",
			existing:  "v1:cognition:participant:abc",
			want:      "acme:v1:cognition:participant:abc",
		},
		{
			name:      "already has partition -- unchanged",
			partition: "beta",
			existing:  "acme:v1:cognition:participant:abc",
			want:      "acme:v1:cognition:participant:abc",
		},
		{
			name:      "empty partition defaults",
			partition: "",
			existing:  "v1:cognition:participant:abc",
			want:      "default:v1:cognition:participant:abc",
		},
		{
			name:      "empty existing returns empty",
			partition: "acme",
			existing:  "",
			want:      "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PrependPartition(tt.partition, tt.existing)
			if got != tt.want {
				t.Errorf("PrependPartition(%q, %q) = %q, want %q",
					tt.partition, tt.existing, got, tt.want)
			}
		})
	}
}

func TestStripPartition(t *testing.T) {
	tests := []struct {
		input         string
		wantPartition string
		wantRemainder string
	}{
		{"acme:v1:cognition:participant:abc", "acme", "v1:cognition:participant:abc"},
		{"v1:cognition:participant:abc", "", "v1:cognition:participant:abc"},
		{"", "", ""},
		{"abc123", "", "abc123"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			partition, remainder := StripPartition(tt.input)
			if partition != tt.wantPartition {
				t.Errorf("StripPartition(%q) partition = %q, want %q", tt.input, partition, tt.wantPartition)
			}
			if remainder != tt.wantRemainder {
				t.Errorf("StripPartition(%q) remainder = %q, want %q", tt.input, remainder, tt.wantRemainder)
			}
		})
	}
}

func TestRoundTrip(t *testing.T) {
	// Build → Parse should round-trip.
	partition, concept, shortId := "acme", "v1:cognition:participant", "a9f3b7c2e1d0"

	fullId := BuildNodeId(partition, concept, shortId)

	gotPartition, gotConcept, gotShortId, err := ParseNodeId(fullId)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPartition != partition {
		t.Errorf("partition: got %q, want %q", gotPartition, partition)
	}
	if gotConcept != concept {
		t.Errorf("concept: got %q, want %q", gotConcept, concept)
	}
	if gotShortId != shortId {
		t.Errorf("shortId: got %q, want %q", gotShortId, shortId)
	}
}
