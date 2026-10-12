// Render gates on the error pages (the 2026-10-11 outage).
//
// THE FAILURE THESE CATCH. On 2026-10-11 the database filled, every pod lost
// readiness, and every host the cluster serves -- the platform's own and every
// customer's -- answered ingress-nginx's bare "503 Service Temporarily
// Unavailable". `memql error-pages` is the replacement, and two things have to
// be true for a visitor to see it, neither of which fails loudly when it is
// not:
//
//   - the Ingress has to NAME it (`nginx.ingress.kubernetes.io/default-backend`),
//     because ingress-nginx only falls back to a custom default backend for the
//     Ingresses that ask. An Ingress that lost the annotation in a regeneration
//     builds, reconciles and serves traffic exactly as before, and shows the
//     bare 503 the first time its Service has no endpoints; and
//   - the Deployment behind that name has to exist in every overlay whose front
//     door names it -- cloud and cloud-entry, which compose
//     deploy/k8s/components/error-pages -- on the image the overlay pins,
//     listening where the Service and the probes expect, and needing nothing
//     from the cluster: it is the one workload that must still answer when
//     every other one does not.
//
// AND NOWHERE ELSE. The local overlay renders none of it, and
// TestTheLocalOverlayRendersNoErrorPages says why.
//
// The annotation key and the Service name are written out below as literals,
// not read from the generator or from integrations/customdomain. They are
// ingress-nginx's contract and the component manifest's name; a gate that took
// them from the code under test would pass whatever the code said. The equality
// with the engine's copy is asserted in cmd/frontdoorhosts.
package overlays

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	defaultBackendAnnotation   = "nginx.ingress.kubernetes.io/default-backend"
	backendProtocolAnnotation  = "nginx.ingress.kubernetes.io/backend-protocol"
	customHTTPErrorsAnnotation = "nginx.ingress.kubernetes.io/custom-http-errors"

	errorPagesName = "error-pages"
)

// errorPagesOverlays are the overlays that render the error pages: the two
// whose front door is ingress-nginx's, and so carries the annotation that
// points at them. Each composes deploy/k8s/components/error-pages; base does
// not carry it, so the local overlay (traefik, which ignores the annotation)
// renders none of it.
var errorPagesOverlays = []string{cloudOverlay, entryOverlay}

// errorPagesExcluded are the front-door Ingresses that must NOT name the error
// pages, and why.
//
// Two are ruled out by their backend protocol. ingress-nginx builds the custom
// default backend as a copy of the location's own upstream and keeps the
// location's protocol: only its built-in default backend is forced back to plain
// http (template.go buildProxyPass, v1.15.1). `memql error-pages` is a plain
// HTTP/1.1 server, so each protocol below rules an Ingress out ON ITS OWN
// ACCOUNT (errorPagesUnfitReason says how): HTTPS is not the GRPC case under
// another name, and would hold if the gRPC Ingress did not exist. Either way the
// proxy answers 502, worse than the bare 503 it would replace. The exclusion is
// pinned by NAME as well as by the protocol rule, so that flipping an Ingress's
// protocol annotation and adding the default backend in the same edit cannot
// satisfy the rule by changing the question.
//
// One is ruled out by what it serves, not by its protocol, which is plain HTTP:
// mcp-front-door. Its protocol is empty below and its reason says why.
var errorPagesExcluded = []struct{ name, protocol, reason string }{
	{name: "api-front-door-grpc", protocol: "GRPC"},
	{name: "identity-front-door", protocol: "HTTPS"},
	{name: "mcp-front-door", reason: "mcp is disabled by default -- cloud-entry renders it at 0 replicas -- so on every entry " +
		"installation the page would answer \"MemQL is being updated, back in a few minutes\" with Retry-After 120 for " +
		"good, which is false; and MCP is a machine surface, whose clients read the status, not a page"},
}

// excludedWhy says why an excluded Ingress must not name the error pages.
func excludedWhy(protocol, reason string) string {
	if protocol == "" {
		return reason
	}
	return "backend protocol " + protocol + ": " + errorPagesUnfitReason(protocol)
}

// plainHTTPBackend reports whether an Ingress's proxy speaks plain HTTP to its
// backends, which is the only thing the error pages can stand behind.
func plainHTTPBackend(annotations map[string]string) bool {
	protocol := strings.ToUpper(strings.TrimSpace(annotations[backendProtocolAnnotation]))
	return protocol == "" || protocol == "HTTP"
}

