// Package local — see render_domain_test.go for why these are tests and not
// reviews. This file covers the front door itself: which hosts it serves, and
// which entrances no longer exist.
package local

import (
	"errors"
	"io"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/frontdoor"
	"gopkg.in/yaml.v3"
)

// The seven hosts the front door serves (design D3, plus the platform sites'
// own exact rules from memql#4224: the OS shell and the VS Code landing page).
// The COUNT is the invariant: it must not grow with customers, apps or sites.
//
// COMPUTED from component/frontdoor rather than listed, since memql#3767. Local
// is ONE environment (TestLocalStaysOneEnvironment) and it is the UNPREFIXED
// one, so its host set is exactly what the derivation yields for the committed
// default domain -- the same set cmd/frontdoorhosts writes into the two cloud
// overlays.
//
// The point of computing it is that this overlay's six front-door files stay
// HAND-AUTHORED, and deliberately so: they are traefik rather than nginx, and
// they carry the measured reasoning for a priority ranking that broke the API
// once already (memql#3810). Hand-authored is not the same as unchecked. This
// binds them to the same derivation the generator uses, so local's committed
// defaults cannot drift from what the cloud overlay serves -- which is what
// would make the local cluster stop proving anything about the cloud one.
//
// The platform sites are the hosts this gate most needs to keep honest: locally
// the mkcert wildcard covers os.memql.localhost and vscode.memql.localhost
// whether or not an exact rule exists, so a developer would never notice a rule
// missing. In the cloud the certificate names exact hosts only (HTTP-01 cannot
// issue a wildcard), and without the rule the site serves the ingress
// controller's self-signed default. Same seven rules everywhere is what lets
// local prove the shape. (It was the portal's rule that taught this, and the
// portal was retired in epic memql#4984 -- the OS inherited the exception
// whole, the VS Code landing page joined it in memql#5518, and
// TestEachPlatformSiteRuleReachesTheEdge below is the portal test generalized
// rather than a new one.)
var frontDoorHosts = func() []string {
	var out []string
	for _, h := range frontdoor.Hosts("memql.localhost") {
		out = append(out, h.Name)
	}
	return out
}()

// hostsIn returns every Ingress rule host in the rendered overlay. Parsed by
// line rather than with a YAML decoder because the rendered stream is many
// documents of many kinds, and the only thing wanted is `host:` under
// spec.rules — which is unambiguous at the text level and needs no schema.
func hostsIn(rendered string) map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(rendered, "\n") {
		t := strings.TrimSpace(line)
		t = strings.TrimPrefix(t, "- ")
		if !strings.HasPrefix(t, "host: ") {
			continue
		}
		h := strings.Trim(strings.TrimSpace(strings.TrimPrefix(t, "host: ")), `"'`)
		if h != "" {
			out[h] = true
		}
	}
	return out
}

func TestFrontDoorServesExactlyTheDerivedHosts(t *testing.T) {
	got := hostsIn(render(t))

	for _, want := range frontDoorHosts {
		if !got[want] {
			t.Errorf("front door does not serve %q", want)
		}
	}
	if len(got) != len(frontDoorHosts) {
		var extra []string
		want := map[string]bool{}
		for _, h := range frontDoorHosts {
			want[h] = true
		}
		for h := range got {
			if !want[h] {
				extra = append(extra, h)
			}
		}
		sort.Strings(extra)
		t.Errorf("front door serves %d hosts, want %d; unexpected: %v",
			len(got), len(frontDoorHosts), extra)
	}
}

// D4: the endpoint is named for its role. Pre-release, so there is no alias
// and no redirect — the old name is simply gone.
func TestCockpitHostIsGone(t *testing.T) {
	if strings.Contains(render(t), "cockpit.") {
		t.Error("the rendered overlay still names a cockpit. host; D4 renamed it to api.")
	}
}

// A second entrance is a connection path that exists in one environment and
// not the others, which is what environment-parity.md forbids. identity was
// reachable both through the front door and directly on host port 8085.
func TestNoSecondEntranceToIdentity(t *testing.T) {
	if strings.Contains(render(t), "identity-external") {
		t.Error("identity-external still exists; identity is reachable only through the front door")
	}
}

// TestEachPlatformSiteRuleReachesTheEdge: every platform site
// (frontdoor.PlatformSites) has a hand-authored exact rule here, pointing at
// svc/edge and declaring no router.priority -- the local mirror of the rule the
// cloud generator emits per site. Iterated rather than written per site so a
// site the derivation names and this overlay forgot fails here rather than
// being covered by the mkcert wildcard and noticed only in the cloud.
func TestEachPlatformSiteRuleReachesTheEdge(t *testing.T) {
	rendered := render(t)

	for _, site := range frontdoor.PlatformSites() {
		host := frontdoor.PlatformSiteHost(site, "memql.localhost")

		var found bool
		for _, doc := range strings.Split(rendered, "\n---\n") {
			if !strings.Contains(doc, "kind: Ingress") || !strings.Contains(doc, "host: "+host) {
				continue
			}
			found = true
			if !strings.Contains(doc, "name: edge") {
				t.Errorf("the Ingress serving %q does not point at svc/edge; a platform site is a site and takes the site path", host)
			}
			if strings.Contains(doc, "router.priority") {
				t.Errorf("the Ingress serving %q declares a router.priority; precedence is declared on the wildcard only (memql#3810)", host)
			}
		}
		if !found {
			t.Errorf("no Ingress in the rendered overlay carries an exact rule for %q (platform site %q)", host, site)
		}
	}
}

