package maketargets

import (
	"strings"
	"testing"
)

const sampleMakefile = `.PHONY: all
all: build

## Bring the cluster up
up:
	@bash scripts/k3d/bringup.sh

up-refresh:
	@bash scripts/k3d/bringup.sh --clean

install-deps:
	@bash scripts/dev/install-deps.sh

secrets:
	@bash scripts/k3d/seed-secrets.sh

%.o: %.c
	$(CC) -c $<

MEMQL_VERSION := 1.2.3
`

func TestTargetsParsesRealTargetsOnly(t *testing.T) {
	got := Targets(sampleMakefile)
	for _, want := range []string{"all", "up", "up-refresh", "install-deps", "secrets"} {
		if !got[want] {
			t.Errorf("target %q not parsed", want)
		}
	}
	// A pattern rule and a variable assignment are not things a human types
	// after `make`, and admitting either would let a citation "resolve"
	// against something unrunnable.
	for _, notTarget := range []string{"%.o", "MEMQL_VERSION"} {
		if got[notTarget] {
			t.Errorf("%q parsed as a target", notTarget)
		}
	}
	// `.PHONY:` is a dot target; the leading character class excludes it.
	if got[".PHONY"] {
		t.Error(".PHONY parsed as a target")
	}
}

func TestCitationsExtractsTargetAndLine(t *testing.T) {
	text := "line one\nrun `make up SERVERS=2` first\nthen `make install-deps && go test`\n"
	got := Citations(text)
	if len(got) != 2 {
		t.Fatalf("want 2 citations, got %d: %+v", len(got), got)
	}
	if got[0].Target != "up" || got[0].Line != 2 {
		t.Errorf("first citation = %+v, want target=up line=2", got[0])
	}
	// The target is the FIRST word: everything after it is arguments, and a
	// citation that carried them into the name would never match a target.
	if got[1].Target != "install-deps" || got[1].Line != 3 {
		t.Errorf("second citation = %+v, want target=install-deps line=3", got[1])
	}
}

func TestUnknownFindsTheDeadTargetAndClearsTheLiveOne(t *testing.T) {
	real := Targets(sampleMakefile)

	// POSITIVE CONTROL. Without this the negative below proves only that the
	// extractor found nothing, which is also what a broken extractor reports.
	if got := Unknown("seed it with `make secret-set NAME=X`", real); len(got) != 1 || got[0].Target != "secret-set" {
		t.Fatalf("the dead target was not reported: %+v", got)
	}
	if got := Unknown("locally `make secrets`", real); len(got) != 0 {
		t.Fatalf("a real target was reported as unknown: %+v", got)
	}
}

func TestFamilyReferenceNeedsAtLeastOneRealTargetWithThePrefix(t *testing.T) {
	real := Targets(sampleMakefile)

	// `make up-*` is satisfied: up-refresh exists.
	if got := Unknown("the `make up-*` family", real); len(got) != 0 {
		t.Errorf("a satisfied family reference was reported: %+v", got)
	}
	// `make secrets-*` is NOT satisfied -- this is the exact shape
	// docs/public/operate/env-vars.md carried, a whole documented workflow
	// over a prefix no target has ever had.
	got := Unknown("seeded via the `make secrets-*` workflow", real)
	if len(got) != 1 || got[0].Target != "secrets-" || !got[0].Family {
		t.Fatalf("the unsatisfied family reference was not reported: %+v", got)
	}
}

func TestUnknownTargetsIsSortedAndDeduplicated(t *testing.T) {
	real := Targets(sampleMakefile)
	text := "`make zzz` `make aaa` `make zzz`"
	got := UnknownTargets(text, real)
	if strings.Join(got, ",") != "aaa,zzz" {
		t.Errorf("UnknownTargets = %v, want [aaa zzz]", got)
	}
}

func TestBareMakeIsNotACitation(t *testing.T) {
	// `make` with no target names no command to check, and treating it as a
	// citation of the empty target would fail every doc that says "run
	// `make`".
	if got := Citations("just run `make` and wait"); len(got) != 0 {
		t.Errorf("bare `make` read as a citation: %+v", got)
	}
}