// errorPagesUnfitReason says why an Ingress with this backend protocol cannot
// name the error pages: one reason per protocol, each sufficient alone.
func errorPagesUnfitReason(protocol string) string {
	switch strings.ToUpper(strings.TrimSpace(protocol)) {
	case "GRPC", "GRPCS":
		return "grpc_pass would hand the error pages HTTP/2, and they speak HTTP/1.1"
	case "HTTPS":
		return "the proxy would open a TLS connection to the error pages, which speak no TLS, and the handshake would fail"
	case "AUTO_HTTP":
		return "the proxy would use the client's own scheme, so a TLS client gets a TLS connection to the error pages, which speak no TLS"
	case "FCGI":
		return "fastcgi_pass is not HTTP"
	}
	return "its backend protocol is not plain HTTP"
}

// TestOnlyPlainHTTPBackendsFitTheErrorPages reads the rule itself, one protocol
// at a time, so that HTTPS is held as a reason in its own right and not only
// through the two Ingresses that happen to set it today.
func TestOnlyPlainHTTPBackendsFitTheErrorPages(t *testing.T) {
	for _, tc := range []struct {
		protocol string
		set      bool
		fits     bool
	}{
		{set: false, fits: true},
		{protocol: "HTTP", set: true, fits: true},
		{protocol: "http", set: true, fits: true},
		{protocol: "HTTPS", set: true, fits: false},
		{protocol: "https", set: true, fits: false},
		{protocol: "GRPC", set: true, fits: false},
		{protocol: "GRPCS", set: true, fits: false},
		{protocol: "AUTO_HTTP", set: true, fits: false},
		{protocol: "FCGI", set: true, fits: false},
	} {
		annotations := map[string]string{}
		if tc.set {
			annotations[backendProtocolAnnotation] = tc.protocol
		}
		if got := plainHTTPBackend(annotations); got != tc.fits {
			t.Errorf("backend protocol %q (set=%v): fits = %v, want %v", tc.protocol, tc.set, got, tc.fits)
		}
		if !tc.fits && strings.Contains(errorPagesUnfitReason(tc.protocol), "not plain HTTP") {
			t.Errorf("backend protocol %q has no reason of its own in errorPagesUnfitReason", tc.protocol)
		}
	}
}

// annotatedIngress is the slice of an Ingress these gates read.
type annotatedIngress struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name        string            `yaml:"name"`
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
}

// ingressesIn decodes every Ingress of a multi-document stream. A decode error
// is fatal for the reason parse() gives: a truncated prefix of the stream would
// leave every assertion below passing over a fraction of it.
func ingressesIn(t *testing.T, stream string) []annotatedIngress {
	t.Helper()
	var out []annotatedIngress
	dec := yaml.NewDecoder(strings.NewReader(stream))
	for i := 1; ; i++ {
		var d annotatedIngress
		err := dec.Decode(&d)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("decoding document %d: %v", i, err)
		}
		if d.Kind == "Ingress" {
			out = append(out, d)
		}
	}
}

