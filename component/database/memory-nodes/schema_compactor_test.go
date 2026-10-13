package memoryNodes

import (
	"os"
	"testing"
)

func TestSchemaReferenceActivationRequiresExplicitStartupOptIn(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   string
		unset   bool
		want    bool
		wantErr bool
	}{
		{name: "unset holds", unset: true},
		{name: "false holds", value: "false"},
		{name: "true opts in", value: "true", want: true},
		{name: "invalid refuses startup", value: "activate", wantErr: true},
		{name: "empty refuses startup", value: "", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MEMQL_DATABASE_SCHEMA_REFERENCES_ACTIVATE", tc.value)
			if tc.unset {
				if err := os.Unsetenv("MEMQL_DATABASE_SCHEMA_REFERENCES_ACTIVATE"); err != nil {
					t.Fatal(err)
				}
			}
			c, err := NewSchemaCompactor(nil, nil, nil)
			if (err != nil) != tc.wantErr {
				t.Fatalf("construction error=%v, want error=%v", err, tc.wantErr)
			}
			if tc.wantErr {
				if c != nil {
					t.Fatal("invalid configuration returned a runnable compactor")
				}
				return
			}
			if c.allowActivation != tc.want {
				t.Fatalf("allowActivation=%v, want %v", c.allowActivation, tc.want)
			}
			// Startup configuration is captured; changing process environment is
			// not a second activation surface for an already running component.
			t.Setenv("MEMQL_DATABASE_SCHEMA_REFERENCES_ACTIVATE", "false")
			if c.allowActivation != tc.want {
				t.Fatal("activation permission changed after construction")
			}
		})
	}
}
