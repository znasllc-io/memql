package media

// provision_hetzner_test.go -- scripts/media/provision-hetzner.sh, the Hetzner media
// target, and the cloud-init it renders from deploy/media/cloud-init.yaml.tmpl.
//
// NOTHING HERE CALLS HETZNER. A dry run calls nothing at all, and every real run is
// against a FAKE curl on PATH that answers from fixture files, records each call's argv
// and keeps what it was handed on stdin and as a body. That is how these tests can say
// where the API token went: into curl's configuration on stdin, never into an argv
// (which every process on the operator's machine can read with ps) or the output.
//
// Tokens and credentials are BUILT at run time, never written here (see local_up_test.go).

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// fakeHcloudCurl stands in for curl. It reads the configuration curl is given on stdin
// (--config -), the body (--data-binary @<path>), the method and the URL, and answers
// from $FAKE_HCLOUD_DIR/responses/<METHOD><path with / as _>, whose first line is the
// HTTP status and the rest the body. A request without the expected bearer token is
// answered 401, as Hetzner answers it. It ends its output with a newline and the status,
// which is what --write-out '\n%{http_code}' asks curl for.
const fakeHcloudCurl = `#!/usr/bin/env bash
dir="$FAKE_HCLOUD_DIR"
[ -n "${HCLOUD_TOKEN:-}" ] && printf 'curl inherited HCLOUD_TOKEN\n' >> "$dir/inherited.log"
n=$(( $(cat "$dir/count" 2>/dev/null || echo 0) + 1 ))
printf '%s' "$n" > "$dir/count"
printf 'argv' >> "$dir/argv.log"
for a in "$@"; do printf ' %s' "$a" >> "$dir/argv.log"; done
printf '\n' >> "$dir/argv.log"
method=GET url="" body="" stdin_config=""
while [ $# -gt 0 ]; do
  case "$1" in
    --request) method="$2"; shift 2 ;;
    --data-binary) body="${2#@}"; shift 2 ;;
    --config) [ "$2" = "-" ] && stdin_config=1; shift 2 ;;
    --header|--write-out|--proto|--connect-timeout|--max-time) shift 2 ;;
    --*) shift ;;
    *) url="$1"; shift ;;
  esac
done
config=""
if [ -n "$stdin_config" ]; then config="$(cat)"; fi
printf '%s\n' "$config" > "$dir/config.$n"
path="${url#https://api.hetzner.cloud/v1}"
key="${method}$(printf '%s' "$path" | tr '/' '_')"
printf '%s %s\n' "$method" "$path" >> "$dir/requests.log"
if [ -n "$body" ]; then cat "$body" > "$dir/body.$n"; fi
if [ "$config" != "header = \"Authorization: Bearer ${FAKE_HCLOUD_TOKEN}\"" ]; then
  printf '{"error":{"code":"unauthorized","message":"unable to authenticate","details":null}}\n401'
  exit 0
fi
if [ -f "$dir/responses/$key" ]; then
  status="$(head -n 1 "$dir/responses/$key")"
  tail -n +2 "$dir/responses/$key"
  printf '\n%s' "$status"
  exit 0
fi
printf '{"error":{"code":"not_found","message":"no fixture for %s"}}\n404' "$key"
`

// hetznerFixture is one operator's machine: a fake curl first on PATH, the fixture
// answers, and the environment a run sees.
type hetznerFixture struct {
	dir, bin, hcloud string
	token            string
	env              []string // extra environment for a run
}

