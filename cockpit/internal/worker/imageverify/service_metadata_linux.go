//go:build linux

package imageverify

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type limitedBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if n > b.limit-b.Len() {
		b.exceeded = true
		p = p[:max(0, b.limit-b.Len())]
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}

func serviceProperties(ctx context.Context, c ServiceConfig, unit string) (map[string]string, error) {
	if unit != "memql-imageverify.socket" && !serviceInstance.MatchString(unit) {
		return nil, errors.New("invalid fixed service locator")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Tools["systemctl"].Path, "--system", "show", unit, "--no-pager")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
	cmd.WaitDelay = time.Second
	out := &limitedBuffer{limit: 256 << 10}
	discard := &limitedBuffer{limit: 4096}
	cmd.Stdout = out
	cmd.Stderr = discard
	if err := cmd.Run(); err != nil || out.exceeded || discard.exceeded {
		return nil, errors.New("fixed image service metadata is unavailable")
	}
	props := map[string]string{}
	for _, line := range strings.Split(out.String(), "\n") {
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, errors.New("invalid service metadata")
		}
		if _, dup := props[key]; dup {
			return nil, errors.New("duplicate service metadata")
		}
		props[key] = value
	}
	return props, nil
}

func readLoadedService(ctx context.Context, c ServiceConfig, unit string) (LoadedService, error) {
	p, err := serviceProperties(ctx, c, unit)
	if err != nil {
		return LoadedService{}, err
	}
	pid, err := strconv.Atoi(p["MainPID"])
	if err != nil {
		return LoadedService{}, errors.New("service PID metadata is missing")
	}
	argv, err := parseServiceExecStart(p["ExecStart"])
	if err != nil {
		return LoadedService{}, err
	}
	if p["Transient"] != "yes" && p["Transient"] != "no" {
		return LoadedService{}, errors.New("service origin metadata is missing")
	}
	return LoadedService{Unit: p["Id"], InvocationID: p["InvocationID"], ControlGroup: p["ControlGroup"], FragmentPath: p["FragmentPath"], MainPID: pid, DropInPaths: strings.Fields(p["DropInPaths"]), Transient: p["Transient"] == "yes", ExecStart: argv, Properties: p}, nil
}

func parseServiceExecStart(raw string) ([]string, error) {
	// Executable paths are restricted installation values without whitespace or
	// systemd interpolation. Refuse additional commands/argv instead of trying
	// to interpret arbitrary systemctl command syntax as a shell language.
	if strings.Count(raw, "argv[]=") != 1 || strings.Count(raw, "path=") != 1 {
		return nil, errors.New("unexpected service executable metadata")
	}
	_, tail, ok := strings.Cut(raw, "argv[]=")
	if !ok {
		return nil, errors.New("missing service arguments")
	}
	argvPart, _, ok := strings.Cut(tail, " ;")
	if !ok {
		return nil, errors.New("unterminated service arguments")
	}
	argv := strings.Fields(argvPart)
	if len(argv) != 2 || !fixedServicePath(argv[0]) || argv[1] != "--serve-image-verification-socket" {
		return nil, errors.New("service executable arguments changed")
	}
	_, tail, _ = strings.Cut(raw, "path=")
	path, _, ok := strings.Cut(tail, " ;")
	if !ok || path != argv[0] {
		return nil, errors.New("service executable path changed")
	}
	return argv, nil
}

func readLoadedSocket(ctx context.Context, c ServiceConfig) (LoadedSocket, error) {
	p, err := serviceProperties(ctx, c, "memql-imageverify.socket")
	if err != nil {
		return LoadedSocket{}, err
	}
	if p["Transient"] != "yes" && p["Transient"] != "no" {
		return LoadedSocket{}, errors.New("socket origin metadata is missing")
	}
	listen, ok := strings.CutSuffix(p["Listen"], " (Stream)")
	if !ok || listen != c.SocketPath {
		return LoadedSocket{}, errors.New("fixed socket listener changed")
	}
	return LoadedSocket{FragmentPath: p["FragmentPath"], DropInPaths: strings.Fields(p["DropInPaths"]), Transient: p["Transient"] == "yes", ListenStream: listen, Properties: p}, nil
}

func currentBootID() (string, error) {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if len(s) != 36 {
		return "", errors.New("kernel boot identity is unavailable")
	}
	return s, nil
}
