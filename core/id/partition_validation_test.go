package id_test

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/id"
)

func TestValidatePartitionName(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		input   string
		want    string
		wantErr string
	}{
		// happy paths
		{name: "simple lowercase", input: "acme", want: "acme"},
		{name: "with inner dash", input: "acme-prod", want: "acme-prod"},
		{name: "alphanum", input: "team42", want: "team42"},
		{name: "trims whitespace", input: "  acme  ", want: "acme"},
		{name: "lowercases input", input: "Acme-Prod", want: "acme-prod"},
		{name: "exactly 50 chars", input: strings.Repeat("a", 50), want: strings.Repeat("a", 50)},

		// rejected by shape
		{name: "empty", input: "", wantErr: "required"},
		{name: "whitespace only", input: "   ", wantErr: "required"},
		{name: "with space", input: "jose space", wantErr: "DNS-label"},
		{name: "leading dash", input: "-acme", wantErr: "DNS-label"},
		{name: "trailing dash", input: "acme-", wantErr: "DNS-label"},
		{name: "underscore", input: "acme_prod", wantErr: "DNS-label"},
		{name: "colon", input: "acme:prod", wantErr: "DNS-label"},
		{name: "unicode", input: "acmé", wantErr: "DNS-label"},
		{name: "leading underscore", input: "_acme", wantErr: "DNS-label"},

		// rejected by length
		{name: "51 chars", input: strings.Repeat("a", 51), wantErr: "too long"},

		// reserved
		{name: "_system reserved", input: "_system", wantErr: "DNS-label"}, // shape rejects it first
		{name: "_system uppercase", input: "_SYSTEM", wantErr: "DNS-label"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := id.ValidatePartitionName(tc.input)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil (returned %q)", tc.wantErr, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSuggestPartitionSlug(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		displayName string
		want        string
		validates   bool // result should pass ValidatePartitionName
	}{
		{name: "plain space", displayName: "Jose's Workspace", want: "joses-workspace", validates: true},
		{name: "all caps", displayName: "ACME PROD", want: "acme-prod", validates: true},
		{name: "mixed punctuation", displayName: "Team #42!", want: "team-42", validates: true},
		{name: "trims trailing dashes", displayName: "Sales --- ", want: "sales", validates: true},
		{name: "collapses runs", displayName: "a---b", want: "a-b", validates: true},
		{name: "all junk -> empty", displayName: "!!!", want: ""},
		{name: "empty -> empty", displayName: "", want: ""},
		{name: "truncates 50 chars", displayName: strings.Repeat("a", 60), want: strings.Repeat("a", 50), validates: true},
		{name: "truncates and trims trailing dash", displayName: strings.Repeat("a", 49) + "-bbbbb", want: strings.Repeat("a", 49), validates: true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := id.SuggestPartitionSlug(tc.displayName)
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			if tc.validates {
				if _, err := id.ValidatePartitionName(got); err != nil {
					t.Fatalf("suggested slug %q failed validation: %v", got, err)
				}
			}
		})
	}
}

func TestNormalizePartitionName(t *testing.T) {
	t.Parallel()
	if got := id.NormalizePartitionName("  Acme-Prod  "); got != "acme-prod" {
		t.Fatalf("got %q, want %q", got, "acme-prod")
	}
}
