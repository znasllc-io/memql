package media

// media_server_command_test.go -- memql-media, the command a rented media server's first
// boot installs (deploy/media/cloud-init.yaml.tmpl) and the operator's step runs over SSH:
//
//	ssh root@<ipv4> memql-media configure --domain=<instance domain> < media.env
//
// The command and the LiveKit template are taken from the cloud-init the provision script
// renders, written into a directory standing in for the server, and run there against a
// fake docker and a fake curl (the metadata service and the answer through TLS). What is
// asserted is the operator's step itself: the key pairs arrive on stdin and land in one
// file, 0600, and nowhere else -- not in the output, not in docker's argv or environment.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// fakeServerDocker answers what memql-media asks of Docker: the Compose version, which
// services run (FAKE_RUNNING, one name a line), starting them, and Redis's room count.
const fakeServerDocker = `#!/usr/bin/env bash
{ printf 'argv'; for a in "$@"; do printf ' %s' "$a"; done; printf '\n'; env | grep -i -e secret -e key | sort; } >> "$FAKE_SERVER_LOG"
case "$*" in
  "compose version"*) printf 'Docker Compose version 2.40.3\n'; exit 0 ;;
  *" ps "*) [ -n "$FAKE_RUNNING" ] && printf '%s\n' $FAKE_RUNNING; exit 0 ;;
  *" up "*) [ -n "$FAKE_UP_FAILS" ] && { printf 'Error response from daemon: boom\n' >&2; exit 1; }; exit 0 ;;
  *"redis-cli"*) printf '%s\n' "$FAKE_ROOMS"; exit 0 ;;
esac
exit 0
`

// fakeServerCurl is the metadata service (FAKE_PUBLIC_IPV4; empty fails as an unreachable
// service does) and LiveKit's answer through Caddy (OK when FAKE_ANSWERS is set).
const fakeServerCurl = `#!/usr/bin/env bash
printf 'curl %s\n' "$*" >> "$FAKE_SERVER_LOG"
for a in "$@"; do
  case "$a" in
    */hetzner/v1/metadata/public-ipv4)
      [ -n "$FAKE_PUBLIC_IPV4" ] || exit 7
      printf '%s' "$FAKE_PUBLIC_IPV4"; exit 0 ;;
    https://*/)
      [ -n "$FAKE_ANSWERS" ] || exit 7
      printf 'OK'; exit 0 ;;
  esac
done
exit 2
`

// serverFixture is one media server after its first boot: the command, the template and
// the compose file where cloud-init wrote them, under a directory of this test's.
type serverFixture struct {
	dir, etc, data, bin, log, command string
	values                            map[string]string
	running, rooms, publicIPv4        string
	answers, upFails                  bool
}

