//go:build bff

package app

import (
	"testing"

	"github.com/znasllc-io/memql/component/identity/githubconnect"
	"github.com/znasllc-io/memql/component/inbound"
	"github.com/znasllc-io/memql/component/packages"
)

// THREE SPELLINGS OF ONE NAME, in three modules that cannot import each other
// downward: the URL the cluster's GitHub App is told to post its webhook to
// (githubconnect.WebhookPath, composed into the manifest), the route the
// inbound receiver serves (inbound.RoutePrefix + source), and the source the
// packages pipeline reads a staged delivery under (its default). A drift
// between any two is silent: GitHub posts, the receiver answers 404 or stages a
// row nothing reads, and the update cue falls back to the ten-minute poll with
// nothing anywhere saying why. app/ is the one place that can see all three.
func TestTheGitHubWebhookIsOneNameInThreeModules(t *testing.T) {
	if got, want := githubconnect.WebhookPath, inbound.RoutePrefix+packages.DefaultWebhookSource; got != want {
		t.Errorf("the app's manifest posts to %q and the pipeline listens on %q", got, want)
	}
	if githubWebhookSource != packages.DefaultWebhookSource {
		t.Errorf("the registered inbound policy answers for %q and the pipeline reads %q", githubWebhookSource, packages.DefaultWebhookSource)
	}
}
