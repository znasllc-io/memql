package memql

import (
	"reflect"
	"strings"
	"testing"
)

func TestAccountCreationDomainNormalization(t *testing.T) {
	row := map[string]any{"domains": []any{" EXAMPLE.com. ", "mail.example.com", "example.com"}, "defaultDomain": "MAIL.EXAMPLE.COM."}
	if err := normalizeAccountCreationDomains(row); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(row["domains"], []any{"example.com", "mail.example.com"}) || row["defaultDomain"] != "mail.example.com" {
		t.Fatalf("normalized row: %#v", row)
	}
	for _, name := range []string{"https://example.com", "*.example.com", "example.com/path", "localhost", "127.0.0.1", "-bad.example", "a..example", strings.Repeat("a", 64) + ".com"} {
		if _, err := normalizedAssignedDomain(name); err == nil {
			t.Errorf("accepted %q", name)
		}
	}
	for _, row := range []map[string]any{
		{"domains": []string{"example.com"}, "defaultDomain": "other.com"},
		{"domains": []string{}, "defaultDomain": "example.com"},
		{"domains": []string{"example.com"}},
		{"defaultDomain": "example.com"},
		{"domains": []any{12}},
	} {
		if err := normalizeAccountCreationDomains(row); err == nil {
			t.Errorf("accepted invalid defaults: %#v", row)
		}
	}
	for _, row := range []map[string]any{{"domain": "Legacy profile text"}, {"domains": []string{}, "defaultDomain": ""}} {
		if err := normalizeAccountCreationDomains(row); err != nil {
			t.Errorf("valid legacy/opt-out: %v", err)
		}
	}
}
