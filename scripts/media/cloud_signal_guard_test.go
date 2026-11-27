package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"
)

type cloudHTTPRoute struct {
	Match []struct {
		Method     []string            `json:"method"`
		Path       []string            `json:"path"`
		Header     map[string][]string `json:"header"`
		PathRegexp struct {
			Pattern string `json:"pattern"`
		} `json:"path_regexp"`
	} `json:"match"`
	Handle []struct {
		Handler    string `json:"handler"`
		URI        string `json:"uri"`
		StatusCode int    `json:"status_code"`
		MaxSize    int    `json:"max_size"`
		Upstreams  []struct {
			Dial string `json:"dial"`
		} `json:"upstreams"`
		Headers struct {
			Request struct {
				Delete []string            `json:"delete"`
				Set    map[string][]string `json:"set"`
			} `json:"request"`
		} `json:"headers"`
		Transport struct {
			Protocol string `json:"protocol"`
			TLS      struct {
				ServerName         string `json:"server_name"`
				InsecureSkipVerify bool   `json:"insecure_skip_verify"`
			} `json:"tls"`
		} `json:"transport"`
	} `json:"handle"`
	Terminal bool `json:"terminal"`
}

func cloudGuardRoutes(t *testing.T, s *serverFixture) []cloudHTTPRoute {
	t.Helper()
	var config struct {
		Logging struct {
			Logs map[string]struct {
				Exclude []string `json:"exclude"`
			} `json:"logs"`
		} `json:"logging"`
		Apps struct {
			HTTP struct {
				Servers map[string]struct {
					Listen         []string `json:"listen"`
					AutomaticHTTPS struct {
						Disable bool `json:"disable"`
					} `json:"automatic_https"`
					Routes []cloudHTTPRoute `json:"routes"`
				} `json:"servers"`
			} `json:"http"`
		} `json:"apps"`
	}
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(s.etc, "caddy.json"))), &config); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(config.Logging.Logs["default"].Exclude, "http.log.error") {
		t.Fatal("provider URL error logging must not retain access_token")
	}
	server, ok := config.Apps.HTTP.Servers["media_guard"]
	if !ok || len(config.Apps.HTTP.Servers) != 1 || !slices.Equal(server.Listen, []string{"127.0.0.1:7884"}) || !server.AutomaticHTTPS.Disable {
		t.Fatal("the HTTP admission boundary must have only its private loopback listener")
	}
	expected := 5
	if _, err := os.Stat(filepath.Join(s.etc, "ingress.yaml")); err == nil {
		expected = 7
	}
	if len(server.Routes) != expected {
		t.Fatalf("want fixed native/control/health/deny and only enabled WHIP routes; got %d, want %d", len(server.Routes), expected)
	}
	for _, route := range server.Routes {
		if !route.Terminal {
			t.Fatal("every route must stop before less restrictive routes")
		}
		for _, match := range route.Match {
			for _, path := range match.Path {
				if strings.ContainsAny(path, "*?{") {
					t.Fatalf("route is not an exact path: %q", path)
				}
			}
		}
	}
	return server.Routes
}