func newServerFixture(t *testing.T) *serverFixture {
	t.Helper()
	c := parseCloudInit(t, renderedCloudInit(t))
	dir := t.TempDir()
	s := &serverFixture{
		dir:        dir,
		etc:        filepath.Join(dir, "etc", "memql-media"),
		data:       filepath.Join(dir, "var", "lib", "memql-media"),
		bin:        filepath.Join(dir, "bin"),
		log:        filepath.Join(dir, "server.log"),
		command:    filepath.Join(dir, "memql-media"),
		publicIPv4: "203.0.113.7",
		answers:    true,
		values: map[string]string{
			"MEMQL_CALLS_MEDIA_PROVIDER":       "hetzner",
			"MEMQL_CALLS_MEDIA_API_URL":        "https://m1.media.example.com",
			"MEMQL_CALLS_MEDIA_PUBLIC_URL":     "wss://m1.media.example.com",
			"MEMQL_CALLS_MEDIA_SEATS":          "50",
			"MEMQL_CALLS_MEDIA_API_KEY":        "callsapi" + randomHex(t, 12),
			"MEMQL_CALLS_MEDIA_API_SECRET":     randomHex(t, 32),
			"MEMQL_CALLS_MEDIA_WEBHOOK_KEY":    "callshook" + randomHex(t, 12),
			"MEMQL_CALLS_MEDIA_WEBHOOK_SECRET": randomHex(t, 32),
		},
	}
	for _, d := range []string{s.etc, s.data, s.bin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join(s.bin, "docker"): fakeServerDocker,
		filepath.Join(s.bin, "curl"):   fakeServerCurl,
	}
	for _, w := range []struct{ from, to string }{
		{"/usr/local/sbin/memql-media", s.command},
		{"/etc/memql-media/livekit.yaml.tmpl", filepath.Join(s.etc, "livekit.yaml.tmpl")},
		{"/etc/memql-media/compose.yaml", filepath.Join(s.etc, "compose.yaml")},
	} {
		content, _ := writtenFile(t, c, w.from)
		files[w.to] = content
	}
	for path, body := range files {
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func (s *serverFixture) stdin() string {
	var b strings.Builder
	b.WriteString("# the values the memql-calls-media Secret is made from\n")
	for _, k := range MediaValueNames() {
		if v, ok := s.values[k]; ok {
			b.WriteString(k + "=" + v + "\n")
		}
	}
	return b.String()
}

// MediaValueNames is the eight, in integrations/calls's order.
func MediaValueNames() []string {
	return []string{
		"MEMQL_CALLS_MEDIA_PROVIDER", "MEMQL_CALLS_MEDIA_API_URL", "MEMQL_CALLS_MEDIA_PUBLIC_URL", "MEMQL_CALLS_MEDIA_SEATS",
		"MEMQL_CALLS_MEDIA_API_KEY", "MEMQL_CALLS_MEDIA_API_SECRET", "MEMQL_CALLS_MEDIA_WEBHOOK_KEY", "MEMQL_CALLS_MEDIA_WEBHOOK_SECRET",
	}
}

func (s *serverFixture) secrets() []string {
	return []string{
		s.values["MEMQL_CALLS_MEDIA_API_KEY"], s.values["MEMQL_CALLS_MEDIA_API_SECRET"],
		s.values["MEMQL_CALLS_MEDIA_WEBHOOK_KEY"], s.values["MEMQL_CALLS_MEDIA_WEBHOOK_SECRET"],
	}
}

func flag(on bool) string {
	if on {
		return "1"
	}
	return ""
}

// run executes memql-media on the fixture's server with stdin, and holds it to the rule
// every run keeps: no key or secret in its output.
func (s *serverFixture) run(t *testing.T, stdin string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{s.command}, args...)...)
	cmd.Dir = s.dir
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Env = append(os.Environ(),
		"PATH="+s.bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"MEMQL_MEDIA_DIR="+s.etc,
		"MEMQL_MEDIA_DATA_DIR="+s.data,
		"MEMQL_MEDIA_CLOUD_INIT=no-cloud-init-here",
		"FAKE_SERVER_LOG="+s.log,
		"FAKE_RUNNING="+s.running,
		"FAKE_ROOMS="+s.rooms,
		"FAKE_PUBLIC_IPV4="+s.publicIPv4,
		"FAKE_ANSWERS="+flag(s.answers),
		"FAKE_UP_FAILS="+flag(s.upFails),
	)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running memql-media: %v", err)
	}
	t.Logf("memql-media %v: exit=%d\nstdout: %s\nstderr:\n%s", args, code, stdout.String(), stderr.String())
	for _, secret := range s.secrets() {
		if len(secret) >= 12 && strings.Contains(stdout.String()+stderr.String(), secret) {
			t.Fatal("memql-media printed a key or a secret")
		}
	}
	return stdout.String(), stderr.String(), code
}

func (s *serverFixture) configure(t *testing.T, extra ...string) (string, string, int) {
	t.Helper()
	return s.run(t, s.stdin(), append([]string{"configure", "--domain=example.com", "--wait-seconds=5"}, extra...)...)
}

func (s *serverFixture) dockerCalls(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(s.log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "argv ") {
			out = append(out, strings.TrimPrefix(line, "argv "))
		}
	}
	return out
}

func (s *serverFixture) upCalls(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, c := range s.dockerCalls(t) {
		if strings.Contains(c, " up ") {
			out = append(out, c)
		}
	}
	return out
}

// caddyRoute is one layer-4 route of the server's Caddy configuration.
type caddyRoute struct {
	Match []struct {
		TLS struct {
			SNI []string `json:"sni"`
		} `json:"tls"`
	} `json:"match"`
	Handle []struct {
		Handler            string `json:"handler"`
		ProxyProtocol      string `json:"proxy_protocol"`
		ConnectionPolicies []struct {
			ALPN []string `json:"alpn"`
		} `json:"connection_policies"`
		Upstreams []struct {
			Dial []string `json:"dial"`
		} `json:"upstreams"`
	} `json:"handle"`
}

