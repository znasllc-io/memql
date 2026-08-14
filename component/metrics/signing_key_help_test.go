package metrics

import (
	"strings"
	"testing"
)

// signing_key_help_test.go -- memql#3804.
//
// THE DEFECT WAS A CHANNEL MISMATCH, not a missing thought. init() already
// said, correctly and on purpose, that the three signing-key series are
// written only by the identity binary, that every other node exports them at a
// constant 0 because it holds no signing key, and that "every alert over them
// must select app=\"identity\"".
//
// But that sentence is a Go comment. The operator writing the alerting rule is
// reading the SCRAPE, where Help is the only prose that exists -- and
// signing_key_age_known's Help said "Alert on 0: key age is UNOBSERVABLE, not
// zero" with no scope at all. So the one audience that needed the qualifier was
// the one audience structurally unable to see it, and a rule written from the
// sentence they COULD see fires forever on bff, cognition, agent, planner,
// workbench, mcp and edge -- burying the single condition the metric exists to
// surface under seven node types' worth of permanent noise.
//
// Found by reading /metrics on a live cluster during memql#3784: bff reported
// signing_key_age_known 0 while identity reported 1 with a real timestamp. Both
// were correct; only the Help was wrong about what to do with them.
//
// This test holds the two channels together. Help is a shipped artifact -- it
// travels in every scrape and lands in dashboards -- so it is the copy that must
// carry the scope, and it must not lose it to a later reword.

// scopedSigningKeySeries are the series written only by the identity binary, so
// only meaningful when selected to it.
var scopedSigningKeySeries = map[string]string{
	"memql_identity_signing_key_created_timestamp_seconds": "identitySigningKeyCreatedTimestamp",
	"memql_identity_signing_key_age_known":                 "identitySigningKeyAgeKnown",
	"memql_identity_signing_key_rotation_supported":        "identitySigningKeyRotationSupported",
}

func TestSigningKeyHelpNamesTheIdentityScope(t *testing.T) {
	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather registry: %v", err)
	}

	seen := map[string]bool{}
	for _, mf := range families {
		name := mf.GetName()
		if _, scoped := scopedSigningKeySeries[name]; !scoped {
			continue
		}
		seen[name] = true

		help := mf.GetHelp()
		if !strings.Contains(help, `app="identity"`) {
			t.Errorf("%s: Help does not name the app=\"identity\" scope.\n"+
				"Every node type exports this series, and every node that is not identity "+
				"exports it at a constant 0 -- so an alert written from this Help without the "+
				"selector fires forever on most of the mesh. init() has said so since before "+
				"memql#3804, but a Go comment does not travel in a scrape; Help does.\n"+
				"Help was: %s", name, help)
		}
		// The stronger half. A bare mention of the scope somewhere in the
		// sentence is not enough if the imperative is still unqualified: "Alert
		// on 0" is the instruction an operator acts on, and it is exactly the
		// one that must carry the selector.
		if idx := strings.Index(help, "Alert on 0"); idx >= 0 {
			rest := help[idx:]
			if !strings.HasPrefix(rest, `Alert on 0 WHERE app="identity"`) {
				t.Errorf("%s: Help still gives an UNQUALIFIED \"Alert on 0\" instruction.\n"+
					"That is the sentence an operator turns into a rule, so the selector has to "+
					"be in it rather than elsewhere in the paragraph.\nHelp was: %s", name, help)
			}
		}
	}

	for name := range scopedSigningKeySeries {
		if !seen[name] {
			t.Errorf("%s was not found in the registry, so its Help was never checked. "+
				"Either the series was renamed or dropped -- update scopedSigningKeySeries -- "+
				"or this test is silently measuring nothing.", name)
		}
	}
}
