package customdomain

import (
	"strings"
	"testing"
)

// error_pages_backend_test.go -- the Ingresses this package renders fall back
// to the error pages when their Service has no ready endpoints (the
// 2026-10-11 outage: every host answered ingress-nginx's bare 503).
//
// The annotation key and the Service name are written out as literals here on
// purpose. They are ingress-nginx's contract and the base manifest's name, and
// a test that read them back from the constants under test would pass whatever
// the constants said. The equality with the front-door generator's own copy is
// asserted in cmd/frontdoorhosts.

const (
	defaultBackendKey  = "nginx.ingress.kubernetes.io/default-backend"
	backendProtocolKey = "nginx.ingress.kubernetes.io/backend-protocol"
	customErrorsKey    = "nginx.ingress.kubernetes.io/custom-http-errors"
)

func ingressAnnotations(t *testing.T, ingress map[string]any) map[string]any {
	t.Helper()
	meta, ok := ingress["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("an Ingress with no metadata: %v", ingress)
	}
	annotations, ok := meta["annotations"].(map[string]any)
	if !ok {
		t.Fatalf("Ingress %v has no annotations map", meta["name"])
	}
	return annotations
}

func TestTheErrorPagesNamesAreIngressNginxsAndTheBaseManifests(t *testing.T) {
	if ErrorPagesBackendAnnotation != defaultBackendKey {
		t.Errorf("ErrorPagesBackendAnnotation = %q, want ingress-nginx's %q", ErrorPagesBackendAnnotation, defaultBackendKey)
	}
	if ErrorPagesServiceName != "error-pages" {
		t.Errorf("ErrorPagesServiceName = %q, want the base manifest's Service %q", ErrorPagesServiceName, "error-pages")
	}
}

// A client's own domain reaches the edge over plain HTTP, so when the edge has
// no ready pod its visitors get the neutral page and not ingress-nginx's bare
// 503.
func TestCustomDomainIngressHasErrorPagesBackend(t *testing.T) {
	req := BindRequest{
		Hostname: "shop.example.org", DomainID: "d1", SiteID: "s1",
		Namespace: "memql", Issuer: "letsencrypt-prod", IngressClass: "nginx",
		Service: "edge", Port: 8085,
	}
	annotations := ingressAnnotations(t, IngressObject(req))

	if got := annotations[defaultBackendKey]; got != "error-pages" {
		t.Errorf("the custom domain Ingress carries %s=%v, want error-pages", defaultBackendKey, got)
	}
	if _, ok := annotations[customErrorsKey]; ok {
		t.Errorf("the custom domain Ingress sets %s, which would replace the application's own error responses with the maintenance page", customErrorsKey)
	}
	if _, ok := annotations[backendProtocolKey]; ok {
		t.Errorf("the custom domain Ingress sets %s; it reaches the edge over plain HTTP, and the error pages only stand behind plain HTTP", backendProtocolKey)
	}
}

// An account's front door is four Ingresses and only two of them can fall back
// to the error pages. The proxy keeps the Ingress's backend protocol when it
// swaps in the default backend (ingress-nginx's buildProxyPass reads the
// location's protocol, and only its own built-in default backend is forced back
// to http), so a default backend behind `backend-protocol: GRPC` is sent h2c
// and one behind HTTPS is sent a TLS handshake. The error pages speak neither,
// so both would answer 502 -- worse than the bare 503 they replace.
func TestAccountDoorIngressHasErrorPagesBackend(t *testing.T) {
	door := DoorBindRequest{
		AccountID: "a1", ReservedName: "memql.example.com", Namespace: "memql",
		Issuer: "letsencrypt-prod", IngressClass: "nginx",
		EdgeService: "edge", EdgePort: 8085,
		BFFHTTPService: "bff-http", BFFHTTPPort: 8085,
		BFFGRPCService: "bff", BFFGRPCPort: 50051,
		AgentGRPCService: "agent", AgentGRPCPort: 50051,
		IdentityService: "identity", IdentityPort: 8085,
	}
	objects := doorIngressObjects(door, []string{"/healthz"})
	if len(objects) != 4 {
		t.Fatalf("a door renders %d Ingresses, want 4 (app, api-grpc, id, api)", len(objects))
	}

	want := map[string]bool{
		"-app":      true,  // edge, plain HTTP
		"-api":      true,  // bff-http, plain HTTP
		"-api-grpc": false, // grpc_pass: the error pages answer HTTP/1.1
		"-id":       false, // identity speaks TLS in-cluster, so the proxy does too
	}
	seen := map[string]bool{}
	for _, obj := range objects {
		name, _ := obj["metadata"].(map[string]any)["name"].(string)
		suffix := strings.TrimPrefix(name, doorObjectName(door.AccountID))
		fit, known := want[suffix]
		if !known {
			t.Errorf("an Ingress named %q has no expectation here; extend this table and decide whether the error pages can stand behind it", name)
			continue
		}
		seen[suffix] = true

		annotations := ingressAnnotations(t, obj)
		got, has := annotations[defaultBackendKey]
		switch {
		case fit && got != "error-pages":
			t.Errorf("%s carries %s=%v, want error-pages", name, defaultBackendKey, got)
		case !fit && has:
			t.Errorf("%s carries %s=%v, but its backend protocol is %v and the error pages speak plain HTTP/1.1: the proxy would answer 502",
				name, defaultBackendKey, got, annotations[backendProtocolKey])
		}
		if _, ok := annotations[customErrorsKey]; ok {
			t.Errorf("%s sets %s, which would replace the application's own error responses with the maintenance page", name, customErrorsKey)
		}
	}
	for suffix := range want {
		if !seen[suffix] {
			t.Errorf("no Ingress with the %q suffix was rendered; this gate would pass on a door it did not read", suffix)
		}
	}
}

// The rule is read from the Ingress's own backend protocol rather than from a
// list of suffixes, so a backend added tomorrow is classified by what it
// speaks, not by whether somebody remembered to extend a table.
func TestErrorPagesAreOfferedOnlyToPlainHTTPBackends(t *testing.T) {
	for _, tc := range []struct {
		protocol string
		set      bool
		fits     bool
	}{
		{set: false, fits: true},
		{protocol: "HTTP", set: true, fits: true},
		{protocol: "http", set: true, fits: true},
		{protocol: "HTTPS", set: true, fits: false},
		{protocol: "GRPC", set: true, fits: false},
		{protocol: "GRPCS", set: true, fits: false},
		{protocol: "AUTO_HTTP", set: true, fits: false},
		{protocol: "FCGI", set: true, fits: false},
	} {
		annotations := map[string]any{}
		if tc.set {
			annotations[backendProtocolKey] = tc.protocol
		}
		withErrorPagesBackend(annotations)
		_, has := annotations[defaultBackendKey]
		if has != tc.fits {
			t.Errorf("backend protocol %q (set=%v): default-backend present = %v, want %v", tc.protocol, tc.set, has, tc.fits)
		}
	}
}