type caddyConfig struct {
	Admin struct {
		Disabled bool `json:"disabled"`
	} `json:"admin"`
	Storage struct {
		Module string `json:"module"`
		Root   string `json:"root"`
	} `json:"storage"`
	Apps struct {
		TLS struct {
			Certificates struct {
				Automate []string `json:"automate"`
			} `json:"certificates"`
		} `json:"tls"`
		Layer4 struct {
			Servers map[string]struct {
				Listen []string     `json:"listen"`
				Routes []caddyRoute `json:"routes"`
			} `json:"servers"`
		} `json:"layer4"`
	} `json:"apps"`
}

// TestConfigureRendersTheServerFromTheValuesOnStdin is the operator's step: the eight
// values on stdin become LiveKit's configuration -- the local target's template, filled
// for a host with a public address -- and Caddy's, and the server is started.
func TestConfigureRendersTheServerFromTheValuesOnStdin(t *testing.T) {
	s := newServerFixture(t)
	stdout, _, code := s.configure(t)
	if code != 0 {
		t.Fatalf("configure exited %d", code)
	}
	if !strings.Contains(stdout, "wss://m1.media.example.com") {
		t.Errorf("configure does not say where browsers connect: %q", stdout)
	}

	path := filepath.Join(s.etc, "livekit.yaml")
	if m := mode(t, path); m != 0o600 {
		t.Errorf("livekit.yaml is %o, want 600: it holds both key pairs", m)
	}
	if m := mode(t, s.etc); m != 0o700 {
		t.Errorf("%s is %o, want 700", s.etc, m)
	}
	var cfg renderedLiveKit
	if err := yaml.Unmarshal([]byte(readFile(t, path)), &cfg); err != nil {
		t.Fatalf("the rendered livekit.yaml does not parse: %v", err)
	}
	if len(cfg.Keys) != 2 || cfg.Keys[s.values["MEMQL_CALLS_MEDIA_API_KEY"]] != s.values["MEMQL_CALLS_MEDIA_API_SECRET"] ||
		cfg.Keys[s.values["MEMQL_CALLS_MEDIA_WEBHOOK_KEY"]] != s.values["MEMQL_CALLS_MEDIA_WEBHOOK_SECRET"] {
		t.Errorf("keys hold %d pairs, not the room key and the webhook key from stdin", len(cfg.Keys))
	}
	if cfg.Webhook.APIKey != s.values["MEMQL_CALLS_MEDIA_WEBHOOK_KEY"] {
		t.Error("webhook.api_key is not the webhook key")
	}
	if !slices.Equal(cfg.Webhook.URLs, []string{"https://api.example.com/inbound/livekit"}) {
		t.Errorf("webhook.urls = %v, want the instance's front door", cfg.Webhook.URLs)
	}
	// Where the server is: its HTTP port on loopback (Caddy is its one client), Redis on
	// loopback, ICE candidates naming the public address the metadata service gave.
	if !slices.Equal(cfg.BindAddresses, []string{"127.0.0.1"}) {
		t.Errorf("bind_addresses = %v, want loopback only", cfg.BindAddresses)
	}
	if cfg.Redis.Address != "127.0.0.1:6379" {
		t.Errorf("redis.address = %q, want 127.0.0.1:6379", cfg.Redis.Address)
	}
	if cfg.RTC.NodeIP != "203.0.113.7" || cfg.RTC.TCPPort != 7881 || cfg.RTC.UDPPort != "7882" {
		t.Errorf("rtc = %+v, want node_ip 203.0.113.7, tcp 7881, udp 7882", cfg.RTC)
	}
	// TURN on 443 through Caddy (external TLS, the client's address in a PROXY header)
	// and on UDP 3478.
	if !cfg.TURN.Enabled || cfg.TURN.Domain != "turn.m1.media.example.com" || cfg.TURN.TLSPort != 5349 ||
		cfg.TURN.UDPPort != 3478 || !cfg.TURN.ExternalTLS || !cfg.TURN.ProxyProtocol {
		t.Errorf("turn = %+v", cfg.TURN)
	}
	// And what a call is, the same as the local target's.
	if cfg.Room.AutoCreate == nil || *cfg.Room.AutoCreate {
		t.Error("room.auto_create is not false")
	}
	var codecs []string
	for _, c := range cfg.Room.EnabledCodecs {
		codecs = append(codecs, c.Mime)
	}
	if !slices.Equal(codecs, []string{"audio/opus", "audio/red", "video/vp8", "video/h264", "video/rtx"}) {
		t.Errorf("enabled_codecs = %v", codecs)
	}
	if cfg.Limit.SubscriptionLimitVideo != 10 {
		t.Errorf("limit.subscription_limit_video = %d, want 10", cfg.Limit.SubscriptionLimitVideo)
	}
	if !slices.Equal(cfg.Webhook.FilterParams.IncludeEvents, []string{"participant_joined", "participant_left", "track_published", "room_finished"}) {
		t.Errorf("webhook.filter_params.include_events = %v", cfg.Webhook.FilterParams.IncludeEvents)
	}

	var caddy caddyConfig
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(s.etc, "caddy.json"))), &caddy); err != nil {
		t.Fatalf("the rendered caddy.json does not parse: %v", err)
	}
	if !caddy.Admin.Disabled || caddy.Storage.Root != "/data" {
		t.Errorf("admin disabled=%v storage=%+v, want no admin endpoint and certificates under /data", caddy.Admin.Disabled, caddy.Storage)
	}
	if got := caddy.Apps.TLS.Certificates.Automate; !slices.Equal(got, []string{"m1.media.example.com", "turn.m1.media.example.com"}) {
		t.Errorf("Caddy obtains certificates for %v, want the public host and its TURN name", got)
	}
	server, ok := caddy.Apps.Layer4.Servers["media"]
	if !ok || !slices.Equal(server.Listen, []string{":443"}) || len(server.Routes) != 2 {
		t.Fatalf("layer4 = %+v, want one server on :443 with two routes", caddy.Apps.Layer4)
	}
	routes := map[string]caddyRoute{}
	for _, r := range server.Routes {
		if len(r.Match) != 1 || len(r.Match[0].TLS.SNI) != 1 {
			t.Fatalf("a route does not match exactly one server name: %+v", r)
		}
		routes[r.Match[0].TLS.SNI[0]] = r
	}
	turn := routes["turn.m1.media.example.com"]
	if len(turn.Handle) != 2 || turn.Handle[0].Handler != "tls" || turn.Handle[1].Handler != "proxy" ||
		turn.Handle[1].ProxyProtocol != "v2" || !slices.Equal(turn.Handle[1].Upstreams[0].Dial, []string{"127.0.0.1:5349"}) {
		t.Errorf("the TURN route is %+v, want TLS ended here and the connection handed to 127.0.0.1:5349 with a PROXY v2 header", turn)
	}
	signal := routes["m1.media.example.com"]
	if len(signal.Handle) != 2 || signal.Handle[0].Handler != "tls" || len(signal.Handle[0].ConnectionPolicies) != 1 ||
		!slices.Equal(signal.Handle[0].ConnectionPolicies[0].ALPN, []string{"http/1.1"}) ||
		signal.Handle[1].ProxyProtocol != "" || !slices.Equal(signal.Handle[1].Upstreams[0].Dial, []string{"127.0.0.1:7880"}) {
		t.Errorf("the signalling route is %+v, want TLS ended here over HTTP/1.1 and the connection handed to 127.0.0.1:7880", signal)
	}

	ups := s.upCalls(t)
	if len(ups) != 1 || !strings.Contains(ups[0], "compose --project-name memql-media --file "+filepath.Join(s.etc, "compose.yaml")+" up --detach --wait --remove-orphans --force-recreate") {
		t.Errorf("docker compose was not asked to (re)start the server: %v", ups)
	}
	for _, secret := range s.secrets() {
		if strings.Contains(readFile(t, s.log), secret) {
			t.Fatal("a key or a secret reached docker's argv or environment")
		}
	}
	if _, err := os.Stat(filepath.Join(s.data, "caddy")); err != nil {
		t.Errorf("Caddy's certificate directory was not made: %v", err)
	}
}

