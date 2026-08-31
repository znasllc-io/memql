package dslconformance

import (
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/dslfs"
	"github.com/znasllc-io/memql/dsl"
)

// bootstrapCluster must PASS every argument its mutations accept (memql#4766).
//
// # The defect this exists for
//
// v1:cluster:database and v1:cluster:identityProvider carried fields that were
// declared, documented, and written by nothing: `engineVersion`, `extensions`,
// `extensionVersions`, `jwksUrl`, `acceptedAudiences`, and both `clusterId`
// links. Two of them -- `jwksUrl` and `acceptedAudiences` -- were already
// COMPUTED by app/cluster.go's parseIdentityProviderInfo and sitting in the
// startup payload. The automation step simply forwarded four fields out of six
// and dropped the rest on the floor.
//
// Nothing caught it because nothing READ those concepts: no query bound either
// one, so there was no surface to contradict the rows and no gate to notice.
// It surfaced only when the OS Settings app tried to render them (memql#4742)
// and found half the panel unshippable.
//
// # Why this shape of test
//
// The failure was a MISMATCH between two lists -- what a mutation accepts and
// what its one caller passes -- and neither list was wrong on its own. A test
// of the mutation passes; a test of the automation passes; only comparing them
// fails. So this compares them, and it fails on the NEXT field somebody adds
// to one side and forgets on the other, which is exactly how this happened.
//
// It reads argument names out of the mutation SOURCE rather than a hand-kept
// list, so the two sides cannot drift into agreement with a stale copy.

var (
	// `mutate <concept> <name> {` followed by its `args { ... }` block.
	mutationArgsRe = regexp.MustCompile(`(?s)mutate\s+\w+\s+(createDatabase|createIdentityProvider|createCluster)\s*\{.*?args\s*\{(.*?)\n  \}`)
	// One declared field: leading name at two-space indent, ignoring `///` docs.
	argNameRe = regexp.MustCompile(`(?m)^\s{4}([a-zA-Z][a-zA-Z0-9]*)\s+\S`)
	// `mutation <name> (` ... `)` inside the automation.
	callRe = regexp.MustCompile(`(?s)mutation\s+(createDatabase|createIdentityProvider|createCluster)\s*\((.*?)\n\s*\)`)
)

// mustReadTreeFile FAILS rather than returning "" on a missing file.
//
// runtime_mount_test.go has a same-named helper that swallows the error and
// returns an empty string, which is right for what it does and fatal for what
// this file does: every assertion below is driven by regex matches over the
// text, so an empty read makes this gate pass while examining nothing.
func mustReadTreeFile(t *testing.T, path string) string {
	t.Helper()
	f, err := dsl.Tree().Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	body, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

// Args the automation may legitimately omit, each with its reason.
//
// AN EXPLICIT MAP RATHER THAN A PATTERN, deliberately. The tempting rule is
// "exempt anything the mutation body defaults with `??`" -- and that rule
// would have waved through the exact bug this gate exists for: `port` is
// `args.port ?? 5432`, and stamping that literal while the DSN parse held the
// real value IS memql#4766. A default is what makes a dropped field silent,
// not what makes dropping it acceptable.
//
// So an omission has to be argued for, one entry at a time.
var bootstrapOmittedArgs = map[string]string{
	// cluster.status has the same defect as the two fields memql#4766
	// removed -- one writer, a constant, no refresher -- but it is a
	// different concept, it is outside that issue's scope, and unlike its
	// siblings it has a LIVE consumer: the OS Settings cluster panel renders
	// it as "Status", which means it currently shows an operator a hardcoded
	// verdict. Removing it is a change to that surface as well, so it is
	// tracked on its own (memql#4772) rather than folded in here.
	"createCluster.status": "memql#4772 -- frozen like its siblings, but has a live consumer and is tracked separately",
}

func TestBootstrapClusterForwardsEveryDeclaredField(t *testing.T) {
	// Guard the path itself: a rename that moved these files would otherwise
	// make this test vacuously pass on an empty match set.
	paths, err := dslfs.WalkMemqlFiles(dsl.Tree())
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}
	var haveMutations, haveAutomations bool
	for _, p := range paths {
		switch p {
		case "cluster/mutations.memql":
			haveMutations = true
		case "cluster/automations.memql":
			haveAutomations = true
		}
	}
	if !haveMutations || !haveAutomations {
		t.Fatalf("cluster/mutations.memql or cluster/automations.memql is gone; "+
			"this gate is now testing nothing (mutations=%v automations=%v)",
			haveMutations, haveAutomations)
	}

	mutationsSrc := mustReadTreeFile(t, "cluster/mutations.memql")
	automationSrc := mustReadTreeFile(t, "cluster/automations.memql")

	declared := map[string][]string{}
	for _, m := range mutationArgsRe.FindAllStringSubmatch(mutationsSrc, -1) {
		name, block := m[1], m[2]
		for _, a := range argNameRe.FindAllStringSubmatch(block, -1) {
			declared[name] = append(declared[name], a[1])
		}
	}
	if len(declared) != 3 {
		t.Fatalf("found arg blocks for %d of the 3 bootstrap mutations (%v) -- "+
			"the parser here has drifted from the file's shape, so it is not "+
			"checking what it claims to", len(declared), keysOf(declared))
	}

	passed := map[string]map[string]bool{}
	for _, c := range callRe.FindAllStringSubmatch(automationSrc, -1) {
		name, argsText := c[1], c[2]
		set := map[string]bool{}
		for _, line := range strings.Split(argsText, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "//") {
				continue
			}
			if colon := strings.Index(line, ":"); colon > 0 {
				set[strings.TrimSpace(line[:colon])] = true
			}
		}
		passed[name] = set
	}

	for mutation, args := range declared {
		got, ok := passed[mutation]
		if !ok {
			t.Errorf("bootstrapCluster never calls %s -- if that is deliberate, this gate "+
				"needs to say so; if not, the mutation has no caller at all", mutation)
			continue
		}
		for _, arg := range args {
			if got[arg] {
				continue
			}
			if reason, exempt := bootstrapOmittedArgs[mutation+"."+arg]; exempt {
				t.Logf("%s.%s omitted by exemption: %s", mutation, arg, reason)
				continue
			}
			t.Errorf("%s declares %q and bootstrapCluster does not pass it.\n"+
				"A field the only writer never sends is a field written by NOTHING, which is "+
				"the whole of memql#4766: `jwksUrl` and `acceptedAudiences` were computed in "+
				"app/cluster.go, sat in the startup payload, and were dropped right here. "+
				"Either forward it or remove it from the mutation -- a declared field with no "+
				"writer reads to every consumer as 'this cluster has no value for that'.",
				mutation, arg)
		}
	}
}

func keysOf(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