// assertErrorPagesBackends applies the rule to the front-door Ingresses of one
// stream: every one whose proxy speaks plain HTTP names the error pages, but
// mcp-front-door (errorPagesExcluded), every other one does not, and none of
// them replaces the application's own error responses.
//
// Scoped to the generator's closed set (frontDoorIngressNames) rather than to
// every Ingress the stream happens to carry, so an Ingress an overlay adds for
// some other purpose is not this gate's business -- while one of the seven
// going missing is.
func assertErrorPagesBackends(t *testing.T, where string, ingresses []annotatedIngress) {
	t.Helper()

	byName := map[string]annotatedIngress{}
	for _, ing := range ingresses {
		byName[ing.Metadata.Name] = ing
	}
	// Excluded by what it serves rather than by its protocol: plain HTTP, and
	// still no error pages. Its absence is asserted by name, below.
	byChoice := map[string]bool{}
	for _, x := range errorPagesExcluded {
		if x.protocol == "" {
			byChoice[x.name] = true
		}
	}

	var plain, excluded int
	for _, name := range frontDoorIngressNames {
		ing, ok := byName[name]
		if !ok {
			t.Errorf("%s: no Ingress named %s; the generator's closed set is out of step with this stream", where, name)
			continue
		}
		annotations := ing.Metadata.Annotations
		got, has := annotations[defaultBackendAnnotation]

		if _, ok := annotations[customHTTPErrorsAnnotation]; ok {
			t.Errorf("%s: %s sets %s. That makes ingress-nginx replace the application's own 4xx/5xx "+
				"responses -- the API's JSON error bodies included -- with the maintenance page. The "+
				"error pages are the fallback for a Service with no endpoints and nothing else.",
				where, name, customHTTPErrorsAnnotation)
		}

		if byChoice[name] {
			excluded++
			continue
		}

		if plainHTTPBackend(annotations) {
			plain++
			if !has || got != errorPagesName {
				t.Errorf("%s: %s names no error pages (%s=%q, want %q). When its Service has no ready "+
					"endpoints a visitor gets ingress-nginx's bare 503, which is what 2026-10-11 looked like.",
					where, name, defaultBackendAnnotation, got, errorPagesName)
			}
			continue
		}

		excluded++
		if has {
			t.Errorf("%s: %s sets %s=%q but its backend protocol is %q. ingress-nginx keeps that protocol "+
				"when it swaps in the default backend: %s. The proxy would answer 502 instead of the bare 503.",
				where, name, defaultBackendAnnotation, got, annotations[backendProtocolAnnotation],
				errorPagesUnfitReason(annotations[backendProtocolAnnotation]))
		}
	}

	// The exclusions, by name: the protocol rule above is satisfied just as well
	// by changing an Ingress's protocol annotation and adding the default backend
	// in the one edit, which would change the question rather than answer it.
	for _, x := range errorPagesExcluded {
		ing, ok := byName[x.name]
		if !ok {
			continue // reported above
		}
		if x.protocol != "" {
			if got := strings.ToUpper(ing.Metadata.Annotations[backendProtocolAnnotation]); got != x.protocol {
				t.Errorf("%s: %s has backend protocol %q, want %q (that is why it is excluded)", where, x.name, got, x.protocol)
			}
		}
		if got, has := ing.Metadata.Annotations[defaultBackendAnnotation]; has {
			t.Errorf("%s: %s names the error pages (%s=%q); it must not (%s)",
				where, x.name, defaultBackendAnnotation, got, excludedWhy(x.protocol, x.reason))
		}
	}

	// Floors, so a stream that parsed to nothing cannot pass.
	if plain == 0 || excluded != len(errorPagesExcluded) {
		t.Errorf("%s: %d plain-HTTP and %d excluded Ingress(es), want some and exactly %d; this gate needs both kinds to mean anything",
			where, plain, excluded, len(errorPagesExcluded))
	}
}

// TestEveryFrontDoorIngressHasErrorPagesBackend reads the GENERATED files, not
// a render, so it runs on a machine with neither kustomize nor kubectl, and so
// it is the file that is held to the rule rather than whatever an overlay
// patch did to it afterwards. The rendered half is the next test.
func TestEveryFrontDoorIngressHasErrorPagesBackend(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("*", "front-door.generated.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var overlays []string
	for _, f := range files {
		overlays = append(overlays, filepath.Dir(f))
	}
	slices.Sort(overlays)
	want := slices.Clone(generatedOverlays)
	slices.Sort(want)
	if !slices.Equal(overlays, want) {
		t.Fatalf("generated front doors are in %v, want exactly %v -- a generated front door this gate does not "+
			"list is one nobody holds to the error-pages rule", overlays, want)
	}

	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("reading %s: %v", f, err)
		}
		assertErrorPagesBackends(t, f, ingressesIn(t, string(raw)))
	}
}

// TestRenderedFrontDoorsKeepTheirErrorPagesBackend holds the RENDERED overlay
// to the same rule. The annotation reaching an Ingress is a kustomize outcome,
// and an overlay patch against one of these objects is exactly where it could
// be lost (cloud and cloud-entry both patch edge-front-door).
func TestRenderedFrontDoorsKeepTheirErrorPagesBackend(t *testing.T) {
	for _, overlay := range generatedOverlays {
		t.Run(overlay, func(t *testing.T) {
			assertErrorPagesBackends(t, overlay, ingressesIn(t, render(t, overlay)))
		})
	}
}