func newHetznerFixture(t *testing.T) *hetznerFixture {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()
	f := &hetznerFixture{
		dir:    dir,
		bin:    filepath.Join(dir, "bin"),
		hcloud: filepath.Join(dir, "hcloud"),
		// The shape of a Hetzner Cloud API token: 64 letters and digits.
		token: randomHex(t, 32),
	}
	for _, d := range []string{f.bin, filepath.Join(f.hcloud, "responses")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(f.bin, "curl"), []byte(fakeHcloudCurl), 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

// respond makes the fake answer one request: method and path as the API names them
// (path without /v1, query included), the status and the body.
func (f *hetznerFixture) respond(t *testing.T, method, path string, status int, body string) {
	t.Helper()
	key := method + strings.ReplaceAll(path, "/", "_")
	content := strings.TrimSpace(body)
	if err := os.WriteFile(filepath.Join(f.hcloud, "responses", key), []byte(strconv.Itoa(status)+"\n"+content+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// calls is every request the fake answered, as "METHOD /path".
func (f *hetznerFixture) calls(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.hcloud, "requests.log"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// bodyOf is the JSON body the fake was handed with the request that named path, decoded.
func (f *hetznerFixture) bodyOf(t *testing.T, method, path string) map[string]any {
	t.Helper()
	for i, call := range f.calls(t) {
		if call != method+" "+path {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(f.hcloud, "body."+strconv.Itoa(i+1)))
		if err != nil {
			t.Fatalf("%s %s was sent with no body: %v", method, path, err)
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("%s %s body is not JSON: %v\n%s", method, path, err, raw)
		}
		return out
	}
	t.Fatalf("no %s %s was sent; calls: %v", method, path, f.calls(t))
	return nil
}

type provisionEnvelope struct {
	OK         bool           `json:"ok"`
	Capability string         `json:"capability"`
	Changed    bool           `json:"changed"`
	Result     map[string]any `json:"result"`
	Error      *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// run executes provision-hetzner.sh with HCLOUD_TOKEN set to the fixture's token (unless
// the test's env says otherwise), and holds it to the rules every run keeps: exactly one
// envelope on stdout, and the token in no output and no argv.
func (f *hetznerFixture) run(t *testing.T, args ...string) (provisionEnvelope, string, int) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{filepath.Join(repoRoot(t), "scripts", "media", "provision-hetzner.sh")}, args...)...)
	cmd.Dir = repoRoot(t)
	cmd.Stdin = nil
	cmd.Env = append(os.Environ(),
		"PATH="+f.bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_HCLOUD_DIR="+f.hcloud,
		"FAKE_HCLOUD_TOKEN="+f.token,
		"HCLOUD_TOKEN="+f.token,
		"TMPDIR="+t.TempDir(),
	)
	cmd.Env = append(cmd.Env, f.env...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running provision-hetzner.sh: %v\nstderr:\n%s", err, stderr.String())
	}
	t.Logf("provision-hetzner.sh %v: exit=%d\nstdout: %.2000s\nstderr:\n%.3000s", args, code, stdout.String(), stderr.String())
	var env provisionEnvelope
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &env); err != nil {
		t.Fatalf("stdout is not exactly one JSON envelope (%v): %q", err, stdout.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), f.token) {
		t.Fatal("provision-hetzner.sh printed the Hetzner API token")
	}
	if argv, err := os.ReadFile(filepath.Join(f.hcloud, "argv.log")); err == nil && strings.Contains(string(argv), f.token) {
		t.Fatal("the Hetzner API token reached curl's argv, where ps shows it to every process on the machine")
	}
	if _, err := os.Stat(filepath.Join(f.hcloud, "inherited.log")); err == nil {
		t.Fatal("curl inherited HCLOUD_TOKEN: the script leaves the token in its environment for every command it runs")
	}
	return env, stderr.String(), code
}

// m1Args are a valid request for m1 in Ashburn.
func m1Args(extra ...string) []string {
	return append([]string{
		"--name=m1",
		"--location=ash",
		"--server-type=cpx31",
		"--image=ubuntu-24.04",
		"--ssh-key=ops",
	}, extra...)
}

// hetznerRequest is one request a dry run says it would send.
type hetznerRequest struct {
	Method string         `json:"method"`
	URL    string         `json:"url"`
	Body   map[string]any `json:"body"`
}

func dryRunRequests(t *testing.T, env provisionEnvelope) []hetznerRequest {
	t.Helper()
	raw, err := json.Marshal(env.Result["requests"])
	if err != nil {
		t.Fatal(err)
	}
	var out []hetznerRequest
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("result.requests is not a list of {method, url, body}: %v\n%s", err, raw)
	}
	return out
}

// The firewall a media server gets: SSH for the operator's step, 80 for certificate
// issuance, 443 for signalling, the control API and TURN over TLS (routed by name), ICE
// over TCP 7881 and UDP 7882, TURN over UDP 3478, and ICMP.
var wantFirewallPorts = []string{"icmp", "tcp/22", "tcp/443", "tcp/7881", "tcp/80", "udp/3478", "udp/7882"}

func firewallPorts(t *testing.T, rules []any) []string {
	t.Helper()
	var out []string
	for _, r := range rules {
		rule, _ := r.(map[string]any)
		if rule["direction"] != "in" {
			t.Errorf("a firewall rule is not inbound: %v", rule)
		}
		sources, _ := rule["source_ips"].([]any)
		if !slices.Equal(sources, []any{"0.0.0.0/0", "::/0"}) {
			t.Errorf("rule %v admits %v, want every IPv4 and IPv6 address", rule["port"], sources)
		}
		port, _ := rule["port"].(string)
		proto, _ := rule["protocol"].(string)
		if proto == "icmp" {
			out = append(out, "icmp")
			continue
		}
		out = append(out, proto+"/"+port)
	}
	sort.Strings(out)
	return out
}

// TestProvisionHetznerDryRunPrintsTheRequestsAndCallsNothing: the dry run is the brief's
// test. It calls nothing, needs no token, and answers the two requests a run against an
// empty project would send -- the shared firewall, then the server -- exactly as they
// would be sent.
func TestProvisionHetznerDryRunPrintsTheRequestsAndCallsNothing(t *testing.T) {
	f := newHetznerFixture(t)
	f.env = []string{"HCLOUD_TOKEN="} // a dry run needs none
	env, stderr, code := f.run(t, m1Args("--dry-run=true")...)
	if code != 0 || !env.OK {
		t.Fatalf("dry run failed: exit %d, %+v", code, env.Error)
	}
	if env.Capability != "media.provisionHetzner" {
		t.Errorf("capability = %q, want media.provisionHetzner", env.Capability)
	}
	if env.Changed {
		t.Error("a dry run reported a change")
	}
	if calls := f.calls(t); len(calls) != 0 {
		t.Fatalf("a dry run called Hetzner: %v", calls)
	}
	if env.Result["dryRun"] != true {
		t.Errorf("dryRun = %v", env.Result["dryRun"])
	}
	for _, k := range []string{"serverId", "ipv4", "ipv6"} {
		if v, ok := env.Result[k]; !ok || v != nil {
			t.Errorf("a dry run answered %s = %v, want null: there is no server yet", k, v)
		}
	}

	reqs := dryRunRequests(t, env)
	if len(reqs) != 2 {
		t.Fatalf("a dry run lists %d requests, want the firewall and the server", len(reqs))
	}
	fw := reqs[0]
	if fw.Method != "POST" || fw.URL != "https://api.hetzner.cloud/v1/firewalls" {
		t.Errorf("first request %s %s, want POST https://api.hetzner.cloud/v1/firewalls", fw.Method, fw.URL)
	}
	if fw.Body["name"] != "memql-media" {
		t.Errorf("firewall name = %v, want memql-media", fw.Body["name"])
	}
	if labels, _ := fw.Body["labels"].(map[string]any); len(labels) != 1 || labels["memql-media"] != "" {
		t.Errorf("firewall labels = %v, want {memql-media: \"\"}, the mark that this script made it", fw.Body["labels"])
	}
	rules, _ := fw.Body["rules"].([]any)
	if got := firewallPorts(t, rules); !slices.Equal(got, wantFirewallPorts) {
		t.Errorf("firewall opens %v, want %v", got, wantFirewallPorts)
	}
	applyTo, _ := json.Marshal(fw.Body["apply_to"])
	if string(applyTo) != `[{"label_selector":{"selector":"memql-media"},"type":"label_selector"}]` {
		t.Errorf("firewall apply_to = %s, want the label selector memql-media, so every media server gets it", applyTo)
	}

	srv := reqs[1]
	if srv.Method != "POST" || srv.URL != "https://api.hetzner.cloud/v1/servers" {
		t.Errorf("second request %s %s, want POST https://api.hetzner.cloud/v1/servers", srv.Method, srv.URL)
	}
	for k, want := range map[string]any{
		"name":               "m1",
		"server_type":        "cpx31",
		"image":              "ubuntu-24.04",
		"location":           "ash",
		"start_after_create": true,
	} {
		if srv.Body[k] != want {
			t.Errorf("server %s = %v, want %v", k, srv.Body[k], want)
		}
	}
	if keys, _ := srv.Body["ssh_keys"].([]any); !slices.Equal(keys, []any{"ops"}) {
		t.Errorf("server ssh_keys = %v, want [ops]", srv.Body["ssh_keys"])
	}
	if labels, _ := srv.Body["labels"].(map[string]any); len(labels) != 1 || labels["memql-media"] != "m1" {
		t.Errorf("server labels = %v, want {memql-media: m1}, which is what puts it behind the firewall", srv.Body["labels"])
	}
	publicNet, _ := json.Marshal(srv.Body["public_net"])
	if string(publicNet) != `{"enable_ipv4":true,"enable_ipv6":true}` {
		t.Errorf("server public_net = %s, want both an IPv4 and an IPv6 address", publicNet)
	}
	if _, ok := srv.Body["firewalls"]; ok {
		t.Error("the server request names a firewall id, which a dry run cannot know; the label selector applies it")
	}
	userData, _ := srv.Body["user_data"].(string)
	if !strings.HasPrefix(userData, "#cloud-config\n") {
		t.Fatalf("user_data is not a cloud-config document: %.80q", userData)
	}
	if userData != renderedCloudInit(t) {
		t.Error("the dry run's user_data is not the cloud-init a real run renders")
	}
	if !strings.Contains(stderr, "#cloud-config") {
		t.Error("a dry run does not show the operator the rendered cloud-init")
	}
}

// renderedCloudInit is the cloud-init a dry run renders, for the tests that read it.
func renderedCloudInit(t *testing.T) string {
	t.Helper()
	f := newHetznerFixture(t)
	f.env = []string{"HCLOUD_TOKEN="}
	env, _, code := f.run(t, m1Args("--dry-run=true")...)
	if code != 0 || !env.OK {
		t.Fatalf("dry run failed: exit %d, %+v", code, env.Error)
	}
	reqs := dryRunRequests(t, env)
	if len(reqs) != 2 {
		t.Fatalf("a dry run lists %d requests", len(reqs))
	}
	userData, _ := reqs[1].Body["user_data"].(string)
	return userData
}

// cloudConfig is the slice of the rendered cloud-init these tests read.
type cloudConfig struct {
	PackageUpdate  bool     `yaml:"package_update"`
	PackageUpgrade bool     `yaml:"package_upgrade"`
	Packages       []string `yaml:"packages"`
	SSHPwauth      *bool    `yaml:"ssh_pwauth"`
	WriteFiles     []struct {
		Path        string `yaml:"path"`
		Permissions string `yaml:"permissions"`
		Encoding    string `yaml:"encoding"`
		Content     string `yaml:"content"`
	} `yaml:"write_files"`
	Runcmd []any `yaml:"runcmd"`
}

func parseCloudInit(t *testing.T, userData string) cloudConfig {
	t.Helper()
	var c cloudConfig
	if err := yaml.Unmarshal([]byte(userData), &c); err != nil {
		t.Fatalf("the rendered cloud-init does not parse as YAML: %v", err)
	}
	return c
}

// writtenFile is the content cloud-init writes at path, and its mode.
func writtenFile(t *testing.T, c cloudConfig, path string) (string, string) {
	t.Helper()
	for _, w := range c.WriteFiles {
		if w.Path == path {
			if w.Encoding != "" {
				t.Fatalf("%s is written %s-encoded; every file is written as plain text, so the user data reads as what the server gets", path, w.Encoding)
			}
			return w.Content, w.Permissions
		}
	}
	t.Fatalf("the cloud-init writes no %s", path)
	return "", ""
}

// runcmdLines is each runcmd entry as one line, for matching.
func runcmdLines(c cloudConfig) []string {
	var out []string
	for _, entry := range c.Runcmd {
		switch v := entry.(type) {
		case string:
			out = append(out, v)
		case []any:
			var parts []string
			for _, p := range v {
				parts = append(parts, toString(p))
			}
			out = append(out, strings.Join(parts, " "))
		}
	}
	return out
}

func toString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	default:
		b, _ := json.Marshal(s)
		return string(b)
	}
}

