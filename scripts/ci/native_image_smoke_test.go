package ci

import (
	"regexp"
	"strings"
	"testing"
)

// Selecting a target called "verified" is insufficient if that stage ceases to
// execute its smoke suite. Publication consumes the artifact exported from this
// stage, so its test must be a build instruction that cannot ignore failure.
func nativeSmokeProblem(dockerfile, base, command string) string {
	lines := strings.Split(dockerfile, "\n")
	var instructions []string
	var continued string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		continued += " " + strings.TrimSuffix(line, `\`)
		if strings.HasSuffix(line, `\`) {
			continue
		}
		instructions = append(instructions, strings.TrimSpace(continued))
		continued = ""
	}
	active, found := false, false
	for _, instruction := range instructions {
		if strings.HasPrefix(instruction, "FROM ") {
			active = instruction == "FROM "+base+" AS verified"
			continue
		}
		if !active || !strings.HasPrefix(instruction, "RUN ") {
			continue
		}
		if !strings.Contains(instruction, command) {
			continue
		}
		if strings.Contains(instruction, "||") || strings.Contains(instruction, "set +e") || regexp.MustCompile(`\b(?:echo|printf)\b`).MatchString(instruction) {
			return "smoke instruction may print or suppress its result"
		}
		found = true
	}
	if !found {
		return "verified target does not execute its smoke suite"
	}
	return ""
}

func TestNativeImageExportsRequireTheSmokeStage(t *testing.T) {
	for _, tc := range []struct{ path, base, command string }{
		{"deploy/toolchain-image/Dockerfile", "runtime", "bash /source/deploy/toolchain-image/smoke-test.sh --in-image"},
		{"deploy/db-image/Dockerfile", "operand", `bash /memql-db-smoke.sh "${PG_MAJOR}" "${TIMESCALEDB_VERSION}" "${TIMESCALEDB_PREVIOUS_VERSION}"`},
	} {
		t.Run(tc.path, func(t *testing.T) {
			raw := toolchainRead(t, tc.path)
			if problem := nativeSmokeProblem(raw, tc.base, tc.command); problem != "" {
				t.Fatal(problem)
			}
			for name, changed := range map[string]string{
				"stage removed":   strings.ReplaceAll(raw, "AS verified", "AS unchecked"),
				"test removed":    strings.ReplaceAll(raw, tc.command, "true"),
				"failure ignored": strings.ReplaceAll(raw, tc.command, tc.command+" || true"),
				"command printed": strings.ReplaceAll(raw, tc.command, "echo "+tc.command),
			} {
				t.Run(name, func(t *testing.T) {
					if changed == raw || nativeSmokeProblem(changed, tc.base, tc.command) == "" {
						t.Fatal("mutation bypassed the smoke prerequisite")
					}
				})
			}
		})
	}
}
