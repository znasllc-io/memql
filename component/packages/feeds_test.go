package packages

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
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

const pushBody = `{"ref":"refs/heads/main","after":"newsha0000000000","repository":{"html_url":"https://github.com/acme/widget","default_branch":"main"}}`

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

// AN ARCHIVED SOURCE IS NOT FED (memql#5293). `packagesByRepoUrl` carried
// no status filter, so a webhook -- and the poll, which reaches the same
// read per repository -- went on writing the two feed-owned fields onto a
// source somebody had archived, and an armed one would have auto-deployed.
// The read excludes archived packages now, exactly as packagesTrackingRepos
// does; and the feed checks the status it was handed as well, because the
// fake here answers whatever it is given and a test over the read alone
// would pass on an empty table.
func TestAnArchivedPackageIsNotWrittenByTheFeed(t *testing.T) {
	archived := trackedPackage("oldsha0000000000", "", false)
	archived["status"] = "archived"
	i, engine := feedHarness(t, archived)
	if _, err := i.handleNoteUpstreamFromWebhook(context.Background(), map[string]any{
		"source": "github", "body": pushBody,
	}, 0); err != nil {
		t.Fatalf("webhook: %v", err)
	}
	if engine.sawStatement("mutation ") {
		t.Fatalf("an archived package must not be written by the feed; statements: %v", engine.statements())
	}
	// The reachable positive is TestAWebhookFlipsTheTwoFeedOwnedFields: the
	// same delivery against the same package at `active` writes.
}

var packagesByRepoUrlBlock = regexp.MustCompile(`(?s)query package packagesByRepoUrl \{(.*?)\n\}`)