// TestTheCloudInitCarriesNoSecret: Hetzner keeps a server's user data and hands it out
// again, so nothing in it may be a credential. The dry run runs with HCLOUD_TOKEN and
// all eight media values in its environment, and none of them reaches the cloud-init or
// any output; LiveKit's configuration travels as the template, its keys as placeholders.
func TestTheCloudInitCarriesNoSecret(t *testing.T) {
	f := newHetznerFixture(t)
	secrets := map[string]string{
		"MEMQL_CALLS_MEDIA_API_KEY":        "callsapi" + randomHex(t, 12),
		"MEMQL_CALLS_MEDIA_API_SECRET":     randomHex(t, 32),
		"MEMQL_CALLS_MEDIA_WEBHOOK_KEY":    "callshook" + randomHex(t, 12),
		"MEMQL_CALLS_MEDIA_WEBHOOK_SECRET": randomHex(t, 32),
	}
	for k, v := range secrets {
		f.env = append(f.env, k+"="+v)
	}
	f.env = append(f.env,
		"MEMQL_CALLS_MEDIA_PROVIDER=hetzner",
		"MEMQL_CALLS_MEDIA_API_URL=https://m1.media.example.com",
		"MEMQL_CALLS_MEDIA_PUBLIC_URL=wss://m1.media.example.com",
		"MEMQL_CALLS_MEDIA_SEATS=50",
	)
	env, stderr, code := f.run(t, m1Args("--dry-run=true")...)
	if code != 0 || !env.OK {
		t.Fatalf("dry run failed: exit %d, %+v", code, env.Error)
	}
	reqs := dryRunRequests(t, env)
	userData, _ := reqs[1].Body["user_data"].(string)
	everything, _ := json.Marshal(env)
	for name, value := range secrets {
		if strings.Contains(userData, value) || strings.Contains(string(everything), value) || strings.Contains(stderr, value) {
			t.Errorf("%s from the environment reached the user data or the output", name)
		}
	}
	if strings.Contains(userData, f.token) {
		t.Error("the Hetzner API token reached the user data")
	}

	c := parseCloudInit(t, userData)
	for _, w := range c.WriteFiles {
		if w.Path == "/etc/memql-media/livekit.yaml" {
			t.Fatal("the cloud-init writes a rendered livekit.yaml, which would carry the key pairs; it writes the template")
		}
	}
	tmpl, _ := writtenFile(t, c, "/etc/memql-media/livekit.yaml.tmpl")
	var keys struct {
		Keys    map[string]string `yaml:"keys"`
		Webhook struct {
			APIKey string `yaml:"api_key"`
		} `yaml:"webhook"`
	}
	if err := yaml.Unmarshal([]byte(tmpl), &keys); err != nil {
		t.Fatalf("the LiveKit template in the cloud-init does not parse: %v", err)
	}
	if len(keys.Keys) != 2 || keys.Keys["{{API_KEY}}"] != "{{API_SECRET}}" || keys.Keys["{{WEBHOOK_KEY}}"] != "{{WEBHOOK_SECRET}}" {
		t.Errorf("the LiveKit template's keys are %v, want only the two placeholders", keys.Keys)
	}
	if keys.Webhook.APIKey != "{{WEBHOOK_KEY}}" {
		t.Errorf("webhook.api_key in the cloud-init is %q, want the placeholder", keys.Webhook.APIKey)
	}
	// Hetzner refuses user data over 32 KiB.
	if len(userData) > 32*1024 {
		t.Errorf("the rendered cloud-init is %d bytes, over Hetzner's 32 KiB limit for user data", len(userData))
	}
}

