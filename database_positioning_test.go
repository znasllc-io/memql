package main

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestNoDatabaseProductClaims is a LICENSE-COMPLIANCE gate wearing a prose
// test's clothes (memql#3843, epic memql#3842).
//
// memQL embeds TimescaleDB Community Edition, which is source-available under
// the Timescale License rather than an OSI-approved one. Self-hosting it is free
// for our model only because of the TSL §2.1(b) "Value Added Products or
// Services" grant -- and §3.10 prong (i) withholds that grant from a product
// that is "primarily [a] database storage or operations" product. This
// repository is PUBLIC, so a sentence in it describing memQL as a database is a
// public claim bearing directly on the condition the grant turns on.
//
// The rule this enforces:
//
//   - ALLOWED: naming the storage layer precisely -- "built on a time-series
//     memory graph", "PostgreSQL + TimescaleDB substrate". It genuinely is one.
//   - REFUSED: claiming memQL IS a database, of any kind, in a public-facing
//     file.
//
// WHY A TEST AND NOT A CONVENTION. "memQL is a time-series database with
// event-driven automations" is a natural sentence, reads as accurate, and was
// the literal first line of CLAUDE.md until this task. It is also the exact
// sentence §3.10 prong (i) is about. A rule enforced only by review re-acquires
// that sentence the first time somebody writes a reasonable summary of the
// project, and nothing about the symptom would point at a licence.
//
// The full posture, the client-agreement notice text, and the confirmation
// request to TigerData: docs/internal/ops/timescaledb-license-compliance.md.
func TestNoDatabaseProductClaims(t *testing.T) {
	// Matched case-insensitively against each line. Kept to the phrases that
	// assert memQL's CATEGORY -- deliberately not the bare "is a database",
	// which would flag the compliance doc and this comment saying it must not
	// be claimed, and would push authors toward evading the test rather than
	// fixing the sentence.
	claims := []string{
		"time-series database",
		"time series database",
		"timeseries database",
		"memory graph database",
	}

	// docs/internal/** is EXEMPT by design, not by oversight. Internal design
	// and ops docs are where the storage layer is described exactly, and the
	// compliance doc itself has to quote the banned phrasing to prohibit it.
	// The TSL condition is about how the product is PRESENTED; an unpublished
	// engineering note is not a presentation of it.
	exemptPrefixes := []string{
		"docs/internal/",
	}
	exemptFiles := map[string]bool{
		// Names the phrases by necessity.
		"database_positioning_test.go": true,
	}

	skipTopDirs := map[string]bool{
		".git":         true,
		"bin":          true,
		"node_modules": true,
	}

	// Git-tracked files only: untracked scratch files are not repo content,
	// and are not published with it.
	out, err := exec.Command("git", "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}

	var hits []string
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel == "" || exemptFiles[rel] {
			continue
		}
		if skipTopDirs[strings.SplitN(rel, string(filepath.Separator), 2)[0]] {
			continue
		}
		var exempt bool
		for _, p := range exemptPrefixes {
			if strings.HasPrefix(rel, p) {
				exempt = true
				break
			}
		}
		if exempt {
			continue
		}

		f, err := os.Open(rel)
		if err != nil {
			continue // deleted-in-worktree, symlink to nowhere, etc.
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		line := 0
		for scanner.Scan() && len(hits) <= 40 {
			line++
			lower := strings.ToLower(scanner.Text())
			for _, claim := range claims {
				if strings.Contains(lower, claim) {
					hits = append(hits, rel+":"+strconv.Itoa(line)+": "+strings.TrimSpace(scanner.Text()))
				}
			}
		}
		f.Close()
		if len(hits) > 40 {
			break
		}
	}

	if len(hits) > 0 {
		t.Errorf("a public-facing file claims memQL IS a database, which is the condition "+
			"TSL §3.10 prong (i) withholds the §2.1(b) grant on. Describe the substrate instead "+
			"(\"built on a time-series memory graph\"); the canonical line is \"memQL -- the AI "+
			"memory platform: agents, automations, and voice on a time-series memory graph\". "+
			"See docs/internal/ops/timescaledb-license-compliance.md. %d hit(s):\n  %s",
			len(hits), strings.Join(hits, "\n  "))
	}
}

// TestLicenseCompliancePackExists guards the two affirmative artifacts.
//
// The sweep above is the negative half of memql#3843 -- it proves no file makes
// the claim. The positive half is that the client-agreement notice text and the
// TigerData confirmation request actually exist somewhere findable, because
// those are what a licensing conversation would ask for and they are useless if
// they live only in a closed issue.
func TestLicenseCompliancePackExists(t *testing.T) {
	const pack = "docs/internal/ops/timescaledb-license-compliance.md"

	b, err := os.ReadFile(pack)
	if err != nil {
		t.Fatalf("the TSL compliance pack is missing (%s): %v", pack, err)
	}
	body := string(b)

	for _, required := range []struct{ needle, why string }{
		{"licensing@tigerdata.com", "the confirmation request has no addressee"},
		{"tigerdata.com/legal/licenses", "the client notice must cite the TSL's canonical URL"},
		{"data-definition statements (DDL)", "the client notice's DDL prohibition is missing"},
		{"2.1(b)", "the pack must name the grant it relies on"},
		{"3.10", "the pack must name the prong that conditions it"},
	} {
		if !strings.Contains(body, required.needle) {
			t.Errorf("%s does not contain %q -- %s", pack, required.needle, required.why)
		}
	}
}