// The read half, held to the file: the DSL query the feed reads through
// excludes archived packages by the same trait packagesTrackingRepos uses.
func TestPackagesByRepoUrlExcludesArchivedPackages(t *testing.T) {
	src, err := os.ReadFile("../../dsl/platform/queries.memql")
	if err != nil {
		t.Fatalf("read queries.memql: %v", err)
	}
	m := packagesByRepoUrlBlock.FindSubmatch(src)
	if m == nil {
		t.Fatal("packagesByRepoUrl not found in dsl/platform/queries.memql")
	}
	if !strings.Contains(string(m[1]), "statusIsActive") {
		t.Fatalf("packagesByRepoUrl must filter on statusIsActive, or the upstream feed keeps writing to archived packages:\n%s", m[1])
	}
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
		`{"repository":{"html_url":"https://github.com/acme/widget","default_branch":"main"}}`, // no version
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
	ev, err := parseGitHubPush(`{"release":{"tag_name":"v1.4.0"},"after":"abc123","repository":{"html_url":"https://github.com/acme/widget","default_branch":"main"}}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.Version != "v1.4.0" {
		t.Fatalf("a release is its tag, got %q", ev.Version)
	}
	if ev.Ref != "refs/tags/v1.4.0" {
		t.Fatalf("a release names its tag's ref, got %q", ev.Ref)
	}
}

// ---------------------------------------------------------------------------
// Only a push or a release is an upstream event (epic memql#5477, Review
// Focus 2)
// ---------------------------------------------------------------------------

const widgetRepository = `"repository":{"html_url":"https://github.com/acme/widget","default_branch":"main","full_name":"acme/widget"}`

// releaseBody is a release published on the same source, as GitHub sends it.
const releaseBody = `{"action":"published","release":{"tag_name":"v1.4.0","target_commitish":"main"},` +
	widgetRepository + `,"installation":{"id":42}}`

// notAPushBodies are deliveries the GitHub App sends on the same source once it
// subscribes to pipelines' events. Every one names the repository a push names
// and a SHA somewhere -- the pull request's synchronize carries `before` and
// `after` at the top, exactly where a push does -- which is why the feed must
// read the body's shape rather than hunt for a version.
var notAPushBodies = map[string]string{
	"pull_request synchronize": `{"action":"synchronize","number":7,"before":"basesha00000000","after":"prheadsha0000000",` +
		`"pull_request":{"number":7,"head":{"sha":"prheadsha0000000","ref":"feature","repo":{"full_name":"acme/widget"}},"base":{"sha":"basesha00000000","ref":"main"}},` +
		widgetRepository + `,"installation":{"id":42}}`,
	"check_suite requested": `{"action":"requested","check_suite":{"id":5,"head_branch":"main","head_sha":"suitesha0000000","before":"basesha00000000","after":"suitesha0000000"},` +
		widgetRepository + `,"installation":{"id":42}}`,
	"check_run rerequested": `{"action":"rerequested","check_run":{"id":9,"head_sha":"runsha000000000","external_id":"v1:pipelines:run:abc"},` +
		widgetRepository + `,"installation":{"id":42}}`,
	"merge_group checks_requested": `{"action":"checks_requested","merge_group":{"head_sha":"queuesha0000000","head_ref":"refs/heads/gh-readonly-queue/main/pr-7-basesha","base_sha":"basesha00000000","base_ref":"refs/heads/main"},` +
		widgetRepository + `,"installation":{"id":42}}`,
	// A push-shaped body with no ref and no release tag: today that skipped
	// the branch filter and noted its SHA on every package tracking the
	// repository.
	"a body naming no ref and no release": `{"after":"newsha0000000000",` + widgetRepository + `}`,
}

func TestOnlyAPushOrAReleaseIsReadAsAnUpstreamEvent(t *testing.T) {
	for name, body := range notAPushBodies {
		t.Run(name, func(t *testing.T) {
			if _, err := parseGitHubPush(body); !errors.Is(err, errNotAPush) {
				t.Fatalf("want errNotAPush, got %v", err)
			}
		})
	}

	// THE REACHABLE POSITIVE: a push to a branch, a push of a tag and a
	// release are upstream events. A push carries the commit SHA and a release
	// its tag, each mirroring what sourceVersion records for that ref -- a
	// version of another kind would make the comparison that lights the cue
	// permanently true.
	for name, tc := range map[string]struct{ body, ref, version string }{
		"a push to a branch": {pushBody, "refs/heads/main", "newsha0000000000"},
		"a push of a tag": {`{"ref":"refs/tags/v1.4.0","before":"0000000000000000","after":"tagsha0000000000",` + widgetRepository + `}`,
			"refs/tags/v1.4.0", "tagsha0000000000"},
		"a release": {releaseBody, "refs/tags/v1.4.0", "v1.4.0"},
	} {
		t.Run(name, func(t *testing.T) {
			ev, err := parseGitHubPush(tc.body)
			if err != nil {
				t.Fatalf("an upstream event must parse: %v", err)
			}
			if ev.Ref != tc.ref || ev.Version != tc.version || ev.RepoUrl != "https://github.com/acme/widget" {
				t.Fatalf("got %+v", ev)
			}
		})
	}
}

// TestAReleaseDeliveryStillNotesItsTag: the app now delivers releases for
// pipelines too, and that changes nothing here. A source tracking the tag
// learns of it, exactly as before pipelines existed.
func TestAReleaseDeliveryStillNotesItsTag(t *testing.T) {
	pkg := trackedPackage("v1.3.0", "", false)
	pkg["repoRef"] = "v1.4.0"
	i, engine := feedHarness(t, pkg)
	if _, err := i.handleNoteUpstreamFromWebhook(context.Background(), map[string]any{
		"inboundRequestId": "v1:platform:inboundRequest:1", "source": "github", "body": releaseBody,
	}, 0); err != nil {
		t.Fatalf("release: %v", err)
	}
	if !engine.sawStatement(`mutation recordPackageUpstreamVersion(packageId: "v1:platform:package:abc", latestKnownVersion: "v1.4.0", updateAvailable: true)`) {
		t.Fatalf("a release must note its tag; statements: %v", engine.statements())
	}
}

// TestADeliveryThatIsNotAPushMovesNothingAndStartsNothing is Review Focus 2:
// a pull request's synchronize and a check suite -- and the merge queue and
// check run deliveries the app now receives beside them -- must not move the
// update cue (latestKnownVersion) and must not start an auto-deploy, against a
// package armed to start one. Each is skipped without an error: it is
// somebody else's delivery, not a fault.
func TestADeliveryThatIsNotAPushMovesNothingAndStartsNothing(t *testing.T) {
	armed := func(t *testing.T) (*Integration, *harness) {
		t.Helper()
		pkg := autoPackage()
		h := newHarness(t, spaOnlyPackage(), pkg)
		h.engine.rows["query packagesByRepoUrl"] = []map[string]any{pkg}
		i := NewIntegration(h.engine, discardLogger())
		i.depsOnce.Do(func() { i.deps = h.deps })
		return i, h
	}

	for name, body := range notAPushBodies {
		t.Run(name, func(t *testing.T) {
			i, h := armed(t)
			res, err := i.handleNoteUpstreamFromWebhook(context.Background(), map[string]any{
				"inboundRequestId": "v1:platform:inboundRequest:1", "source": "github", "body": body,
			}, 0)
			if err != nil {
				t.Fatalf("another event's delivery is skipped, never failed: %v", err)
			}
			if len(res) == 0 {
				t.Fatal("want a result envelope saying the delivery was skipped")
			}
			if h.engine.sawStatement("query packagesByRepoUrl") {
				t.Fatalf("a delivery that is not a push must not even look for packages; statements: %v", h.engine.statements())
			}
			if h.engine.sawStatement("mutation ") {
				t.Fatalf("a delivery that is not a push wrote something; statements: %v", h.engine.statements())
			}
		})
	}

	// THE REACHABLE POSITIVE: the same armed package, a real push. The cue
	// moves and the auto-run opens, so the silence above is about the
	// delivery and not about a harness that could never write.
	i, h := armed(t)
	if _, err := i.handleNoteUpstreamFromWebhook(context.Background(), map[string]any{
		"inboundRequestId": "v1:platform:inboundRequest:1", "source": "github", "body": pushBody,
	}, 0); err != nil {
		t.Fatalf("push: %v", err)
	}
	if !h.engine.sawStatement(`mutation recordPackageUpstreamVersion(packageId: "v1:platform:package:abc", latestKnownVersion: "newsha0000000000"`) {
		t.Fatalf("a push must move the cue; statements: %v", h.engine.statements())
	}
	if !h.engine.sawStatement("mutation openPackageDeployment") {
		t.Fatalf("a push to an armed source must start its auto-run; statements: %v", h.engine.statements())
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
