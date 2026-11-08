package memql

import (
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// THE REVIEWS PART REACHES THE SHARED ORGANIZATION GATE. Review moderation is
// a Deployables part: a person granted `execute app:deployables/reviews` in
// one organization must not moderate another's reviews with it. The gate
// judges a builtin against the organization of its target row, so each acting
// builtin of the reviews pack names one: a review (whose accountId was copied
// off its site when it was written) or the site. The pack's own gate_test.go
// pins the DSL side -- the part and the argument on each builtin -- since the
// pack's tree is not loadable from here.
func TestTheReviewsBuiltinsNameTheirOrganizationTarget(t *testing.T) {
	if !organizationDeployableCapability("app:deployables/reviews") {
		t.Fatal("app:deployables/reviews is not an organization deployable capability, so every moderation act is judged against the caller's global capability alone")
	}
	for name, want := range map[string][2]string{
		"reviewsDecide":           {"v1:reviews:review", "reviewId"},
		"reviewsList":             {"v1:platform:site", "siteId"},
		"reviewsUpdateSettings":   {"v1:platform:site", "siteId"},
		"reviewsSetPublicDisplay": {"v1:platform:site", "siteId"},
		"reviewsIssueRequest":     {"v1:platform:site", "siteId"},
		"reviewsExportStore":      {"v1:platform:site", "siteId"},
		"reviewsEraseReviewer":    {"v1:reviews:review", "reviewId"},
		"reviewsGet":              {"v1:reviews:review", "reviewId"},
		"reviewsDecisionLog":      {"v1:platform:site", "siteId"},
		"reviewsSettings":         {"v1:platform:site", "siteId"},
		"reviewsPeople":           {"v1:platform:site", "siteId"},
	} {
		fn := &Function{Name: name, FunctionKind: "builtin",
			RequiresCapability: CapabilityRequirement{Verb: auth.VerbExecute, Resource: "app:deployables/reviews"}}
		concept, argument := organizationCapabilityTarget(fn)
		if concept != want[0] || argument != want[1] {
			t.Errorf("%s resolves its organization target to (%q, %q), want (%q, %q)", name, concept, argument, want[0], want[1])
		}
	}
}
