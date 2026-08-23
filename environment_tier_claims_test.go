package main

import (
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// MemQL ships ONE INSTALLATION SHAPE (epic memql#3943). There is no
// staging-versus-production dimension inside the product: an operator who wants
// a second environment installs a second instance, with its own domain and its
// own ArgoCD. One cloud overlay, one Application, one namespace.
//
// TestNoEnvironmentBranchingInEngineCode enforces that in Go, and its exemption
// map is EMPTY -- engine code may not so much as name `staging` or `production`,
// in a comparison, a switch case or a map key. Docs, shell scripts and kustomize
// comments sit outside that gate, and the tree drifted into saying the opposite
// of itself: the engine asserted in Go that staging does not exist while
// docs/internal/ops/workbench-production.md tabulated a **Staging** row as a
// deployment target and docs/public/operate/infrastructure.md named the cluster
// after that same tier. A reader following those docs learned the opposite of
// what the design decided (memql#4286).
//
// (The literals themselves are deliberately not spelled here. They are banned
// repo-wide by core/vendorname, and that sweep polices GIT-TRACKED files -- so
// a new file naming one is invisible to it until the moment it is staged,
// which is a poor moment to find out.)
//
// WHY THIS GATE IS NOT A WORD BAN, which is the version that does not work.
// `production` is perfectly good ordinary English -- "production traffic", "a
// production-grade cluster", "the production of a build artifact" -- and
// `staging` has an innocent sense too: a staging directory, staging a file in
// git. A naive noun ban fires on prose that is correct, and a guard that cries
// wolf gets exemptions bolted on until it means nothing.
//
// So this gate reads STRUCTURE, not vocabulary. A markdown HEADING or the FIRST
// CELL OF A TABLE ROW whose whole text is an environment tier is a claim about
// the product's shape: it is presented as a thing the product has, a category
// readers can be in. The same word inside a sentence is not that claim, and is
// left alone. The resource-name spellings -- one operator's cluster, resource
// group, storage account and key vault, each with the tier in its name -- have
// no innocent reading at all and are banned outright by the sibling list in
// core/vendorname.
//
// WHY `development` AND `local` ARE NOT TIERS HERE. They distinguish deploy
// TARGETS -- k3d versus AKS -- which the design keeps and which carries its own
// field, `provider` (`docker-local` | `azure`). CLAUDE.md says so explicitly.
// Only the environment dimension epic memql#3943 removed is forbidden.

// tierClaim matches a heading or first table cell that IS an environment tier,
// with nothing else in it. Anchored at both ends on purpose: "Production
// readiness" is a topic, "Production" is a tier.
var tierClaim = regexp.MustCompile(`^(staging|production|prod|pre-prod|preprod|uat)( (environment|env|tier|cluster|instance|deployment))?$`)

// headingLine and tableRow are the two structures a tier claim hides in.
var (
	headingLine = regexp.MustCompile(`^\s{0,3}#{1,6}\s+(.*?)\s*#*\s*$`)
	tableRow    = regexp.MustCompile(`^\s*\|(.*)$`)
)

// tierRoots are the trees outside the Go gate's reach. Engine code is already
// covered, with an empty exemption map, by environment_branching_test.go.
var tierRoots = []string{"docs/", "deploy/", "scripts/"}

// tierExempt are files that must contain these strings to do their job: this
// gate, the Go gate it is the sibling of, and the banned-name list.
var tierExempt = map[string]bool{
	"environment_tier_claims_test.go": true,
	"environment_branching_test.go":   true,
	"core/vendorname/vendorname.go":   true,
}

// normalizeCell strips the markdown a heading or cell may be dressed in, so
// `**Staging**`, "`staging`" and "Staging" are one thing.
func normalizeCell(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "*_`")
	s = strings.TrimSpace(s)
	return strings.ToLower(s)
}

func TestDocsDeployAndScriptsClaimNoEnvironmentTier(t *testing.T) {
	out, err := exec.Command("git", "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}

	var hits []string
	var scanned, unread int
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel == "" || tierExempt[rel] {
			continue
		}
		var inRoot bool
		for _, r := range tierRoots {
			if strings.HasPrefix(rel, r) {
				inRoot = true
				break
			}
		}
		if !inRoot || strings.Contains(rel, "/node_modules/") {
			continue
		}

		// Whole-file read, never bufio.Scanner: this repository tracks a 40 MB
		// single-line JSON artifact that a Scanner refuses outright ("token too
		// long"), and a helper that works everywhere is worth more than one
		// that happens to work on the files this gate looks at today.
		body, err := os.ReadFile(rel)
		if err != nil {
			// Counted rather than swallowed. A checker that hides what it
			// could not examine makes its own pass a claim about the tool
			// rather than about the tree.
			unread++
			continue
		}
		scanned++

		for i, line := range strings.Split(string(body), "\n") {
			var cell string
			switch {
			case headingLine.MatchString(line):
				cell = normalizeCell(headingLine.FindStringSubmatch(line)[1])
			case tableRow.MatchString(line):
				// The first cell only. A later column may legitimately say
				// "production traffic"; the row LABEL is the claim.
				fields := strings.Split(tableRow.FindStringSubmatch(line)[1], "|")
				if len(fields) == 0 {
					continue
				}
				cell = normalizeCell(fields[0])
				// A separator row (|---|---|) is not a claim.
				if strings.Trim(cell, "-: ") == "" {
					continue
				}
			default:
				continue
			}
			if !tierClaim.MatchString(cell) {
				continue
			}
			hits = append(hits, rel+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
			if len(hits) > 40 {
				break
			}
		}
		if len(hits) > 40 {
			break
		}
	}

	// COVERAGE FLOOR, in the first commit rather than after (memql#4286). A
	// guard's first passing run is exactly when nobody looks at it closely, and
	// the neighbouring sweep's first widened run passed while silently reading
	// nothing under two roots -- its floor is the only reason that became a
	// failure instead of a green tick.
	if scanned < 200 {
		t.Fatalf("only %d files scanned under %v -- this gate is not looking at the tree",
			scanned, tierRoots)
	}
	if unread > 0 {
		t.Logf("%d tracked file(s) could not be read and were NOT examined", unread)
	}

	if len(hits) > 0 {
		t.Errorf("MemQL ships ONE installation shape (epic memql#3943): a heading or a table "+
			"row LABELLED with an environment tier claims the product has a dimension it "+
			"deliberately does not, and engine code is already forbidden from naming one. "+
			"Say what it actually is -- one installation, or a deploy target (local | cloud). "+
			"The same word inside a sentence is fine and is not matched. %d hit(s):\n  %s",
			len(hits), strings.Join(hits, "\n  "))
	}
}
