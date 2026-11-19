package media

// local_up_test.go -- scripts/media/local-up.sh and local-down.sh, the local calls media
// target (make media-up / make media-down).
//
// Every run here is against a FAKE kubectl and a fake or absent docker. Nothing needs a
// Docker daemon or a cluster: the CI step that runs this package (gates, `./scripts/...`)
// has a Docker client and no daemon, and a test that started containers would be a test
// of the machine it ran on. What a real server does with the rendered configuration is
// the live check (TestLiveKitBackendRoundTrip in integrations/calls, run against the
// started target).
//
// Credentials are BUILT at run time, never written here: a hex run in source is the
// shape gitleaks' generic-api-key rule fires on (see scripts/k3d/seed_secrets_master_key_test.go).

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
}

// fakeKubectl answers exactly the reads local-up.sh makes, records every call, and
// refuses anything else, so a write it did not expect fails the run instead of passing.
const fakeKubectl = `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$FAKE_KUBECTL_LOG"
args="$*"
case "$args" in
  "get namespace "*)
    if [ "$FAKE_CLUSTER" = "down" ]; then
      printf 'The connection to the server 127.0.0.1:6443 was refused\n' >&2; exit 1
    fi
    printf 'namespace/memql\n'; exit 0 ;;
  *"get configmap memql-domain"*)
    printf '%s' "$FAKE_CLUSTER_DOMAIN"; exit 0 ;;
  *"get configmap memql-front-door-ca"*)
    if [ -z "$FAKE_CA_PEM" ]; then
      printf 'Error from server (NotFound): configmaps "memql-front-door-ca" not found\n' >&2; exit 1
    fi
    printf '%s' "$FAKE_CA_PEM"; exit 0 ;;
  *"get secret memql-calls-media"*"-o name"*)
    case "$FAKE_SECRET_STATE" in
      present) printf 'secret/memql-calls-media\n'; exit 0 ;;
      error)   printf 'Error from server: etcdserver: request timed out\n' >&2; exit 1 ;;
      *)       printf 'Error from server (NotFound): secrets "memql-calls-media" not found\n' >&2; exit 1 ;;
    esac ;;
  *"get secret memql-calls-media"*jsonpath*)
    printf '%s' "$FAKE_SECRET_VALUES"; exit 0 ;;
esac
printf 'fake kubectl: unexpected call: %s\n' "$args" >&2
exit 1
`

// fakeDocker records its argv and the MEDIA_* environment it was handed, answers info and
// compose version, and succeeds at compose up and down. With FAKE_DOCKER_RUNNING_DIGEST
// set, every service of the project is running and carries that MEDIA_CONFIG_DIGEST.
const fakeDocker = `#!/usr/bin/env bash
{ printf 'argv %s\n' "$*"; env | grep '^MEDIA_' | sort; } >> "$FAKE_DOCKER_LOG"
case "$1" in
  info)
    if [ -n "$FAKE_DOCKER_DOWN" ]; then
      printf 'Cannot connect to the Docker daemon at unix:///var/run/docker.sock\n' >&2; exit 1
    fi
    exit 0 ;;
  ps)
    [ -n "$FAKE_DOCKER_RUNNING_DIGEST" ] && printf 'c0ffee\n'
    exit 0 ;;
  inspect)
    printf 'PATH=/usr/bin\nMEDIA_CONFIG_DIGEST=%s\n' "$FAKE_DOCKER_RUNNING_DIGEST"
    exit 0 ;;
  compose)
    case "$*" in *" version"*) printf 'Docker Compose version v5.6.0\n' ;; esac
    exit 0 ;;
esac
exit 0
`

