package rbac

import (
	"testing"

	componentAuth "github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/id"
)

// A grant on an app names a resource with a colon ("app:deployables") and a
// part with a slash ("app:deployables/reviews"). The capability row's short id
// may not carry a colon, so roleCreate wrote the role row and the grants before
// the first app grant and was then refused: a half-built role holding none of
// the app grants the OS New role screen sent. The id is spelled the way
// dsl/rbac/seeds.memql spells the predefined roles' own.
func TestAGrantRowIdIsAShortIdForAnAppGrant(t *testing.T) {
	for _, c := range []struct{ verb, resource, want string }{
		{"read", "data", "cap-fylo-moderator-read-data"},
		{"read", "app:deployables", "cap-fylo-moderator-read-app-deployables"},
		{"execute", "app:deployables/reviews", "cap-fylo-moderator-execute-app-deployables-reviews"},
	} {
		got := grantRowId("fylo-moderator", componentAuth.VerbResource{Verb: c.verb, Resource: c.resource})
		if got != c.want {
			t.Errorf("grantRowId(%s %s) = %q, want %q", c.verb, c.resource, got, c.want)
		}
		if err := id.ValidateShortId("v1:rbac:capability", got); err != nil {
			t.Errorf("grantRowId(%s %s) is refused as a short id: %v", c.verb, c.resource, err)
		}
	}
}