// Configure is a re-runnable step: the same values on a running server change nothing,
// and a new key pair (a rotation) restarts it.
func TestConfigureRestartsTheServerOnlyWhenItsConfigurationChanges(t *testing.T) {
	s := newServerFixture(t)
	if _, _, code := s.configure(t); code != 0 {
		t.Fatalf("first configure exited %d", code)
	}
	s.running = "caddy livekit redis"
	_ = os.Remove(s.log)
	if _, _, code := s.configure(t); code != 0 {
		t.Fatalf("second configure exited %d", code)
	}
	if ups := s.upCalls(t); len(ups) != 0 {
		t.Errorf("the same values restarted a running server: %v", ups)
	}

	_ = os.Remove(s.log)
	s.values["MEMQL_CALLS_MEDIA_API_SECRET"] = randomHex(t, 32)
	if _, _, code := s.configure(t); code != 0 {
		t.Fatalf("configure with a new secret exited %d", code)
	}
	if ups := s.upCalls(t); len(ups) != 1 || !strings.Contains(ups[0], "--force-recreate") {
		t.Errorf("a new secret did not restart the server: %v", ups)
	}

	// A server that is not running is started even when nothing changed.
	_ = os.Remove(s.log)
	s.running = "redis"
	if _, _, code := s.configure(t); code != 0 {
		t.Fatalf("configure of a stopped server exited %d", code)
	}
	if ups := s.upCalls(t); len(ups) != 1 || strings.Contains(ups[0], "--force-recreate") {
		t.Errorf("a stopped server with an unchanged configuration was not started as it stands: %v", ups)
	}
}

