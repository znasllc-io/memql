package dslconformance

import (
	"github.com/znasllc-io/memql/dsl"
	"io"
	"regexp"
	"strings"
	"testing"
)

// per_part_hashed_id_test.go -- memql#3009.
//
// component/memql's TestPerPartHashingClosesUtteranceIdAliasing proves the
// per-part derivation IS injective. This proves it is still WRITTEN. Revert
// the authored mutation to the separator form and that test stays green,
// which is the failure mode memql#3009 is itself an instance of -- a rule
// recorded somewhere and enforced nowhere.
//
// # The rule
//
// `hash(concat(a, sep, b))` keys two values into one string and is injective
// only if the split is recoverable. memql#2980 closed one instance by
// constraining the trailing part with `@pattern("^[^:]+$")`. That works where
// the part is drawn from a known set; it does not work where the part is
// nested in an untyped `object!` (nowhere to hang the annotation) or is a
// caller-chosen opaque string (banning a colon rejects "order:123" to work
// around an internal encoding choice).
//
// The general remedy is to remove the separator: hash each part, then hash the
// concatenation of the digests. hash() is sha256-hex, so every part renders to
// exactly 64 characters and a concatenation has exactly one decomposition.
//
// # Scope, stated honestly
//
// This checks the DSL files whose id derivations were converted, by path. It
// is NOT the tree-wide shape detector -- find every `id: hash(concat(...))`
// and verify its parts are constrained or digested -- which memql#3009 argues
// for and which triage explicitly scoped out as its own piece with its own
// false-positive design problem. A NEW file adopting the separator form will
// not trip this.
//
// `dsl/deployment/mutations.memql` is deliberately absent: its two sites keep
// the separator form and are safe by memql#2980's @pattern constraint, gated
// by TestCompositeHashedIdTrailingPartRejectsTheSeparator in this package.
// Two remedies coexist on purpose -- constrain the input where the input has a
// known shape, digest the parts where it does not.
var perPartHashedIdFiles = []string{
	"cognition/mutations.memql",
	"cognition/logic.memql",
	"cluster/mutations.memql",
}

// separatorInsideHashConcat matches a `":"` literal used as a concat separator
// inside a hash(concat(...)) call: a bare `, ":",` sequence.
//
// Deliberately narrow. A colon inside a string VALUE (`"v1:cluster:node"`) or
// in prose is not a separator, and flagging those would make the gate noisy
// enough to be suppressed -- which is how a rule stops being a rule.
var separatorInsideHashConcat = regexp.MustCompile(`,\s*":"\s*,`)

func TestConvertedIdDerivationsKeepPerPartHashing(t *testing.T) {
	var checked int
	for _, path := range perPartHashedIdFiles {
		fh, err := dsl.Tree().Open(path)
		if err != nil {
			t.Fatalf("open %s: %v", path, err)
		}
		raw, err := io.ReadAll(fh)
		fh.Close()
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		checked++

		for i, line := range strings.Split(string(raw), "\n") {
			// Comments explain the OLD form on purpose; the rule is about
			// authored expressions, not about prose describing them.
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if !separatorInsideHashConcat.MatchString(line) {
				continue
			}
			t.Errorf("%s:%d uses a `\":\"` separator inside a composite id derivation:\n  %s\n"+
				"memql#3009 converted these to per-part hashing -- hash each part, then hash the "+
				"concatenation of the fixed-width digests -- because the separator form aliases: "+
				"(\"chat\",\"k:1\") and (\"chat:k\",\"1\") derive one id, and two distinct actions "+
				"collapse onto one row. Neither part here can carry @pattern (they are nested in "+
				"an untyped object!) and constraining a caller-chosen idempotencyKey would be the "+
				"wrong remedy anyway.",
				path, i+1, strings.TrimSpace(line))
		}
	}

	// A path that stops resolving reads exactly like a compliant file.
	if checked != len(perPartHashedIdFiles) {
		t.Fatalf("checked %d of %d files -- the sweep has stopped resolving them and this gate "+
			"would now pass vacuously", checked, len(perPartHashedIdFiles))
	}
}

// The gate must actually fire, or the regex could be wrong in a way that
// matches nothing and the sweep above reports clean forever.
//
// It exercises separatorInsideHashConcat, the SAME variable the sweep uses.
func TestPerPartHashingGateMatchesASeparatorAndNotAValue(t *testing.T) {
	for _, bad := range []string{
		`          args.partitionId, ":",`,
		`      canonicalId(args.participantId, participant), ":",`,
		`      shortId(args.deploymentId), ":", args.nodeType`,
	} {
		if !separatorInsideHashConcat.MatchString(bad) {
			t.Errorf("the gate does not match a separator, so it would report clean forever: %q", bad)
		}
	}
	for _, ok := range []string{
		`          hash(args.partitionId),`,
		`    id: concat("utt-", hash(concat(`,
		`  filter  kind == "v1:cognition:utterance"`,       // a colon inside a VALUE
		`// was hash(concat(a, ":", b)) before memql#3009`, // prose (also skipped by the comment check)
		`      hash(canonicalId(args.participantId, participant)),`,
	} {
		if separatorInsideHashConcat.MatchString(ok) && !strings.HasPrefix(strings.TrimSpace(ok), "//") {
			t.Errorf("the gate fires on a legitimate line, which is how a gate gets suppressed "+
				"and then stops catching the real case: %q", ok)
		}
	}
}
