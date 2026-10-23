package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/znasllc-io/memql-cockpit/internal/config"
	"github.com/znasllc-io/memql-cockpit/internal/worker/tools"
	"gopkg.in/yaml.v3"
)

type managedHome struct {
	home        Home
	enabled     bool
	cancel      context.CancelFunc
	runner      *Runner
	wake        chan struct{}
	prepared    *homeRun
	duplicateOf string
	retrying    bool
}

type LocalHomeStatus struct {
	ID      string `json:"id"`
	Server  string `json:"server"`
	URL     string `json:"url"`
	OSURL   string `json:"os_url,omitempty"`
	Enabled bool   `json:"enabled"`
	State   string `json:"state"`
}

type LocalStatus struct {
	Version                  string            `json:"version"`
	PID                      int               `json:"pid"`
	Homes                    []LocalHomeStatus `json:"homes"`
	Accessibility            string            `json:"accessibility"`
	ScreenRecording          string            `json:"screen_recording"`
	PermissionDetail         string            `json:"permission_detail"`
	CheckedAt                time.Time         `json:"checked_at"`
	Executable               string            `json:"executable,omitempty"`
	BundleID                 string            `json:"bundle_id,omitempty"`
	BundlePath               string            `json:"bundle_path,omitempty"`
	PermissionRequests       bool              `json:"permission_requests"`
	PermissionRequestPending bool              `json:"permission_request_pending"`
	// Setup is the installer's report (setupprogress.go), with the person's
	// answers: what the Permissions window reads to know that setup is
	// waiting on it, and what `worker setup --permissions` reads back.
	Setup *LocalSetup `json:"setup,omitempty"`
}

func permissionLabel(d permissionDecision) string {
	switch d {
	case permissionGranted:
		return "Granted"
	case permissionDenied:
		return "Not granted"
	default:
		return "Unknown"
	}
}

func (f *Fleet) LocalStatus() LocalStatus {
	p := currentPermissionSnapshot()
	status := LocalStatus{Version: cockpitVersion(), PID: os.Getpid(), Homes: []LocalHomeStatus{}, Accessibility: permissionLabel(p.accessibility), ScreenRecording: permissionLabel(p.screenRecording), PermissionDetail: p.detail, CheckedAt: time.Now()}
	status.Executable, _ = os.Executable()
	status.BundleID, status.BundlePath = currentWorkerBundle()
	status.PermissionRequests = permissionRequestsSupported()
	status.PermissionRequestPending = localPermissionRequests.pending.Load()
	status.Setup = f.setup.Local()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, home := range f.workers.Homes {
		h := f.managed[home.ID]
		enabled, state := home.IsEnabled(), "Starting"
		if h != nil {
			enabled = h.enabled
			switch {
			case h.duplicateOf != "":
				state = "Duplicate of " + h.duplicateOf
			case !h.enabled && h.runner != nil:
				state = "Pausing"
			case !h.enabled:
				state = "Paused"
			case h.runner != nil && h.runner.RegistrationId() != "":
				state = "Connected"
			case h.runner != nil:
				state = "Connecting / retrying"
			case h.retrying:
				state = "Disconnected / retrying"
			default:
				state = "Starting"
			}
		} else if !enabled {
			state = "Paused"
		}
		u, _ := url.Parse(home.ClusterURL)
		server, address := home.ID, ""
		if u != nil && (u.Scheme == "http" || u.Scheme == "https") {
			// Never expose URL userinfo, query secrets or enrollment paths to UI.
			server = u.Host
			address = (&url.URL{Scheme: u.Scheme, Host: u.Host}).String()
		}
		status.Homes = append(status.Homes, LocalHomeStatus{ID: home.ID, Server: server, URL: address, OSURL: homeOSURL(home), Enabled: enabled, State: state})
	}
	return status
}

func (f *Fleet) SetHomeEnabled(id string, enabled bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	h := f.managed[id]
	if h == nil {
		return errors.New("server is not configured or worker is still starting")
	}
	// A duplicate enrollment must never silently reconnect a server whose
	// active home was paused. Refuse ambiguous controls until it is resolved.
	key := strings.ToLower(hostOf(h.home.ClusterURL))
	for _, other := range f.managed {
		if other != h && strings.ToLower(hostOf(other.home.ClusterURL)) == key {
			return errors.New("multiple enrollments target this server; resolve the duplicate in workers.yaml before changing its connection policy")
		}
	}
	if h.enabled == enabled {
		return nil
	}
	// Persist first. A disk failure must leave the current connection policy
	// unchanged, and a restart must never undo a successful pause.
	if err := saveHomeEnabled(f.workersPath, id, enabled); err != nil {
		return err
	}
	h.enabled = enabled
	if !enabled && h.cancel != nil {
		h.cancel()
	}
	select {
	case h.wake <- struct{}{}:
	default:
	}
	f.logger.Info("local connection policy changed", "home", id, "enabled", enabled)
	return nil
}