// TestTheCloudInitInstallsTheMediaServer: the first boot installs Docker from Ubuntu's
// archive, writes the server's compose file, the shared LiveKit template and its own
// command, opens the host firewall to exactly the ports the Hetzner firewall opens, and
// pulls the pinned images -- and starts nothing, because nothing can start before the
// operator's step hands the server its keys.
func TestTheCloudInitInstallsTheMediaServer(t *testing.T) {
	userData := renderedCloudInit(t)
	// The template's comments at the start of a line are the repository's explanation and
	// are not sent (Hetzner takes at most 32 KiB); only #cloud-config, which cloud-init
	// reads, starts a line with #.
	for i, line := range strings.Split(userData, "\n") {
		if i > 0 && strings.HasPrefix(line, "#") {
			t.Fatalf("user data line %d is a comment the provision script should have dropped: %q", i+1, line)
		}
	}
	c := parseCloudInit(t, userData)
	if !c.PackageUpdate || !c.PackageUpgrade {
		t.Errorf("package_update=%v package_upgrade=%v, want a first boot on current packages", c.PackageUpdate, c.PackageUpgrade)
	}
	for _, p := range []string{"docker.io", "docker-compose-v2", "ufw"} {
		if !slices.Contains(c.Packages, p) {
			t.Errorf("the cloud-init does not install %s (packages: %v)", p, c.Packages)
		}
	}
	if c.SSHPwauth == nil || *c.SSHPwauth {
		t.Error("ssh_pwauth is not false: the server is reached with the operator's key and no password")
	}

	// The template is the local target's own file, not a copy of it.
	tmpl, _ := writtenFile(t, c, "/etc/memql-media/livekit.yaml.tmpl")
	if want := readFile(t, filepath.Join(repoRoot(t), "deploy", "media", "livekit.yaml.tmpl")); tmpl != want {
		t.Error("the cloud-init's LiveKit template is not deploy/media/livekit.yaml.tmpl as it stands")
	}

	command, perm := writtenFile(t, c, "/usr/local/sbin/memql-media")
	if perm != "0755" || !strings.HasPrefix(command, "#!/usr/bin/env bash\n") {
		t.Errorf("/usr/local/sbin/memql-media is %s and starts %.30q, want an executable bash script", perm, command)
	}
	if out, err := exec.Command("bash", "-n", "-c", command).CombinedOutput(); err != nil {
		t.Errorf("memql-media is not valid bash: %v\n%s", err, out)
	}
	// Inside the template it is YAML to every linter, so it is linted here, as the server
	// receives it, wherever shellcheck is installed.
	if _, err := exec.LookPath("shellcheck"); err == nil {
		path := filepath.Join(t.TempDir(), "memql-media")
		if err := os.WriteFile(path, []byte(command), 0o755); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("shellcheck", "-S", "warning", path).CombinedOutput(); err != nil {
			t.Errorf("shellcheck finds fault with memql-media as the server receives it: %v\n%s", err, out)
		}
	} else {
		t.Log("shellcheck is not installed, so memql-media was checked for syntax only")
	}

	sysctl, _ := writtenFile(t, c, "/etc/sysctl.d/90-memql-media.conf")
	for _, want := range []string{"net.core.rmem_max", "net.core.wmem_max"} {
		if !strings.Contains(sysctl, want) {
			t.Errorf("the sysctl file sets no %s, so LiveKit's UDP socket buffers stay at the kernel's small default", want)
		}
	}

	lines := runcmdLines(c)
	var ufw []string
	for _, l := range lines {
		if strings.HasPrefix(l, "ufw allow ") {
			ufw = append(ufw, strings.TrimPrefix(l, "ufw allow "))
		}
	}
	sort.Strings(ufw)
	if want := []string{"22/tcp", "3478/udp", "443/tcp", "7881/tcp", "7882/udp", "80/tcp"}; !slices.Equal(ufw, want) {
		t.Errorf("the host firewall allows %v, want %v (the Hetzner firewall's ports)", ufw, want)
	}
	for _, want := range []string{
		"ufw --force enable",
		"systemctl enable --now docker",
		"docker compose --project-name memql-media --file /etc/memql-media/compose.yaml pull --quiet",
	} {
		if !slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, want) }) {
			t.Errorf("runcmd does not run %q:\n%s", want, strings.Join(lines, "\n"))
		}
	}
	for _, l := range lines {
		if strings.Contains(l, " up") && strings.Contains(l, "compose") {
			t.Errorf("runcmd starts the server (%q); it cannot run before the operator's step hands it its keys", l)
		}
	}
}

// TestTheServerKeepsItsCallsThroughADockerRestart: Ubuntu installs security updates by
// itself, and an update of docker.io restarts the Docker daemon, which stops every
// container it runs -- every call on the server, with nobody there to drain it. The first
// boot writes the daemon's configuration with live-restore on, so a restart of the daemon
// leaves the containers running. write_files runs at the start of the first boot, before
// the packages are installed, so the daemon has the setting from its first start.
func TestTheServerKeepsItsCallsThroughADockerRestart(t *testing.T) {
	c := parseCloudInit(t, renderedCloudInit(t))
	daemon, perm := writtenFile(t, c, "/etc/docker/daemon.json")
	var cfg map[string]any
	if err := json.Unmarshal([]byte(daemon), &cfg); err != nil {
		t.Fatalf("/etc/docker/daemon.json is not JSON, so the Docker daemon would not start: %v\n%s", err, daemon)
	}
	if cfg["live-restore"] != true {
		t.Errorf("/etc/docker/daemon.json sets live-restore to %v, want true: a restart of the daemon would end every call", cfg["live-restore"])
	}
	if perm != "0644" {
		t.Errorf("/etc/docker/daemon.json is %s, want 0644", perm)
	}
}

