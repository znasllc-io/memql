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
// on PATH must use the existing pinned local fallback; the exact pin must
// avoid both the fallback and curl. The image smoke test additionally
// compiles real CSS with the real binary and networking disabled.
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
			write("bin/tools/tailwindcss-"+suffix, compiler("fallback", "printf '≈ tailwindcss "+pin+"\\n'"))
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

// A bin/tools compiler is held to the pin, like one on PATH. The Dockerfiles
// pre-fetch it at their own ARG TAILWIND_VERSION and a checkout keeps whatever
// an earlier build downloaded, so a copy at another version, or one that
// cannot execute, is replaced by the pinned release before anything compiles
// with it. uname is stubbed to one Linux host, so every host runs the same
// cases, and PATH holds only stubs plus the real commands the script calls,
// so the host's own curl or tailwindcss can never stand in for a stub.
func TestBuildCSSReplacesACachedCompilerThatIsNotThePin(t *testing.T) {
	pin := tailwindPin(t)
	tools := hostTools(t, "bash", "basename", "cat", "chmod", "cp", "dirname", "mkdir", "rm", "tr")
	const asset = "tailwindcss-linux-x64"
	release := "https://github.com/tailwindlabs/tailwindcss/releases/download/" + pin + "/" + asset
	compiler := func(name, version string) string {
		return "#!/usr/bin/env bash\n" +
			"[[ ${BUN_JSC_useJIT:-} == 0 ]] || { echo 'Tailwind requires interpreter mode' >&2; exit 97; }\n" +
			"[[ ${1:-} != --help ]] || { printf '≈ tailwindcss %s\\n\\nUsage:\\n' " + version + "; exit 0; }\n" +
			"while (($#)); do [[ $1 != --output ]] || printf %s " + name + " > \"$2\"; shift; done\n"
	}
	for _, tc := range []struct {
		name     string
		cached   string // bin/tools/<asset> before the build
		replaced bool   // the pinned release was downloaded over it
	}{
		{"the pin is reused", compiler("cached", pin), false},
		{"another version is replaced", compiler("cached", "v3.4.17"), true},
		{"a version suffix is replaced", compiler("cached", pin+"-dev"), true},
		// The kernel refuses it the way it refuses a glibc build on a musl
		// host: the interpreter its header names does not exist.
		{"one that cannot execute is replaced", "#!/nonexistent/ld-linux-x86-64.so.2\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, write := buildCSSCheckout(t)
			write("path/uname", "#!/bin/sh\ncase $1 in -s) echo Linux ;; -m) echo x86_64 ;; *) exit 64 ;; esac\n")
			write("bin/tools/"+asset, tc.cached)
			downloaded := compiler("downloaded", pin)
			if tc.replaced {
				write("release/"+asset, downloaded)
				// Like curl -o, this writes over the existing file in place;
				// any other URL is a download of something other than the pin.
				write("path/curl", "#!/usr/bin/env bash\nout= url=\n"+
					"while (($#)); do case $1 in -o) out=$2; shift ;; https://*) url=$1 ;; esac; shift; done\n"+
					"[[ $url == '"+release+"' ]] || { echo \"unexpected download: $url\" >&2; exit 99; }\n"+
					"cat '"+filepath.Join(root, "release", asset)+"' > \"$out\"\n")
			} else {
				write("path/curl", "#!/bin/sh\necho unexpected download >&2\nexit 99\n")
			}

			cmd := exec.Command("bash", "scripts/identity/build-css.sh")
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "PATH="+filepath.Join(root, "path")+string(os.PathListSeparator)+tools)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("build-css: %v\n%s", err, out)
			}
			wantCSS, wantBinary := "cached", tc.cached
			if tc.replaced {
				wantCSS, wantBinary = "downloaded", downloaded
			}
			if css, err := os.ReadFile(filepath.Join(root, "component/identity/web/static/app.css")); err != nil || string(css) != wantCSS {
				t.Errorf("app.css = %q, err = %v; want %q from that compiler\n%s", css, err, wantCSS, out)
			}
			if got, err := os.ReadFile(filepath.Join(root, "bin/tools", asset)); err != nil || string(got) != wantBinary {
				t.Errorf("bin/tools/%s = %q, err = %v; want %q", asset, got, err, wantBinary)
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

// Every Dockerfile that downloads a Tailwind release downloads the version
// build-css.sh pins. The engine Dockerfiles pre-fetch the compiler into
// bin/tools in an early cached layer (memql#1351) and the toolchain image
// bakes one onto PATH (memql#5816), each so a build does not download it
// again. The script uses a copy only at exactly its pin, so a copy at another
// version is never used. docker/memql.Dockerfile stayed at v3.4.17 after the
// script moved to v4.1.11 (8c71f3d31), back when bin/tools was reused at any
// version, and v3 cannot read the v4 input.css.
//
// The walk is `git ls-files`, so a new Dockerfile is checked once it is added,
// and it fails closed: the two Dockerfiles that pre-fetch for build-css.sh
// must be among those it checked.
func TestEveryDockerfileDownloadsTheTailwindPin(t *testing.T) {
	pin := tailwindPin(t)
	root := filepath.Join("..", "..")
	ls := exec.Command("git", "ls-files", "-z", "--", "*Dockerfile")
	ls.Dir = root
	out, err := ls.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	arg := regexp.MustCompile(`(?m)^ARG TAILWIND_VERSION=(\S*)\s*$`)
	checked := map[string]bool{}
	for _, path := range strings.Split(string(out), "\x00") {
		if base := filepath.Base(path); base != "Dockerfile" && !strings.HasSuffix(base, ".Dockerfile") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), "github.com/tailwindlabs/tailwindcss/releases/download/") {
			continue
		}
		checked[path] = true
		switch args := arg.FindAllSubmatch(body, -1); {
		case len(args) != 1:
			t.Errorf("%s downloads a Tailwind release, so it must declare ARG TAILWIND_VERSION exactly once; found %d", path, len(args))
		case string(args[0][1]) != pin:
			t.Errorf("%s downloads Tailwind %s, but scripts/identity/build-css.sh pins %s.\n"+
				"The script uses a compiler only at exactly its pin, so this copy is never used and every "+
				"build downloads the pin again. Move every ARG TAILWIND_VERSION in the same change as the "+
				"pin; the toolchain image's also carries a SHA-256 for each architecture.", path, args[0][1], pin)
		}
	}
	for _, path := range []string{"Dockerfile", "docker/memql.Dockerfile"} {
		if !checked[path] {
			t.Errorf("%s pre-fetches build-css.sh's compiler, but this scan never checked it", path)
		}
	}
}