func TestConfigureRefusesValuesTheServerCannotUse(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spoil func(*serverFixture)
	}{
		{"a value missing", func(s *serverFixture) { delete(s.values, "MEMQL_CALLS_MEDIA_WEBHOOK_SECRET") }},
		{"a short API secret", func(s *serverFixture) { s.values["MEMQL_CALLS_MEDIA_API_SECRET"] = "tiny" }},
		{"one key for both pairs", func(s *serverFixture) {
			s.values["MEMQL_CALLS_MEDIA_WEBHOOK_KEY"] = s.values["MEMQL_CALLS_MEDIA_API_KEY"]
		}},
		{"a key that would break the YAML", func(s *serverFixture) { s.values["MEMQL_CALLS_MEDIA_API_KEY"] = `callsapi": x` }},
		{"another provider", func(s *serverFixture) { s.values["MEMQL_CALLS_MEDIA_PROVIDER"] = "docker-local" }},
		{"a public URL with a path", func(s *serverFixture) { s.values["MEMQL_CALLS_MEDIA_PUBLIC_URL"] = "wss://m1.media.example.com/rtc" }},
		{"a public URL on another port", func(s *serverFixture) { s.values["MEMQL_CALLS_MEDIA_PUBLIC_URL"] = "wss://m1.media.example.com:8443" }},
		{"a control API on another host", func(s *serverFixture) { s.values["MEMQL_CALLS_MEDIA_API_URL"] = "https://m2.media.example.com" }},
		{"a control API in the clear", func(s *serverFixture) { s.values["MEMQL_CALLS_MEDIA_API_URL"] = "http://m1.media.example.com" }},
		{"no seats", func(s *serverFixture) { s.values["MEMQL_CALLS_MEDIA_SEATS"] = "0" }},
		{"a name that is not one of the eight", func(s *serverFixture) { s.values["MEMQL_CALLS_MEDIA_API_KEYS"] = "x" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newServerFixture(t)
			tc.spoil(s)
			stdin := s.stdin()
			if _, ok := s.values["MEMQL_CALLS_MEDIA_API_KEYS"]; ok {
				stdin += "MEMQL_CALLS_MEDIA_API_KEYS=x\n"
			}
			_, _, code := s.run(t, stdin, "configure", "--domain=example.com")
			if code != 3 {
				t.Fatalf("exited %d, want a code-3 refusal", code)
			}
			if _, err := os.Stat(filepath.Join(s.etc, "livekit.yaml")); err == nil {
				t.Error("refused values were rendered anyway")
			}
			if ups := s.upCalls(t); len(ups) != 0 {
				t.Errorf("refused values started the server: %v", ups)
			}
		})
	}

	// A line that is not NAME=value is refused without being repeated: it may be a
	// secret pasted on its own.
	s := newServerFixture(t)
	pasted := randomHex(t, 32)
	_, stderr, code := s.run(t, s.stdin()+pasted+"\n", "configure", "--domain=example.com")
	if code != 3 || strings.Contains(stderr, pasted) {
		t.Errorf("a stray line exited %d (want 3), and was repeated: %v", code, strings.Contains(stderr, pasted))
	}
}