// TestTheServerKeepsItsLogsUnderAWeek: LiveKit logs participant identities, rooms and
// connection details on every join and leave, and Caddy logs every connection, and the
// design's retention commitment, which the privacy notice rests on, is that LiveKit's
// logs live less than seven days (design section 11). Ubuntu's Docker would keep every
// container's log in json-file with no size or age limit, for as long as the server
// runs -- and a server keeps its first boot until it is replaced. So the first boot sends
// every container's log to the system journal (daemon.json's log-driver, the default for
// every container, none of which sets its own) and caps the journal at six days and a
// size: MaxRetentionSec deletes a journal file once its oldest entry is past six days,
// MaxFileSec rotates files daily so a deletion takes at most a day's logs early and never
// leaves one past the bound, and SystemMaxUse keeps the disk safe. journald is restarted
// at first boot so the cap holds from the first container on. `docker logs` and `docker
// compose logs`, which the runbook uses, read the journald driver.
func TestTheServerKeepsItsLogsUnderAWeek(t *testing.T) {
	c := parseCloudInit(t, renderedCloudInit(t))
	daemon, _ := writtenFile(t, c, "/etc/docker/daemon.json")
	var cfg map[string]any
	if err := json.Unmarshal([]byte(daemon), &cfg); err != nil {
		t.Fatalf("/etc/docker/daemon.json is not JSON: %v\n%s", err, daemon)
	}
	if cfg["log-driver"] != "journald" {
		t.Errorf("/etc/docker/daemon.json sets log-driver %v, want journald: json-file keeps every container's log for good", cfg["log-driver"])
	}

	dropIn, perm := writtenFile(t, c, "/etc/systemd/journald.conf.d/90-memql-media.conf")
	if perm != "0644" {
		t.Errorf("the journald drop-in is %s, want 0644", perm)
	}
	settings := map[string]string{}
	section := ""
	for _, line := range strings.Split(dropIn, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
		case strings.HasPrefix(line, "["):
			section = line
		default:
			name, value, _ := strings.Cut(line, "=")
			if section == "[Journal]" {
				settings[strings.TrimSpace(name)] = strings.TrimSpace(value)
			}
		}
	}
	for name, bound := range map[string]time.Duration{"MaxRetentionSec": 6 * 24 * time.Hour, "MaxFileSec": 24 * time.Hour} {
		got, err := journaldSpan(settings[name])
		if err != nil {
			t.Errorf("[Journal] %s = %q: %v", name, settings[name], err)
			continue
		}
		if got <= 0 || got > bound {
			t.Errorf("[Journal] %s is %v, want more than none and at most %v, so no log outlives a week", name, got, bound)
		}
	}
	if settings["SystemMaxUse"] == "" {
		t.Error("[Journal] sets no SystemMaxUse, so the journal can fill the disk before it is six days old")
	}

	lines := runcmdLines(c)
	if !slices.Contains(lines, "systemctl restart systemd-journald") {
		t.Errorf("runcmd does not restart journald, so its cap holds only from the next reboot:\n%s", strings.Join(lines, "\n"))
	}

	raw, _ := writtenFile(t, c, "/etc/memql-media/compose.yaml")
	var compose struct {
		Services map[string]map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(raw), &compose); err != nil {
		t.Fatal(err)
	}
	for name, service := range compose.Services {
		if _, own := service["logging"]; own {
			t.Errorf("the %s container sets a log driver of its own, outside the journal's cap", name)
		}
	}
}

// journaldSpan reads a journald time span as the drop-in writes one: a whole number with
// one unit (s, min, h, day, week), or bare seconds.
func journaldSpan(value string) (time.Duration, error) {
	units := []struct {
		suffix string
		unit   time.Duration
	}{{"week", 7 * 24 * time.Hour}, {"day", 24 * time.Hour}, {"min", time.Minute}, {"h", time.Hour}, {"s", time.Second}}
	number, unit := value, time.Second
	for _, u := range units {
		if strings.HasSuffix(value, u.suffix) {
			number, unit = strings.TrimSuffix(value, u.suffix), u.unit
			break
		}
	}
	n, err := strconv.Atoi(number)
	if err != nil {
		return 0, fmt.Errorf("not a whole number of one unit")
	}
	return time.Duration(n) * unit, nil
}

// productionCompose is the slice of the server's compose file these tests read.
type productionCompose struct {
	Name     string `yaml:"name"`
	Services map[string]struct {
		Image       string   `yaml:"image"`
		Command     []string `yaml:"command"`
		NetworkMode string   `yaml:"network_mode"`
		Restart     string   `yaml:"restart"`
		Ports       []string `yaml:"ports"`
		Volumes     []string `yaml:"volumes"`
		Ulimits     struct {
			Nofile struct {
				Soft int `yaml:"soft"`
				Hard int `yaml:"hard"`
			} `yaml:"nofile"`
		} `yaml:"ulimits"`
	} `yaml:"services"`
}

// TestTheServerRunsTheReviewedImages pins the server's three images by tag AND digest
// (an internet-facing host pulls them unattended, and a tag can be pushed again), and
// holds LiveKit and Redis to the tags the local target runs: an upgrade is reviewed for
// CreateJobToken first, and it is one review, not one per target.
//
// The Caddy build is LiveKit's own (livekit/caddyl4, from github.com/livekit/deploy at
// a6e5931: Caddy v2.11.3 with the layer4 and YAML modules), the one its self-hosting
// generator writes into docker-compose.yaml.
func TestTheServerRunsTheReviewedImages(t *testing.T) {
	c := parseCloudInit(t, renderedCloudInit(t))
	raw, perm := writtenFile(t, c, "/etc/memql-media/compose.yaml")
	if perm != "0644" {
		t.Errorf("compose.yaml is %s, want 0644: it holds no credential", perm)
	}
	var prod productionCompose
	if err := yaml.Unmarshal([]byte(raw), &prod); err != nil {
		t.Fatalf("the server's compose file does not parse: %v", err)
	}
	if prod.Name != "memql-media" {
		t.Errorf("project %q, want memql-media", prod.Name)
	}
	for service, image := range map[string]string{
		"livekit": "livekit/livekit-server:v1.13.7@sha256:6fd3b7088874c4d119160dd688798dfec852bc014786d392caad15f6f63912a3",
		"redis":   "redis:7.4-alpine@sha256:7f7d30b7fdd09467bac58198a1f166dd69789d348d4fe859cccb20e0cf287fbe",
		"caddy":   "livekit/caddyl4:v2.11.3@sha256:9dd090159a6042a907dbff0ce9dda27a21d2a559dc7b03b60a8e6b1b60b8348e",
	} {
		if got := prod.Services[service].Image; got != image {
			t.Errorf("%s runs %q, want %q", service, got, image)
		}
	}
	if len(prod.Services) != 3 {
		t.Errorf("the server's compose file defines %d services, want livekit, redis and caddy", len(prod.Services))
	}

	var local composeFile
	if err := yaml.Unmarshal([]byte(readFile(t, filepath.Join(repoRoot(t), "deploy", "media", "compose.yaml"))), &local); err != nil {
		t.Fatal(err)
	}
	for _, service := range []string{"livekit", "redis"} {
		if want := local.Services[service].Image + "@sha256:"; !strings.HasPrefix(prod.Services[service].Image, want) {
			t.Errorf("the server's %s is %q and the local target's %q: the two targets run one reviewed version",
				service, prod.Services[service].Image, local.Services[service].Image)
		}
	}

	for name, s := range prod.Services {
		// Host networking, as LiveKit's layout runs it: TURN's relay and LiveKit's media
		// are one process talking to the host's own address, and Caddy reaches LiveKit on
		// loopback.
		if s.NetworkMode != "host" {
			t.Errorf("%s runs on network %q, want host", name, s.NetworkMode)
		}
		if len(s.Ports) != 0 {
			t.Errorf("%s publishes ports %v, which host networking ignores", name, s.Ports)
		}
		if s.Restart != "unless-stopped" {
			t.Errorf("%s restart = %q, want unless-stopped, so a reboot brings the server back", name, s.Restart)
		}
	}
	if got := prod.Services["livekit"].Volumes; !slices.Equal(got, []string{"./livekit.yaml:/etc/livekit/livekit.yaml:ro"}) {
		t.Errorf("livekit mounts %v", got)
	}
	if n := prod.Services["livekit"].Ulimits.Nofile; n.Soft < 65536 || n.Hard < 65536 {
		t.Errorf("livekit's open-file limit is %+v, want at least 65536", n)
	}
	if got := prod.Services["caddy"].Volumes; !slices.Equal(got, []string{"./caddy.json:/etc/caddy/caddy.json:ro", "/var/lib/memql-media/caddy:/data"}) {
		t.Errorf("caddy mounts %v; its certificates must live on the host, or every restart asks for new ones", got)
	}
	if got := strings.Join(prod.Services["redis"].Command, " "); !strings.Contains(got, "--bind 127.0.0.1") || !strings.Contains(got, "--protected-mode yes") {
		t.Errorf("redis runs %q, want it bound to loopback in protected mode", got)
	}

	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skip("docker compose is not installed, so the server's compose file was not validated by Compose")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("docker", "compose", "--file", path, "config", "--quiet").CombinedOutput(); err != nil {
		t.Errorf("docker compose config refused the server's compose file: %v\n%s", err, out)
	}
}