// Edit only the selected enabled node, retaining tokens, other homes, unknown
// keys and comments. Write atomically with credential-file permissions.
func saveHomeEnabled(path, id string, enabled bool) error {
	if path == "" {
		path = DefaultWorkersPath()
	}
	if err := config.VerifyCredentialFileMode(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || !ownedByCurrentUser(info) {
		return errors.New("workers file must be a regular file owned by the current user")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return err
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return errors.New("invalid workers file")
	}
	root := doc.Content[0]
	found := false
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "homes" {
			continue
		}
		if root.Content[i+1].Kind != yaml.SequenceNode {
			return errors.New("invalid homes list")
		}
		for _, home := range root.Content[i+1].Content {
			if home.Kind != yaml.MappingNode {
				return errors.New("invalid home entry")
			}
			match, enabledIndex := false, -1
			for j := 0; j+1 < len(home.Content); j += 2 {
				if home.Content[j].Value == "id" && home.Content[j+1].Value == id {
					match = true
				}
				if home.Content[j].Value == "enabled" {
					enabledIndex = j + 1
				}
			}
			if !match {
				continue
			}
			if found {
				return errors.New("duplicate home ID")
			}
			found = true
			value := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: fmt.Sprint(enabled)}
			if enabledIndex >= 0 {
				home.Content[enabledIndex].Tag = "!!bool"
				home.Content[enabledIndex].Value = value.Value
			} else {
				home.Content = append(home.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "enabled"}, value)
			}
		}
	}
	if !found {
		return errors.New("server is no longer in workers.yaml")
	}
	updated, err := yaml.Marshal(&doc)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".workers-control-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(updated); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func defaultControlPath() string { return filepath.Join(homeDir(), ".memql", "control", "worker.sock") }

type controlRequest struct {
	Action     string `json:"action"`
	Home       string `json:"home,omitempty"`
	Enabled    *bool  `json:"enabled,omitempty"`
	Permission string `json:"permission,omitempty"`
	PID        int    `json:"pid,omitempty"`
	// Setup is the installer's report for the "setup" action
	// (setupprogress.go).
	Setup *SetupControl `json:"setup,omitempty"`
}
type controlResponse struct {
	OK          bool                     `json:"ok"`
	Error       string                   `json:"error,omitempty"`
	Status      *LocalStatus             `json:"status,omitempty"`
	Logs        []LocalLogEntry          `json:"logs,omitempty"`
	InputTarget *tools.InputTargetStatus `json:"input_target,omitempty"`
}

func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Getuid()
}

func verifyControlDirectory(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 || !ownedByCurrentUser(info) {
		return errors.New("control directory must be owned by this user with mode 0700")
	}
	return nil
}

func listenControl(ctx context.Context, path string, fleet *Fleet) (net.Listener, error) {
	if err := verifyControlDirectory(path); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 || !ownedByCurrentUser(info) {
			return nil, errors.New("unsafe existing control socket")
		}
		conn, err := net.DialTimeout("unix", path, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil, errors.New("worker control socket already in use; another worker is running")
		}
		if !errors.Is(err, syscall.ECONNREFUSED) && !errors.Is(err, syscall.ENOENT) {
			return nil, err
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		listener.Close()
		return nil, err
	}
	go func() { <-ctx.Done(); listener.Close() }()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go serveControlConnection(conn, fleet)
		}
	}()
	return listener, nil
}

func serveControlConnection(conn net.Conn, fleet *Fleet) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if !controlPeerIsCurrentUser(conn) {
		return
	}
	var request controlRequest
	decoder := json.NewDecoder(io.LimitReader(conn, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		json.NewEncoder(conn).Encode(controlResponse{Error: "invalid control request"})
		return
	}
	response := controlResponse{OK: true}
	switch request.Action {
	case "request-permission":
		if err := localPermissionRequests.request(request.Permission, request.PID, permissionRequestsSupported(), promptForPermission); err != nil {
			response.OK = false
			response.Error = err.Error()
		}
	case "status":
		status := fleet.LocalStatus()
		response.Status = &status
	case "input-target-status":
		status := tools.DesktopInputTargetStatus()
		response.InputTarget = &status
	case "home":
		if request.Enabled == nil || request.Home == "" {
			response.OK = false
			response.Error = "home and enabled are required"
		} else if err := fleet.SetHomeEnabled(request.Home, *request.Enabled); err != nil {
			response.OK = false
			response.Error = sanitizeLocalLog(err.Error())
		} else {
			status := fleet.LocalStatus()
			response.Status = &status
		}
	case "logs":
		logs, err := readLocalWorkerLogs(fleet.workers.StateDir)
		if err != nil {
			response.OK = false
			response.Error = sanitizeLocalLog(err.Error())
		} else {
			response.Logs = logs
		}
	case "setup":
		if request.Setup == nil {
			response.OK = false
			response.Error = "a setup report is required"
		} else if err := fleet.setup.Apply(*request.Setup); err != nil {
			response.OK = false
			response.Error = err.Error()
		}
	default:
		response.OK = false
		response.Error = "unknown control action"
	}
	json.NewEncoder(conn).Encode(response)
}