// apiPathRule is one entry of an Ingress rule's path list, reduced to what
// TestTheWorkerStreamReachesTheAgent reasons about.
type apiPathRule struct {
	Path    string `yaml:"path"`
	Backend struct {
		Service struct {
			Name string `yaml:"name"`
			Port struct {
				Number int `yaml:"number"`
			} `yaml:"port"`
		} `yaml:"service"`
	} `yaml:"backend"`
}

// apiIngressPaths returns the path list of the api host's Ingress, in the
// order the manifest declares it. Decoded with a schema rather than by line
// because the ORDER of two entries is the assertion, and a line scan that
// found both would have to reconstruct which rule each backend belongs to.
func apiIngressPaths(t *testing.T, rendered string) []apiPathRule {
	t.Helper()
	apiHost := frontdoor.RoleHost(frontdoor.RoleAPI, "memql.localhost")

	var doc struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Name string `yaml:"name"`
		} `yaml:"metadata"`
		Spec struct {
			Rules []struct {
				Host string `yaml:"host"`
				HTTP struct {
					Paths []apiPathRule `yaml:"paths"`
				} `yaml:"http"`
			} `yaml:"rules"`
		} `yaml:"spec"`
	}
	dec := yaml.NewDecoder(strings.NewReader(rendered))
	for i := 0; ; i++ {
		doc.Spec.Rules = nil
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decoding document %d of the rendered overlay: %v", i+1, err)
		}
		if doc.Kind != "Ingress" {
			continue
		}
		for _, rule := range doc.Spec.Rules {
			if rule.Host == apiHost {
				return rule.HTTP.Paths
			}
		}
	}
	t.Fatalf("no Ingress in the rendered overlay carries a rule for %q", apiHost)
	return nil
}

// TestTheWorkerStreamReachesTheAgent is the local half of epic memql#5218's
// D10: the api host carries the worker stream's service prefix to the agent,
// ABOVE the h2c catch-all.
//
// WorkerService.Stream is served by the agent node and by nothing else, and
// gRPC puts the fully qualified service name in the request path. With only
// the `/` catch-all, every cockpit dialling the documented https://api.<domain>
// was answered `Unimplemented: unknown service` by the bff -- locally and in
// the cloud alike, for as long as the front door has existed -- and the
// machine never registered. This is the assertion that the hand-authored
// local front door carries the rule the cloud generator carries (the
// counterpart lives in ../frontdoor_worker_test.go, over both generated
// overlays), so local keeps proving the shape.
//
// Three things are pinned, because each failed silently on its own:
//
//   - the rule exists, with the exact prefix component/frontdoor names and the
//     agent's gRPC port behind it;
//   - it is declared BEFORE the catch-all. traefik ranks by rule length so the
//     order is not what routes it, but a reader scanning for `/` stops there,
//     and the local file and the generated one should show one shape;
//   - the agent Service carries the h2c serversscheme annotation the bff's
//     does. traefik's backend scheme is per-SERVICE, so the rule alone hands
//     the cockpit an HTTP/1.1 hop to a gRPC port, which fails with a protocol
//     error naming nothing -- the same failure a missing rule produces, from
//     the other side.
func TestTheWorkerStreamReachesTheAgent(t *testing.T) {
	rendered := render(t)
	paths := apiIngressPaths(t, rendered)

	worker, catchAll := -1, -1
	for i, p := range paths {
		switch p.Path {
		case frontdoor.WorkerServicePath:
			if worker >= 0 {
				t.Errorf("the api host declares %q twice", p.Path)
			}
			worker = i
			if p.Backend.Service.Name != "agent" || p.Backend.Service.Port.Number != 50051 {
				t.Errorf("%q reaches %s:%d, want agent:50051 -- WorkerService.Stream is registered on the agent node and nowhere else",
					p.Path, p.Backend.Service.Name, p.Backend.Service.Port.Number)
			}
		case "/":
			catchAll = i
		}
	}
	if worker < 0 {
		t.Fatalf("the api host carries no rule for %q, so a cockpit dialling https://api.<domain> is answered "+
			"`Unimplemented: unknown service` by the bff's catch-all and never registers (epic memql#5218, D10)",
			frontdoor.WorkerServicePath)
	}
	if catchAll < 0 {
		t.Fatal("the api host carries no `/` catch-all, so this ordering assertion would be vacuous")
	}
	if worker > catchAll {
		t.Errorf("the worker rule is declared at index %d, after the `/` catch-all at %d; it belongs above the catch-all, "+
			"where the generated overlays put it", worker, catchAll)
	}

	// The hop, not just the rule. Read the same way for both Services so the
	// bff's annotation is the control that the reader works.
	for _, svc := range []string{"bff", "agent"} {
		if got := serviceAnnotation(t, rendered, svc, "traefik.ingress.kubernetes.io/service.serversscheme"); got != "h2c" {
			t.Errorf("Service %q carries serversscheme %q, want h2c: the front door routes gRPC to it, and traefik's "+
				"backend scheme is per-Service, so without the annotation the hop is HTTP/1.1 to a gRPC port", svc, got)
		}
	}
}

// serviceAnnotation reads one annotation off the named Service in a rendered
// stream, or "" when the Service or the annotation is absent.
func serviceAnnotation(t *testing.T, rendered, service, key string) string {
	t.Helper()
	var doc struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Name        string            `yaml:"name"`
			Annotations map[string]string `yaml:"annotations"`
		} `yaml:"metadata"`
	}
	dec := yaml.NewDecoder(strings.NewReader(rendered))
	for i := 0; ; i++ {
		doc.Metadata.Annotations = nil
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decoding document %d of the rendered overlay: %v", i+1, err)
		}
		if doc.Kind == "Service" && doc.Metadata.Name == service {
			return doc.Metadata.Annotations[key]
		}
	}
	t.Fatalf("no Service named %q in the rendered overlay", service)
	return ""
}