func TestProvisionHetznerRefusesARequestItCannotServe(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no name", []string{"--location=ash", "--server-type=cpx31", "--ssh-key=ops"}},
		{"a name that is not one DNS label", m1Args("--name=m1.media")},
		{"an upper-case name", m1Args("--name=M1")},
		{"a location outside the US", m1Args("--location=fsn1")},
		{"no location", []string{"--name=m1", "--server-type=cpx31", "--ssh-key=ops"}},
		{"no server type", []string{"--name=m1", "--location=ash", "--ssh-key=ops"}},
		{"a server type that is not a name", m1Args("--server-type=cpx 31")},
		{"an image the cloud-init is not written for", m1Args("--image=debian-12")},
		{"no ssh key", []string{"--name=m1", "--location=ash", "--server-type=cpx31"}},
		{"an ssh key name that is not a name", m1Args("--ssh-key=ops key")},
		{"a dry run that is not a boolean", m1Args("--dry-run=ture")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHetznerFixture(t)
			env, _, code := f.run(t, append(tc.args, "--confirm=provision-m1")...)
			if code != 2 || env.OK || env.Error == nil || env.Error.Code != 2 {
				t.Fatalf("exited %d with %+v, want a code-2 refusal", code, env.Error)
			}
			if calls := f.calls(t); len(calls) != 0 {
				t.Errorf("a refused request still called Hetzner: %v", calls)
			}
		})
	}
}

// A real run creates a server that bills by the hour, so it is refused without the
// phrase that names it -- and before anything reads the token or calls Hetzner.
func TestProvisionHetznerRealRunNeedsTheConfirmation(t *testing.T) {
	for _, confirm := range []string{"", "--confirm=provision-m2", "--confirm=yes"} {
		t.Run(confirm, func(t *testing.T) {
			f := newHetznerFixture(t)
			args := m1Args()
			if confirm != "" {
				args = append(args, confirm)
			}
			env, _, code := f.run(t, args...)
			if code != 3 || env.Error == nil || env.Error.Code != 3 {
				t.Fatalf("exited %d with %+v, want a code-3 refusal", code, env.Error)
			}
			if !strings.Contains(env.Error.Message, "provision-m1") {
				t.Errorf("the refusal does not name the phrase: %q", env.Error.Message)
			}
			if calls := f.calls(t); len(calls) != 0 {
				t.Errorf("an unconfirmed run called Hetzner: %v", calls)
			}
		})
	}
}

func TestProvisionHetznerRealRunNeedsTheToken(t *testing.T) {
	f := newHetznerFixture(t)
	f.env = []string{"HCLOUD_TOKEN="}
	env, _, code := f.run(t, m1Args("--confirm=provision-m1")...)
	if code != 4 || env.Error == nil || env.Error.Code != 4 {
		t.Fatalf("exited %d with %+v, want a code-4 failure", code, env.Error)
	}
	if !strings.Contains(env.Error.Message, "HCLOUD_TOKEN") {
		t.Errorf("the failure does not name HCLOUD_TOKEN: %q", env.Error.Message)
	}
	if calls := f.calls(t); len(calls) != 0 {
		t.Errorf("a run with no token called Hetzner: %v", calls)
	}

	// A value that could not be a token is refused before it goes anywhere: it is
	// written into curl's configuration, where a quote or a newline would be a new option.
	f = newHetznerFixture(t)
	f.env = []string{"HCLOUD_TOKEN=abc\"def"}
	env, _, code = f.run(t, m1Args("--confirm=provision-m1")...)
	if code != 2 || env.Error == nil || env.Error.Code != 2 {
		t.Fatalf("a malformed token exited %d with %+v, want a code-2 refusal", code, env.Error)
	}
	if calls := f.calls(t); len(calls) != 0 {
		t.Errorf("a run with a malformed token called Hetzner: %v", calls)
	}
}