func TestConfigureNeedsTheInstanceDomain(t *testing.T) {
	s := newServerFixture(t)
	for _, args := range [][]string{
		{"configure"},
		{"configure", "--domain=Example.COM"},
		{"configure", "--domain=example"},
		{"configure", "--domain=example.com", "--node-ip=300.1.2.3"},
		{"configure", "--domain=example.com", "--wait-seconds=soon"},
		{"configure", "--domain=example.com", "--colour=blue"},
		{"reboot"},
	} {
		if _, _, code := s.run(t, s.stdin(), args...); code != 2 {
			t.Errorf("%v exited %d, want a code-2 refusal", args, code)
		}
	}
}

// The ICE address comes from Hetzner's metadata service unless the operator names it;
// with neither, nothing is rendered.
func TestConfigureFindsTheServersPublicAddress(t *testing.T) {
	s := newServerFixture(t)
	s.publicIPv4 = ""
	if _, _, code := s.configure(t); code != 4 {
		t.Fatalf("no metadata and no --node-ip exited %d, want a code-4 failure", code)
	}
	if _, err := os.Stat(filepath.Join(s.etc, "livekit.yaml")); err == nil {
		t.Error("a server that does not know its address was configured anyway")
	}
	if _, _, code := s.configure(t, "--node-ip=198.51.100.4"); code != 0 {
		t.Fatalf("--node-ip exited %d", code)
	}
	var cfg renderedLiveKit
	if err := yaml.Unmarshal([]byte(readFile(t, filepath.Join(s.etc, "livekit.yaml"))), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.RTC.NodeIP != "198.51.100.4" {
		t.Errorf("node_ip = %q, want the --node-ip given", cfg.RTC.NodeIP)
	}
}

// Until Caddy has the certificate (the DNS records point here and ports 80 and 443 reach
// the server), LiveKit cannot answer through TLS, and configure says so -- having started
// the server, so a second run only waits again.
func TestConfigureSaysWhenTheServerDoesNotAnswerThroughTLS(t *testing.T) {
	s := newServerFixture(t)
	s.answers = false
	_, stderr, code := s.configure(t, "--wait-seconds=1")
	if code != 5 {
		t.Fatalf("exited %d, want a code-5 failure", code)
	}
	for _, want := range []string{"m1.media.example.com", "turn.m1.media.example.com", "80", "443"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the failure does not name %q:\n%s", want, stderr)
		}
	}
	if ups := s.upCalls(t); len(ups) != 1 {
		t.Errorf("the server was not started before the wait: %v", ups)
	}
}

func TestConfigureReportsAServerThatWouldNotStart(t *testing.T) {
	s := newServerFixture(t)
	s.upFails = true
	if _, _, code := s.configure(t); code != 5 {
		t.Fatalf("exited %d, want a code-5 failure", code)
	}
}

// The edge admits only hosts under its media suffix, which is derived as media.<domain>;
// a server named elsewhere works only where the installation overrides the suffix, so
// configure warns.
func TestConfigureWarnsAboutAHostOutsideTheDerivedSuffix(t *testing.T) {
	s := newServerFixture(t)
	s.values["MEMQL_CALLS_MEDIA_PUBLIC_URL"] = "wss://calls.example.net"
	s.values["MEMQL_CALLS_MEDIA_API_URL"] = "https://calls.example.net"
	_, stderr, code := s.configure(t)
	if code != 0 {
		t.Fatalf("exited %d", code)
	}
	if !strings.Contains(stderr, "MEMQL_CALLS_MEDIA_HOST_SUFFIX") || !strings.Contains(stderr, "media.example.com") {
		t.Errorf("no warning names the suffix the host is outside of:\n%s", stderr)
	}
}

// rooms is how a drain is watched: the number of rooms LiveKit holds in Redis.
func TestRoomsCountsTheRoomsTheServerHolds(t *testing.T) {
	s := newServerFixture(t)
	s.rooms = "3"
	stdout, _, code := s.run(t, "", "rooms")
	if code != 0 || strings.TrimSpace(stdout) != "3" {
		t.Fatalf("rooms exited %d and printed %q, want 3", code, stdout)
	}
	calls := s.dockerCalls(t)
	if !slices.ContainsFunc(calls, func(c string) bool {
		return strings.Contains(c, "exec -T redis redis-cli HLEN rooms")
	}) {
		t.Errorf("rooms did not ask Redis: %v", calls)
	}
	s.rooms = "not a number"
	if _, _, code := s.run(t, "", "rooms"); code != 5 {
		t.Errorf("an answer that is not a count exited %d, want 5", code)
	}
}
