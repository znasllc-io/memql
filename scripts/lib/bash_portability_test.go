package lib

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/repowalk"
)

// bash_portability_test.go -- the installer that only ran on Linux
// (znasllc-io/memql local-install portability).
//
// THE CONSTRAINT. macOS ships bash 3.2.57 (2007) as /bin/bash and always will:
// bash 4 moved to GPLv3, which Apple does not ship. Pop!_OS, Ubuntu and every
// CI runner here ship bash 5. `scripts/lib/platform.sh` lists darwin/arm64 as
// a SUPPORTED platform and `scripts/install/tool-pins.env` carries verified
// darwin/arm64 digests, so every script an operator's install can reach has to
// run on 3.2 -- and "the install lane" is most of scripts/.
//
// THE BUG THIS CLOSES. `scripts/install/seed-bootstrap.sh` used `local -n`, a
// bash 4.3 nameref. On macOS bash 3.2 answers
//
//	local: -n: invalid option
//
// and returns 2. Under `set -euo pipefail` the script aborts, so cap_ok is
// never reached and capability.sh's EXIT trap emits
//
//	{"ok":false, ..., "error":{"code":2,
//	 "message":"capability 'install.seedBootstrap' aborted (exit 2) without an explicit result"}}
//
// which names no variable and no line. It fired on 100% of macOS installs,
// 131ms into the step, after the image build had already spent 12 minutes.
// `scripts/lib/docker.sh` used `${out,,}` (bash 4.0) on the branch that
// classifies WHY docker is unreachable -- so the function whose whole job is
// explaining "Docker Desktop is not running" was the one that died, on the
// most likely first-run state a new Mac has.
//
// WHY A PATTERN SCAN AND NOT `bash -n`. Measured: under bash 3.2, `bash -n`
// exits 0 on both `local -n` and `${x,,}`. A nameref is a runtime BUILTIN
// error and a case modification a runtime EXPANSION error, so the syntax check
// TestCapabilityScriptsAreValidBash runs is structurally blind to this whole
// class, on any bash. Executing every script under 3.2 would catch it, but
// only for the code paths a test happens to reach; the pattern is what covers
// the branch nobody exercises -- which is exactly where docker.sh's hid.
//
// WHY IT IS A GATE AND NOT A CONVENTION. It was already a written convention,
// twice: scripts/lib/agents.sh's header states "Portability: bash 3.2 (stock
// macOS) -- no associative arrays, no `mapfile`, no `${var^^}`", and
// seed-bootstrap.sh avoids `mapfile` and `declare -A` BY NAME twenty lines
// from the nameref that shipped. Prose did not hold; every CI job runs on
// ubuntu-latest, so nothing else could.
//
// COMMENTS ARE EXEMPT ON PURPOSE. The scripts discuss these constructs in
// prose in order to warn about them, and a gate that punished the warning
// would delete its own documentation.

// bash4Construct is one refused construct: how to spot it, when bash gained
// it, and what to write instead. The repair matters more than the ban -- a
// gate that only says "no" gets worked around.
type bash4Construct struct {
	name    string
	since   string
	repair  string
	pattern *regexp.Regexp
}

// Line-level patterns. Anchored at statement start where the construct is a
// command, so a flag inside a quoted string ("kubectl get app memql-local -n
// argocd") is not a hit.
var bash4LinePatterns = []bash4Construct{
	{
		name:    "case-modifying expansion (${x,,} / ${x^^})",
		since:   "bash 4.0",
		repair:  "for a case-insensitive COMPARISON use `shopt -s nocasematch` (bash 3.1) and match directly; to transform, `tr '[:upper:]' '[:lower:]'` -- but note that reintroduces a PATH dependency the install lane may not have",
		pattern: regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*(\[[^]]*\])?(,,?|\^\^?)\}`),
	},
	{
		name:    "`wait -n`",
		since:   "bash 4.3",
		repair:  "collect the PIDs and `wait` for each in turn",
		pattern: regexp.MustCompile(`^wait[ \t]+-[a-zA-Z]*n`),
	},
	{
		name:    "`[[ -v ]]`",
		since:   "bash 4.2",
		repair:  "`[[ -n \"${x+set}\" ]]`",
		pattern: regexp.MustCompile(`\[\[[ \t]+-v[ \t]`),
	},
	{
		name:    "`shopt -s globstar`",
		since:   "bash 4.0",
		repair:  "`find` with `-name`, or an explicit loop",
		pattern: regexp.MustCompile(`^shopt[ \t]+-s[ \t]+globstar\b`),
	},
	{
		name:    "append-both-streams redirect (`&>>`)",
		since:   "bash 4.0",
		repair:  "`>>file 2>&1`",
		pattern: regexp.MustCompile(`&>>`),
	},
}

