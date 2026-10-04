package pipelines

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The SHAs the fixtures under testdata/github carry.
const (
	shaMain      = "772c6a1d531d6f34ca4bc594a70987215f3ce1c8" // the default branch before the merge
	shaPROpened  = "c5fa05f142af4f7c6bd9b18c24924ebed165a4ab" // pull request #42 as opened
	shaPRPushed  = "4d3a8f9483a084a44043bd93b16a4b4414d0c3cc" // #42 after its next push
	shaForkHead  = "466b2bf6caf80eca5c375e0ea27c0687ad2b74d1" // a stranger's head
	shaQueueHead = "b19c6b22ab1f138a3a69ed266fc59fb07c59f5ad" // the merge queue's temporary branch
	shaMerged    = "a944ae33e9bcd7b2f5d1e5d74dcd8acc6a0ae9df" // the default branch after the merge
)

const fixtureInstallation = 51234567

func readDelivery(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "github", name))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// One case per recorded delivery. The header is what GitHub sends beside each
// body; the classification must come from the body either way.
func TestClassifyDeliveryReadsEachRecordedDelivery(t *testing.T) {
	cases := []struct {
		fixture string
		header  string
		want    Trigger
		ignored string
	}{
		{
			fixture: "pull_request_opened.json", header: "pull_request",
			want: Trigger{
				Event: EventPullRequest, Repository: "acme-corp/storefront", DefaultBranch: "main",
				InstallationID: fixtureInstallation, SHA: shaPROpened, BaseSHA: shaMain,
				Branch: "cart-badge", PullRequest: 42, HeadRepository: "Acme-Corp/Storefront",
				Title: "Show the cart count on the badge",
			},
		},
		{
			// Top-level before/after and no ref: a push parser that only looked
			// for before/after would read this as a push (Review Focus 2).
			fixture: "pull_request_synchronize.json", header: "pull_request",
			want: Trigger{
				Event: EventPullRequest, Repository: "acme-corp/storefront", DefaultBranch: "main",
				InstallationID: fixtureInstallation, SHA: shaPRPushed, BaseSHA: shaMain,
				Branch: "cart-badge", PullRequest: 42, HeadRepository: "Acme-Corp/Storefront",
				Title: "Show the cart count on the badge",
			},
		},
		{fixture: "pull_request_closed.json", header: "pull_request", ignored: "pull request closed"},
		{
			fixture: "pull_request_fork.json", header: "pull_request",
			want: Trigger{
				Event: EventPullRequest, Repository: "acme-corp/storefront", DefaultBranch: "main",
				InstallationID: fixtureInstallation, SHA: shaForkHead, BaseSHA: shaMain,
				Branch: "main", PullRequest: 43, Fork: true, HeadRepository: "stranger/Storefront",
				Title: "Fix a typo in the README",
			},
		},
		{
			// A fork deleted after the pull request opened: GitHub sends the
			// head repository as null. Nobody can vouch for that head either.
			fixture: "pull_request_deleted_fork.json", header: "pull_request",
			want: Trigger{
				Event: EventPullRequest, Repository: "acme-corp/storefront", DefaultBranch: "main",
				InstallationID: fixtureInstallation, SHA: shaForkHead, BaseSHA: shaMain,
				Branch: "patch-1", PullRequest: 44, Fork: true,
				Title: "Update the checkout copy",
			},
		},
		{
			fixture: "merge_group_checks_requested.json", header: "merge_group",
			want: Trigger{
				Event: EventMergeGroup, Repository: "acme-corp/storefront", DefaultBranch: "main",
				InstallationID: fixtureInstallation, SHA: shaQueueHead, BaseSHA: shaMain,
				Branch: "gh-readonly-queue/main/pr-42-" + shaMain,
				Title:  "Merge pull request #42 from Acme-Corp/cart-badge",
			},
		},
		{
			fixture: "push_default.json", header: "push",
			want: Trigger{
				Event: EventPush, Repository: "acme-corp/storefront", DefaultBranch: "main",
				InstallationID: fixtureInstallation, SHA: shaMerged, BaseSHA: shaMain,
				Branch: "main", Title: "Show the cart count on the badge (#42)",
			},
		},
		{fixture: "push_other_branch.json", header: "push", ignored: "push to cart-badge, not the default branch main"},
		{fixture: "push_tag.json", header: "push", ignored: "tag v1.4.0 pushed"},
		{fixture: "push_delete.json", header: "push", ignored: "branch cart-badge deleted"},
		{
			fixture: "check_run_rerequested.json", header: "check_run",
			want: Trigger{
				Rerequest: true, CheckRunID: 30431907812, Repository: "acme-corp/storefront",
				DefaultBranch: "main", InstallationID: fixtureInstallation, SHA: shaPRPushed,
			},
		},
		{
			fixture: "check_suite_rerequested.json", header: "check_suite",
			want: Trigger{
				Rerequest: true, Repository: "acme-corp/storefront", DefaultBranch: "main",
				InstallationID: fixtureInstallation, SHA: shaPRPushed,
			},
		},
		// Arrives on every push once the app holds checks: write.
		{fixture: "check_suite_requested.json", header: "check_suite", ignored: "check suite requested"},
		{
			fixture: "release_published.json", header: "release",
			want: Trigger{
				Event: EventRelease, Repository: "acme-corp/storefront", DefaultBranch: "main",
				InstallationID: fixtureInstallation, ReleaseTag: "v1.4.0", Title: "Storefront 1.4",
			},
		},
		{fixture: "ping.json", header: "ping", ignored: "not a delivery pipelines run on"},
	}

	seen := map[string]bool{}
	for _, c := range cases {
		seen[c.fixture] = true
		t.Run(strings.TrimSuffix(c.fixture, ".json"), func(t *testing.T) {
			body := readDelivery(t, c.fixture)
			// The header is a cross-check only: with it and without it, the
			// body says the same thing.
			for _, header := range []string{c.header, ""} {
				got, ignored, err := ClassifyDelivery(header, body)
				if err != nil {
					t.Fatalf("header %q: error %v", header, err)
				}
				if c.ignored != "" {
					if ignored == nil {
						t.Fatalf("header %q: classified as %+v, want ignored %q", header, got, c.ignored)
					}
					if ignored.Reason != c.ignored {
						t.Errorf("header %q: ignored because %q, want %q", header, ignored.Reason, c.ignored)
					}
					if got != (Trigger{}) {
						t.Errorf("header %q: an ignored delivery also returned a trigger %+v", header, got)
					}
					continue
				}
				if ignored != nil {
					t.Fatalf("header %q: ignored (%q), want %+v", header, ignored.Reason, c.want)
				}
				if got != c.want {
					t.Errorf("header %q:\n got %+v\nwant %+v", header, got, c.want)
				}
			}
		})
	}

	// Every recorded delivery is exercised: a fixture nobody reads is a case
	// somebody believes is covered.
	entries, err := os.ReadDir(filepath.Join("testdata", "github"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !seen[e.Name()] {
			t.Errorf("testdata/github/%s has no case in this test", e.Name())
		}
	}
}

// Decision 4: X-GitHub-Event is not signed, the body is. A header that names
// another event than the body is a delivery nobody can vouch for.
func TestClassifyDeliveryIgnoresAHeaderTheBodyContradicts(t *testing.T) {
	got, ignored, err := ClassifyDelivery("push", readDelivery(t, "pull_request_opened.json"))
	if err != nil {
		t.Fatal(err)
	}
	if ignored == nil {
		t.Fatalf("a pull_request body under X-GitHub-Event: push was classified as %+v", got)
	}
	if want := "header says push, body is pull_request"; ignored.Reason != want {
		t.Errorf("reason = %q, want %q", ignored.Reason, want)
	}
}

func TestClassifyDeliveryRefusesAMalformedSHA(t *testing.T) {
	cases := []struct{ name, fixture, old, new string }{
		{"short head", "pull_request_opened.json", shaPROpened, "c5fa05f"},
		{"non-hex head", "push_default.json", `"after": "` + shaMerged, `"after": "` + strings.Repeat("z", 40)},
		{"malformed base", "merge_group_checks_requested.json", `"base_sha": "` + shaMain, `"base_sha": "` + shaMain + "0"},
		{"missing head", "check_suite_rerequested.json", `"head_sha": "` + shaPRPushed + `",`, ``},
	}
	for _, c := range cases {
		original := string(readDelivery(t, c.fixture))
		body := strings.Replace(original, c.old, c.new, 1)
		if body == original {
			t.Fatalf("%s: the replacement matched nothing, so this case would test the unmodified fixture", c.name)
		}
		got, ignored, err := ClassifyDelivery("", []byte(body))
		if err == nil {
			t.Errorf("%s: classified as (%+v, %+v), want an error", c.name, got, ignored)
		}
	}
}

// A SHA is one value however it is spelled, so the run key a webhook and a
// poll compute for one head agree.
func TestClassifyDeliveryLowerCasesTheSHA(t *testing.T) {
	body := strings.Replace(string(readDelivery(t, "pull_request_opened.json")),
		shaPROpened, strings.ToUpper(shaPROpened), 1)
	got, ignored, err := ClassifyDelivery("pull_request", []byte(body))
	if err != nil || ignored != nil {
		t.Fatalf("got (%+v, %+v, %v)", got, ignored, err)
	}
	if got.SHA != shaPROpened {
		t.Errorf("SHA = %q, want %q", got.SHA, shaPROpened)
	}
}

func TestClassifyDeliveryRefusesADeliveryWithNoRepositoryOrInstallation(t *testing.T) {
	opened := string(readDelivery(t, "pull_request_opened.json"))
	cases := map[string]string{
		"no repository":   strings.Replace(opened, `"repository": { "full_name": "Acme-Corp/Storefront", "default_branch": "main", "private": false },`, ``, 1),
		"no installation": strings.Replace(opened, `"installation": { "id": 51234567 },`, ``, 1),
		"not an object":   `["pull_request"]`,
		"not json":        `{"pull_request": `,
	}
	for name, body := range cases {
		if body == opened {
			t.Fatalf("%s: the replacement matched nothing, so this case would test the unmodified fixture", name)
		}
		got, ignored, err := ClassifyDelivery("pull_request", []byte(body))
		if err == nil {
			t.Errorf("%s: classified as (%+v, %+v), want an error", name, got, ignored)
		}
	}
}

// D6: the head of a pull request from another repository is a stranger's
// code. Both shapes GitHub sends are forks; the same repository under another
// spelling is not.
func TestClassifyDeliveryDetectsAFork(t *testing.T) {
	for _, fixture := range []string{"pull_request_fork.json", "pull_request_deleted_fork.json"} {
		got, _, err := ClassifyDelivery("pull_request", readDelivery(t, fixture))
		if err != nil {
			t.Fatal(err)
		}
		if !got.Fork {
			t.Errorf("%s: Fork = false for a head in another repository", fixture)
		}
	}
	sameRepo := strings.Replace(string(readDelivery(t, "pull_request_opened.json")),
		`"sha": "`+shaPROpened+`",
      "repo": { "full_name": "Acme-Corp/Storefront" }`,
		`"sha": "`+shaPROpened+`",
      "repo": { "full_name": "acme-corp/STOREFRONT" }`, 1)
	got, _, err := ClassifyDelivery("pull_request", []byte(sameRepo))
	if err != nil {
		t.Fatal(err)
	}
	if got.HeadRepository != "acme-corp/STOREFRONT" {
		t.Fatalf("the replacement did not reach the head repository (HeadRepository = %q)", got.HeadRepository)
	}
	if got.Fork {
		t.Error("Fork = true for the base repository spelled in another case")
	}
}

// The deletion rule, on its own: a zero `after` on the DEFAULT branch, where
// the branch rule would otherwise let it through.
func TestClassifyDeliveryIgnoresADeletionOfTheDefaultBranch(t *testing.T) {
	body := strings.Replace(string(readDelivery(t, "push_default.json")),
		`"after": "`+shaMerged, `"after": "`+zeroSHA, 1)
	got, ignored, err := ClassifyDelivery("push", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if ignored == nil || ignored.Reason != "branch main deleted" {
		t.Errorf("got (%+v, %+v), want ignored %q", got, ignored, "branch main deleted")
	}
}

// A new default branch has no parent to diff against: its base is unknown,
// not the zero SHA.
func TestClassifyDeliveryLeavesTheBaseOfANewBranchEmpty(t *testing.T) {
	body := strings.Replace(string(readDelivery(t, "push_default.json")),
		`"before": "`+shaMain, `"before": "`+zeroSHA, 1)
	got, ignored, err := ClassifyDelivery("push", []byte(body))
	if err != nil || ignored != nil {
		t.Fatalf("got (%+v, %+v, %v)", got, ignored, err)
	}
	if got.BaseSHA != "" {
		t.Errorf("BaseSHA = %q, want empty for a branch with no previous head", got.BaseSHA)
	}
}

// A pull request's review, comment and thread events carry a top-level
// pull_request too. Their shape, not only their action, says they are not
// a pull request event.
func TestClassifyDeliveryIgnoresThePullRequestReviewFamily(t *testing.T) {
	opened := string(readDelivery(t, "pull_request_opened.json"))
	for _, key := range []string{"review", "comment", "thread"} {
		body := strings.Replace(opened, `"action": "opened",`, `"action": "opened", "`+key+`": {"id": 1},`, 1)
		got, ignored, err := ClassifyDelivery("", []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if ignored == nil {
			t.Errorf("a pull_request body carrying %q was classified as %+v", key, got)
		}
	}
}

// A release names a tag; its version is the tag, and a release without a
// name reads as its tag.
func TestClassifyDeliveryTitlesAnUnnamedReleaseByItsTag(t *testing.T) {
	body := strings.Replace(string(readDelivery(t, "release_published.json")),
		`"name": "Storefront 1.4",`, `"name": null,`, 1)
	got, ignored, err := ClassifyDelivery("release", []byte(body))
	if err != nil || ignored != nil {
		t.Fatalf("got (%+v, %+v, %v)", got, ignored, err)
	}
	if got.Title != "v1.4.0" {
		t.Errorf("Title = %q, want the tag", got.Title)
	}
	if got.SHA != "" {
		t.Errorf("SHA = %q: a release's commit is resolved by the trigger, not read from the body", got.SHA)
	}
}
