package macos_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Losing local signing configuration must stop before building an ad-hoc
// replacement that strands the installed app's permission approvals.
func TestLocalSigningRefusesMissingOrInvalidIdentity(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "security"), []byte("#!/bin/sh\necho '0 identities found'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, tc := range []struct {
		name, value string
		code        int
	}{
		{"missing", "", 4},
		{"empty", "", 3},
		{"ad-hoc", "-\n", 3},
		{"malformed", "not-a-fingerprint\n", 3},
		{"multiple", "0123456789012345678901234567890123456789\n0123456789012345678901234567890123456789\n", 3},
		{"unavailable", "0123456789012345678901234567890123456789\n", 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "signing-identity")
			if tc.name != "missing" {
				if err := os.WriteFile(file, []byte(tc.value), 0600); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("bash", "build-local-app.sh", "--signing-identity-file="+file, "--worker=/must-not-build")
			out, err := cmd.Output()
			if e, ok := err.(*exec.ExitError); !ok || e.ExitCode() != tc.code {
				t.Fatalf("wrong refusal: %v: %s", err, out)
			}
			var result struct {
				OK    bool `json:"ok"`
				Error *struct {
					Code int `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(out, &result); err != nil || result.OK || result.Error == nil || result.Error.Code != tc.code {
				t.Fatalf("refusal must emit one accurate result: %s (%v)", out, err)
			}
		})
	}
}