func callControl(path string, request controlRequest) (controlResponse, error) {
	var response controlResponse
	if err := verifyControlDirectory(path); err != nil {
		return response, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return response, errors.New("background worker is not available")
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0600 || !ownedByCurrentUser(info) {
		return response, errors.New("unsafe control socket")
	}
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return response, errors.New("background worker is not available")
	}
	defer conn.Close()
	if !controlPeerIsCurrentUser(conn) {
		return response, errors.New("worker socket belongs to another user")
	}
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err = json.NewEncoder(conn).Encode(request); err != nil {
		return response, err
	}
	err = json.NewDecoder(bufio.NewReader(io.LimitReader(conn, 1024*1024))).Decode(&response)
	return response, err
}

func handleControl(args []string) {
	fs := flag.NewFlagSet("worker control", flag.ContinueOnError)
	action := fs.String("action", "status", "status, input-target-status (explicit read-only focus diagnostic), logs, home, setup (the installer's progress), or request-permission (explicit local macOS prompt)")
	permission := fs.String("permission", "", "accessibility or screen_recording, for request-permission")
	pid := fs.Int("pid", 0, "expected worker process ID, required for request-permission")
	home := fs.String("home", "", "configured server ID")
	enabled := fs.String("enabled", "", "true to allow connecting, false to pause")
	plan := fs.String("plan", "", "for setup: the stages the installer will run, comma-separated (permissions, runtime, models, workbench)")
	finished := fs.Bool("finished", false, "for setup: the installer has finished")
	reset := fs.Bool("reset", false, "for setup: this install runs no stage; forget what an earlier one reported")
	stage := fs.String("stage", "", "for setup with --answer: the stage waiting on the person (permissions)")
	answer := fs.String("answer", "", "for setup: the person's choice for --stage (skip: go on without what is not allowed yet)")
	wait := fs.Duration("wait", 0, "for setup: keep trying this long while the worker is still starting")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		os.Exit(2)
	}
	request := controlRequest{Action: *action, Home: *home, Permission: *permission, PID: *pid}
	if *enabled != "" {
		if *enabled != "true" && *enabled != "false" {
			fmt.Fprintln(os.Stderr, "enabled must be true or false")
			os.Exit(2)
		}
		v := *enabled == "true"
		request.Enabled = &v
	}
	if request.Action == "setup" {
		request.Setup = setupControlFromFlags(*plan, *finished, *reset, *stage, *answer)
	}
	response, err := callControl(defaultControlPath(), request)
	// The installer reports its plan the moment it has started the worker,
	// which is before the worker has opened this socket. --wait covers that
	// start rather than making the installer guess a sleep.
	for deadline := time.Now().Add(*wait); err != nil && request.Action == "setup" && time.Now().Before(deadline); {
		time.Sleep(250 * time.Millisecond)
		response, err = callControl(defaultControlPath(), request)
	}
	if err != nil && request.Action == "logs" {
		var logs []LocalLogEntry
		logs, err = readLocalWorkerLogs(Defaults().StateDir)
		if err == nil {
			response = controlResponse{OK: true, Logs: logs}
		}
	}
	if err != nil {
		response = controlResponse{Error: sanitizeLocalLog(err.Error())}
	}
	json.NewEncoder(os.Stdout).Encode(response)
	if err != nil || !response.OK {
		os.Exit(1)
	}
}

// An explicit per-home URL is authoritative. The local default is the one
// published local ingress contract; arbitrary API hosts are never rewritten.
func homeOSURL(home Home) string {
	raw := home.OSURL
	if raw == "" {
		u, err := url.Parse(home.ClusterURL)
		if err == nil && u.Hostname() == "api.memql.localhost" {
			raw = "https://os.memql.localhost/"
		}
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	return u.String()
}

// setupControlFromFlags is `worker control --action=setup`'s report: the
// plan when --plan names stages, otherwise the person's answer when --answer
// is set, otherwise the end or a reset. A report that is none of them, or
// more than one, is sent as such and refused by the worker, which says why.
func setupControlFromFlags(plan string, finished, reset bool, stage, answer string) *SetupControl {
	c := &SetupControl{}
	for _, name := range strings.Split(plan, ",") {
		if name = strings.TrimSpace(name); name != "" {
			c.Plan = append(c.Plan, name)
		}
	}
	if strings.TrimSpace(answer) != "" {
		c.Answer = &SetupAnswer{Stage: strings.TrimSpace(stage), Choice: strings.TrimSpace(answer)}
	}
	if len(c.Plan) == 0 && c.Answer == nil {
		c.Finished, c.Reset = finished, reset
	}
	return c
}
