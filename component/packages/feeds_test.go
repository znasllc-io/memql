package packages

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
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

// ---------------------------------------------------------------------------
// The poll under a personal credential (epic memql#4885, D10)
// ---------------------------------------------------------------------------

// recordingTransport is a fake GitHub for the poll: it records every request
// it is handed and answers the commits endpoint with one fixed sha. What the
// tests read off it is which requests were MADE and what bearer they carried
// -- and, for a credential that does not resolve, that none was made at all.
type recordingTransport struct {
	mu       sync.Mutex
	requests []*http.Request
	sha      string
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.requests = append(r.requests, req.Clone(context.Background()))
	r.mu.Unlock()
	body := fmt.Sprintf(`{"sha":%q}`, r.sha)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}, nil
}

func (r *recordingTransport) seen() []*http.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*http.Request, len(r.requests))
	copy(out, r.requests)
	return out
}

func credentialledPackage() map[string]any {
	pkg := trackedPackage("oldsha0000000000", "", false)
	pkg["ownerUserId"] = "v1:identity:user:someone"
	pkg["credentialId"] = "v1:platform:sourceCredential:acme"
	return pkg
}

// TestThePollRefusesACredentialThatDoesNotResolveBeforeAnyRequest is the
// feed half of D10's "resolution is owner-scoped by construction". A
// credential the package owner cannot read -- revoked, or somebody else's --
// is a REFUSAL of that package's poll: no request leaves the cluster, nothing
// is written, and the sweep goes on to the next package exactly as it does
// past an unreachable repository. Polling anonymously instead would answer
// 404 for a private repository, and the person whose credential was revoked
// would learn it from a stale cue rather than from the warning.
func TestThePollRefusesACredentialThatDoesNotResolveBeforeAnyRequest(t *testing.T) {
	i, engine := feedHarness(t, credentialledPackage())
	gh := &recordingTransport{sha: "newsha0000000000"}
	i.deps.HTTP = &http.Client{Transport: gh}

	var asked []string
	i.deps.Credentials = func(_ context.Context, credentialId, ownerUserId string) (ResolvedCredential, error) {
		asked = append(asked, credentialId+" as "+ownerUserId)
		return ResolvedCredential{}, refuse(CodeCredentialRevoked, "revoked in the test")
	}

	if _, err := i.handlePollUpstream(context.Background(), nil, 0); err != nil {
		t.Fatalf("one package whose credential refused must not stop the sweep: %v", err)
	}
	if got := gh.seen(); len(got) != 0 {
		t.Fatalf("a request left the cluster for a package whose credential did not resolve: %s", got[0].URL)
	}
	if engine.sawStatement("mutation ") {
		t.Fatalf("nothing may be written for a package whose credential refused; statements: %v", engine.statements())
	}
	// Resolved under the PACKAGE owner, by name, and exactly once.
	if len(asked) != 1 || asked[0] != "v1:platform:sourceCredential:acme as v1:identity:user:someone" {
		t.Fatalf("the resolver must be asked for the row's credential under the row's owner, got %v", asked)
	}

	// THE REACHABLE POSITIVE: the same package, a credential that resolves.
	// One request, carrying the bearer, and the version it answered recorded
	// -- so the silence above is about the refusal, not about the fake.
	i.deps.Credentials = func(_ context.Context, id, _ string) (ResolvedCredential, error) {
		return ResolvedCredential{Id: id, Kind: credentialKindToken, Bearer: "ghp_POLLTOKEN"}, nil
	}
	if _, err := i.handlePollUpstream(context.Background(), nil, 0); err != nil {
		t.Fatalf("poll: %v", err)
	}
	got := gh.seen()
	if len(got) != 1 {
		t.Fatalf("want exactly one request once the credential resolves, got %d", len(got))
	}
	if auth := got[0].Header.Get("Authorization"); auth != "Bearer ghp_POLLTOKEN" {
		t.Fatalf("the request must carry the resolved credential as its bearer, got %q", auth)
	}
	if !engine.sawStatement("mutation recordPackageUpstreamVersion") {
		t.Fatalf("the moved upstream must be recorded; statements: %v", engine.statements())
	}
	for _, q := range engine.statements() {
		if strings.Contains(q, "ghp_POLLTOKEN") {
			t.Fatalf("the token VALUE reached a row: %s", q)
		}
	}
}

// A public repository -- no credentialId -- is polled with NO bearer at all,
// and never consults the resolver. The control for the test above's bearer
// assertion: the header is set by the credential path and by nothing else.
func TestThePollSendsNoBearerForAPublicRepository(t *testing.T) {
	i, _ := feedHarness(t, trackedPackage("oldsha0000000000", "", false))
	gh := &recordingTransport{sha: "newsha0000000000"}
	i.deps.HTTP = &http.Client{Transport: gh}
	i.deps.Credentials = func(context.Context, string, string) (ResolvedCredential, error) {
		t.Fatal("a package naming no credential must not consult the resolver")
		return ResolvedCredential{}, nil
	}

	if _, err := i.handlePollUpstream(context.Background(), nil, 0); err != nil {
		t.Fatalf("poll: %v", err)
	}
	got := gh.seen()
	if len(got) != 1 {
		t.Fatalf("want one request, got %d", len(got))
	}
	if auth := got[0].Header.Get("Authorization"); auth != "" {
		t.Fatalf("a public repository is polled anonymously, got Authorization %q", auth)
	}
}

// A node with no resolver wired refuses a credentialled package rather than
// polling it anonymously -- the same direction the fetcher takes.
func TestThePollRefusesACredentialledPackageOnANodeThatCannotResolve(t *testing.T) {
	i, engine := feedHarness(t, credentialledPackage())
	gh := &recordingTransport{sha: "newsha0000000000"}
	i.deps.HTTP = &http.Client{Transport: gh}
	i.deps.Credentials = nil

	if _, err := i.handlePollUpstream(context.Background(), nil, 0); err != nil {
		t.Fatalf("the sweep itself must not fail: %v", err)
	}
	if len(gh.seen()) != 0 {
		t.Fatal("a node that cannot resolve credentials must not poll a credentialled package anonymously")
	}
	if engine.sawStatement("mutation ") {
		t.Fatal("nothing may be written for a package that was not polled")
	}
}
