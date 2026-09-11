package overlays

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/frontdoor"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"gopkg.in/yaml.v3"
)

// The worker stream's front-door rule (epic memql#5218, D10).
//
// WorkerService.Stream is served by the AGENT node and by nothing else, and
// gRPC puts the fully qualified service name in the request path. The api
// host's gRPC half was a single `/` catch-all to the bff, so every cockpit
// dialling the documented https://api.<domain> was answered `Unimplemented:
// unknown service znasllc.memql.worker.v1.WorkerService` -- in the local
// cluster and in the cloud alike, for as long as the front door has existed --
// and the machine never registered. The fix is one more rule on the api host,
// above the catch-all, and these are the gates that keep it there.
//
// TWO ASSERTIONS, IN THIS MODULE FOR A REASON. component/frontdoor names the
// prefix (it is a leaf every domain-deriving module imports, so it cannot
// import component/grpc to read the descriptor), and the generated descriptor
// names the service. The two must agree, and the root module already depends
// on both -- so this is where they are held equal. The second assertion is the
// cloud half of overlays/local's TestTheWorkerStreamReachesTheAgent: both
// generated overlays' gRPC Ingress carries the rule, to the agent, before `/`.

// TestTheWorkerServicePathIsTheDescriptorsName holds the hand-spelled prefix
// equal to the generated service name. A proto package rename would move the
// service and leave the front door routing a path nothing serves, which
// presents exactly as the bug the rule fixes.
func TestTheWorkerServicePathIsTheDescriptorsName(t *testing.T) {
	want := "/" + memqlv1.WorkerService_ServiceDesc.ServiceName + "/"
	if frontdoor.WorkerServicePath != want {
		t.Fatalf("frontdoor.WorkerServicePath = %q, but the generated descriptor names the service %q -- want %q; "+
			"the front door would route a prefix no server registers",
			frontdoor.WorkerServicePath, memqlv1.WorkerService_ServiceDesc.ServiceName, want)
	}
	// Every method of the service shares the prefix, which is the whole reason
	// a PREFIX rule routes the stream.
	if !strings.HasPrefix(memqlv1.WorkerService_Stream_FullMethodName, frontdoor.WorkerServicePath) {
		t.Errorf("Stream's full method name %q does not start with the front-door prefix %q",
			memqlv1.WorkerService_Stream_FullMethodName, frontdoor.WorkerServicePath)
	}
}

// grpcPathRule is one entry of an Ingress rule's path list, with the fields
// the worker gate reads. ingressDoc above decodes the backend Service name
// only, and the ORDER and PORT are half of this assertion.
type grpcPathRule struct {
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

// apiHostPaths returns, per Ingress name, the path list every Ingress in a
// rendered stream declares for the api host.
func apiHostPaths(t *testing.T, rendered string) map[string][]grpcPathRule {
	t.Helper()
	apiHost := frontdoor.RoleHost(frontdoor.RoleAPI, committedDomain)

	out := map[string][]grpcPathRule{}
	dec := yaml.NewDecoder(strings.NewReader(rendered))
	for i := 0; ; i++ {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
			Spec struct {
				Rules []struct {
					Host string `yaml:"host"`
					HTTP struct {
						Paths []grpcPathRule `yaml:"paths"`
					} `yaml:"http"`
				} `yaml:"rules"`
			} `yaml:"spec"`
		}
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("decoding document %d of the rendered overlay: %v", i+1, err)
		}
		if doc.Kind != "Ingress" {
			continue
		}
		for _, rule := range doc.Spec.Rules {
			if rule.Host == apiHost {
				out[doc.Metadata.Name] = append(out[doc.Metadata.Name], rule.HTTP.Paths...)
			}
		}
	}
}

