package identity

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// Execute the real resolver in a fresh checkout. A wrong or broken compiler
// on PATH must use the existing local fallback; the exact pin must avoid both
// the fallback and curl. The image smoke test additionally compiles real CSS
// with the real binary and networking disabled.
func TestBuildCSSUsesOnlyThePinnedCompilerOnPath(t *testing.T) {
	script, err := os.ReadFile("build-css.sh")
	if err != nil {
		t.Fatal(err)
	}
	lib, err := os.ReadFile("lib.sh")
	if err != nil {
		t.Fatal(err)
	}
	pin := regexp.MustCompile(`(?m)^TAILWIND_VERSION="([^"]+)"`).FindSubmatch(script)
	if len(pin) != 2 {
		t.Fatal("could not read the canonical Tailwind version")
	}
	suffix := map[string]string{"linux/amd64": "linux-x64", "linux/arm64": "linux-arm64", "darwin/amd64": "macos-x64", "darwin/arm64": "macos-arm64"}[runtime.GOOS+"/"+runtime.GOARCH]
	if suffix == "" {
		t.Skip("Tailwind has no standalone asset for this host")
	}
	for _, tc := range []struct{ name, help, want string }{
		{"pinned", "printf '≈ tailwindcss " + string(pin[1]) + "\\n\\nUsage:\\n'", "installed"},
		{"wrong version", "printf '≈ tailwindcss v0.0.0\\n'", "fallback"},
		{"version suffix", "printf '≈ tailwindcss " + string(pin[1]) + "-dev\\n'", "fallback"},
		{"broken", "exit 1", "fallback"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			write := func(path, content string) {
				t.Helper()
				path = filepath.Join(root, path)
				if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(content), 0755); err != nil {
					t.Fatal(err)
				}
			}
			write("scripts/identity/build-css.sh", string(script))
			write("scripts/identity/lib.sh", string(lib))
			write("component/identity/web/tailwind/input.css", "input")
			write("component/identity/web/static/app.css", "")
			write("brand/fonts/probe.woff2", "font")
			write("brand/favicon.svg", "icon")
			compiler := func(name, help string) string {
				return fmt.Sprintf("#!/usr/bin/env bash\nset -eu\n[[ ${BUN_JSC_useJIT:-} == 0 ]] || { echo 'Tailwind requires interpreter mode' >&2; exit 97; }\nif [[ ${1:-} == --help ]]; then\n%s\nexit 0\nfi\nprintf '%%s' '%s' > component/identity/web/static/app.css\n", help, name)
			}
			write("path/tailwindcss", compiler("installed", tc.help))
			write("bin/tools/tailwindcss-"+suffix, compiler("fallback", "exit 1"))
			write("path/curl", "#!/bin/sh\necho unexpected download >&2\nexit 99\n")
			// The override belongs to the compiler process only. An inherited
			// JIT setting must not reach either --help or compilation, while
			// unrelated build tools retain the caller's environment.
			write("path/cp", "#!/bin/sh\n[ \"${BUN_JSC_useJIT:-}\" = 1 ] || exit 98\nexec /bin/cp \"$@\"\n")
			cmd := exec.Command("bash", "scripts/identity/build-css.sh")
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "BUN_JSC_useJIT=1", "PATH="+filepath.Join(root, "path")+string(os.PathListSeparator)+os.Getenv("PATH"))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("build-css: %v\n%s", err, out)
			}
			got, err := os.ReadFile(filepath.Join(root, "component/identity/web/static/app.css"))
			if err != nil || strings.TrimSpace(string(got)) != tc.want {
				t.Fatalf("compiler = %q, err = %v; want %q", got, err, tc.want)
			}
		})
	}
}
