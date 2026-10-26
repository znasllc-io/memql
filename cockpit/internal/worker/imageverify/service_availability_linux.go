//go:build linux

package imageverify

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

type installedServiceHostReader struct{}

func (installedServiceHostReader) AppArmorProfileEntries() ([]string, error) {
	f, err := os.Open(serviceAppArmorProfiles)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	names, err := f.Readdirnames(serviceAppArmorProfileLimit + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(names) > serviceAppArmorProfileLimit {
		return nil, errors.New("AppArmor policy directory exceeds its bound")
	}
	return names, nil
}

func (installedServiceHostReader) Read(path string, max int64) ([]byte, error) {
	if strings.HasPrefix(path, "/etc/") {
		return readImmutableServiceFile(installedServiceFiles{}, path, max)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("host metadata is not regular kernel content")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > max {
		return nil, errors.New("host metadata exceeds its bound")
	}
	return raw, nil
}

type serviceVersionOutput struct {
	bytes.Buffer
	exceeded bool
}

func (b *serviceVersionOutput) Write(p []byte) (int, error) {
	n := len(p)
	if n > 4096-b.Len() {
		b.exceeded = true
		p = p[:max(0, 4096-b.Len())]
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}
func (installedServiceHostReader) ManagerVersion(ctx context.Context, c ServiceConfig) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// No unit argument means manager properties. Query the running manager's
	// Version, not the potentially newer systemctl executable's --version.
	cmd := exec.CommandContext(ctx, c.Tools["systemctl"].Path, "--system", "show", "--property=Version", "--value", "--no-pager")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
	cmd.WaitDelay = time.Second
	out := &serviceVersionOutput{}
	stderr := &serviceVersionOutput{}
	cmd.Stdout = out
	cmd.Stderr = stderr
	if cmd.Run() != nil || out.exceeded || stderr.exceeded {
		return "", errors.New("system manager version is unavailable")
	}
	version := strings.TrimSpace(out.String())
	if version == "" || len(version) > 256 || strings.ContainsAny(version, "\r\n\x00") {
		return "", errors.New("invalid system manager version")
	}
	return version, nil
}

// QualifiedService always rehashes the installed immutable code and reads the
// current attestation, manager, owner mappings and kernel policy. The advertised
// cache is deliberately not consulted at this pre-execution boundary.
func QualifiedService(ctx context.Context, c ServiceConfig) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ValidateServiceConfig(c); err != nil {
		return err
	}
	if runtime.GOARCH != "amd64" || c.OwnerUID != os.Geteuid() {
		return errors.New("image verification is unavailable for this account or architecture")
	}
	before, err := serviceAvailabilityStamp(installedServiceFiles{}, c)
	if err != nil {
		return err
	}
	if err = InspectServiceInstallation(c); err != nil {
		return err
	}
	return serviceAvailabilityObservation(ctx, c, before)
}
func QualifiedServiceAvailable(ctx context.Context) bool {
	if runtime.GOARCH != "amd64" {
		return false
	}
	c, err := LoadServiceConfig()
	if err != nil || c.OwnerUID != os.Geteuid() {
		return false
	}
	stamp, err := serviceAvailabilityStamp(installedServiceFiles{}, c)
	if err != nil {
		return false
	}
	// Only immutable content verification is cached. Manager/kernel/socket and
	// attestation checks below remain fresh, and no tool executes before its
	// installation identity has passed either this hash or an unchanged stamp.
	if !imageServiceAvailability.check(ctx, time.Now(), stamp, func() error { return InspectServiceInstallation(c) }) {
		return false
	}
	return serviceAvailabilityObservation(ctx, c, stamp) == nil
}

// Metadata and small host-policy reads remain fresh on cache hits. Only the
// expensive helper/tool content hashes are cached. Loaded socket drift, changed
// root-owned files, changed kernel/mappings or failed reads cannot borrow a prior
// green; admission performs the full check regardless of this hint.
func serviceAvailabilityObservation(ctx context.Context, c ServiceConfig, before string) error {
	fs := installedServiceFiles{}
	raw, err := readImmutableServiceFile(fs, ServiceQualificationPath, serviceConfigMaxBytes)
	if err != nil {
		return err
	}
	host, err := collectServiceHostFacts(ctx, c, installedServiceHostReader{})
	if err != nil {
		return err
	}
	if err = verifyServiceQualification(c, host, raw, time.Now()); err != nil {
		return err
	}
	socket, err := readLoadedSocket(ctx, c)
	if err != nil {
		return err
	}
	if err = verifyServiceSocket(fs, c, socket); err != nil {
		return err
	}
	after, err := serviceAvailabilityStamp(fs, c)
	if err != nil || after != before {
		return errors.New("image installation changed during qualification")
	}
	return ctx.Err()
}