// emptyProject makes the fake answer like a project with the SSH key and nothing else.
func (f *hetznerFixture) emptyProject(t *testing.T) {
	t.Helper()
	f.respond(t, "GET", "/servers?name=m1", 200, `{"servers":[],"meta":{}}`)
	f.respond(t, "GET", "/ssh_keys?name=ops", 200, `{"ssh_keys":[{"id":7,"name":"ops","fingerprint":"b7:2f"}],"meta":{}}`)
	f.respond(t, "GET", "/firewalls?name=memql-media", 200, `{"firewalls":[],"meta":{}}`)
	f.respond(t, "POST", "/firewalls", 201, `{"firewall":{"id":42,"name":"memql-media","labels":{"memql-media":""},
		"applied_to":[{"type":"label_selector","label_selector":{"selector":"memql-media"}}]},"actions":[]}`)
	f.respond(t, "POST", "/servers", 201, `{"server":{"id":1001,"name":"m1","status":"initializing",
		"labels":{"memql-media":"m1"},"location":{"name":"ash"},"server_type":{"name":"cpx31"},
		"public_net":{"ipv4":{"ip":"203.0.113.7"},"ipv6":{"ip":"2001:db8:4f8:1::/64"},"firewalls":[]}},
		"action":{"id":555,"status":"running","error":null},"next_actions":[],"root_password":null}`)
	f.respond(t, "GET", "/actions/555", 200, `{"action":{"id":555,"status":"success","error":null}}`)
	f.respond(t, "GET", "/servers/1001", 200, `{"server":{"id":1001,"name":"m1","status":"running",
		"labels":{"memql-media":"m1"},"location":{"name":"ash"},"server_type":{"name":"cpx31"},
		"public_net":{"ipv4":{"ip":"203.0.113.7"},"ipv6":{"ip":"2001:db8:4f8:1::/64"},"firewalls":[{"id":42,"status":"applied"}]}}}`)
}

