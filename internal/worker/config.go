// Package worker implements the `memql worker` run mode.
// It connects to a memql cluster using a worker token, registers the
// local machine via WorkerService.Stream, and serves dispatched
// tool calls.
package worker

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/znasllc-io/memql-cockpit/internal/config"
)

// Config is the worker run mode configuration. Backed by
// ~/.memql/worker.yaml; CLI flags / env vars override.
type Config struct {
	workbenchReady bool
	ClusterURL     string            `yaml:"cluster_url"`
	Token          string            `yaml:"token"`
	Name           string            `yaml:"name"`
	Labels         map[string]string `yaml:"labels"`
	Concurrency    map[string]uint32 `yaml:"concurrency"`
	StateDir       string            `yaml:"state_dir"`
	LogLevel       string            `yaml:"log_level"`
	Capabilities   []string          `yaml:"capabilities"`

	// Home is the id this cluster is enrolled under in workers.yaml (the
	// cluster's URL host on the single-home path). It names the stream in
	// metrics and scopes the consent window. Never written to worker.yaml:
	// the file's own cluster_url is where a reader derives it from.
	Home string `yaml:"-"`
	// MachineID is this machine's id, resolved once for the whole machine
	// by a caller that runs several homes (see machineIDFor). Empty
	// resolves it from StateDir's machine root at Register.
	MachineID string `yaml:"-"`
}

// homeID is the stream's name: the enrolled home id, else the cluster's
// URL host, which is what a single-home run would have been enrolled as.
func (c Config) homeID() string {
	if id := strings.TrimSpace(c.Home); id != "" {
		return id
	}
	return HomeIDFromURL(c.ClusterURL)
}

// Defaults returns a Config seeded with sensible defaults for the
// running platform. The caller layers in user-supplied YAML + CLI
// flags on top.
func Defaults() Config {
	hostname, _ := os.Hostname()
	return Config{
		Name:     hostname,
		LogLevel: "info",
		StateDir: defaultStateDir(),
		Labels: map[string]string{
			"os":       runtime.GOOS,
			"arch":     runtime.GOARCH,
			"hostname": hostname,
		},
		Concurrency: map[string]uint32{
			"HEADLESS":    8,
			"COMPUTERUSE": 1,
		},
		Capabilities: []string{"HEADLESS"},
	}
}

// LoadFile reads a worker.yaml file at the supplied path. Missing
// file returns the defaults; malformed file is an error.
//
// The worker config stores the worker token (mql_wkr_*) in
// plaintext; the file MUST be 0600. VerifyCredentialFileMode rejects
// loads from a group- or world-readable file so a drift in
// permissions surfaces loudly instead of silently exposing the
// worker token to other local users.
func LoadFile(path string) (Config, error) {
	cfg := Defaults()
	if _, statErr := os.Stat(path); statErr == nil {
		if err := config.VerifyCredentialFileMode(path); err != nil {
			return cfg, err
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("worker config: read %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("worker config: parse %s: %w", path, err)
	}
	if cfg.Name == "" {
		hostname, _ := os.Hostname()
		cfg.Name = hostname
	}
	if cfg.Concurrency == nil {
		cfg.Concurrency = map[string]uint32{"HEADLESS": 8, "COMPUTERUSE": 1}
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	if cfg.StateDir == "" {
		cfg.StateDir = defaultStateDir()
	}
	if len(cfg.Capabilities) == 0 {
		cfg.Capabilities = []string{"HEADLESS"}
	}
	return cfg, nil
}

// Validate confirms required fields are present.
func (c Config) Validate() error {
	if strings.TrimSpace(c.ClusterURL) == "" {
		return errors.New("worker config: cluster_url required")
	}
	if strings.TrimSpace(c.Token) == "" {
		return errors.New("worker config: token required")
	}
	if !strings.HasPrefix(strings.TrimSpace(c.Token), "mql_wkr_") {
		return errors.New("worker config: token must start with mql_wkr_")
	}
	if strings.TrimSpace(c.Name) == "" {
		return errors.New("worker config: name required")
	}
	hasHeadless := false
	for _, cap := range c.Capabilities {
		if cap == "HEADLESS" {
			hasHeadless = true
			break
		}
	}
	if !hasHeadless {
		return errors.New("worker config: HEADLESS capability is mandatory")
	}
	return nil
}

// DefaultConfigPath returns ~/.memql/worker.yaml.
func DefaultConfigPath() string {
	home := homeDir()
	if home == "" {
		return "worker.yaml"
	}
	return filepath.Join(home, ".memql", "worker.yaml")
}

func defaultStateDir() string {
	home := homeDir()
	if home == "" {
		return ".memql/state"
	}
	return filepath.Join(home, ".memql", "state")
}

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	if u, err := user.Current(); err == nil {
		return u.HomeDir
	}
	return ""
}
