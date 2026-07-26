package identity

// server_only_origin_test.go -- memql#2881.
//
// The DSL half of a @serverOnly gate is only half the change. The construct
// refuses a client-origin call, so the trusted server-side caller has to stamp
// internal origin explicitly -- and if it does not, the feature simply breaks:
// magic-link verification, the /login existing-user branch and startAdminSession
// all fail with `function "userByEmail" is server-only and cannot be called by a
// client`.
//
// That half had NO coverage. Reverting LookupUserByEmail to the unstamped
// executeAndExtract left `go test ./...` entirely green while login was broken
// (found in review). The DSL-side test cannot catch it: it passes
// auth.OriginInternal in by hand, which is precisely the thing the store is
// responsible for supplying.
//
// This test asserts the store's side of the contract at the Engine seam, so it
// needs no database.

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// originRecorder captures the origin of the context each query is executed
// with, keyed by the construct name appearing in the query text.
type originRecorder struct {
	seen map[string]auth.CallOrigin
}

func (r *originRecorder) Execute(ctx context.Context, query string) (*memqlengine.ExecuteResult, error) {
	if r.seen == nil {
		r.seen = map[string]auth.CallOrigin{}
	}
	for _, name := range []string{"userByEmail", "userByIdSystem"} {
		if strings.Contains(query, name+"(") {
			r.seen[name] = auth.OriginFromContext(ctx)
		}
	}
	return nil, nil
}

// TestStoreStampsInternalOriginForServerOnlyQueries pins that every Store
// method calling a @serverOnly construct routes through the internal-origin
// helper.
//
// Failing-first: swap either call site back to the unstamped
// s.executeAndExtract and the corresponding subtest reports OriginClient.
func TestStoreStampsInternalOriginForServerOnlyQueries(t *testing.T) {
	for _, tc := range []struct {
		name      string
		call      func(s *Store) error
		construct string
	}{
		{
			name:      "LookupUserByEmail",
			construct: "userByEmail",
			call: func(s *Store) error {
				_, err := s.LookupUserByEmail(context.Background(), "someone@example.com")
				return err
			},
		},
		{
			name:      "LookupUserById",
			construct: "userByIdSystem",
			call: func(s *Store) error {
				_, err := s.LookupUserById(context.Background(), "v1:identity:user:someone")
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &originRecorder{}
			s := &Store{Engine: rec}
			if err := tc.call(s); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			got, ok := rec.seen[tc.construct]
			if !ok {
				t.Fatalf("%s never executed a query naming %q -- if the construct was renamed, move this guard with it", tc.name, tc.construct)
			}
			if !got.IsInternal() {
				t.Errorf("%s executed %s with origin %v, not internal.\n"+
					"%s is @serverOnly, so an unstamped call is refused at runtime and the\n"+
					"login path breaks. Route the call through executeAndExtractInternal.",
					tc.name, tc.construct, got, tc.construct)
			}
		})
	}
}
