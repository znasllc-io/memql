package dslimports

import (
	"strings"
	"testing"
	"testing/fstest"
)

// lane3_import_suppression_2977_test.go -- memql#2977.
//
// Two halves, and they pull in opposite directions.
//
// # 1. The pinned-domain import must KEEP lane-3 coverage
//
// This is the regression guard for what memql#2945 fixed. Before that, adding
// `use cluster.concepts.{ deployment }` to the top of a pinned domain's
// mutations file dropped `createDeployment` and `updateDeploymentStatus` --
// two mutations the edit never touched -- out of insert/update field
// validation, because the import suppressed the same-domain fallback and then
// failed to resolve anything itself.
//
// #2945 made the import resolve (a `use` path's leading segment is a
// NAMESPACE, so it reaches every domain assembling under it, the pinned one
// included). That closed the symptom. Nothing pinned it, so this does: the
// fixture is the exact shape, and it fails against the pre-#2945 resolver.
//
// # 2. When a concept genuinely does not resolve, the skip must name the cause
//
// The suppression itself is deliberate and stays: an explicit import must win
// over an ambient same-domain match, or the two disagree about which
// declaration binds. What was wrong is that its REACH was invisible. The gate
// named two innocent mutations and said nothing about the import at the top of
// the file, so the failure pointed away from its own cause.

// pinnedDomainWithSiblingsTree is the memql#2977 shape.
//
// deploy/ is pinned to cluster. Its mutations file declares THREE mutations,
// all binding `deployment` -- the concept the file-top import names. That is
// the reported shape and the only one where the suppression reaches: it is
// keyed on the NAME, so every construct in the file binding that name loses
// its same-domain fallback, whether or not the author touched it.
//
// An earlier cut of this fixture gave the siblings a different concept. They
// were never at risk, so the test failed pre-#2945 on one mutation instead of
// three and its own comment about "untouched siblings" was not true of it.
func pinnedDomainWithSiblingsTree() fstest.MapFS {
	return fstest.MapFS{
		"deploy/namespace.pin": file("cluster\n"),
		"deploy/concepts.memql": file(`@version("1.0.0")
@namespace("cluster")
@description("Declared under deploy/, namespaced to cluster.")
concept deployment {
  status  string  @required @description("Status.")
}

@version("1.0.0")
@namespace("cluster")
@description("A second concept in the same domain, bound by the sibling mutations.")
concept deploymentNote {
  body  string  @required @description("Body.")
}`),
		// A real directory for the pin target, declaring something ELSE. This
		// is what made the pre-#2945 lane-1 reject the import rather than skip
		// it, and it is the half the earlier fixtures did not cover.
		"cluster/concepts.memql": file(`@version("1.0.0")
@namespace("cluster")
@description("The pin target exists but declares no deployment.")
concept gadget {
  label  string  @required @description("Label.")
}`),
		"other/concepts.memql": file(`@version("1.0.0")
@namespace("other")
@description("A second deployment so the bare name is ambiguous without the import.")
concept deployment {
  other  string  @required @description("Other.")
}`),
		"deploy/mutations.memql": file(`use cluster.concepts.{ deployment }

@enabled
@description("Binds the imported concept by its PINNED namespace.")
mutate deployment createDeployment {
  args {
    status  string  @required
  }
  insert {
    status: args.status
  }
}

@enabled
@description("A sibling binding the SAME concept, untouched by whoever added the import.")
mutate deployment updateDeploymentStatus {
  args {
    deploymentId  string  @required
    status        string  @required
  }
  update {
    id: args.deploymentId
    status: args.status
  }
}

@enabled
@description("A second such sibling.")
mutate deployment retireDeployment {
  args {
    deploymentId  string  @required
    status        string  @required
  }
  update {
    id: args.deploymentId
    status: args.status
  }
}`),
	}
}

// TestLane3_PinnedDomainImportKeepsFieldCoverage is the regression guard.
//
// Every mutation in the file must stay covered. The two siblings are the point:
// they bind a DIFFERENT concept and are untouched by the import, so if they go
// uncovered the suppression has reached past the construct that caused it --
// which is the memql#2977 report, and what memql#2945 made stop happening.
func TestLane3_PinnedDomainImportKeepsFieldCoverage(t *testing.T) {
	tree := loadTree(t, pinnedDomainWithSiblingsTree())

	total, skipped := tree.MutationFieldCoverage()
	if total != 3 {
		t.Fatalf("expected 3 mutations in the fixture, counted %d -- the probe is broken, not the tree", total)
	}
	if len(skipped) != 0 {
		t.Errorf("lane 3 skipped %d of %d mutations in a pinned domain carrying a pinned-namespace "+
			"import.\nThe import resolves (memql#2945: a `use` path's leading segment is a "+
			"NAMESPACE, so it reaches every domain assembling under it), so nothing here should "+
			"fall out of insert/update field validation. The suppression is keyed on the NAME, so "+
			"a regression takes every mutation in the file with it -- including the ones whoever "+
			"added the import never touched (memql#2977).\n  %s",
			len(skipped), total, strings.Join(skipped, "\n  "))
	}
}

// TestLane3_SkipNamesTheImportAsTheCause is the diagnosability half.
//
// Here the import names a concept that resolves NOWHERE, so the suppression
// genuinely costs coverage and the gate genuinely should report it. What it
// must not do is report only the constructs -- the author needs to know an
// import at the top of the file is why, because the mutation named may be one
// they never edited.
func TestLane3_SkipNamesTheImportAsTheCause(t *testing.T) {
	root := pinnedDomainWithSiblingsTree()
	// Import a name nothing declares, under a namespace the root DOES own, so
	// the external-namespace escape does not apply.
	root["deploy/mutations.memql"] = file(`use cluster.concepts.{ deployment }

@enabled
@description("Binds a concept that is declared nowhere.")
mutate deployment createDeployment {
  args {
    status  string  @required
  }
  insert {
    status: args.status
  }
}`)
	root["deploy/concepts.memql"] = file(`@version("1.0.0")
@namespace("cluster")
@description("No deployment concept anywhere now.")
concept deploymentNote {
  body  string  @required @description("Body.")
}`)
	delete(root, "other/concepts.memql")

	tree := loadTree(t, root)
	_, skipped := tree.MutationFieldCoverage()
	if len(skipped) == 0 {
		t.Fatalf("nothing skipped in this fixture -- the resolver reaches the concept by some path " +
			"this test did not anticipate, so there is no message to assert on")
	}

	got := strings.Join(skipped, "\n")
	for _, want := range []string{
		"use cluster.concepts", // the import, by its path
		"deployment",           // the name it suppresses
		"WHOLE FILE",           // the reach, which is the non-obvious part
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the lane-3 skip message does not mention %q.\nAn explicit import suppresses "+
				"the same-domain fallback for that name across the whole file, so a skip can name "+
				"a construct the author never edited. Without the cause the message points away "+
				"from its own reason (memql#2977).\n  got: %s", want, got)
		}
	}
}
