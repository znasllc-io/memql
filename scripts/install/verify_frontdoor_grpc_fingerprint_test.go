package install

import (
	"strings"
	"testing"
)

// verify_frontdoor_grpc_fingerprint_test.go -- memql#3814.
//
// WHAT THE CHECK COULD SEE AND DID NOT USE. During memql#3810 every HTTP path
// on api.<domain> was answered by the gRPC backend, and `precedence` reported
// INCONCLUSIVE for that host for the whole life of the defect:
//
//	the wildcard router is live (nodeType=edge) but api.memql.localhost
//	answered HTTP 415 without naming a memQL node, so which backend serves it
//	cannot be established
//
// Every word of that is true. The response named no node, so the check could
// not read the backend off it THE WAY IT READS BACKENDS. But the response was
// not silent -- it was
//
//	HTTP/1.1 415 Unsupported Media Type
//	Content-Type: application/grpc
//	Grpc-Status: 3
//
// and /healthz routes to the bff's HTTP Service. Only an h2c gRPC server
// produces that, and only because it was handed an HTTP/1.1 request it cannot
// parse. No correct configuration yields it at this path.
//
// So the evidence was SUFFICIENT and merely INDIRECT, and the check declined to
// use it. That is the failure mode of an honest-uncertainty verdict: built so a
// check can never claim a pass it has not earned, it becomes its own way of not
// looking. The rule these tests encode:
//
//	report inconclusive when the evidence is INSUFFICIENT,
//	never when it is merely INDIRECT.
//
// The distinction to apply to any new case: could a CORRECT configuration have
// produced this response? If no, it is a failure however indirectly the
// responder identified itself.

// TestPrecedenceFailsOnAGrpcContentTypeAtAnHttpPath is the regression, using
// the exact response shape memql#3810 produced.
func TestPrecedenceFailsOnAGrpcContentTypeAtAnHttpPath(t *testing.T) {
	env := fdWorldWithContentTypes(t,
		map[string]string{fdAPI: "127.0.0.1"},
		map[string]string{
			fdAPI:   "0|2|415|",
			fdProbe: "0|2|200|" + fdHealth(fdEdgeNodeType, "edge-1"),
		},
		map[string]string{fdAPI: "application/grpc"},
	)

	stdout, _, _ := fdRun(t, env, "--hosts="+fdAPI)
	_, res := fdParse(t, stdout)
	c := fdFind(t, res, "precedence", fdAPI)

	if c.Status != "failed" {
		t.Errorf("status = %q, want failed.\n"+
			"A 415 with Content-Type application/grpc at /healthz is positive "+
			"identification of the WRONG backend -- an h2c gRPC server handed an "+
			"HTTP/1.1 request -- even though it names no node. Reporting it "+
			"inconclusive is how memql#3810 stayed un-gated while this check was "+
			"looking straight at it.\ndetail: %s", c.Status, c.Detail)
	}
	if c.Passed {
		t.Error("passed=true for a host answered by the wrong backend")
	}
	// The detail has to say what is wrong in terms an operator can act on. The
	// status alone does not distinguish this from the wildcard swallowing the
	// host, and the remedies are completely different.
	if !strings.Contains(c.Detail, "application/grpc") {
		t.Errorf("detail does not name the fingerprint it decided on: %q", c.Detail)
	}
	if !strings.Contains(c.Detail, "router.priority") {
		t.Errorf("detail does not point at the known cause of this shape "+
			"(memql#3810's uniform Ingress-level priority), which is the one thing "+
			"that turns the verdict into an action: %q", c.Detail)
	}
}

// TestPrecedenceStaysInconclusiveWhenTheResponseIsGenuinelyUnidentifiable is
// the other side, and it is what keeps the new branch from being "fail whenever
// no node is named".
//
// A 401 from an auth proxy, or any other response that names no node and
// carries no backend-specific fingerprint, is genuinely insufficient: several
// correct configurations could produce it. That must remain inconclusive.
func TestPrecedenceStaysInconclusiveWhenTheResponseIsGenuinelyUnidentifiable(t *testing.T) {
	env := fdWorldWithContentTypes(t,
		map[string]string{fdAPI: "127.0.0.1"},
		map[string]string{
			fdAPI:   "0|2|401|Unauthorized",
			fdProbe: "0|2|200|" + fdHealth(fdEdgeNodeType, "edge-1"),
		},
		map[string]string{fdAPI: "text/plain"},
	)

	stdout, _, code := fdRun(t, env, "--hosts="+fdAPI)
	if code != 0 {
		t.Fatalf("exit %d, want 0 -- an inconclusive must not fail the run\nstdout: %s", code, stdout)
	}
	_, res := fdParse(t, stdout)
	c := fdFind(t, res, "precedence", fdAPI)

	if c.Status != "inconclusive" {
		t.Errorf("status = %q, want inconclusive.\n"+
			"A 401 naming no node is genuinely insufficient -- more than one correct "+
			"configuration produces it -- and the memql#3814 change must not turn "+
			"every unidentified response into a failure.\ndetail: %s", c.Status, c.Detail)
	}
}

// TestPrecedenceGrpcFingerprintDoesNotOverrideAHealthyAnswer guards the
// ordering. A host that DOES name itself is decided on the node type, and a
// content type must not get a vote -- the gRPC edge legitimately serves
// application/grpc to a gRPC client, and this probe is an HTTP one only by
// convention.
func TestPrecedenceGrpcFingerprintDoesNotOverrideAHealthyAnswer(t *testing.T) {
	env := fdWorldWithContentTypes(t,
		map[string]string{fdAPI: "127.0.0.1"},
		map[string]string{
			fdAPI:   "0|2|200|" + fdHealth("bff", "bff-1"),
			fdProbe: "0|2|200|" + fdHealth(fdEdgeNodeType, "edge-1"),
		},
		// Deliberately contradictory: a body that names a node, alongside a
		// content type the new branch keys on. The node wins.
		map[string]string{fdAPI: "application/grpc"},
	)

	stdout, _, code := fdRun(t, env, "--hosts="+fdAPI)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s", code, stdout)
	}
	_, res := fdParse(t, stdout)
	c := fdFind(t, res, "precedence", fdAPI)

	if !c.Passed {
		t.Errorf("status = %q, want passed. A response that NAMES a node is decided "+
			"on the node type; the content-type fingerprint is a fallback for "+
			"responses that name nobody, not an override.\ndetail: %s", c.Status, c.Detail)
	}
}