// TestTheWorkerStreamReachesTheAgentInEveryGeneratedOverlay is the cloud half:
// each generated overlay's api-front-door-grpc Ingress carries the worker
// prefix to agent:50051, before the `/` catch-all, and no other Ingress on the
// api host carries it.
//
// The name is one of frontDoorIngressNames rather than discovered, because the
// rule has to live in the GRPC Ingress specifically: nginx's backend-protocol
// is Ingress-scoped, and the same path in api-front-door would hand the stream
// to an HTTP/1.1 hop. nginx orders locations by prefix length so the emission
// order does not route it, but the local overlay and the generated one should
// show one shape, and a reader who scans for `/` and stops should already have
// passed it.
func TestTheWorkerStreamReachesTheAgentInEveryGeneratedOverlay(t *testing.T) {
	const grpcIngress = "api-front-door-grpc"
	for _, overlay := range generatedOverlays {
		t.Run(overlay, func(t *testing.T) {
			byIngress := apiHostPaths(t, render(t, overlay))

			for name, paths := range byIngress {
				if name == grpcIngress {
					continue
				}
				for _, p := range paths {
					if p.Path == frontdoor.WorkerServicePath {
						t.Errorf("Ingress %q on the api host declares %q; the rule belongs in %q, whose backend-protocol is GRPC",
							name, p.Path, grpcIngress)
					}
				}
			}

			paths, ok := byIngress[grpcIngress]
			if !ok {
				t.Fatalf("the %s overlay has no Ingress %q on the api host", overlay, grpcIngress)
			}
			worker, catchAll := -1, -1
			for i, p := range paths {
				switch p.Path {
				case frontdoor.WorkerServicePath:
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
				t.Fatalf("%s carries no rule for %q, so a cockpit dialling https://api.<domain> is answered "+
					"`Unimplemented: unknown service` by the bff's catch-all and never registers (epic memql#5218, D10)",
					grpcIngress, frontdoor.WorkerServicePath)
			}
			if catchAll < 0 {
				t.Fatalf("%s carries no `/` catch-all, so this ordering assertion would be vacuous", grpcIngress)
			}
			if worker > catchAll {
				t.Errorf("the worker rule is declared at index %d, after the `/` catch-all at %d; cmd/frontdoorhosts emits it first",
					worker, catchAll)
			}
		})
	}
}


// TestTheWorkerStreamIngressIdleTimeoutsAndSticky gates the annotations that
// stop Jose's 408@60.000s on api-front-door-grpc and keep reconnects on one
// of the two agent pods. Read/send must be hours (well above the ~30s gRPC
// keepalive); upstream-hash-by pins by client address (MCP cookie affinity is
// the HTTP prior art; gRPC-go has no cookie jar).
func TestTheWorkerStreamIngressIdleTimeoutsAndSticky(t *testing.T) {
	const grpcIngress = "api-front-door-grpc"
	want := map[string]string{
		"nginx.ingress.kubernetes.io/backend-protocol":    "GRPC",
		"nginx.ingress.kubernetes.io/proxy-read-timeout":  "14400",
		"nginx.ingress.kubernetes.io/proxy-send-timeout":  "14400",
		"nginx.ingress.kubernetes.io/upstream-hash-by":    "$binary_remote_addr",
	}
	for _, overlay := range generatedOverlays {
		t.Run(overlay, func(t *testing.T) {
			anns := apiHostIngressAnnotations(t, render(t, overlay))[grpcIngress]
			if anns == nil {
				t.Fatalf("no annotations on %s", grpcIngress)
			}
			for k, v := range want {
				if got := anns[k]; got != v {
					t.Errorf("%s: annotation %q = %q, want %q (WorkerService idle/sticky)", grpcIngress, k, got, v)
				}
			}
			readSec := anns["nginx.ingress.kubernetes.io/proxy-read-timeout"]
			if readSec == "60" || readSec == "" {
				t.Fatalf("proxy-read-timeout still default-ish %q; Jose saw HTTP 408 at exactly 60.000s", readSec)
			}
		})
	}
}

// apiHostIngressAnnotations returns annotation maps keyed by Ingress name for
// every Ingress that declares a rule on the api host.
func apiHostIngressAnnotations(t *testing.T, rendered string) map[string]map[string]string {
	t.Helper()
	apiHost := frontdoor.RoleHost(frontdoor.RoleAPI, committedDomain)
	out := map[string]map[string]string{}
	dec := yaml.NewDecoder(strings.NewReader(rendered))
	for i := 0; ; i++ {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name        string            `yaml:"name"`
				Annotations map[string]string `yaml:"annotations"`
			} `yaml:"metadata"`
			Spec struct {
				Rules []struct {
					Host string `yaml:"host"`
				} `yaml:"rules"`
			} `yaml:"spec"`
		}
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("decoding document %d: %v", i+1, err)
		}
		if doc.Kind != "Ingress" {
			continue
		}
		for _, rule := range doc.Spec.Rules {
			if rule.Host == apiHost {
				out[doc.Metadata.Name] = doc.Metadata.Annotations
			}
		}
	}
}