// Commands refused outright when they appear in command position.
var bash4Commands = map[string]bash4Construct{
	"mapfile": {
		name:   "`mapfile`",
		since:  "bash 4.0",
		repair: "`while IFS= read -r line; do arr+=(\"$line\"); done < <(...)`",
	},
	"readarray": {
		name:   "`readarray`",
		since:  "bash 4.0",
		repair: "`while IFS= read -r line; do arr+=(\"$line\"); done < <(...)`",
	},
	"coproc": {
		name:   "`coproc`",
		since:  "bash 4.0",
		repair: "a named pipe, or a background job with explicit redirections",
	},
}

// declareLike are the builtins whose OPTION LETTERS decide the verdict, so
// they are tokenised rather than pattern-matched: `local -a` (indexed array)
// is bash 2 and fine, `local -n` and `local -A` are not.
var declareLike = map[string]bool{
	"local": true, "declare": true, "typeset": true, "readonly": true, "export": true,
}

var bash4OptionLetters = map[rune]bash4Construct{
	'n': {
		name:   "nameref (`-n`)",
		since:  "bash 4.3",
		repair: "pass the VALUE and return it, or -- when every call site names the same variable, which is usually true -- append to a module-global array directly",
	},
	'A': {
		name:   "associative array (`-A`)",
		since:  "bash 4.0",
		repair: "parallel indexed arrays, as scripts/lib/agents.sh does",
	},
}

// allShellScripts walks scripts/ and returns every .sh file. Unlike
// capabilityScripts it does NOT filter on capability.sh: a plain library or a
// dev helper that an installer sources still has to run on the operator's
// bash.
func allShellScripts(t *testing.T) []string {
	t.Helper()
	scriptsDir := filepath.Join(repoRoot(t), "scripts")
	var found []string
	err := filepath.Walk(scriptsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if repowalk.SkipDir(info.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".sh") {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk scripts/: %v", err)
	}
	return found
}

// firstToken returns the first whitespace-separated word of a line, which is
// the command when the line is a simple statement.
func firstToken(line string) string {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func TestShellScriptsRunOnStockMacOSBash(t *testing.T) {
	scripts := allShellScripts(t)
	if len(scripts) == 0 {
		t.Fatal("no shell scripts found under scripts/ -- the walk is broken, and a broken " +
			"walk reports a clean bill of health about nothing")
	}

	report := func(path string, line int, c bash4Construct, code string) {
		rel, err := filepath.Rel(repoRoot(t), path)
		if err != nil {
			rel = path
		}
		t.Errorf("%s:%d: %s is %s, and macOS ships bash 3.2:\n"+
			"    %s\n"+
			"On stock /bin/bash this fails at RUNTIME (`bash -n` does not see it), so it "+
			"reaches the operator as an abort with no name in it.\n"+
			"Repair: %s",
			rel, line, c.name, c.since, code, c.repair)
	}

	for _, path := range scripts {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for i, raw := range strings.Split(string(body), "\n") {
			trimmed := strings.TrimSpace(raw)
			// Comments are exempt: the scripts warn about these constructs in
			// prose, and punishing the warning would delete the documentation.
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}

			for _, c := range bash4LinePatterns {
				if c.pattern.MatchString(trimmed) {
					report(path, i+1, c, trimmed)
				}
			}

			tok := firstToken(trimmed)
			if c, refused := bash4Commands[tok]; refused {
				report(path, i+1, c, trimmed)
				continue
			}

			if !declareLike[tok] {
				continue
			}
			// Option letters only, and only while they still look like flags:
			// `local -a x=-n` must not be read as a nameref.
			for _, field := range strings.Fields(trimmed)[1:] {
				if !strings.HasPrefix(field, "-") || field == "-" || field == "--" {
					break
				}
				for _, letter := range field[1:] {
					if c, refused := bash4OptionLetters[letter]; refused {
						report(path, i+1, c, trimmed)
					}
				}
			}
		}
	}
}
