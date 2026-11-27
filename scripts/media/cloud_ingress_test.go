package media

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCloudIngressIsOptionalAndResourceBounded(t *testing.T) {
	s := newServerFixture(t)
	if _, _, code := s.configure(t, "--wait-seconds=0"); code != 0 {
		t.Fatalf("configure exit=%d", code)
	}
	if _, err := os.Stat(filepath.Join(s.etc, "ingress.yaml")); !os.IsNotExist(err) {
		t.Fatal("ordinary configure enabled a publisher")
	}
	for _, call := range s.upCalls(t) {
		if strings.Contains(call, "--profile") {
			t.Fatal("ordinary configure activated a resource profile")
		}
	}
	var compose struct {
		Services map[string]struct {
			Image       string            `yaml:"image"`
			Profiles    []string          `yaml:"profiles"`
			Network     string            `yaml:"network_mode"`
			CPUs        float64           `yaml:"cpus"`
			Memory      string            `yaml:"mem_limit"`
			PIDs        int               `yaml:"pids_limit"`
			Environment map[string]string `yaml:"environment"`
			Volumes     []string          `yaml:"volumes"`
			Ports       []string          `yaml:"ports"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, filepath.Join(s.etc, "compose.yaml"))), &compose); err != nil {
		t.Fatal(err)
	}
	i := compose.Services["ingress"]
	if i.Image != "livekit/ingress:v1.6.0@sha256:8417446ede56ed378a83fc97bcdc335a328d62b5fb5b1061377f56741b3c276e" || !slices.Equal(i.Profiles, []string{"resources"}) || i.Network != "host" || i.CPUs != 1.25 || i.Memory != "1536m" || i.PIDs != 256 {
		t.Fatal("optional ingress must retain exact reviewed image and explicit CPU/memory/process caps")
	}
	if !reflect.DeepEqual(i.Environment, map[string]string{"INGRESS_CONFIG_FILE": "/etc/livekit/ingress.yaml"}) || !slices.Equal(i.Volumes, []string{"./ingress.yaml:/etc/livekit/ingress.yaml:ro"}) || len(i.Ports) != 0 {
		t.Fatal("ingress credentials must live only in its private mounted config")
	}
	_, stderr, code := s.configure(t, "--ingress=true", "--wait-seconds=0")
	if code != 0 {
		t.Fatalf("enable exit=%d", code)
	}
	if !strings.Contains(stderr, "UDP 7885") || !strings.Contains(stderr, "raw TCP 8080/8081/9090 must remain closed") {
		t.Fatal("configure must disclose required optional UDP and private HTTP firewall boundary")
	}
	var cfg struct {
		APIKey    string `yaml:"api_key"`
		APISecret string `yaml:"api_secret"`
		WSURL     string `yaml:"ws_url"`
		Redis     struct {
			Address string `yaml:"address"`
		} `yaml:"redis"`
		RTMP       int  `yaml:"rtmp_port"`
		WHIP       int  `yaml:"whip_port"`
		Relay      int  `yaml:"http_relay_port"`
		Health     int  `yaml:"health_port"`
		Debug      int  `yaml:"debug_handler_port"`
		Prometheus int  `yaml:"prometheus_port"`
		UDPPull    bool `yaml:"enable_udp_url_pull"`
		RTC        struct {
			UDP      int    `yaml:"udp_port"`
			NodeIP   string `yaml:"node_ip"`
			External bool   `yaml:"use_external_ip"`
		} `yaml:"rtc_config"`
		CPU     map[string]float64 `yaml:"cpu_cost"`
		Logging map[string]string  `yaml:"logging"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, filepath.Join(s.etc, "ingress.yaml"))), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.APIKey != s.values["MEMQL_CALLS_MEDIA_API_KEY"] || cfg.APISecret != s.values["MEMQL_CALLS_MEDIA_API_SECRET"] || cfg.WSURL != "ws://127.0.0.1:7880" || cfg.Redis.Address != "127.0.0.1:6379" {
		t.Fatal("trusted publisher must retain the exact operator key and loopback provider/Redis")
	}
	if cfg.RTMP != -1 || cfg.WHIP != 8080 || cfg.Relay != 9090 || cfg.Health != 0 || cfg.Debug != 0 || cfg.Prometheus != 0 || cfg.UDPPull || cfg.RTC.UDP != 7885 || cfg.RTC.NodeIP != s.publicIPv4 || cfg.RTC.External {
		t.Fatal("unexpected ingress input/listener or ICE address")
	}
	if !reflect.DeepEqual(cfg.CPU, map[string]float64{"whip_cpu_cost": 1, "whip_bypass_transcoding_cpu_cost": 1, "rtmp_cpu_cost": 1, "url_cpu_cost": 1, "min_idle_ratio": 0.1}) || !reflect.DeepEqual(cfg.Logging, map[string]string{"level": "warn"}) {
		t.Fatal("provider load-admission/log bounds changed")
	}
	if info, err := os.Stat(filepath.Join(s.etc, "ingress.yaml")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("ingress credentials must be mode0600")
	}
	if !strings.Contains(readFile(t, filepath.Join(s.etc, "livekit.yaml")), "whip_base_url: \"https://m1.media.example.com/whip\"") {
		t.Fatal("resource endpoint must use existing verified public TLS host")
	}
	for _, secret := range s.secrets() {
		if strings.Contains(readFile(t, s.log), secret) {
			t.Fatal("credentials leaked to an executed command")
		}
	}
}

