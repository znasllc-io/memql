package ast

import "testing"

// SplitTriggerTopic is declared as the inverse of BuildTriggerTopic, so the
// property that matters is the ROUND TRIP -- not a table of strings somebody
// transcribed by hand, which would pass just as happily if both functions
// agreed on the wrong format.
//
// It is driven over the real closed set (AllowedEventKinds) rather than a
// literal list, so adding a fourth kind exercises it automatically instead of
// leaving a gap the test cannot see.
func TestSplitTriggerTopicRoundTripsBuildTriggerTopic(t *testing.T) {
	concepts := []string{
		"",                         // concept-less: matches any concept
		"v1:cognition:participant", // the ordinary case
		"v1:memql:backend:user",    // four segments, to prove nothing splits on ':'
	}
	for _, kind := range AllowedEventKinds() {
		for _, concept := range concepts {
			topic, err := BuildTriggerTopic(kind, concept)
			if err != nil {
				t.Fatalf("BuildTriggerTopic(%q, %q): %v", kind, concept, err)
			}
			gotKind, gotConcept, ok := SplitTriggerTopic(topic)
			if !ok {
				t.Errorf("SplitTriggerTopic(%q) did not recognise a topic BuildTriggerTopic just produced", topic)
				continue
			}
			if gotKind != kind || gotConcept != concept {
				t.Errorf("round trip of (%q, %q) via %q returned (%q, %q)", kind, concept, topic, gotKind, gotConcept)
			}
		}
	}
}

// Everything the function must REFUSE, and the reason each one is here.
//
// The refusals matter as much as the successes: a false positive would hand
// the run form a concept id assembled out of an application topic's segments,
// and the row picker would browse a concept that does not exist.
func TestSplitTriggerTopicRefusesWhatItCannotDecompose(t *testing.T) {
	for _, tc := range []struct {
		topic string
		why   string
	}{
		{"system.startup", "an application topic, not a graph one"},
		{"automation.invocation.someName", "the synthetic manual-run topic"},
		{"", "empty"},
		{"graph.", "the prefix alone"},
		{"graph.node.archived.v1:x:y", "an action outside the closed set -- a node.archived kind does not exist"},
		{"graph.node.createdish", "a kind that merely starts with one of the three"},
		{"node.created.v1:x:y", "no graph. prefix"},
	} {
		if kind, concept, ok := SplitTriggerTopic(tc.topic); ok {
			t.Errorf("SplitTriggerTopic(%q) decomposed to (%q, %q) but should have refused: %s",
				tc.topic, kind, concept, tc.why)
		}
	}
}

// A glob topic decomposes only when its ACTION is concrete. `graph.node.*` is
// refused because `node.*` is not one of the three kinds; `graph.node.created.*`
// succeeds and reports `*` as the concept, which is the honest reading -- the
// author wrote a wildcard concept and the form has no row set to browse, which
// automationFormPlan already handles by falling through to the JSON mode.
func TestSplitTriggerTopicOnGlobs(t *testing.T) {
	if _, _, ok := SplitTriggerTopic("graph.node.*"); ok {
		t.Error(`SplitTriggerTopic("graph.node.*") should refuse: "node.*" is not one of the three event kinds`)
	}
	kind, concept, ok := SplitTriggerTopic("graph.node.created.*")
	if !ok || kind != "node.created" || concept != "*" {
		t.Errorf(`SplitTriggerTopic("graph.node.created.*") = (%q, %q, %v), want ("node.created", "*", true)`, kind, concept, ok)
	}
}
