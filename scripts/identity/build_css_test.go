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

// tailwindPin reads the Tailwind version build-css.sh pins.
func tailwindPin(t *testing.T) string {
	t.Helper()
	script, err := os.ReadFile("build-css.sh")
	if err != nil {
		t.Fatal(err)
	}
	pin := regexp.MustCompile(`(?m)^TAILWIND_VERSION="([^"]+)"`).FindSubmatch(script)
	if len(pin) != 2 {
		t.Fatal("could not read the canonical Tailwind version")
	}
	return string(pin[1])
}

// buildCSSCheckout stages build-css.sh, its library and the inputs it reads in
// a fresh checkout, returning the root and a writer for further files. Every
// file is written executable, so the writer also places stub tools.
func buildCSSCheckout(t *testing.T) (string, func(path, content string)) {
	t.Helper()
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
	for _, name := range []string{"build-css.sh", "lib.sh"} {
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		write("scripts/identity/"+name, string(source))
	}
	write("component/identity/web/tailwind/input.css", "input")
	write("component/identity/web/static/app.css", "")
	write("brand/fonts/probe.woff2", "font")
	write("brand/favicon.svg", "icon")
	return root, write
}

// hostTools links the named real commands into a fresh directory, so a PATH
// can hold them and nothing else from the host.
func hostTools(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		real, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// Execute the real resolver in a fresh checkout. A wrong or broken compiler
// on PATH must use the existing local fallback; the exact pin must avoid both
// the fallback and curl. The image smoke test additionally compiles real CSS
// with the real binary and networking disabled.
func TestBuildCSSUsesOnlyThePinnedCompilerOnPath(t *testing.T) {
	pin := tailwindPin(t)
	suffix := map[string]string{"linux/amd64": "linux-x64", "linux/arm64": "linux-arm64", "darwin/amd64": "macos-x64", "darwin/arm64": "macos-arm64"}[runtime.GOOS+"/"+runtime.GOARCH]
	if suffix == "" {
		t.Skip("Tailwind has no standalone asset for this host")
	}
	for _, tc := range []struct{ name, help, want string }{
		{"pinned", "printf '≈ tailwindcss " + pin + "\\n\\nUsage:\\n'", "installed"},
		{"wrong version", "printf '≈ tailwindcss v0.0.0\\n'", "fallback"},
		{"version suffix", "printf '≈ tailwindcss " + pin + "-dev\\n'", "fallback"},
		{"broken", "exit 1", "fallback"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, write := buildCSSCheckout(t)
			compiler := func(name, help string) string {
				return fmt.Sprintf("#!/usr/bin/env bash\nset -eu\n[[ ${BUN_JSC_useJIT:-} == 0 ]] || { echo 'Tailwind requires interpreter mode' >&2; exit 97; }\nif [[ ${1:-} == --help ]]; then\n%s\nexit 0\nfi\nprintf '%%s' '%s' > component/identity/web/static/app.css\n", help, name)
			}
			write("path/tailwindcss", compiler("installed", tc.help))
			write("bin/tools/tailwindcss-"+suffix, compiler("fallback", "exit 1"))
			write("path/curl", "#!/bin/sh\necho unexpected download >&2\nexit 99\n")
			// On a macOS host the fallback's signature is checked before it
			// runs. That decision has its own test below; here every compiler
			// verifies, and a re-sign would fail the build.
			write("path/codesign", "#!/bin/sh\n[ \"$1\" = --verify ]\n")
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

// macOS SIGKILLs a process whose pages do not match its code signature, and
// Tailwind's macOS release assets ship that way: v4.1.11 carries the linker's
// ad-hoc signature over a file modified after linking, so even --help exits
// 137. The stand-in compiler here behaves like that kernel: on Darwin it kills
// itself unless its signature verifies, so a build only gets CSS when
// build-css.sh repaired the signature before the first run. uname is stubbed,
// so every host runs the macOS cases, and PATH holds only stubs plus the real
// commands the script and stubs call, so the host's own codesign, curl or
// tailwindcss can never stand in for a stub.
func TestBuildCSSRepairsTheMacOSCompilerSignatureBeforeRunningIt(t *testing.T) {
	pin := tailwindPin(t)
	tools := hostTools(t, "bash", "basename", "cat", "chmod", "cp", "dirname", "grep", "mkdir", "rm", "tr")
	// The stand-in signature is a line of the compiler file itself, so like an
	// embedded Mach-O signature it travels with the file's contents.
	const valid, modified = "# signature: valid\n", "# signature: modified\n"
	compiler := func(name, signature string) string {
		return "#!/usr/bin/env bash\n" +
			"[[ $(uname -s) != Darwin ]] || grep -qx '# signature: valid' \"$0\" || kill -KILL $$\n" +
			"[[ ${1:-} != --help ]] || { printf '≈ tailwindcss %s\\n' " + pin + "; exit 0; }\n" +
			"while (($#)); do [[ $1 != --output ]] || printf %s " + name + " > \"$2\"; shift; done\n" +
			"exit 0\n" + signature
	}
	// codesign as build-css.sh drives it: --verify checks the signature line,
	// and an ad-hoc --force --sign - appends a valid one. Real codesign
	// refuses to replace an existing signature without --force.
	const codesign = "#!/bin/sh\n" +
		"if [ $# -eq 2 ] && [ \"$1\" = --verify ]; then exec grep -qx '# signature: valid' \"$2\"; fi\n" +
		"if [ $# -eq 4 ] && [ \"$1 $2 $3\" = '--force --sign -' ]; then echo '# signature: valid' >> \"$4\"; exit 0; fi\n" +
		"echo \"codesign: unexpected arguments: $*\" >&2\nexit 2\n"
	for _, tc := range []struct {
		name     string
		host     string // what uname -s and -m report
		asset    string // the release asset build-css.sh resolves for host
		onPath   string // signature of a pinned compiler on PATH; "" = none
		cached   string // signature of bin/tools/<asset>; "" = none
		download string // signature of the asset curl serves; "" = must not download
		codesign bool   // codesign is on PATH
		want     string // the compiler that wrote app.css; "" = the build refuses
		resigned bool   // bin/tools/<asset> was re-signed before it ran
	}{
		{name: "a fresh download is re-signed", host: "Darwin arm64", asset: "tailwindcss-macos-arm64", download: modified, codesign: true, want: "downloaded", resigned: true},
		{name: "a cached compiler that fails verify is re-signed", host: "Darwin arm64", asset: "tailwindcss-macos-arm64", cached: modified, codesign: true, want: "cached", resigned: true},
		{name: "an Intel Mac re-signs too", host: "Darwin x86_64", asset: "tailwindcss-macos-x64", cached: modified, codesign: true, want: "cached", resigned: true},
		{name: "a cached compiler that verifies is left alone", host: "Darwin arm64", asset: "tailwindcss-macos-arm64", cached: valid, codesign: true, want: "cached"},
		{name: "a compiler on PATH the kernel kills is skipped, not re-signed", host: "Darwin arm64", asset: "tailwindcss-macos-arm64", onPath: modified, cached: valid, codesign: true, want: "cached"},
		{name: "Linux needs no codesign", host: "Linux x86_64", asset: "tailwindcss-linux-x64", cached: modified, want: "cached"},
		{name: "macOS without codesign stops before the compiler runs", host: "Darwin arm64", asset: "tailwindcss-macos-arm64", cached: modified},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, write := buildCSSCheckout(t)
			kernel, arch, _ := strings.Cut(tc.host, " ")
			write("path/uname", "#!/bin/sh\ncase $1 in -s) echo "+kernel+" ;; -m) echo "+arch+" ;; *) exit 64 ;; esac\n")
			if tc.codesign {
				write("path/codesign", codesign)
			}
			if tc.onPath != "" {
				write("path/tailwindcss", compiler("installed", tc.onPath))
			}
			var cachedBefore string
			switch {
			case tc.cached != "":
				cachedBefore = compiler("cached", tc.cached)
				write("bin/tools/"+tc.asset, cachedBefore)
			case tc.download != "":
				cachedBefore = compiler("downloaded", tc.download)
				write("release/"+tc.asset, cachedBefore)
			}
			if tc.download != "" {
				// Like curl -o, this creates a new file that is not executable.
				write("path/curl", "#!/usr/bin/env bash\nwhile (($#)); do [[ $1 != -o ]] || cat '"+filepath.Join(root, "release", tc.asset)+"' > \"$2\"; shift; done\n")
			} else {
				write("path/curl", "#!/bin/sh\necho unexpected download >&2\nexit 99\n")
			}

			cmd := exec.Command("bash", "scripts/identity/build-css.sh")
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "PATH="+filepath.Join(root, "path")+string(os.PathListSeparator)+tools)
			out, err := cmd.CombinedOutput()
			css, readErr := os.ReadFile(filepath.Join(root, "component/identity/web/static/app.css"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			switch {
			// Stumbling into "codesign: command not found" also fails, but
			// names no fix; the refusal must say where macOS keeps codesign.
			case tc.want == "" && (err == nil || !regexp.MustCompile(`(?m)^\[error\] .*/usr/bin/codesign`).Match(out)):
				t.Fatalf("build-css: err = %v; want a refusal naming /usr/bin/codesign\n%s", err, out)
			case tc.want != "" && err != nil:
				t.Fatalf("build-css: %v\n%s", err, out)
			case string(css) != tc.want:
				t.Fatalf("app.css = %q; want %q from that compiler\n%s", css, tc.want, out)
			}

			want := cachedBefore
			if tc.resigned {
				want += valid
			}
			if got, err := os.ReadFile(filepath.Join(root, "bin/tools", tc.asset)); err != nil || string(got) != want {
				t.Errorf("bin/tools/%s = %q, err = %v; want %q", tc.asset, got, err, want)
			}
			if tc.onPath != "" {
				if got, err := os.ReadFile(filepath.Join(root, "path/tailwindcss")); err != nil || string(got) != compiler("installed", tc.onPath) {
					t.Errorf("the compiler on PATH was rewritten: %q, err = %v", got, err)
				}
			}
		})
	}
}