func TestCloudIngressAllowsOnlyExactWHIPProtocolRoutes(t *testing.T) {
	s := newServerFixture(t)
	if _, _, code := s.configure(t, "--ingress=true", "--wait-seconds=0"); code != 0 {
		t.Fatalf("configure exit=%d", code)
	}
	routes := cloudGuardRoutes(t, s)
	for _, index := range []int{4, 5} {
		r := routes[index]
		if len(r.Match) != 1 || len(r.Match[0].Path) != 0 || len(r.Match[0].Header) != 0 || len(r.Handle) != 2 || r.Handle[0].Handler != "request_body" || r.Handle[0].MaxSize != 131072 || r.Handle[1].Handler != "reverse_proxy" || len(r.Handle[1].Upstreams) != 1 || r.Handle[1].Upstreams[0].Dial != "127.0.0.1:8080" {
			t.Fatal("WHIP may only forward bounded bodies to the fixed local worker")
		}
		if !slices.Contains(r.Handle[1].Headers.Request.Delete, "X-MemQL-Media-Signal") || slices.Contains(r.Handle[1].Headers.Request.Delete, "Authorization") {
			t.Fatal("WHIP must remove signal guard while retaining its stream-key authentication")
		}
	}
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{"POST", "/whip", 4}, {"POST", "/whip/stream_KEY-12", 4}, {"OPTIONS", "/whip/stream_KEY-12", 4},
		{"PATCH", "/whip/stream_KEY-12/WH_resource", 5}, {"DELETE", "/whip/stream_KEY-12/WH_resource", 5}, {"OPTIONS", "/whip/stream_KEY-12/WH_resource", 5},
		{"GET", "/whip/stream_KEY-12", -1}, {"PUT", "/whip/stream_KEY-12", -1}, {"POST", "/whip/key/resource", -1},
		{"POST", "/whip/", -1}, {"POST", "/whip/../rtc", -1}, {"POST", "/whip/key/extra/nested", -1},
		{"POST", "/rtmp/key", -1}, {"GET", "/whip/health", -1}, {"POST", "/whip/" + strings.Repeat("a", 257), -1},
	} {
		got := -1
		for _, index := range []int{4, 5} {
			m := routes[index].Match[0]
			if slices.Contains(m.Method, tc.method) && regexp.MustCompile(m.PathRegexp.Pattern).MatchString(tc.path) {
				got = index
				break
			}
		}
		if got != tc.want {
			t.Errorf("%s %s route%d,want%d", tc.method, tc.path, got, tc.want)
		}
	}
	if len(routes[6].Match) != 0 || len(routes[6].Handle) != 1 || routes[6].Handle[0].StatusCode != 404 {
		t.Fatal("other resource paths must fail closed")
	}
}

func TestCloudIngressUpgradePreservesExistingCredentialsTURNAndCertificates(t *testing.T) {
	s := newServerFixture(t)
	if _, _, code := s.configure(t, "--wait-seconds=0"); code != 0 {
		t.Fatalf("baseline exit=%d", code)
	}
	var before map[string]any
	if err := yaml.Unmarshal([]byte(readFile(t, filepath.Join(s.etc, "livekit.yaml"))), &before); err != nil {
		t.Fatal(err)
	}
	certificate := filepath.Join(s.data, "caddy", "preserved-certificate")
	if err := os.WriteFile(certificate, []byte("existing operator certificate state"), 0o600); err != nil {
		t.Fatal(err)
	}
	// An existing host retains its earlier rendered file until the reviewed
	// helper/template installation and explicit configure. No cloud-init rerun.
	if err := os.WriteFile(filepath.Join(s.etc, "caddy.json"), []byte(`{"legacy":"direct-provider"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s.running = "caddy livekit redis"
	_ = os.Remove(s.log)
	if _, _, code := s.configure(t, "--ingress=true", "--wait-seconds=0"); code != 0 {
		t.Fatalf("upgrade exit=%d", code)
	}
	var after map[string]any
	if err := yaml.Unmarshal([]byte(readFile(t, filepath.Join(s.etc, "livekit.yaml"))), &after); err != nil {
		t.Fatal(err)
	}
	delete(after, "ingress")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("adding the publisher changed existing provider credentials, TURN or RTC configuration")
	}
	if readFile(t, certificate) != "existing operator certificate state" {
		t.Fatal("upgrade replaced certificate state")
	}
	if calls := s.upCalls(t); len(calls) != 1 || !strings.Contains(calls[0], "--profile resources") || !strings.Contains(calls[0], "--force-recreate") {
		t.Fatal("drained upgrade must recreate exact existing project with its resource profile")
	}
	if strings.Contains(readFile(t, s.log), "ufw") || strings.Contains(readFile(t, s.log), "hcloud") {
		t.Fatal("configure must not overwrite an operator's host/cloud firewall rules")
	}
	s.running = "caddy ingress livekit redis"
	_ = os.Remove(s.log)
	if _, _, code := s.configure(t, "--wait-seconds=0"); code != 0 {
		t.Fatalf("retained configure exit=%d", code)
	}
	if len(s.upCalls(t)) != 0 {
		t.Fatal("unchanged configured publisher was not retained idempotently")
	}
	if _, _, code := s.configure(t, "--ingress=false", "--wait-seconds=0"); code != 2 {
		t.Fatal("unreviewed publisher disable must be refused")
	}
	if _, err := os.Stat(filepath.Join(s.etc, "ingress.yaml")); err != nil {
		t.Fatal("refusal removed existing publisher configuration")
	}
}