// mediaFixture is one fake cluster holding a memql-calls-media Secret, one TLS pair, and
// the places a run writes to.
type mediaFixture struct {
	dir, home, bin, stateDir string
	cert, key                string
	kubectlLog, dockerLog    string

	apiKey, apiSecret         string
	webhookKey, webhookSecret string
	publicURL                 string
	secretState               string // present | absent | error
	clusterDomain             string
	caPEM                     string
	cluster                   string // "" | down
	dockerDown                bool
	runningDigest             string // the digest the fake's running services carry; "" for none running
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func newMediaFixture(t *testing.T) *mediaFixture {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()
	f := &mediaFixture{
		dir:           dir,
		home:          filepath.Join(dir, "home"),
		bin:           filepath.Join(dir, "bin"),
		stateDir:      filepath.Join(dir, "state"),
		cert:          filepath.Join(dir, "tls", "dev.crt"),
		key:           filepath.Join(dir, "tls", "dev.key"),
		kubectlLog:    filepath.Join(dir, "kubectl.log"),
		dockerLog:     filepath.Join(dir, "docker.log"),
		apiKey:        "callsapi" + randomHex(t, 6),
		apiSecret:     randomHex(t, 32),
		webhookKey:    "callshook" + randomHex(t, 6),
		webhookSecret: randomHex(t, 32),
		publicURL:     "wss://media.memql.localhost:7443",
		secretState:   "present",
		clusterDomain: "memql.localhost",
		caPEM:         "-----BEGIN CERTIFICATE-----\nfixture-ca\n-----END CERTIFICATE-----\n",
	}
	for _, d := range []string{f.home, f.bin, filepath.Dir(f.cert)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, body := range map[string]string{
		filepath.Join(f.bin, "kubectl"): fakeKubectl,
		filepath.Join(f.dir, "docker"):  fakeDocker,
		f.cert:                          "fixture certificate\n",
		f.key:                           "fixture key\n",
	} {
		perm := os.FileMode(0o600)
		if strings.HasSuffix(path, "kubectl") || strings.HasSuffix(path, "docker") {
			perm = 0o755
		}
		if err := os.WriteFile(path, []byte(body), perm); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *mediaFixture) dockerStub() string { return filepath.Join(f.dir, "docker") }

func (f *mediaFixture) noDocker() string { return filepath.Join(f.dir, "no-such-docker") }

func (f *mediaFixture) secretValues() string {
	enc := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	return strings.Join([]string{enc(f.apiKey), enc(f.apiSecret), enc(f.webhookKey), enc(f.webhookSecret), enc(f.publicURL)}, "|")
}

// credentials are the four values no output, argv or environment may ever carry. The
// keys are identifiers rather than secrets, and are held to the same rule because
// nothing has a reason to print them. A value too short to be distinctive (a refusal
// case's deliberately bad one) is not searched for: it could match unrelated text.
func (f *mediaFixture) credentials() []string {
	var out []string
	for _, c := range []string{f.apiKey, f.apiSecret, f.webhookKey, f.webhookSecret} {
		if len(c) >= 12 {
			out = append(out, c)
		}
	}
	return out
}

type envelope struct {
	OK         bool           `json:"ok"`
	Capability string         `json:"capability"`
	Changed    bool           `json:"changed"`
	Result     map[string]any `json:"result"`
	Error      *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// run executes one of the media scripts against the fixture and returns its envelope,
// its stderr and its exit code. stdout must be exactly one envelope.
func (f *mediaFixture) run(t *testing.T, script string, args ...string) (envelope, string, int) {
	t.Helper()
	path := filepath.Join(repoRoot(t), "scripts", "media", script)
	cmd := exec.Command("bash", append([]string{path}, args...)...)
	cmd.Dir = repoRoot(t)
	cmd.Stdin = nil
	down := ""
	if f.dockerDown {
		down = "1"
	}
	cmd.Env = append(os.Environ(),
		"PATH="+f.bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HOME="+f.home,
		"FAKE_KUBECTL_LOG="+f.kubectlLog,
		"FAKE_DOCKER_LOG="+f.dockerLog,
		"FAKE_DOCKER_DOWN="+down,
		"FAKE_DOCKER_RUNNING_DIGEST="+f.runningDigest,
		"FAKE_CLUSTER="+f.cluster,
		"FAKE_CLUSTER_DOMAIN="+f.clusterDomain,
		"FAKE_CA_PEM="+f.caPEM,
		"FAKE_SECRET_STATE="+f.secretState,
		"FAKE_SECRET_VALUES="+f.secretValues(),
		"MEMQL_K3D_NAMESPACE=",
		"MEMQL_LOCAL_TLS_CERT=",
		"MEMQL_LOCAL_TLS_KEY=",
	)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running %s: %v\nstderr:\n%s", script, err, stderr.String())
	}
	t.Logf("%s %v: exit=%d\nstdout: %s\nstderr:\n%s", script, args, code, stdout.String(), stderr.String())
	var env envelope
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &env); err != nil {
		t.Fatalf("stdout is not exactly one JSON envelope (%v): %q", err, stdout.String())
	}
	for _, c := range f.credentials() {
		if strings.Contains(stdout.String()+stderr.String(), c) {
			t.Fatalf("%s printed a credential", script)
		}
	}
	return env, stderr.String(), code
}

// upArgs are a run's arguments against the fixture, with docker at dockerPath.
func (f *mediaFixture) upArgs(dockerPath string, extra ...string) []string {
	return append([]string{
		"--state-dir=" + f.stateDir,
		"--tls-cert=" + f.cert,
		"--tls-key=" + f.key,
		"--docker=" + dockerPath,
	}, extra...)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return st.Mode().Perm()
}

// liveKitConfig is the rendered LiveKit configuration, decoded with the keys
// livekit-server v1.13.7 reads (pkg/config/config.go at that tag).
type liveKitConfig struct {
	Port  int `yaml:"port"`
	Redis struct {
		Address string `yaml:"address"`
	} `yaml:"redis"`
	Keys map[string]string `yaml:"keys"`
	RTC  struct {
		TCPPort int    `yaml:"tcp_port"`
		UDPPort string `yaml:"udp_port"`
		NodeIP  string `yaml:"node_ip"`
	} `yaml:"rtc"`
	Room struct {
		AutoCreate    *bool `yaml:"auto_create"`
		EnabledCodecs []struct {
			Mime string `yaml:"mime"`
		} `yaml:"enabled_codecs"`
	} `yaml:"room"`
	Limit struct {
		SubscriptionLimitVideo int `yaml:"subscription_limit_video"`
	} `yaml:"limit"`
	Webhook struct {
		APIKey       string   `yaml:"api_key"`
		URLs         []string `yaml:"urls"`
		FilterParams struct {
			IncludeEvents []string `yaml:"include_events"`
		} `yaml:"filter_params"`
	} `yaml:"webhook"`
}

// TestLocalUpDryRunRendersTheMediaServer: a dry run reads the cluster, renders the
// server's configuration and starts nothing -- with no Docker at all, which is how it
// is run here.
func TestLocalUpDryRunRendersTheMediaServer(t *testing.T) {
	f := newMediaFixture(t)
	env, _, code := f.run(t, "local-up.sh", f.upArgs(f.noDocker(), "--dry-run=true")...)
	if code != 0 || !env.OK {
		t.Fatalf("dry run failed: exit %d, %+v", code, env.Error)
	}
	if env.Capability != "media.localUp" {
		t.Errorf("capability = %q, want media.localUp", env.Capability)
	}
	if env.Result["dryRun"] != true || env.Result["started"] != false {
		t.Errorf("a dry run reported dryRun=%v started=%v", env.Result["dryRun"], env.Result["started"])
	}
	if got := env.Result["publicUrl"]; got != f.publicURL {
		t.Errorf("publicUrl = %v, want the Secret's %s", got, f.publicURL)
	}
	if got := env.Result["webhookUrl"]; got != "https://api.memql.localhost/inbound/livekit" {
		t.Errorf("webhookUrl = %v, want https://api.memql.localhost/inbound/livekit", got)
	}

	raw := readFile(t, filepath.Join(f.stateDir, "livekit.yaml"))
	var cfg liveKitConfig
	if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("the rendered livekit.yaml does not parse: %v", err)
	}
	if cfg.Port != 7880 || cfg.Redis.Address != "redis:6379" {
		t.Errorf("port %d, redis %q; want 7880 and the compose redis service", cfg.Port, cfg.Redis.Address)
	}
	if len(cfg.Keys) != 2 || cfg.Keys[f.apiKey] != f.apiSecret || cfg.Keys[f.webhookKey] != f.webhookSecret {
		t.Errorf("keys hold %d pairs, not the Secret's room key and webhook key", len(cfg.Keys))
	}
	if cfg.RTC.TCPPort != 7881 || cfg.RTC.UDPPort != "7882" || cfg.RTC.NodeIP != "127.0.0.1" {
		t.Errorf("rtc = %+v, want tcp 7881, udp 7882 and node_ip 127.0.0.1", cfg.RTC)
	}
	if cfg.Room.AutoCreate == nil || *cfg.Room.AutoCreate {
		t.Error("room.auto_create is not false: a pass for a room the engine never created would open one " +
			"without the room's participant limit")
	}
	var codecs []string
	for _, c := range cfg.Room.EnabledCodecs {
		codecs = append(codecs, c.Mime)
	}
	// Opus, VP8 and H.264, plus RED (audio redundancy) and RTX (video retransmission):
	// v1.13.7 registers either only when the list names it (pkg/rtc/mediaengine.go), and
	// they are what carries a call across a lossy network. The rest of the server's
	// default list stays off.
	if !slices.Equal(codecs, []string{"audio/opus", "audio/red", "video/vp8", "video/h264", "video/rtx"}) {
		t.Errorf("enabled_codecs = %v, want audio/opus, audio/red, video/vp8, video/h264, video/rtx", codecs)
	}
	if cfg.Limit.SubscriptionLimitVideo != 10 {
		t.Errorf("limit.subscription_limit_video = %d, want 10", cfg.Limit.SubscriptionLimitVideo)
	}
	if cfg.Webhook.APIKey != f.webhookKey {
		t.Error("webhook.api_key is not the webhook key, so the engine's verifier (which knows only that pair) " +
			"would refuse every delivery")
	}
	if !slices.Equal(cfg.Webhook.URLs, []string{"https://api.memql.localhost/inbound/livekit"}) {
		t.Errorf("webhook.urls = %v", cfg.Webhook.URLs)
	}
	if !slices.Equal(cfg.Webhook.FilterParams.IncludeEvents,
		[]string{"participant_joined", "participant_left", "track_published", "room_finished"}) {
		t.Errorf("webhook.filter_params.include_events = %v, want the four events integrations/calls applies",
			cfg.Webhook.FilterParams.IncludeEvents)
	}

	caddy := readFile(t, filepath.Join(f.stateDir, "Caddyfile"))
	for _, want := range []string{
		"https://media.memql.localhost:7443 {",
		"tls /etc/caddy/tls/cert.pem /etc/caddy/tls/key.pem",
		"reverse_proxy livekit:7880",
	} {
		if !strings.Contains(caddy, want) {
			t.Errorf("the rendered Caddyfile does not carry %q:\n%s", want, caddy)
		}
	}
	if got := readFile(t, filepath.Join(f.stateDir, "front-door-ca.pem")); got != f.caPEM {
		t.Errorf("front-door-ca.pem = %q, want the cluster's memql-front-door-ca", got)
	}
	if m := mode(t, f.stateDir); m != 0o700 {
		t.Errorf("the state directory is %o, want 700: it holds both key pairs", m)
	}
	if m := mode(t, filepath.Join(f.stateDir, "livekit.yaml")); m != 0o600 {
		t.Errorf("livekit.yaml is %o, want 600: it holds both key pairs", m)
	}
	if strings.Contains(readFile(t, f.kubectlLog), "create") || strings.Contains(readFile(t, f.kubectlLog), "apply") {
		t.Error("a dry run wrote to the cluster")
	}
	// The script copies a credential Secret onto disk, so it reads only the local cluster,
	// whatever kubectl's current context points at.
	for _, call := range strings.Split(strings.TrimSpace(readFile(t, f.kubectlLog)), "\n") {
		if !strings.Contains(call, "--context=k3d-memql") {
			t.Errorf("a kubectl read followed the current context instead of naming k3d-memql: %s", call)
		}
	}
	if _, err := os.Stat(f.dockerLog); err == nil {
		t.Error("a dry run ran docker")
	}
}

// The rendered files carry both key pairs, so by default they go under the operator's
// home (~/.memql/media, beside the front door's pair in ~/.memql/certs) and never into
// a checkout, where `git add` could reach them.
func TestLocalUpRendersOutsideTheRepositoryByDefault(t *testing.T) {
	f := newMediaFixture(t)
	env, _, code := f.run(t, "local-up.sh", "--tls-cert="+f.cert, "--tls-key="+f.key, "--dry-run=true")
	if code != 0 || !env.OK {
		t.Fatalf("dry run failed: exit %d, %+v", code, env.Error)
	}
	want := filepath.Join(f.home, ".memql", "media")
	if got := env.Result["stateDir"]; got != want {
		t.Errorf("stateDir = %v, want %s", got, want)
	}
	if _, err := os.Stat(filepath.Join(want, "livekit.yaml")); err != nil {
		t.Errorf("nothing rendered at the default state directory: %v", err)
	}
}

func TestLocalUpWithoutDockerIsAMissingPrerequisite(t *testing.T) {
	f := newMediaFixture(t)
	env, _, code := f.run(t, "local-up.sh", f.upArgs(f.noDocker())...)
	if code != 4 || env.OK || env.Error == nil || env.Error.Code != 4 {
		t.Fatalf("a run with no Docker exited %d with %+v, want a code-4 failure", code, env.Error)
	}
	if !strings.Contains(env.Error.Message, "Docker") {
		t.Errorf("the refusal does not name Docker: %q", env.Error.Message)
	}
	if _, err := os.Stat(f.stateDir); err == nil {
		t.Error("a run that could not start anything still rendered the configuration")
	}
}

func TestLocalUpWithTheDockerDaemonStoppedIsAMissingPrerequisite(t *testing.T) {
	f := newMediaFixture(t)
	f.dockerDown = true
	env, _, code := f.run(t, "local-up.sh", f.upArgs(f.dockerStub())...)
	if code != 4 || env.Error == nil || env.Error.Code != 4 {
		t.Fatalf("a run with the daemon stopped exited %d with %+v, want a code-4 failure", code, env.Error)
	}
	if strings.Contains(readFile(t, f.dockerLog), "compose") {
		t.Error("docker compose ran against a stopped daemon")
	}
}

func TestLocalUpWithoutTheSecretIsAMissingPrerequisite(t *testing.T) {
	f := newMediaFixture(t)
	f.secretState = "absent"
	env, _, code := f.run(t, "local-up.sh", f.upArgs(f.noDocker(), "--dry-run=true")...)
	if code != 4 || env.Error == nil || env.Error.Code != 4 {
		t.Fatalf("a missing memql-calls-media exited %d with %+v, want a code-4 failure", code, env.Error)
	}
	if !strings.Contains(env.Error.Message, "make secrets") {
		t.Errorf("the refusal does not say how to seed the Secret: %q", env.Error.Message)
	}

	// An unreadable Secret is not a missing one: it is a failure to tell, never a reason
	// to tell the operator to re-seed.
	f.secretState = "error"
	env, _, code = f.run(t, "local-up.sh", f.upArgs(f.noDocker(), "--dry-run=true")...)
	if code != 5 || env.Error == nil || env.Error.Code != 5 {
		t.Fatalf("an unreadable memql-calls-media exited %d with %+v, want a code-5 failure", code, env.Error)
	}
}

func TestLocalUpRefusesValuesTheMediaServerCannotUse(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spoil func(*mediaFixture)
	}{
		{"a short API secret", func(f *mediaFixture) { f.apiSecret = "tiny" }},
		{"a short webhook secret", func(f *mediaFixture) { f.webhookSecret = "tiny" }},
		{"no webhook key", func(f *mediaFixture) { f.webhookKey = "" }},
		{"one key for both pairs", func(f *mediaFixture) { f.webhookKey = f.apiKey }},
		{"a key that would break the YAML", func(f *mediaFixture) { f.apiKey = "callsapi\": x" }},
		{"a public URL browsers cannot use", func(f *mediaFixture) { f.publicURL = "ws://media.memql.localhost:7443" }},
		{"a public URL with a path", func(f *mediaFixture) { f.publicURL = "wss://media.memql.localhost:7443/rtc" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMediaFixture(t)
			tc.spoil(f)
			env, _, code := f.run(t, "local-up.sh", f.upArgs(f.noDocker(), "--dry-run=true")...)
			if code != 3 || env.Error == nil || env.Error.Code != 3 {
				t.Fatalf("exited %d with %+v, want a code-3 refusal", code, env.Error)
			}
			if _, err := os.Stat(filepath.Join(f.stateDir, "livekit.yaml")); err == nil {
				t.Error("a refused Secret was rendered anyway")
			}
		})
	}
}

