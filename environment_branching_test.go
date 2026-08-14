package main

import (
	"bufio"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// environmentNames is what engine code may not say.
//
// Deliberately just these three. `development` and `local` are NOT here, and
// the omission is the whole design of this gate -- see the "What is allowed"
// section on TestNoEnvironmentBranchingInEngineCode.
//
// Matched as a COMPLETE Go string literal, case-insensitively, so
// `"deploy_staging"` and `"docker-local"` do not trip it. Both quoting forms
// are covered -- a raw string literal is the same value with different
// delimiters, and a gate that only knew about `"` would be one backtick away
// from silent. Longest alternative first: Go's regexp is leftmost-first
// through an alternation, so `prod` ahead of `production` would match `"prod`
// inside `"production"`, fail on the closing quote, and report nothing.
var environmentNames = regexp.MustCompile("(?i)[\"`](production|prod|staging)[\"`]")

// environmentAwareByDesign maps a path prefix to the reason code under it is
// permitted to name a deployment environment.
//
// A prefix, not a file: the point of an entry is "this COMPONENT's job is
// knowing about environments", and pinning it to today's file names would make
// the exemption expire on a rename rather than on the reason going away.
var environmentAwareByDesign = map[string]string{
	"component/deploycontrol/": "the deploy console's own environment mapping -- ConsoleEnvFor / deploymentEnvFor / validEnvs translate between the deployment record's enum (production|staging|development) and promote.sh's console env (prod|staging), and gate which of the two an operator may target. This is not the engine behaving differently per environment; it is the surface whose entire subject is which environment a deploy names, and the mapping lives in ONE place here precisely so no caller re-derives it (memql#2096).",
}

// TestNoEnvironmentBranchingInEngineCode is the structural guard on the
// two-environments-one-installation boundary (epic memql#3748 / memql#3766).
//
// # The rule
//
// Staging and production are ONE cluster, ONE ArgoCD, ONE base and ONE set of
// engine images, separated by nothing but configuration: a namespace, a schema
// search path, a replica count, a host prefix. Environment reaches the engine
// as a VALUE and never as a branch.
//
// So any engine code that can tell staging from production is, by construction,
// a second way to deploy -- the thing CLAUDE.md's parity standard rejects
// outright ("Reject in review: ... `if env=="local"` branches, or a second way
// to deploy -- the moment local diverges in *shape* it stops proving anything
// about staging/prod"). It does not matter whether the branch is spelled as a
// comparison, a switch case, or a map-membership test: this gate fails on the
// engine merely NAMING one of those environments, because a name is what a
// branch is built out of and the intermediate forms are endless. `validEnvs`
// in the deploy console is a map literal, not an `if` -- a gate that looked for
// `==` would have missed it.
//
// # What is allowed, and why the list stops where it does
//
// `development` and `local` are absent from environmentNames on purpose, and
// this is the judgement call in the file.
//
// Those names distinguish DEPLOY TARGETS -- a k3d cluster on a laptop versus
// AKS -- which are genuinely different infrastructure reached by different
// machinery, and the design this gate serves preserves that distinction
// explicitly: "`deploycontrol` still refuses local. Unchanged"
// (docs/superpowers/specs/2026-08-14-virtual-environments-design.md §5). Two
// live sites depend on it -- `validateDeploymentProvider` refusing the retired
// `docker-local` provider, and `deployProvider` in app/cluster.go labelling a
// development cluster's provider -- and neither can tell staging from
// production, which is the boundary this epic creates and this gate protects.
//
// Widening the pattern to `development` would fail the tree today and the fix
// would be to delete a distinction the design keeps. That is the wrong trade,
// so the gate is narrow and says so rather than being broad and disabled.
//
// # Honest limits
//
//   - Like TestEngineIsProductNeutral beside it, this is a NAMES list. It
//     catches these three spellings. Code that reads MEMQL_ENVIRONMENT into a
//     constant somewhere and compares against the constant is invisible to it.
//     Do not read a passing run as proof that nothing branches on environment.
//   - `_test.go` files are skipped. Tests assert on environment values
//     constantly ("the record's env is staging") and a gate that forbade that
//     would forbid testing the thing it polices.
//   - Line comments are skipped. The literal appears in doc comments across
//     generated protobuf, the SDK and the automations preamble, all of them
//     PROSE describing a contract. Flagging prose is how a gate earns a
//     blanket exemption from the next person in a hurry. Block comments are
//     not tracked; no hit in this tree lives in one, and a scanner that
//     understood them would be parsing Go, at which point the git-tracked-file
//     sweep every other root gate uses stops being the shape of this file.
func TestNoEnvironmentBranchingInEngineCode(t *testing.T) {
	out, err := exec.Command("git", "ls-files", "-z", "*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}

	var (
		hits    []string
		scanned int
		// Which exemptions actually suppressed something on this run. An
		// entry that suppresses nothing is stale, and a stale entry silently
		// pre-authorizes its whole subtree for whatever lands there next.
		used = map[string]bool{}
	)

	for _, rel := range strings.Split(string(out), "\x00") {
		if rel == "" || strings.HasSuffix(rel, "_test.go") {
			continue
		}
		scanned++

		var exempt string
		for prefix := range environmentAwareByDesign {
			if strings.HasPrefix(rel, prefix) {
				exempt = prefix
				break
			}
		}

		f, err := os.Open(rel)
		if err != nil {
			continue // deleted-in-worktree etc.
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		line := 0
		for scanner.Scan() && len(hits) <= 40 {
			line++
			text := strings.TrimSpace(scanner.Text())
			if strings.HasPrefix(text, "//") {
				continue
			}
			if !environmentNames.MatchString(text) {
				continue
			}
			if exempt != "" {
				used[exempt] = true
				continue
			}
			hits = append(hits, rel+":"+itoa(line)+": "+text)
		}
		f.Close()
		if len(hits) > 40 {
			break
		}
	}

	// A sweep that enumerated nothing must fail rather than pass: `git
	// ls-files` returning empty (wrong working directory, no git) would
	// otherwise be indistinguishable from a clean tree.
	if scanned < 100 {
		t.Fatalf("scanned only %d non-test Go files; this gate cannot pass vacuously", scanned)
	}

	if len(hits) > 0 {
		t.Errorf("engine code must not name a deployment environment -- staging and production run the SAME "+
			"images from the SAME base and differ only in configuration, so code that can tell them apart is a "+
			"second way to deploy (epic memql#3748). Pass the difference in as a value. %d hit(s):\n  %s",
			len(hits), strings.Join(hits, "\n  "))
	}

	var stale []string
	for prefix, reason := range environmentAwareByDesign {
		if !used[prefix] {
			stale = append(stale, prefix+" ("+reason+")")
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("these exemptions suppressed nothing and are stale -- delete them, or the subtree stays "+
			"pre-authorized for whatever lands in it next:\n  %s", strings.Join(stale, "\n  "))
	}
}
