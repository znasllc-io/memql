package auth_test

import (
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memqlgrpc "github.com/znasllc-io/memql/component/grpc"
)

// RequirePersonSession refuses the no-auth shim by its subject, so the two
// spellings of that subject must not drift apart.
func TestLocalDevSubjectMatchesTheInterceptor(t *testing.T) {
	if auth.LocalDevSubject != memqlgrpc.LocalDevSubject {
		t.Fatalf("auth.LocalDevSubject %q != grpc.LocalDevSubject %q", auth.LocalDevSubject, memqlgrpc.LocalDevSubject)
	}
}