// TestProvisionHetznerCreatesTheFirewallAndTheServer: a confirmed run against an empty
// project creates the firewall and the server, sends exactly what the dry run showed,
// hands the token to curl only on stdin, and answers {serverId, ipv4, ipv6}. The IPv6
// answer is the server's address, the first in its /64, which is what the AAAA record
// names.
func TestProvisionHetznerCreatesTheFirewallAndTheServer(t *testing.T) {
	f := newHetznerFixture(t)
	f.emptyProject(t)
	env, _, code := f.run(t, m1Args("--confirm=provision-m1")...)
	if code != 0 || !env.OK {
		t.Fatalf("run failed: exit %d, %+v", code, env.Error)
	}
	if !env.Changed {
		t.Error("creating a server reported no change")
	}
	for k, want := range map[string]any{
		"serverId":        float64(1001),
		"ipv4":            "203.0.113.7",
		"ipv6":            "2001:db8:4f8:1::1",
		"ipv6Network":     "2001:db8:4f8:1::/64",
		"firewallId":      float64(42),
		"firewallApplied": true,
		"created":         true,
		"dryRun":          false,
	} {
		if env.Result[k] != want {
			t.Errorf("result.%s = %v, want %v", k, env.Result[k], want)
		}
	}
	want := []string{
		"GET /servers?name=m1",
		"GET /ssh_keys?name=ops",
		"GET /firewalls?name=memql-media",
		"POST /firewalls",
		"POST /servers",
		"GET /actions/555",
		"GET /servers/1001",
	}
	if got := f.calls(t); !slices.Equal(got, want) {
		t.Errorf("calls:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}

	// What was sent is what the dry run showed.
	dry := newHetznerFixture(t)
	dryEnv, _, _ := dry.run(t, m1Args("--dry-run=true")...)
	reqs := dryRunRequests(t, dryEnv)
	for i, path := range []string{"/firewalls", "/servers"} {
		sent, _ := json.Marshal(f.bodyOf(t, "POST", path))
		shown, _ := json.Marshal(reqs[i].Body)
		if string(sent) != string(shown) {
			t.Errorf("POST %s sent a body the dry run did not show:\nsent:  %.300s\nshown: %.300s", path, sent, shown)
		}
	}

	// Every request carried the token, and only in curl's configuration on stdin.
	for i := range f.calls(t) {
		config := readFile(t, filepath.Join(f.hcloud, "config."+strconv.Itoa(i+1)))
		if strings.TrimSpace(config) != `header = "Authorization: Bearer `+f.token+`"` {
			t.Errorf("request %d was not authorized through curl's configuration on stdin", i+1)
		}
	}
}

// serverLocations is the two places Hetzner's API documents a server's location: a
// location object of its own, and, in the older form, the location of its datacenter.
var serverLocations = map[string]func(location string) string{
	"its location":              func(l string) string { return `"location":{"name":"` + l + `"}` },
	"its datacenter's location": func(l string) string { return `"datacenter":{"name":"` + l + `-dc1","location":{"name":"` + l + `"}}` },
}

// A second run finds the server it made and answers it, creating nothing: the
// capability is idempotent, and a server is never made twice. Hetzner may say where the
// server is in either of the forms its API documents, and a server in the place asked for
// is answered from either.
func TestProvisionHetznerAnswersTheServerItAlreadyMade(t *testing.T) {
	for form, location := range serverLocations {
		t.Run("a server answered with "+form, func(t *testing.T) {
			f := newHetznerFixture(t)
			f.respond(t, "GET", "/servers?name=m1", 200, `{"servers":[{"id":1001,"name":"m1","status":"running",
				"labels":{"memql-media":"m1"},`+location("ash")+`,"server_type":{"name":"cpx31"},
				"public_net":{"ipv4":{"ip":"203.0.113.7"},"ipv6":{"ip":"2001:db8:4f8:1::/64"},"firewalls":[{"id":42,"status":"applied"}]}}],"meta":{}}`)
			f.respond(t, "GET", "/firewalls?name=memql-media", 200, `{"firewalls":[{"id":42,"name":"memql-media","labels":{"memql-media":""},
				"rules":[],"applied_to":[{"type":"label_selector","label_selector":{"selector":"memql-media"}}]}],"meta":{}}`)
			env, _, code := f.run(t, m1Args("--confirm=provision-m1")...)
			if code != 0 || !env.OK {
				t.Fatalf("run failed: exit %d, %+v", code, env.Error)
			}
			if env.Changed {
				t.Error("finding the server already made reported a change")
			}
			if env.Result["serverId"] != float64(1001) || env.Result["ipv4"] != "203.0.113.7" || env.Result["ipv6"] != "2001:db8:4f8:1::1" {
				t.Errorf("answered %v / %v / %v", env.Result["serverId"], env.Result["ipv4"], env.Result["ipv6"])
			}
			if env.Result["created"] != false {
				t.Errorf("created = %v, want false", env.Result["created"])
			}
			if env.Result["firewallId"] != float64(42) || env.Result["firewallApplied"] != true {
				t.Errorf("firewallId = %v, firewallApplied = %v; want memql-media (42), applied to the server",
					env.Result["firewallId"], env.Result["firewallApplied"])
			}
			for _, call := range f.calls(t) {
				if strings.HasPrefix(call, "POST") {
					t.Errorf("a second run wrote to the project: %s", call)
				}
			}
		})
	}
}

func TestProvisionHetznerRefusesWhatItDidNotMake(t *testing.T) {
	t.Run("a server of that name it did not make", func(t *testing.T) {
		f := newHetznerFixture(t)
		f.respond(t, "GET", "/servers?name=m1", 200, `{"servers":[{"id":9,"name":"m1","labels":{},
			"location":{"name":"ash"},"server_type":{"name":"cpx31"},"public_net":{"ipv4":{"ip":"203.0.113.9"},"ipv6":null}}],"meta":{}}`)
		env, _, code := f.run(t, m1Args("--confirm=provision-m1")...)
		if code != 3 || env.Error == nil || env.Error.Code != 3 {
			t.Fatalf("exited %d with %+v, want a code-3 refusal", code, env.Error)
		}
	})
	for form, location := range serverLocations {
		t.Run("its server, somewhere else, answered with "+form, func(t *testing.T) {
			f := newHetznerFixture(t)
			f.respond(t, "GET", "/servers?name=m1", 200, `{"servers":[{"id":1001,"name":"m1","labels":{"memql-media":"m1"},
				`+location("hil")+`,"server_type":{"name":"cpx31"},"public_net":{"ipv4":{"ip":"203.0.113.7"},"ipv6":null}}],"meta":{}}`)
			env, _, code := f.run(t, m1Args("--confirm=provision-m1")...)
			if code != 3 || env.Error == nil || env.Error.Code != 3 {
				t.Fatalf("exited %d with %+v, want a code-3 refusal: an existing server is never changed to match", code, env.Error)
			}
			if !strings.Contains(env.Error.Message, "already exists in hil") {
				t.Errorf("the refusal does not say where the server is: %q", env.Error.Message)
			}
		})
	}
	t.Run("a firewall of that name it did not make", func(t *testing.T) {
		f := newHetznerFixture(t)
		f.emptyProject(t)
		f.respond(t, "GET", "/firewalls?name=memql-media", 200, `{"firewalls":[{"id":5,"name":"memql-media","labels":{},"rules":[],"applied_to":[]}],"meta":{}}`)
		env, _, code := f.run(t, m1Args("--confirm=provision-m1")...)
		if code != 3 || env.Error == nil || env.Error.Code != 3 {
			t.Fatalf("exited %d with %+v, want a code-3 refusal", code, env.Error)
		}
		for _, call := range f.calls(t) {
			if call == "POST /servers" {
				t.Error("a server was created behind a firewall this script does not own")
			}
		}
	})
}

// Its own firewall without the selector (someone removed it) gets it back before the
// server is created, so a new server is never left outside it.
func TestProvisionHetznerPutsItsFirewallBackOnTheSelector(t *testing.T) {
	f := newHetznerFixture(t)
	f.emptyProject(t)
	f.respond(t, "GET", "/firewalls?name=memql-media", 200, `{"firewalls":[{"id":42,"name":"memql-media","labels":{"memql-media":""},
		"rules":[],"applied_to":[]}],"meta":{}}`)
	f.respond(t, "POST", "/firewalls/42/actions/apply_to_resources", 201, `{"actions":[{"id":77,"status":"running","error":null}]}`)
	env, _, code := f.run(t, m1Args("--confirm=provision-m1")...)
	if code != 0 || !env.OK {
		t.Fatalf("run failed: exit %d, %+v", code, env.Error)
	}
	calls := f.calls(t)
	apply := slices.Index(calls, "POST /firewalls/42/actions/apply_to_resources")
	create := slices.Index(calls, "POST /servers")
	if apply < 0 || create < 0 || apply > create {
		t.Fatalf("the selector was not put back before the server was created: %v", calls)
	}
	body, _ := json.Marshal(f.bodyOf(t, "POST", "/firewalls/42/actions/apply_to_resources"))
	if string(body) != `{"apply_to":[{"label_selector":{"selector":"memql-media"},"type":"label_selector"}]}` {
		t.Errorf("apply_to_resources body = %s", body)
	}
	if slices.Contains(calls, "POST /firewalls") {
		t.Error("a second firewall was created beside the existing one")
	}
}

func TestProvisionHetznerReportsWhatHetznerRefused(t *testing.T) {
	t.Run("the server request", func(t *testing.T) {
		f := newHetznerFixture(t)
		f.emptyProject(t)
		f.respond(t, "POST", "/servers", 422, `{"error":{"code":"invalid_input","message":"invalid input in field 'server_type'","details":null}}`)
		env, _, code := f.run(t, m1Args("--confirm=provision-m1")...)
		if code != 5 || env.Error == nil || env.Error.Code != 5 {
			t.Fatalf("exited %d with %+v, want a code-5 failure", code, env.Error)
		}
		if !strings.Contains(env.Error.Message, "invalid input in field 'server_type'") {
			t.Errorf("the failure does not carry Hetzner's reason: %q", env.Error.Message)
		}
	})
	t.Run("the token", func(t *testing.T) {
		f := newHetznerFixture(t)
		f.emptyProject(t)
		f.env = []string{"HCLOUD_TOKEN=" + randomHex(t, 32)} // not the project's
		env, _, code := f.run(t, m1Args("--confirm=provision-m1")...)
		if code != 4 || env.Error == nil || env.Error.Code != 4 {
			t.Fatalf("exited %d with %+v, want a code-4 failure", code, env.Error)
		}
		if !strings.Contains(env.Error.Message, "HCLOUD_TOKEN") {
			t.Errorf("the failure does not name HCLOUD_TOKEN: %q", env.Error.Message)
		}
	})
	t.Run("a failed create action", func(t *testing.T) {
		f := newHetznerFixture(t)
		f.emptyProject(t)
		f.respond(t, "GET", "/actions/555", 200, `{"action":{"id":555,"status":"error","error":{"code":"resource_unavailable","message":"server location disabled"}}}`)
		env, _, code := f.run(t, m1Args("--confirm=provision-m1")...)
		if code != 5 || env.Error == nil || !strings.Contains(env.Error.Message, "server location disabled") {
			t.Fatalf("exited %d with %+v, want a code-5 failure carrying the action's reason", code, env.Error)
		}
	})
}

func TestProvisionHetznerWithoutTheSSHKeyIsAMissingPrerequisite(t *testing.T) {
	f := newHetznerFixture(t)
	f.emptyProject(t)
	f.respond(t, "GET", "/ssh_keys?name=ops", 200, `{"ssh_keys":[],"meta":{}}`)
	env, _, code := f.run(t, m1Args("--confirm=provision-m1")...)
	if code != 4 || env.Error == nil || env.Error.Code != 4 {
		t.Fatalf("exited %d with %+v, want a code-4 failure", code, env.Error)
	}
	if !strings.Contains(env.Error.Message, "ops") {
		t.Errorf("the failure does not name the key: %q", env.Error.Message)
	}
	for _, call := range f.calls(t) {
		if strings.HasPrefix(call, "POST") {
			t.Errorf("a run with no SSH key wrote to the project: %s", call)
		}
	}
}
