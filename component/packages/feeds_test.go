package packages

import (
	"context"
	"strings"
	"testing"
)

func feedHarness(t *testing.T, pkgs ...map[string]any) (*Integration, *recordingEngine) {
	t.Helper()
	engine := &recordingEngine{rows: map[string][]map[string]any{
		"query packagesByRepoUrl":     pkgs,
		"query packagesTrackingRepos": pkgs,
	}}
	i := NewIntegration(engine, discardLogger())
	// Resolve eagerly with a Deps that reaches no network: the feed's WRITE
	// path is what these tests are about, and a production fetcher would try
	// to dial GitHub.
	i.depsOnce.Do(func() {
		i.deps = &Deps{Store: &store{engine: engine}, Logger: discardLogger()}
	})
	return i, engine
}

const pushBody = `{"ref":"refs/heads/main","after":"newsha0000000000","repository":{"html_url":"https://github.com/acme/widget"}}`

func trackedPackage(deployed, known string, available bool) map[string]any {
	return map[string]any{
		"id":                 "v1:platform:package:abc",
		"repoUrl":            "https://github.com/acme/widget",
		"deployedVersion":    deployed,
		"latestKnownVersion": known,
		"updateAvailable":    available,
		"sourceKind":         "repo",
		"status":             "active",
	}
}

func TestAWebhookFlipsTheTwoFeedOwnedFields(t *testing.T) {
	i, engine := feedHarness(t, trackedPackage("oldsha0000000000", "", false))

	if _, err := i.handleNoteUpstreamFromWebhook(context.Background(), map[string]any{
		"inboundRequestId": "v1:platform:inboundRequest:1",
		"source":           "github",
		"body":             pushBody,
	}, 0); err != nil {
		t.Fatalf("webhook: %v", err)
	}

	if !engine.sawStatement("mutation recordPackageUpstreamVersion") {
		t.Fatalf("the feed must record the new version; statements: %v", engine.statements())
	}
	if !engine.sawStatement(`latestKnownVersion: "newsha0000000000", updateAvailable: true`) {
		t.Fatalf("both fields must move together; statements: %v", engine.statements())
	}
}

// TestNeitherFeedWritesAnythingElse is D11's whole safety property: the feeds
// touch two fields and start nothing.
func TestNeitherFeedWritesAnythingElseAndNeitherDeploys(t *testing.T) {
	i, engine := feedHarness(t, trackedPackage("oldsha0000000000", "", false))
	if _, err := i.handleNoteUpstreamFromWebhook(context.Background(), map[string]any{
		"source": "github", "body": pushBody,
	}, 0); err != nil {
		t.Fatalf("webhook: %v", err)
	}

	for _, q := range engine.statements() {
		if !strings.HasPrefix(q, "mutation ") {
			continue
		}
		if !strings.HasPrefix(q, "mutation recordPackageUpstreamVersion") {
			t.Errorf("a feed wrote something other than the two feed-owned fields: %s", q)
		}
	}
	if engine.sawStatement("openPackageDeployment") || engine.sawStatement("advancePackageDeployment") {
		t.Fatal("a feed must never create a deployment or start a stage -- deploying an update is a person's click")
	}
}

func TestAnUnchangedUpstreamWritesNothing(t *testing.T) {
	// Already known, already flagged. Writing again would broadcast a row
	// change and re-fire the OS arrival cue on what is effectively a
	// heartbeat -- "a heartbeat is not news".
	i, engine := feedHarness(t, trackedPackage("oldsha0000000000", "newsha0000000000", true))
	if _, err := i.handleNoteUpstreamFromWebhook(context.Background(), map[string]any{
		"source": "github", "body": pushBody,
	}, 0); err != nil {
		t.Fatalf("webhook: %v", err)
	}
	if engine.sawStatement("mutation recordPackageUpstreamVersion") {
		t.Fatalf("nothing changed, so nothing may be written; statements: %v", engine.statements())
	}

	// The reachable positive: the same call against a package that has NOT
	// seen this version does write, which TestAWebhookFlipsTheTwoFeedOwnedFields
	// pins -- so the silence above is about the comparison, not about the fake.
}

func TestADeliveryMatchingNoPackageIsANoOpNotAnError(t *testing.T) {
	i, engine := feedHarness(t) // no packages tracked
	res, err := i.handleNoteUpstreamFromWebhook(context.Background(), map[string]any{
		"source": "github", "body": pushBody,
	}, 0)
	if err != nil {
		t.Fatalf("a webhook about a repository nobody tracks is ordinary, not an error: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("want a result envelope")
	}
	if engine.sawStatement("mutation ") {
		t.Fatal("nothing to match means nothing to write")
	}
}

func TestADeliveryFromAnotherSourceIsSkipped(t *testing.T) {
	i, engine := feedHarness(t, trackedPackage("oldsha0000000000", "", false))
	if _, err := i.handleNoteUpstreamFromWebhook(context.Background(), map[string]any{
		"source": "stripe", "body": pushBody,
	}, 0); err != nil {
		t.Fatalf("skipping is not an error: %v", err)
	}
	if engine.sawStatement("mutation ") {
		t.Fatal("a delivery from another source must not be read as a package update")
	}
}

func TestABodyThisClusterCannotReadIsSkippedRatherThanFailed(t *testing.T) {
	i, _ := feedHarness(t, trackedPackage("oldsha0000000000", "", false))
	for _, body := range []string{
		`not json`,
		`{"repository":{"html_url":"https://github.com/acme/widget"}}`, // no version
		`{"after":"abc"}`, // no repository
	} {
		if _, err := i.handleNoteUpstreamFromWebhook(context.Background(), map[string]any{
			"source": "github", "body": body,
		}, 0); err != nil {
			t.Errorf("GitHub sends event types nobody models; %q must be skipped, not failed: %v", body, err)
		}
	}
}

func TestAReleaseIsIdentifiedByItsTag(t *testing.T) {
	// The version has to MIRROR what sourceVersion records, or the comparison
	// that lights the cue is between two different kinds of string and
	// updateAvailable is permanently true.
	ev, err := parseGitHubPush(`{"release":{"tag_name":"v1.4.0"},"after":"abc123","repository":{"html_url":"https://github.com/acme/widget"}}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.Version != "v1.4.0" {
		t.Fatalf("a release is its tag, got %q", ev.Version)
	}
}

func TestRepoUrlSpellingsCollapse(t *testing.T) {
	for _, raw := range []string{
		"https://github.com/acme/widget",
		"https://github.com/acme/widget/",
		"https://github.com/acme/widget.git",
	} {
		if got := normalizeRepoUrl(raw); got != "https://github.com/acme/widget" {
			t.Errorf("%q normalized to %q", raw, got)
		}
	}
}
