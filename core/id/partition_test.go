package id

import (
	"testing"
)

func TestParseNodeId(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		concept string
		shortId string
		wantErr bool
	}{
		{
			name:    "concept + short id",
			input:   "v1:cognition:participant:abc123",
			concept: "v1:cognition:participant",
			shortId: "abc123",
		},
		{
			name:    "long content hash short id",
			input:   "v1:agents:agent:a9f3b7c2e1d04f8a9b3c5d7e2f1a0b4c6d8e0f2a4b6c8d0e2f4a6b8c0d2e4f6",
			concept: "v1:agents:agent",
			shortId: "a9f3b7c2e1d04f8a9b3c5d7e2f1a0b4c6d8e0f2a4b6c8d0e2f4a6b8c0d2e4f6",
		},
		{
			name:    "v2 concept",
			input:   "v2:data:record:xyz",
			concept: "v2:data:record",
			shortId: "xyz",
		},
		{
			name:    "short ID only (no colons)",
			input:   "abc123",
			concept: "",
			shortId: "abc123",
		},
		{
			name:    "empty string",
			input:   "",
			wantErr: true,
		},
		{
			name:    "concept without short id",
			input:   "v1:cognition:space",
			concept: "v1:cognition:space",
			shortId: "",
		},
		{
			name:    "short id with colons in content hash",
			input:   "v1:cognition:participant:hash:with:colons",
			concept: "v1:cognition:participant",
			shortId: "hash:with:colons",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			concept, shortId, err := ParseNodeId(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
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
		name    string
		concept string
		shortId string
		want    string
	}{
		{
			name:    "concept + short id",
			concept: "v1:cognition:participant",
			shortId: "abc123",
			want:    "v1:cognition:participant:abc123",
		},
		{
			name:    "no short ID",
			concept: "v1:cognition:space",
			shortId: "",
			want:    "v1:cognition:space",
		},
		{
			name:    "empty concept yields short id",
			concept: "",
			shortId: "abc",
			want:    "abc",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildNodeId(tt.concept, tt.shortId)
			if got != tt.want {
				t.Errorf("BuildNodeId(%q, %q) = %q, want %q",
					tt.concept, tt.shortId, got, tt.want)
			}
		})
	}
}

func TestRoundTrip(t *testing.T) {
	concept, shortId := "v1:cognition:participant", "a9f3b7c2e1d0"

	fullId := BuildNodeId(concept, shortId)

	gotConcept, gotShortId, err := ParseNodeId(fullId)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotConcept != concept {
		t.Errorf("concept: got %q, want %q", gotConcept, concept)
	}
	if gotShortId != shortId {
		t.Errorf("shortId: got %q, want %q", gotShortId, shortId)
	}
}