// dockerEnv returns the MEDIA_* values the fake docker was handed on its compose up.
func dockerEnv(t *testing.T, log string) map[string]string {
	t.Helper()
	out := map[string]string{}
	inUp := false
	for _, line := range strings.Split(log, "\n") {
		if strings.HasPrefix(line, "argv ") {
			inUp = strings.Contains(line, " up ")
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok && inUp {
			out[k] = v
		}
	}
	return out
}

// TestLocalUpStartsTheComposeProject: a real run hands docker compose the rendered
// directory, the front door's pair, the api host and the port -- paths and host names,
// and never a credential, which reaches the containers only inside the rendered file.
func TestLocalUpStartsTheComposeProject(t *testing.T) {
	f := newMediaFixture(t)
	env, _, code := f.run(t, "local-up.sh", f.upArgs(f.dockerStub(), "--wait-seconds=0")...)
	if code != 0 || !env.OK {
		t.Fatalf("run failed: exit %d, %+v", code, env.Error)
	}
	if env.Result["started"] != true || env.Result["dryRun"] != false || !env.Changed {
		t.Errorf("started=%v dryRun=%v changed=%v, want a started run", env.Result["started"], env.Result["dryRun"], env.Changed)
	}
	log := readFile(t, f.dockerLog)
	compose := filepath.Join(repoRoot(t), "deploy", "media", "compose.yaml")
	if !strings.Contains(log, "argv compose --project-name memql-media --file "+compose+" up --detach --wait --remove-orphans") {
		t.Errorf("docker compose was not asked to start the memql-media project from deploy/media/compose.yaml:\n%s", log)
	}
	got := dockerEnv(t, log)
	for k, want := range map[string]string{
		"MEDIA_CONFIG_DIR": f.stateDir,
		"MEDIA_TLS_CERT":   f.cert,
		"MEDIA_TLS_KEY":    f.key,
		"MEDIA_API_HOST":   "api.memql.localhost",
		"MEDIA_HTTPS_PORT": "7443",
	} {
		if got[k] != want {
			t.Errorf("compose up got %s=%q, want %q", k, got[k], want)
		}
	}
	if got["MEDIA_CONFIG_DIGEST"] == "" {
		t.Error("compose up was given no MEDIA_CONFIG_DIGEST, so a changed configuration would not recreate the server")
	}
	for _, c := range f.credentials() {
		if strings.Contains(log, c) {
			t.Fatal("a credential reached docker's argv or environment")
		}
	}
}

// The containers read their configuration once, at start, and compose recreates a
// container only when its own definition changes. The rendered files' digest is part of
// that definition, so a re-seeded credential restarts the server and an unchanged one
// leaves a running call alone.
func TestLocalUpRecreatesTheServerOnlyWhenItsConfigurationChanges(t *testing.T) {
	f := newMediaFixture(t)
	digest := func() string {
		t.Helper()
		_ = os.Remove(f.dockerLog)
		if env, _, code := f.run(t, "local-up.sh", f.upArgs(f.dockerStub(), "--wait-seconds=0")...); code != 0 || !env.OK {
			t.Fatalf("run failed: exit %d, %+v", code, env.Error)
		}
		return dockerEnv(t, readFile(t, f.dockerLog))["MEDIA_CONFIG_DIGEST"]
	}
	first, again := digest(), digest()
	if first == "" || first != again {
		t.Fatalf("the same configuration rendered twice gave digests %q and %q", first, again)
	}
	f.webhookSecret = randomHex(t, 32)
	if changed := digest(); changed == first {
		t.Error("a new webhook secret left the digest unchanged, so the running server would keep the old one")
	}
}

// changed is the contract's idempotency signal: a run that finds every service already
// running with the configuration it rendered changed nothing, and says so.
func TestLocalUpReportsNoChangeWhenTheServerAlreadyRunsItsConfiguration(t *testing.T) {
	f := newMediaFixture(t)
	run := func() envelope {
		t.Helper()
		_ = os.Remove(f.dockerLog)
		env, _, code := f.run(t, "local-up.sh", f.upArgs(f.dockerStub(), "--wait-seconds=0")...)
		if code != 0 || !env.OK {
			t.Fatalf("run failed: exit %d, %+v", code, env.Error)
		}
		return env
	}
	if env := run(); !env.Changed {
		t.Fatal("starting a server that was not running reported no change")
	}
	digest := dockerEnv(t, readFile(t, f.dockerLog))["MEDIA_CONFIG_DIGEST"]

	f.runningDigest = digest
	if env := run(); env.Changed {
		t.Error("a run that found the server already running its configuration reported a change")
	}
	f.runningDigest = "an-older-configuration"
	if env := run(); !env.Changed {
		t.Error("a run that recreated a server on an older configuration reported no change")
	}
}

func TestLocalDownStopsTheProjectAndRemovesTheRenderedFiles(t *testing.T) {
	f := newMediaFixture(t)
	if err := os.MkdirAll(f.stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"livekit.yaml", "Caddyfile", "front-door-ca.pem", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(f.stateDir, name), []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	env, _, code := f.run(t, "local-down.sh", "--state-dir="+f.stateDir, "--docker="+f.dockerStub())
	if code != 0 || !env.OK || env.Capability != "media.localDown" {
		t.Fatalf("local-down failed: exit %d, %s, %+v", code, env.Capability, env.Error)
	}
	if !strings.Contains(readFile(t, f.dockerLog), "argv compose --project-name memql-media down --remove-orphans") {
		t.Errorf("docker compose was not asked to take the memql-media project down:\n%s", readFile(t, f.dockerLog))
	}
	for _, name := range []string{"livekit.yaml", "Caddyfile", "front-door-ca.pem"} {
		if _, err := os.Stat(filepath.Join(f.stateDir, name)); err == nil {
			t.Errorf("%s survived media-down; it carries the media server's credentials", name)
		}
	}
	if _, err := os.Stat(filepath.Join(f.stateDir, "notes.txt")); err != nil {
		t.Errorf("media-down removed a file it did not write: %v", err)
	}
}

func TestLocalDownWithoutDockerIsAMissingPrerequisite(t *testing.T) {
	f := newMediaFixture(t)
	env, _, code := f.run(t, "local-down.sh", "--state-dir="+f.stateDir, "--docker="+f.noDocker())
	if code != 4 || env.Error == nil || env.Error.Code != 4 {
		t.Fatalf("local-down with no Docker exited %d with %+v, want a code-4 failure", code, env.Error)
	}
}

// composeFile is the slice of deploy/media/compose.yaml these assertions read.
type composeFile struct {
	Name     string `yaml:"name"`
	Services map[string]struct {
		Image       string            `yaml:"image"`
		Ports       []string          `yaml:"ports"`
		ExtraHosts  []string          `yaml:"extra_hosts"`
		Environment map[string]string `yaml:"environment"`
		Volumes     []string          `yaml:"volumes"`
	} `yaml:"services"`
}

// TestTheComposeFileRunsTheReviewedServer pins what the scripts and the engine depend on.
//
// The LiveKit image is pinned at the version its configuration was checked against, and
// at the version the engine's own LiveKit component runs (deploy/k8s/components/ask-voice):
// an upgrade is reviewed for CreateJobToken first, which on LiveKit's development branch
// lets a running agent job request new passes on a self-hosted server. Changing this
// string is that review's checkpoint.
func TestTheComposeFileRunsTheReviewedServer(t *testing.T) {
	var c composeFile
	if err := yaml.Unmarshal([]byte(readFile(t, filepath.Join(repoRoot(t), "deploy", "media", "compose.yaml"))), &c); err != nil {
		t.Fatalf("deploy/media/compose.yaml does not parse: %v", err)
	}
	if c.Name != "memql-media" {
		t.Errorf("project name %q, want memql-media (local-down.sh stops it by that name)", c.Name)
	}
	for service, image := range map[string]string{
		"livekit": "livekit/livekit-server:v1.13.7",
		"redis":   "redis:7.4-alpine",
		"caddy":   "caddy:2.11.9-alpine",
	} {
		if got := c.Services[service].Image; got != image {
			t.Errorf("%s runs %q, want %q", service, got, image)
		}
	}
	if len(c.Services) != 3 {
		t.Errorf("compose.yaml defines %d services, want livekit, redis and caddy", len(c.Services))
	}
	livekit := c.Services["livekit"]
	for _, port := range []string{"7880:7880", "127.0.0.1:7881:7881", "127.0.0.1:7882:7882/udp"} {
		if !slices.Contains(livekit.Ports, port) {
			t.Errorf("livekit does not publish %s (it publishes %v)", port, livekit.Ports)
		}
	}
	if !slices.ContainsFunc(livekit.ExtraHosts, func(h string) bool {
		return strings.HasPrefix(h, "${MEDIA_API_HOST:?") && strings.HasSuffix(h, ":host-gateway")
	}) {
		t.Errorf("livekit does not map the api host to the Docker host, so its webhooks cannot reach the "+
			"front door: %v", livekit.ExtraHosts)
	}
	if livekit.Environment["SSL_CERT_FILE"] == "" {
		t.Error("livekit trusts no certificate authority for the front door, so its webhooks fail TLS")
	}
	caddy := c.Services["caddy"]
	if len(caddy.Ports) != 1 || !strings.HasPrefix(caddy.Ports[0], "127.0.0.1:${MEDIA_HTTPS_PORT:?") {
		t.Errorf("caddy does not publish the public URL's port on loopback alone: %v", caddy.Ports)
	}
	for _, service := range []string{"livekit", "caddy"} {
		if c.Services[service].Environment["MEDIA_CONFIG_DIGEST"] == "" {
			t.Errorf("%s carries no MEDIA_CONFIG_DIGEST, so compose never recreates it for a new configuration", service)
		}
		for _, v := range c.Services[service].Volumes {
			if !strings.HasSuffix(v, ":ro") {
				t.Errorf("%s mounts %q writable; the configuration and the TLS pair are read-only", service, v)
			}
		}
	}
}

// TestTheComposeFileIsValid asks Compose itself, which needs its client and no daemon.
func TestTheComposeFileIsValid(t *testing.T) {
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skip("docker compose is not installed")
	}
	cmd := exec.Command("docker", "compose", "--file", filepath.Join(repoRoot(t), "deploy", "media", "compose.yaml"), "config", "--quiet")
	dir := t.TempDir()
	cmd.Env = append(os.Environ(),
		"MEDIA_CONFIG_DIR="+dir,
		"MEDIA_TLS_CERT="+filepath.Join(dir, "dev.crt"),
		"MEDIA_TLS_KEY="+filepath.Join(dir, "dev.key"),
		"MEDIA_API_HOST=api.memql.localhost",
		"MEDIA_HTTPS_PORT=7443",
		"MEDIA_CONFIG_DIGEST=0",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("docker compose config refused deploy/media/compose.yaml: %v\n%s", err, out)
	}

	// And it refuses to start without the values local-up.sh supplies, so a hand-run
	// `docker compose up` cannot start a server with no configuration.
	bare := exec.Command("docker", "compose", "--file", filepath.Join(repoRoot(t), "deploy", "media", "compose.yaml"), "config", "--quiet")
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "MEDIA_") {
			bare.Env = append(bare.Env, kv)
		}
	}
	if out, err := bare.CombinedOutput(); err == nil {
		t.Errorf("docker compose accepted compose.yaml with none of the MEDIA_* values set:\n%s", out)
	}
}