// ---------------------------------------------------------------------------
// The Deployment, Service and PodDisruptionBudget behind the name
// ---------------------------------------------------------------------------

type errorPagesProbe struct {
	HTTPGet struct {
		Path string `yaml:"path"`
		Port any    `yaml:"port"`
	} `yaml:"httpGet"`
}

type errorPagesDeployment struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string            `yaml:"name"`
		Namespace string            `yaml:"namespace"`
		Labels    map[string]string `yaml:"labels"`
	} `yaml:"metadata"`
	Spec struct {
		Replicas *int `yaml:"replicas"`
		Strategy struct {
			Type          string `yaml:"type"`
			RollingUpdate struct {
				MaxSurge       *int `yaml:"maxSurge"`
				MaxUnavailable *int `yaml:"maxUnavailable"`
			} `yaml:"rollingUpdate"`
		} `yaml:"strategy"`
		Selector struct {
			MatchLabels map[string]string `yaml:"matchLabels"`
		} `yaml:"selector"`
		Template struct {
			Metadata struct {
				Labels map[string]string `yaml:"labels"`
			} `yaml:"metadata"`
			Spec struct {
				ServiceAccountName            string `yaml:"serviceAccountName"`
				AutomountServiceAccountToken  *bool  `yaml:"automountServiceAccountToken"`
				TerminationGracePeriodSeconds *int   `yaml:"terminationGracePeriodSeconds"`
				Volumes                       []any  `yaml:"volumes"`
				InitContainers                []any  `yaml:"initContainers"`
				SecurityContext               struct {
					RunAsNonRoot   *bool `yaml:"runAsNonRoot"`
					RunAsUser      *int  `yaml:"runAsUser"`
					SeccompProfile struct {
						Type string `yaml:"type"`
					} `yaml:"seccompProfile"`
				} `yaml:"securityContext"`
				TopologySpreadConstraints []struct {
					MaxSkew           int    `yaml:"maxSkew"`
					TopologyKey       string `yaml:"topologyKey"`
					WhenUnsatisfiable string `yaml:"whenUnsatisfiable"`
					LabelSelector     struct {
						MatchLabels map[string]string `yaml:"matchLabels"`
					} `yaml:"labelSelector"`
				} `yaml:"topologySpreadConstraints"`
				Containers []struct {
					Name    string   `yaml:"name"`
					Image   string   `yaml:"image"`
					Command []string `yaml:"command"`
					Ports   []struct {
						Name          string `yaml:"name"`
						ContainerPort int    `yaml:"containerPort"`
					} `yaml:"ports"`
					Env []struct {
						Name  string `yaml:"name"`
						Value string `yaml:"value"`
					} `yaml:"env"`
					EnvFrom []struct {
						ConfigMapRef *struct {
							Name     string `yaml:"name"`
							Optional *bool  `yaml:"optional"`
						} `yaml:"configMapRef"`
						SecretRef *struct {
							Name string `yaml:"name"`
						} `yaml:"secretRef"`
					} `yaml:"envFrom"`
					ReadinessProbe *errorPagesProbe `yaml:"readinessProbe"`
					LivenessProbe  *errorPagesProbe `yaml:"livenessProbe"`
					StartupProbe   *errorPagesProbe `yaml:"startupProbe"`
					Resources      struct {
						Requests map[string]string `yaml:"requests"`
						Limits   map[string]string `yaml:"limits"`
					} `yaml:"resources"`
					SecurityContext struct {
						AllowPrivilegeEscalation *bool `yaml:"allowPrivilegeEscalation"`
						ReadOnlyRootFilesystem   *bool `yaml:"readOnlyRootFilesystem"`
						Capabilities             struct {
							Drop []string `yaml:"drop"`
						} `yaml:"capabilities"`
					} `yaml:"securityContext"`
				} `yaml:"containers"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

type errorPagesService struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Spec struct {
		Type     string            `yaml:"type"`
		Selector map[string]string `yaml:"selector"`
		Ports    []struct {
			Name       string `yaml:"name"`
			Port       int    `yaml:"port"`
			TargetPort any    `yaml:"targetPort"`
			Protocol   string `yaml:"protocol"`
		} `yaml:"ports"`
	} `yaml:"spec"`
}

type errorPagesBudget struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Spec struct {
		MinAvailable   any `yaml:"minAvailable"`
		MaxUnavailable any `yaml:"maxUnavailable"`
		Selector       struct {
			MatchLabels map[string]string `yaml:"matchLabels"`
		} `yaml:"selector"`
	} `yaml:"spec"`
}

// namedDocument decodes the one document of a kind and name from a rendered
// stream into out, failing unless there is exactly one. None is an object that
// is not deployed; two is a render that lists it twice.
func namedDocument(t *testing.T, rendered, kind, name string, out any) {
	t.Helper()
	var found int
	eachDocument(t, rendered, func(k string, doc *yaml.Node) {
		if k != kind {
			return
		}
		var meta struct {
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
		}
		if err := doc.Decode(&meta); err != nil {
			t.Fatalf("decoding a %s: %v", kind, err)
		}
		if meta.Metadata.Name != name {
			return
		}
		found++
		if err := doc.Decode(out); err != nil {
			t.Fatalf("decoding %s/%s: %v", kind, name, err)
		}
	})
	if found != 1 {
		t.Fatalf("the render has %d %s named %q, want exactly 1", found, kind, name)
	}
}

// selects reports whether a label selector picks out a pod carrying labels.
func selects(selector, labels map[string]string) bool {
	if len(selector) == 0 {
		return false
	}
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// assertErrorPagesRender is everything a rendered overlay must say about the
// error pages.
func assertErrorPagesRender(t *testing.T, overlay string) {
	t.Helper()
	rendered := render(t, overlay)

	var dep, edge errorPagesDeployment
	namedDocument(t, rendered, "Deployment", errorPagesName, &dep)
	namedDocument(t, rendered, "Deployment", "edge", &edge)
	var svc errorPagesService
	namedDocument(t, rendered, "Service", errorPagesName, &svc)
	var pdb errorPagesBudget
	namedDocument(t, rendered, "PodDisruptionBudget", errorPagesName, &pdb)

	// --- where it lives
	for _, ns := range []string{dep.Metadata.Namespace, svc.Metadata.Namespace, pdb.Metadata.Namespace} {
		if ns != cloudNamespace {
			t.Errorf("%s: an error-pages object lands in namespace %q, want %q. ingress-nginx resolves the "+
				"default backend in the Ingress's OWN namespace, so anywhere else the annotation names a "+
				"Service that does not exist and is ignored.", overlay, ns, cloudNamespace)
		}
	}

	// --- two replicas, one always up through a roll or a drain
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 2 {
		t.Errorf("%s: error-pages has replicas %v, want 2", overlay, deref(dep.Spec.Replicas))
	}
	if mu := dep.Spec.Strategy.RollingUpdate.MaxUnavailable; mu == nil || *mu != 0 {
		t.Errorf("%s: error-pages rolls with maxUnavailable %v, want 0. It holds no database connection, so "+
			"nothing is gained by draining before surging, and a roll that dips below two ready pods is a window "+
			"in which the fallback itself can be missing.", overlay, deref(mu))
	}
	if pdb.Spec.MinAvailable != 1 || pdb.Spec.MaxUnavailable != nil {
		t.Errorf("%s: the error-pages PodDisruptionBudget has minAvailable %v / maxUnavailable %v, want minAvailable 1",
			overlay, pdb.Spec.MinAvailable, pdb.Spec.MaxUnavailable)
	}

	// --- selectors agree: the Deployment, the Service and the budget all mean the same pods
	podLabels := dep.Spec.Template.Metadata.Labels
	for what, selector := range map[string]map[string]string{
		"Deployment": dep.Spec.Selector.MatchLabels,
		"Service":    svc.Spec.Selector,
		"PDB":        pdb.Spec.Selector.MatchLabels,
	} {
		if !selects(selector, podLabels) {
			t.Errorf("%s: the %s selector %v does not select the error-pages pods (labels %v)", overlay, what, selector, podLabels)
		}
	}
	for _, other := range []string{"edge", "bff", "identity"} {
		var o errorPagesDeployment
		namedDocument(t, rendered, "Deployment", other, &o)
		if selects(svc.Spec.Selector, o.Spec.Template.Metadata.Labels) {
			t.Errorf("%s: the error-pages Service selects %s pods too; the fallback would send traffic to a node", overlay, other)
		}
	}
	// The node PodMonitor (deploy/k8s/monitoring/podmonitor.yaml) scrapes the
	// `http` port of every pod labelled part-of=memql, at /metrics. This server
	// answers /metrics with a 503 page, so a pod carrying the label would sit
	// permanently down in Prometheus. The Deployment object states part-of on its
	// own metadata, as every base object gets it; only the PODS are kept out of
	// the scrape.
	if v, ok := podLabels["app.kubernetes.io/part-of"]; ok {
		t.Errorf("%s: the error-pages pods carry app.kubernetes.io/part-of=%s, which the node PodMonitor selects "+
			"and scrapes at /metrics on the http port -- a 503 page, so a target that is down for good", overlay, v)
	}

	// --- the Service: port 80 -> http, and nothing else
	if svc.Spec.Type != "" && svc.Spec.Type != "ClusterIP" {
		t.Errorf("%s: the error-pages Service is %s, want a ClusterIP: only ingress-nginx talks to it", overlay, svc.Spec.Type)
	}
	if len(svc.Spec.Ports) != 1 {
		t.Fatalf("%s: the error-pages Service lists %d ports, want exactly 1. ingress-nginx sends a custom default "+
			"backend to Spec.Ports[0] and nowhere else, and the probe listener must never be reachable through the Service.",
			overlay, len(svc.Spec.Ports))
	}
	if p := svc.Spec.Ports[0]; p.Port != 80 || p.TargetPort != "http" {
		t.Errorf("%s: the error-pages Service port is %d -> %v, want 80 -> \"http\" (the named public container port)",
			overlay, p.Port, p.TargetPort)
	}

	// --- the one container
	spec := dep.Spec.Template.Spec
	if len(spec.Containers) != 1 || len(spec.InitContainers) != 0 {
		t.Fatalf("%s: error-pages has %d containers and %d init containers, want 1 and 0", overlay,
			len(spec.Containers), len(spec.InitContainers))
	}
	c := spec.Containers[0]

	// The image: the edge's, whatever this overlay makes of it. Every overlay's
	// images block retargets the edge image by name, so a name that drifted
	// (a different registry path, a different image) would be the one workload
	// left on the base's placeholder tag.
	if c.Image == "" || c.Image != edge.Spec.Template.Spec.Containers[0].Image {
		t.Errorf("%s: error-pages runs %q but edge runs %q -- the overlay's images block retargets the edge image by name, "+
			"and this workload must ride the same pin", overlay, c.Image, edge.Spec.Template.Spec.Containers[0].Image)
	}
	if !slices.Equal(c.Command, []string{"/app/memql", "error-pages"}) {
		t.Errorf("%s: error-pages runs command %v, want [/app/memql error-pages] (the runtime image's binary, as dsl-fetch's init container names it)",
			overlay, c.Command)
	}

	// Ports: the public one the Service lists, the probe one it does not.
	ports := map[string]int{}
	for _, p := range c.Ports {
		ports[p.Name] = p.ContainerPort
	}
	if len(ports) != 2 || ports["http"] != 8080 || ports["health"] != 8081 {
		t.Errorf("%s: error-pages ports are %v, want exactly http:8080 and health:8081", overlay, ports)
	}
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env["MEMQL_ERROR_PAGES_ADDR"] != ":8080" || env["MEMQL_ERROR_PAGES_HEALTH_ADDR"] != ":8081" {
		t.Errorf("%s: the listener addresses are %q / %q, want :8080 / :8081 -- the same numbers as the container ports, "+
			"stated here so the subcommand's defaults moving cannot silently move the probe off its port",
			overlay, env["MEMQL_ERROR_PAGES_ADDR"], env["MEMQL_ERROR_PAGES_HEALTH_ADDR"])
	}
	if env["GOMEMLIMIT"] != "100MiB" {
		t.Errorf("%s: GOMEMLIMIT is %q, want 100MiB, under the 128Mi limit so the collector works before the kernel does", overlay, env["GOMEMLIMIT"])
	}

	// Probes: on the probe listener, by name. On the public port /healthz is a
	// 503 by design (it is also what a forwarded /healthz reaches), so a probe
	// there would never pass and the pod would never become ready.
	for what, probe := range map[string]*errorPagesProbe{"readiness": c.ReadinessProbe, "liveness": c.LivenessProbe} {
		if probe == nil {
			t.Errorf("%s: error-pages has no %s probe", overlay, what)
			continue
		}
		if probe.HTTPGet.Path != "/healthz" || probe.HTTPGet.Port != "health" {
			t.Errorf("%s: the %s probe is GET %s on port %v, want GET /healthz on port \"health\" (8081, the listener the "+
				"Service does not list; the public listener answers /healthz with the 503 page)",
				overlay, what, probe.HTTPGet.Path, probe.HTTPGet.Port)
		}
	}
	// A startup probe is optional, but one there must be on the probe listener
	// too: on the public port it would read the 503 page for good, never pass,
	// and have the kubelet restart the container each time it gave up.
	if probe := c.StartupProbe; probe != nil && (probe.HTTPGet.Path != "/healthz" || probe.HTTPGet.Port != "health") {
		t.Errorf("%s: the startup probe is GET %s on port %v, want none or GET /healthz on port \"health\" (the public "+
			"listener answers every path, /healthz included, with the 503 page)", overlay, probe.HTTPGet.Path, probe.HTTPGet.Port)
	}

	// Resources: measured, not guessed. RSS is 53-55 MiB for the memql binary,
	// so a 64Mi request is honest and 128Mi is the ceiling with headroom.
	if got := c.Resources.Requests; got["cpu"] != "10m" || got["memory"] != "64Mi" || len(got) != 2 {
		t.Errorf("%s: error-pages requests %v, want cpu 10m and memory 64Mi", overlay, got)
	}
	if got := c.Resources.Limits; got["memory"] != "128Mi" || len(got) != 1 {
		t.Errorf("%s: error-pages limits %v, want memory 128Mi alone (a CPU limit throttles rather than fails)", overlay, got)
	}

	// Configuration: MEMQL_DOMAIN from memql-domain, the way every engine node
	// gets it, and NOTHING else. No Secret, no connection pool: the instance's
	// connection-budget check refuses a workload that holds memql-secrets
	// without a pool size, and this server needs neither -- it is the thing
	// that must answer when the database is gone.
	var domain bool
	for _, ef := range c.EnvFrom {
		switch {
		case ef.SecretRef != nil:
			t.Errorf("%s: error-pages mounts the Secret %q; it needs no secret, and a workload holding memql-secrets "+
				"must come with a pool size", overlay, ef.SecretRef.Name)
		case ef.ConfigMapRef != nil && ef.ConfigMapRef.Name == "memql-domain":
			domain = true
			if ef.ConfigMapRef.Optional != nil && *ef.ConfigMapRef.Optional {
				t.Errorf("%s: error-pages marks memql-domain optional; every engine node reads it as required, and this "+
					"is the same wiring", overlay)
			}
		default:
			name := ""
			if ef.ConfigMapRef != nil {
				name = ef.ConfigMapRef.Name
			}
			t.Errorf("%s: error-pages mounts the ConfigMap %q; it needs MEMQL_DOMAIN and nothing else (memql-db-pool in "+
				"particular marks a workload as a database client)", overlay, name)
		}
	}
	if !domain {
		t.Errorf("%s: error-pages does not mount the memql-domain ConfigMap, so MEMQL_DOMAIN never reaches it and every "+
			"host is treated as a customer's site: the platform's own hosts would get the neutral page", overlay)
	}

	// It needs nothing from the cluster, so it is given nothing: the engine's
	// ServiceAccount carries the custom-domain Role (it can write Ingresses and
	// Certificates) and the vendor identities, and a page server has no use for
	// any of it. No volumes at all.
	if spec.ServiceAccountName == "memql-engine" {
		t.Errorf("%s: error-pages runs as the memql-engine ServiceAccount, which can create and delete Ingresses and "+
			"Certificates; a page server needs no API access", overlay)
	}
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Errorf("%s: error-pages mounts a ServiceAccount token; it has no use for one", overlay)
	}
	if len(spec.Volumes) != 0 {
		t.Errorf("%s: error-pages declares %d volume(s); it reads and writes nothing", overlay, len(spec.Volumes))
	}

	// Hardened: it serves two static pages, on ports above 1024, from a binary
	// that writes no file. (The subcommand was run under exactly these settings
	// on the runtime image's distroless base before this gate was written.)
	if sc := spec.SecurityContext; sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || sc.RunAsUser == nil || *sc.RunAsUser == 0 ||
		sc.SeccompProfile.Type != "RuntimeDefault" {
		t.Errorf("%s: the error-pages pod security context is %+v, want runAsNonRoot with a non-zero user and the RuntimeDefault seccomp profile", overlay, sc)
	}
	if sc := c.SecurityContext; sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation ||
		sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem || !slices.Equal(sc.Capabilities.Drop, []string{"ALL"}) {
		t.Errorf("%s: the error-pages container security context is %+v, want no privilege escalation, a read-only root and ALL capabilities dropped", overlay, sc)
	}

	// Two replicas that land on one node are one replica at the first node loss.
	// Soft, because a one-node cluster must still schedule both.
	if len(spec.TopologySpreadConstraints) != 1 {
		t.Errorf("%s: error-pages has %d topology spread constraints, want 1", overlay, len(spec.TopologySpreadConstraints))
	} else if tsc := spec.TopologySpreadConstraints[0]; tsc.TopologyKey != "kubernetes.io/hostname" ||
		tsc.WhenUnsatisfiable != "ScheduleAnyway" || tsc.MaxSkew != 1 || !selects(tsc.LabelSelector.MatchLabels, podLabels) {
		t.Errorf("%s: the error-pages spread is %+v, want a soft (ScheduleAnyway) maxSkew 1 spread over kubernetes.io/hostname "+
			"that selects its own pods", overlay, tsc)
	}

	// Its own drain is the subcommand's (it keeps serving for app.DefaultShutdownDrainDelay, then has a 10 s shutdown
	// budget), so the pod must be given longer than both before the kubelet kills it.
	if g := spec.TerminationGracePeriodSeconds; g == nil || *g < 20 {
		t.Errorf("%s: error-pages terminationGracePeriodSeconds is %v, want at least 20 (5 s drain + 10 s shutdown + slack)", overlay, deref(g))
	}
}

// TestTheIngressNginxOverlaysRenderTheErrorPages: cloud and cloud-entry, whose
// front-door Ingresses name the error pages, each compose
// deploy/k8s/components/error-pages and render the whole of it.
func TestTheIngressNginxOverlaysRenderTheErrorPages(t *testing.T) {
	for _, overlay := range errorPagesOverlays {
		t.Run(overlay, func(t *testing.T) {
			assertErrorPagesRender(t, overlay)
		})
	}
}

// TestTheLocalOverlayRendersNoErrorPages is the control for the test above, and
// a ruling in its own right: the local overlay runs no error pages at all.
//
// They used to render there from base, where they bought nothing and cost every
// release-image install. traefik, local's ingress controller, ignores
// ingress-nginx's default-backend annotation, so nothing ever sent a local pod a
// request. And install-cluster-e2e's release-image legs (round-trip, repair)
// install a commit's local overlay on DEFAULT_STACK_TAG's PUBLISHED images
// (editors/vscode/src/install/stackPin.ts, imageTagForVersion; the dsl-fetch
// precedent is memql#5632): no published memql-edge has the error-pages
// subcommand yet, so both pods crash-looped and the install never went ready.
func TestTheLocalOverlayRendersNoErrorPages(t *testing.T) {
	var found []string
	for _, r := range parse(t, render(t, "local")) {
		switch r.Kind {
		case "Deployment", "Service", "PodDisruptionBudget":
			if r.Metadata.Name == errorPagesName {
				found = append(found, r.Kind+"/"+r.Metadata.Name)
			}
		}
	}
	if len(found) > 0 {
		t.Errorf("the local overlay renders %s. The error pages belong to the ingress-nginx overlays only "+
			"(deploy/k8s/components/error-pages, composed by cloud and cloud-entry), for two reasons. traefik, "+
			"local's controller, ignores the %s annotation, so a local pod would never be sent a request. And "+
			"install-cluster-e2e's release-image legs (round-trip, repair) run a commit's local overlay on "+
			"DEFAULT_STACK_TAG's published images, whose memql-edge predates the error-pages subcommand: the pods "+
			"crash-loop and the install never goes ready. Compose the component in an nginx overlay; do not add "+
			"it to base or to local.", strings.Join(found, ", "), defaultBackendAnnotation)
	}
}