// This checks the rendered routing contract, not LiveKit's authentication itself.
// Cloud acceptance must additionally exercise the pinned Caddy and provider at the
// real target: a participant JWT never substitutes for the native guard or an admin JWT.
func TestCloudSignallingGuardRoutesExactNativeBoundary(t *testing.T) {
	s := newServerFixture(t)
	if _, _, code := s.configure(t, "--wait-seconds=0"); code != 0 {
		t.Fatalf("configure exit=%d", code)
	}
	routes := cloudGuardRoutes(t, s)
	guardBytes := sha256.Sum256([]byte("memql-media-signal-v1:" + s.values["MEMQL_CALLS_MEDIA_API_SECRET"]))
	guard := hex.EncodeToString(guardBytes[:])
	rtcPaths := []string{"/rtc", "/rtc/validate", "/rtc/v1", "/rtc/v1/validate"}
	for _, index := range []int{0, 1} {
		if len(routes[index].Match) != 1 || !slices.Equal(routes[index].Match[0].Method, []string{"GET"}) || !slices.Equal(routes[index].Match[0].Path, rtcPaths) {
			t.Fatalf("RTC route %d must match exactly the four GET protocol paths", index)
		}
	}
	if len(routes[0].Match[0].Header) != 1 || !slices.Equal(routes[0].Match[0].Header["X-MemQL-Media-Signal"], []string{guard}) {
		t.Fatal("only the exact native purpose-derived guard can select direct upstream RTC")
	}
	if len(routes[1].Match[0].Header) != 0 {
		t.Fatal("public RTC must still reach native admission without a guard")
	}
	for _, index := range []int{0, 2} {
		route := routes[index]
		if len(route.Handle) != 1 || route.Handle[0].Handler != "reverse_proxy" || len(route.Handle[0].Upstreams) != 1 || route.Handle[0].Upstreams[0].Dial != "127.0.0.1:7880" || !slices.Contains(route.Handle[0].Headers.Request.Delete, "X-MemQL-Media-Signal") {
			t.Fatalf("guarded provider route %d must have only the fixed loopback destination and remove its guard", index)
		}
		if slices.Contains(route.Handle[0].Headers.Request.Delete, "Authorization") {
			t.Fatal("provider control must retain its separate signed administrative authorization")
		}
	}
	public := routes[1]
	if len(public.Handle) != 2 || public.Handle[0].Handler != "rewrite" || public.Handle[0].URI != "/media{http.request.uri}" {
		t.Fatal("public RTC must preserve its path/query under the fixed native /media prefix")
	}
	proxy := public.Handle[1]
	if proxy.Handler != "reverse_proxy" || len(proxy.Upstreams) != 1 || proxy.Upstreams[0].Dial != "api.example.com:443" || !slices.Equal(proxy.Headers.Request.Set["Host"], []string{"api.example.com"}) || !slices.Contains(proxy.Headers.Request.Delete, "X-MemQL-Media-Signal") || proxy.Transport.Protocol != "http" || proxy.Transport.TLS.ServerName != "api.example.com" || proxy.Transport.TLS.InsecureSkipVerify {
		t.Fatal("public RTC must strip caller guards and use one fixed, certificate-verified native API destination")
	}
	if len(routes[3].Match) != 1 || !slices.Equal(routes[3].Match[0].Method, []string{"GET"}) || !slices.Equal(routes[3].Match[0].Path, []string{"/"}) || len(routes[3].Handle) != 1 || len(routes[3].Handle[0].Upstreams) != 1 || routes[3].Handle[0].Upstreams[0].Dial != "127.0.0.1:7880" {
		t.Fatal("only exact root GET may expose provider health")
	}
	if len(routes[4].Match) != 0 || len(routes[4].Handle) != 1 || routes[4].Handle[0].Handler != "static_response" || routes[4].Handle[0].StatusCode != http.StatusNotFound {
		t.Fatal("unmatched methods, aliases, metrics and arbitrary destinations must fail closed")
	}
	for _, tc := range []struct {
		method, path, header string
		want                 int
	}{
		{"GET", "/rtc", "", 1}, {"GET", "/rtc/v1", "issued-participant-JWT", 1},
		{"GET", "/rtc/v1/validate", "wrong-guard", 1}, {"GET", "/rtc/validate", guard, 0},
		{"POST", "/rtc", guard, 4}, {"GET", "/rtc/extra", guard, 4},
		{"GET", "/media/rtc", guard, 4}, {"GET", "/metrics", guard, 4},
		{"GET", "/twirp/livekit.RoomService/ListRooms", guard, 4},
		{"POST", "/twirp/livekit.RoomService/ListRooms", "", 2},
		{"POST", "/twirp/livekit.Ingress/UpdateIngress", guard, 4},
		{"GET", "/", "", 3}, {"POST", "/", "", 4},
	} {
		selected := -1
		for index, route := range routes {
			matched := len(route.Match) == 0
			for _, m := range route.Match {
				matched = slices.Contains(m.Method, tc.method) && slices.Contains(m.Path, tc.path)
				for name, values := range m.Header {
					matched = matched && name == "X-MemQL-Media-Signal" && slices.Contains(values, tc.header)
				}
				if matched {
					break
				}
			}
			if matched {
				selected = index
				break
			}
		}
		if selected != tc.want {
			t.Errorf("%s %s selected route%d, want%d", tc.method, tc.path, selected, tc.want)
		}
	}
	if strings.Contains(readFile(t, s.log), guard) {
		t.Fatal("private signal guard reached command argv or environment")
	}
}

func TestCloudIngressControlUsesOnlyPinnedProtocolMethods(t *testing.T) {
	s := newServerFixture(t)
	if _, _, code := s.configure(t, "--wait-seconds=0"); code != 0 {
		t.Fatalf("configure exit=%d", code)
	}
	control := cloudGuardRoutes(t, s)[2]
	if len(control.Match) != 1 || !slices.Equal(control.Match[0].Method, []string{"POST"}) {
		t.Fatal("provider control must require POST")
	}
	transport := &ingressContractHTTPClient{}
	client := livekit.NewIngressProtobufClient("http://fixture.invalid", transport)
	ctx := context.Background()
	_, _ = client.CreateIngress(ctx, &livekit.CreateIngressRequest{})
	_, _ = client.ListIngress(ctx, &livekit.ListIngressRequest{})
	_, _ = client.DeleteIngress(ctx, &livekit.DeleteIngressRequest{})
	_, _ = client.UpdateIngress(ctx, &livekit.UpdateIngressRequest{})
	for index, request := range transport.requests {
		if slices.Contains(control.Match[0].Path, request.URL.Path) != (index < 3) {
			t.Errorf("unexpected allowance of generated method %s", request.URL.Path)
		}
	}
	if len(control.Match[0].Path) != 12 {
		t.Fatal("control surface expanded beyond nine RoomService and three Ingress operations")
	}
}

// This opt-in is entirely local and starts no cloud service or provider. It checks
// the actual pinned production Caddy modules against the rendered JSON, with all
// networking disabled so validation cannot issue a certificate or reach an API.
func TestCloudSignallingGuardPinnedCaddyValidation(t *testing.T) {
	if os.Getenv("MEMQL_TEST_MEDIA_CLOUD_GUARD") != "1" {
		t.Skip("set MEMQL_TEST_MEDIA_CLOUD_GUARD=1 for local pinned-Caddy validation")
	}
	for _, ingress := range []bool{false, true} {
		t.Run(fmt.Sprintf("ingress=%v", ingress), func(t *testing.T) {
			s := newServerFixture(t)
			args := []string{"--wait-seconds=0"}
			if ingress {
				args = append(args, "--ingress=true")
			}
			if _, _, code := s.configure(t, args...); code != 0 {
				t.Fatalf("configure exit=%d", code)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", "none", "--read-only", "--tmpfs", "/data", "--tmpfs", "/config", "--volume", filepath.Join(s.etc, "caddy.json")+":/etc/caddy/caddy.json:ro", "livekit/caddyl4:v2.11.3@sha256:9dd090159a6042a907dbff0ce9dda27a21d2a559dc7b03b60a8e6b1b60b8348e", "validate", "--config", "/etc/caddy/caddy.json")
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("pinned Caddy rejected rendered config: %v\n%s", err, output)
			}
		})
	}
}
