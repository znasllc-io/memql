package main

import (
	"testing"

	"github.com/znasllc-io/memql/integrations/customdomain"
)

// TestTheErrorPagesNamesMatchWhatTheEngineStampsOnCustomDomains holds the
// generator's copy of the two names to the engine's.
//
// The front door is written by this tool and the Ingresses for a client's own
// domain and an account's reserved name are written at runtime by
// integrations/customdomain. They must fall back to the SAME Service through
// the SAME annotation, or an outage shows the maintenance page on the
// platform's hosts and ingress-nginx's bare 503 on every customer's. The
// literals are duplicated rather than imported (a code generator should not
// link the reconciler), so this is what stops the two copies drifting.
func TestTheErrorPagesNamesMatchWhatTheEngineStampsOnCustomDomains(t *testing.T) {
	if defaultBackendAnnotation != customdomain.ErrorPagesBackendAnnotation {
		t.Errorf("the generator annotates with %q, the engine stamps %q on custom domains",
			defaultBackendAnnotation, customdomain.ErrorPagesBackendAnnotation)
	}
	if errorPagesService != customdomain.ErrorPagesServiceName {
		t.Errorf("the generator names the Service %q, the engine names it %q on custom domains",
			errorPagesService, customdomain.ErrorPagesServiceName)
	}
}
