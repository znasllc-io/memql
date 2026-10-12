package customdomain

import (
	"os"
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
// to http), and the error pages are a plain HTTP/1.1 server. Each of the other
// two protocols rules an Ingress out on its own account: behind GRPC the proxy
// speaks h2c to them, and behind HTTPS it opens a TLS connection to a server
// that speaks none, so the handshake fails. Both answer 502 -- worse than the
// bare 503 they replace.
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
		"-api-grpc": false, // backend-protocol GRPC: the proxy would speak h2c to a server that speaks HTTP/1.1
		"-id":       false, // backend-protocol HTTPS: the proxy would start a TLS handshake with a server that speaks none
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

// THE UPGRADE BACKFILL SELECTS WHAT THE BUILDERS ANNOTATE, BY THEIR OWN RULE.
// The reconciler applies an Ingress only while a binding is issuing, so a
// custom domain or an account door that is already live when the annotation
// ships does not carry it until it is bound again.
// docs/public/operate/front-door.md gives the one-off that adds it: it lists
// the custom-domain and door Ingresses by their labels, and annotates each one
// whose backend-protocol annotation is absent or HTTP -- withErrorPagesBackend's
// own rule, read off the object. The protocol and not a name suffix, because the
// objects it acts on were written by OLDER engine versions: what an Ingress's
// proxy speaks is on the object whichever version wrote it, while a suffix is a
// convention of the builder that wrote it. This holds the command to the
// objects it acts on: the labels it selects are the builders' labels, and its
// filter, applied to every Ingress the builders render, picks exactly the ones
// they annotate. If a builder's label moves, or a protocol is added that the
// filter would misread, the doc's command would silently annotate the wrong set
// (or none), and the failure would surface during an outage.
func TestTheUpgradeBackfillInTheDocsSelectsWhatTheBuildersLabel(t *testing.T) {
	const (
		customDomainSelector = "app.kubernetes.io/name=custom-domain"
		doorSelector         = "app.kubernetes.io/name=account-front-door"
		// The doc's jsonpath prints each Ingress's backend-protocol annotation
		// beside its name, and its awk filter keeps the rows whose protocol is
		// empty or HTTP.
		protocolPath   = `{.metadata.annotations.nginx\.ingress\.kubernetes\.io/backend-protocol}`
		protocolFilter = `$2 == "" || toupper($2) == "HTTP"`
	)
	// picks is protocolFilter as awk applies it to the printed protocol.
	picks := func(protocol string) bool { return protocol == "" || strings.ToUpper(protocol) == "HTTP" }

	labelOf := func(obj map[string]any, key string) string {
		labels, _ := obj["metadata"].(map[string]any)["labels"].(map[string]any)
		v, _ := labels[key].(string)
		return v
	}
	protocolOf := func(obj map[string]any) string {
		v, _ := ingressAnnotations(t, obj)[backendProtocolKey].(string)
		return v
	}

	domain := IngressObject(BindRequest{Hostname: "shop.example.org", DomainID: "d1", Namespace: "memql", IngressClass: "nginx", Service: "edge", Port: 8085})
	if got := labelOf(domain, "app.kubernetes.io/name"); got != "custom-domain" {
		t.Errorf("the custom domain Ingress is labelled app.kubernetes.io/name=%q; the backfill selects %q", got, customDomainSelector)
	}
	if _, has := ingressAnnotations(t, domain)[defaultBackendKey]; !has || !picks(protocolOf(domain)) {
		t.Errorf("the custom domain Ingress: carries the annotation = %v, the backfill's filter picks it = %v; want both",
			has, picks(protocolOf(domain)))
	}

	door := DoorBindRequest{AccountID: "a1", ReservedName: "memql.example.com", Namespace: "memql", IngressClass: "nginx"}
	var annotated, selected int
	for _, obj := range doorIngressObjects(door, []string{"/healthz"}) {
		name, _ := obj["metadata"].(map[string]any)["name"].(string)
		if got := labelOf(obj, "app.kubernetes.io/name"); got != "account-front-door" {
			t.Errorf("%s is labelled app.kubernetes.io/name=%q; the backfill selects %q", name, got, doorSelector)
		}
		_, has := ingressAnnotations(t, obj)[defaultBackendKey]
		picked := picks(protocolOf(obj))
		if has != picked {
			t.Errorf("%s (backend protocol %q): carries the annotation = %v, but the backfill's filter %s picks it = %v; "+
				"the one-off would annotate the wrong set of door Ingresses", name, protocolOf(obj), has, protocolFilter, picked)
		}
		if has {
			annotated++
		}
		if picked {
			selected++
		}
	}
	if annotated != 2 || selected != 2 {
		t.Errorf("a door renders %d annotated and %d selected Ingresses, want 2 and 2 (app and api)", annotated, selected)
	}

	raw, err := os.ReadFile("../../docs/public/operate/front-door.md")
	if err != nil {
		t.Fatalf("reading the front-door page: %v", err)
	}
	doc := string(raw)
	// The selectors must be the loop's values AND reach `get` as its -l: a
	// strings.Contains on the selectors alone stays green when the -l is
	// dropped, and the same loop then annotates every plain-HTTP Ingress in the
	// namespace, the front door's own (mcp-front-door included).
	loop := "for sel in " + customDomainSelector + " " + doorSelector + "; do"
	for _, want := range []string{loop, `get ingress -l "$sel"`, protocolPath, "'" + protocolFilter, "default-backend=error-pages"} {
		if !strings.Contains(doc, want) {
			t.Errorf("docs/public/operate/front-door.md does not contain %q; the upgrade backfill the page gives must select exactly what the builders annotate", want)
		}
	}
	if strings.Contains(doc, "-(app|api)$") {
		t.Error("docs/public/operate/front-door.md still selects door Ingresses by name suffix; an older engine's names are a convention, its backend protocol is on the object")
	}
}
