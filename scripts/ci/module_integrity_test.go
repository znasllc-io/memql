package ci

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestModuleIntegrityFailsClosed(t *testing.T) {
	script, err := os.ReadFile("module-integrity.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, fault := range []string{"none", "membership", "build", "vet", "tidy", "sync", "packages"} {
		t.Run(fault, func(t *testing.T) {
			root := t.TempDir()
			for _, dir := range []string{"scripts/ci", "bin", "child"} {
				if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
					t.Fatal(err)
				}
			}
			write := func(name string, data []byte, mode os.FileMode) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(root, name), data, mode); err != nil {
					t.Fatal(err)
				}
			}
			write("scripts/ci/module-integrity.sh", script, 0755)
			write("go.mod", []byte("module example.test/root\n"), 0644)
			write("child/go.mod", []byte("module example.test/child\n"), 0644)
			write("bin/go", []byte(`#!/bin/bash
function main() {
    case "$1:$2" in
        work:edit)
            if [[ "$FAULT" == membership ]]; then
                printf '{"Use":[{"DiskPath":"."}]}\n'
            else
                printf '{"Use":[{"DiskPath":"."},{"DiskPath":"./child"}]}\n'
            fi
            ;;
        work:sync)
            if [[ "$FAULT" == sync ]]; then printf '\n// drift\n' >> go.mod; fi
            ;;
        build:*|vet:*|mod:tidy)
            printf '%s %s %s\n' "$PWD" "$1" "$2" >> "$CALLS"
            if [[ "$FAULT" == "$1" || ( "$FAULT" == tidy && "$1:$2" == mod:tidy ) ]]; then return 1; fi
            ;;
        list:*)
            if [[ "$FAULT" == packages ]]; then printf 'one/package\n'; return; fi
            local n
            for ((n=0; n<181; n++)); do printf 'package/%s\n' "$n"; done
            ;;
        *) return 2 ;;
    esac
    return 0
}
main "$@"
`), 0755)
			for _, args := range [][]string{{"init", "-q"}, {"add", "go.mod", "child/go.mod"}} {
				cmd := exec.Command("git", args...)
				cmd.Dir = root
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git: %s: %v", out, err)
				}
			}
			// Compare sync against the index; no user identity or commit is needed.
			calls := filepath.Join(root, "calls")
			cmd := exec.Command("bash", "scripts/ci/module-integrity.sh")
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "PATH="+filepath.Join(root, "bin")+":"+os.Getenv("PATH"), "FAULT="+fault, "CALLS="+calls)
			out, err := cmd.CombinedOutput()
			if (err == nil) != (fault == "none") {
				t.Fatalf("fault=%s: err=%v\n%s", fault, err, out)
			}
			if fault == "build" || fault == "vet" || fault == "tidy" {
				data, err := os.ReadFile(calls)
				if err != nil || !strings.Contains(string(data), "/child mod tidy") {
					t.Fatalf("failure skipped the later module/tidy diagnosis: %s (%v)", data, err)
				}
			}
		})
	}
}
