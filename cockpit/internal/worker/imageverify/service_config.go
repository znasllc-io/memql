package imageverify

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const ServiceConfigPath = "/etc/memql/image-verification.json"
const ServiceUnitPath = "/etc/systemd/system/memql-imageverify@.service"
const ServiceSocketPath = "/etc/systemd/system/memql-imageverify.socket"
const serviceConfigMaxBytes = 64 << 10
const serviceExecutableMaxBytes = 512 << 20
const servicePrivateTmp = "/tmp:rw,size=16M,mode=1777"

// ServiceConfig is operator-owned installation data, never pipeline input. The
// worker retains its original hardening; only this separately installed, fixed
// non-root service may use the subordinate-ID helpers. No manager mutation or
// arbitrary executable is part of the worker's protocol.
type ServiceConfig struct {
	OwnerUID          int                          `json:"ownerUID"`
	OwnerGID          int                          `json:"ownerGID"`
	Helper            ServiceExecutable            `json:"helper"`
	Tools             map[string]ServiceExecutable `json:"tools"`
	RuntimeFiles      map[string]string            `json:"runtimeFiles"`
	RuntimeAliases    map[string]string            `json:"runtimeAliases"`
	AttemptRoot       string                       `json:"attemptRoot"`
	WorkspaceRoot     string                       `json:"workspaceRoot"`
	SocketPath        string                       `json:"socketPath"`
	MaxCPUMilli       int                          `json:"maxCPUMilli"`
	MaxMemoryMiB      int                          `json:"maxMemoryMiB"`
	MaxRuntimeSeconds int                          `json:"maxRuntimeSeconds"`
	MaxStorageBytes   int64                        `json:"maxStorageBytes"`
}
type ServiceExecutable struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

var serviceToolNames = []string{"rootlesskit", "buildkitd", "buildctl", "runc", "slirp4netns", "nft", "newuidmap", "newgidmap", "git", "systemctl", "ip"}
var serviceHash = regexp.MustCompile(`^[0-9a-f]{64}$`)
var serviceNonce = regexp.MustCompile(`^[0-9a-f]{32}$`)
var serviceInstance = regexp.MustCompile(`^memql-imageverify@[A-Za-z0-9_.:\\-]{1,180}\.service$`)
var servicePath = regexp.MustCompile(`^/[A-Za-z0-9_./-]+$`)

func fixedServicePath(p string) bool {
	return len(p) <= 1024 && p != "/" && servicePath.MatchString(p) && filepath.Clean(p) == p
}
func serviceWithin(p, root string) bool { return p == root || strings.HasPrefix(p, root+"/") }
func ValidateServiceConfig(c ServiceConfig) error {
	if c.OwnerUID <= 0 || c.OwnerGID <= 0 || int64(c.OwnerUID) > 2147483647 || int64(c.OwnerGID) > 2147483647 ||
		c.MaxCPUMilli < 1 || c.MaxCPUMilli > 256000 || c.MaxMemoryMiB < 512 || c.MaxMemoryMiB > 32768 || c.MaxRuntimeSeconds < 60 || c.MaxRuntimeSeconds > 21600 || c.MaxStorageBytes < 1<<30 || c.MaxStorageBytes > 15<<30 {
		return errors.New("invalid fixed image service owner or resource limits")
	}
	if !fixedServicePath(c.AttemptRoot) || !fixedServicePath(c.WorkspaceRoot) || serviceWithin(c.AttemptRoot, c.WorkspaceRoot) || serviceWithin(c.WorkspaceRoot, c.AttemptRoot) || c.SocketPath != fmt.Sprintf("/run/memql-imageverify/%d.sock", c.OwnerUID) {
		return errors.New("invalid or overlapping image service paths")
	}
	if len(c.Tools) != len(serviceToolNames) {
		return errors.New("image service requires its complete fixed tool set")
	}
	executables := []ServiceExecutable{c.Helper}
	for _, name := range serviceToolNames {
		tool, ok := c.Tools[name]
		if !ok {
			return fmt.Errorf("missing fixed image service tool %s", name)
		}
		executables = append(executables, tool)
	}
	for _, e := range executables {
		if !fixedServicePath(e.Path) || !serviceHash.MatchString(e.SHA256) || serviceWithin(e.Path, c.AttemptRoot) || serviceWithin(e.Path, c.WorkspaceRoot) || strings.HasPrefix(e.Path, "/home/") || strings.HasPrefix(e.Path, "/run/") || strings.HasPrefix(e.Path, "/tmp/") || strings.HasPrefix(e.Path, "/var/tmp/") {
			return errors.New("image service executable must have an immutable external path and digest")
		}
	}
	// The operator resolves the exact ELF loader/library closure before setup.
	// Pin those regular targets and the loader cache; executable hashes alone
	// would not notice a library upgrade. No runtime dependency resolver runs.
	if len(c.RuntimeFiles) < 2 || len(c.RuntimeFiles) > 128 || !serviceHash.MatchString(c.RuntimeFiles["/etc/ld.so.cache"]) {
		return errors.New("image service requires its bounded immutable loader and library identities")
	}
	for p, digest := range c.RuntimeFiles {
		if !fixedServicePath(p) || !serviceHash.MatchString(digest) || serviceWithin(p, c.AttemptRoot) || serviceWithin(p, c.WorkspaceRoot) || strings.HasPrefix(p, "/home/") || strings.HasPrefix(p, "/run/") || strings.HasPrefix(p, "/tmp/") || strings.HasPrefix(p, "/var/tmp/") {
			return errors.New("image service runtime dependency must have an immutable external path and digest")
		}
	}
	return validateServiceRuntimeAliases(c)
}

// RenderServiceUnits fixes the entire unit text. Configuration supplies only
// validated installation identities and ceilings; no request controls systemd
// properties, ExecStart, environment or mounts. The small private /tmp is
// RootlessKit mount scratch; build state and export stay under AttemptRoot.
func RenderServiceUnits(c ServiceConfig) (socket, service []byte, err error) {
	if err = ValidateServiceConfig(c); err != nil {
		return nil, nil, err
	}
	socket = []byte(fmt.Sprintf("[Unit]\nDescription=MemQL image verification socket\n[Socket]\nListenStream=%s\nSocketUser=%d\nSocketGroup=%d\nSocketMode=0600\nDirectoryMode=0755\nAccept=yes\nMaxConnections=1\n[Install]\nWantedBy=sockets.target\n", c.SocketPath, c.OwnerUID, c.OwnerGID))
	service = []byte(fmt.Sprintf("[Unit]\nDescription=MemQL fixed image verification connection\n[Service]\nType=exec\nUser=%d\nGroup=%d\nExecStart=%s --serve-image-verification-socket\nWorkingDirectory=/\nStandardInput=socket\nStandardOutput=socket\nStandardError=null\nRestart=no\nKillMode=control-group\nSendSIGKILL=yes\nTimeoutStopSec=10s\nRuntimeMaxSec=%ds\nCPUQuota=%s%%\nMemoryMax=%d\nMemorySwapMax=0\nTasksMax=512\nDelegate=yes\nUMask=0077\nLimitCORE=0\nProtectSystem=strict\nProtectHome=read-only\nPrivateUsers=no\nNoNewPrivileges=no\nTemporaryFileSystem=/tmp:rw,size=16M,mode=1777\nReadWritePaths=%s\n", c.OwnerUID, c.OwnerGID, c.Helper.Path, c.MaxRuntimeSeconds, strconv.FormatFloat(float64(c.MaxCPUMilli)/10, 'f', 1, 64), int64(c.MaxMemoryMiB)<<20, c.AttemptRoot))
	return socket, service, nil
}

type serviceFiles interface {
	Lstat(string) (os.FileInfo, error)
	Open(string) (io.ReadCloser, error)
}
type installedServiceFiles struct{}

func (installedServiceFiles) Lstat(p string) (os.FileInfo, error)  { return os.Lstat(p) }
func (installedServiceFiles) Open(p string) (io.ReadCloser, error) { return os.Open(p) }

// Unix ownership is checked without importing OS-specific syscall types into
// this portable contract. Unsupported stat representations fail closed.
func serviceFileUID(info os.FileInfo) (uint64, bool) {
	if info == nil {
		return 0, false
	}
	v := reflect.ValueOf(info.Sys())
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return 0, false
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return 0, false
	}
	uid := v.FieldByName("Uid")
	if !uid.IsValid() || !uid.CanUint() {
		return 0, false
	}
	return uid.Uint(), true
}
func serviceRootOwned(info os.FileInfo) bool {
	uid, ok := serviceFileUID(info)
	return ok && uid == 0 && info.Mode().Perm()&0022 == 0
}

func immutableServiceFile(fs serviceFiles, p string, executable bool) (os.FileInfo, error) {
	if !fixedServicePath(p) && p != ServiceUnitPath {
		return nil, errors.New("invalid immutable service path")
	}
	info, err := fs.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || !serviceRootOwned(info) || (executable && info.Mode().Perm()&0111 == 0) {
		return nil, errors.New("image service file is not immutable root-owned regular content")
	}
	for parent := filepath.Dir(p); ; parent = filepath.Dir(parent) {
		stat, err := fs.Lstat(parent)
		if err != nil {
			return nil, err
		}
		if !stat.IsDir() || !serviceRootOwned(stat) {
			return nil, errors.New("image service file crosses a writable, foreign or symbolic ancestor")
		}
		if parent == "/" {
			break
		}
	}
	return info, nil
}
func readImmutableServiceFile(fs serviceFiles, p string, max int64) ([]byte, error) {
	info, err := immutableServiceFile(fs, p, false)
	if err != nil {
		return nil, err
	}
	if info.Size() < 1 || info.Size() > max {
		return nil, errors.New("image service configuration exceeds its bound")
	}
	f, err := fs.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) != info.Size() {
		return nil, errors.New("image service file changed during read")
	}
	return raw, nil
}
func decodeServiceConfig(raw []byte) (ServiceConfig, error) {
	var c ServiceConfig
	if len(raw) == 0 || len(raw) > serviceConfigMaxBytes {
		return c, errors.New("image service configuration exceeds its bound")
	}
	if err := decodeClosedServiceObject(raw, &c); err != nil {
		return c, err
	}
	return c, ValidateServiceConfig(c)
}
func LoadServiceConfig() (ServiceConfig, error) { return loadServiceConfig(installedServiceFiles{}) }
func loadServiceConfig(fs serviceFiles) (ServiceConfig, error) {
	raw, err := readImmutableServiceFile(fs, ServiceConfigPath, serviceConfigMaxBytes)
	if err != nil {
		return ServiceConfig{}, err
	}
	return decodeServiceConfig(raw)
}

// ServiceToolPath is the sole sanitized PATH shared with the runtime. RootlessKit
// may resolve only its fixed implicit helpers through these directories.
func ServiceToolPath(c ServiceConfig) string {
	dirs := map[string]bool{}
	for _, tool := range c.Tools {
		dirs[filepath.Dir(tool.Path)] = true
	}
	ordered := make([]string, 0, len(dirs))
	for dir := range dirs {
		ordered = append(ordered, dir)
	}
	sort.Strings(ordered)
	return strings.Join(ordered, ":")
}
func inspectServiceImplicitTools(fs serviceFiles, c ServiceConfig) error {
	for _, name := range []string{"newuidmap", "newgidmap", "ip"} {
		found := false
		for _, dir := range strings.Split(ServiceToolPath(c), ":") {
			candidate := filepath.Join(dir, name)
			info, err := fs.Lstat(candidate)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return errors.New("implicit image helper crosses a symbolic alias")
			}
			if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
				continue
			}
			if candidate != c.Tools[name].Path {
				return errors.New("implicit image helper is shadowed on the fixed PATH")
			}
			if _, err = immutableServiceFile(fs, candidate, true); err != nil {
				return err
			}
			found = true
			break
		}
		if !found {
			return errors.New("implicit image helper is absent from the fixed PATH")
		}
	}
	return nil
}
func InspectServiceInstallation(c ServiceConfig) error {
	return inspectServiceInstallation(installedServiceFiles{}, c)
}
func inspectServiceInstallation(fs serviceFiles, c ServiceConfig) error {
	if err := ValidateServiceConfig(c); err != nil {
		return err
	}
	installed, err := loadServiceConfig(fs)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(installed, c) {
		return errors.New("image service configuration differs from the installed authority")
	}
	if err := inspectServiceRuntimeAliases(fs, c); err != nil {
		return err
	}
	if err := inspectServicePreloadAbsent(fs); err != nil {
		return err
	}
	socket, service, err := RenderServiceUnits(c)
	if err != nil {
		return err
	}
	for path, want := range map[string][]byte{ServiceSocketPath: socket, ServiceUnitPath: service} {
		got, err := readImmutableServiceFile(fs, path, serviceConfigMaxBytes)
		if err != nil {
			return err
		}
		if !bytes.Equal(got, want) {
			return errors.New("image service unit content differs from its fixed definition")
		}
	}
	all := []ServiceExecutable{c.Helper}
	for _, name := range serviceToolNames {
		all = append(all, c.Tools[name])
	}
	for _, e := range all {
		if err := inspectServiceFileDigest(fs, e.Path, e.SHA256, true); err != nil {
			return err
		}
	}
	for p, digest := range c.RuntimeFiles {
		if err := inspectServiceFileDigest(fs, p, digest, false); err != nil {
			return err
		}
	}
	return inspectServiceImplicitTools(fs, c)
}

func inspectServicePreloadAbsent(fs serviceFiles) error {
	// A global preload can inject code independently of the declared ELF closure
	// and ld.so.cache. This optional builder does not accept such a host setup or
	// remove it; the operator must review it instead of silently borrowing proof.
	if _, err := fs.Lstat("/etc/ld.so.preload"); !os.IsNotExist(err) {
		return errors.New("global loader preload is present or cannot be inspected")
	}
	return nil
}
func inspectServiceFileDigest(fs serviceFiles, path, digest string, executable bool) error {
	info, err := immutableServiceFile(fs, path, executable)
	if err != nil {
		return err
	}
	if info.Size() < 1 || info.Size() > serviceExecutableMaxBytes {
		return errors.New("image service executable or runtime dependency exceeds its bound")
	}
	f, err := fs.Open(path)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, readErr := io.Copy(h, io.LimitReader(f, serviceExecutableMaxBytes+1))
	closeErr := f.Close()
	if readErr != nil || closeErr != nil || n != info.Size() || hex.EncodeToString(h.Sum(nil)) != digest {
		return errors.New("image service executable or runtime dependency identity changed")
	}
	return nil
}

// Hello is a locator, not authority. Its identity must agree with a fresh
// system-manager read of the exact accepted service instance and fixed files.
// Client-side peer credentials alone identify the socket manager, not the helper.
type HelloIdentity struct {
	Challenge, RequestSHA256, HelperSHA256, Unit, InvocationID, ControlGroup string
	MainPID, OwnerUID                                                        int
}
type ExpectedIdentity struct{ Challenge, RequestSHA256 string }
type LoadedService struct {
	Unit, InvocationID, ControlGroup, FragmentPath string
	MainPID                                        int
	DropInPaths                                    []string
	Transient                                      bool
	ExecStart                                      []string
	Properties                                     map[string]string
}

func VerifyServiceIdentity(c ServiceConfig, h HelloIdentity, expected ExpectedIdentity, loaded LoadedService) error {
	if err := InspectServiceInstallation(c); err != nil {
		return err
	}
	return verifyServiceIdentity(c, h, expected, loaded)
}
func verifyServiceIdentity(c ServiceConfig, h HelloIdentity, e ExpectedIdentity, l LoadedService) error {
	if err := ValidateServiceConfig(c); err != nil {
		return err
	}
	if !serviceNonce.MatchString(e.Challenge) || !serviceHash.MatchString(e.RequestSHA256) || h.Challenge != e.Challenge || h.RequestSHA256 != e.RequestSHA256 || h.HelperSHA256 != c.Helper.SHA256 || h.OwnerUID != c.OwnerUID || h.MainPID <= 0 || !serviceNonce.MatchString(h.InvocationID) || !serviceInstance.MatchString(h.Unit) || h.Unit != l.Unit || h.MainPID != l.MainPID || h.InvocationID != l.InvocationID || h.ControlGroup != l.ControlGroup || !validServiceCgroup(h.ControlGroup, h.Unit) {
		return errors.New("image service hello differs from its exact admitted invocation")
	}
	if l.Transient || l.FragmentPath != ServiceUnitPath || len(l.DropInPaths) != 0 || !slices.Equal(l.ExecStart, []string{c.Helper.Path, "--serve-image-verification-socket"}) {
		return errors.New("image service loaded definition changed")
	}
	required := map[string]string{"LoadState": "loaded", "ActiveState": "active", "SubState": "running", "Type": "exec", "User": strconv.Itoa(c.OwnerUID), "Group": strconv.Itoa(c.OwnerGID), "Restart": "no", "KillMode": "control-group", "SendSIGKILL": "yes", "Delegate": "yes", "UMask": "0077", "LimitCORE": "0", "ProtectSystem": "strict", "ProtectHome": "read-only", "PrivateUsers": "no", "NoNewPrivileges": "no", "ReadWritePaths": c.AttemptRoot, "WorkingDirectory": "/", "StandardInput": "socket", "StandardOutput": "socket", "StandardError": "null", "MemoryMax": strconv.FormatInt(int64(c.MaxMemoryMiB)<<20, 10), "MemorySwapMax": "0", "TasksMax": "512", "Environment": "", "EnvironmentFiles": "", "PassEnvironment": "", "RootDirectory": "", "RootImage": "", "BindPaths": "", "BindReadOnlyPaths": "", "TemporaryFileSystem": servicePrivateTmp, "ExecStartPre": "", "ExecStartPost": "", "ExecStop": "", "ExecStopPost": ""}
	for key, want := range required {
		got, ok := l.Properties[key]
		if !ok || got != want {
			return fmt.Errorf("image service loaded property %s changed", key)
		}
	}
	for key, want := range map[string]time.Duration{"TimeoutStopUSec": 10 * time.Second, "RuntimeMaxUSec": time.Duration(c.MaxRuntimeSeconds) * time.Second, "CPUQuotaPerSecUSec": time.Duration(c.MaxCPUMilli) * time.Millisecond} {
		got, ok := l.Properties[key]
		d, err := time.ParseDuration(strings.ReplaceAll(strings.ReplaceAll(got, "min", "m"), " ", ""))
		if !ok || err != nil || d != want {
			return fmt.Errorf("image service loaded duration %s changed", key)
		}
	}
	return nil
}
func validServiceCgroup(group, unit string) bool {
	return strings.HasPrefix(group, "/system.slice/") && filepath.Clean(group) == group && filepath.Base(group) == unit && !strings.ContainsAny(group, "\x00\r\n")
}

// LoadedSocket is a fresh read of the fixed system socket, not a caller-selected
// address. It must be checked before treating the service as an offered feature.
type LoadedSocket struct {
	FragmentPath string
	DropInPaths  []string
	Transient    bool
	ListenStream string
	Properties   map[string]string
}

func VerifyServiceSocket(c ServiceConfig, l LoadedSocket) error {
	if err := InspectServiceInstallation(c); err != nil {
		return err
	}
	return verifyServiceSocket(installedServiceFiles{}, c, l)
}
func verifyServiceSocket(fs serviceFiles, c ServiceConfig, l LoadedSocket) error {
	if err := ValidateServiceConfig(c); err != nil {
		return err
	}
	if l.Transient || l.FragmentPath != ServiceSocketPath || len(l.DropInPaths) != 0 || l.ListenStream != c.SocketPath {
		return errors.New("image service loaded socket changed")
	}
	for key, want := range map[string]string{"Id": "memql-imageverify.socket", "LoadState": "loaded", "ActiveState": "active", "SubState": "listening", "Accept": "yes", "MaxConnections": "1", "SocketUser": strconv.Itoa(c.OwnerUID), "SocketGroup": strconv.Itoa(c.OwnerGID), "SocketMode": "0600", "DirectoryMode": "0755", "Service": "memql-imageverify@.service"} {
		got, ok := l.Properties[key]
		if !ok || got != want {
			return fmt.Errorf("image service socket property %s changed", key)
		}
	}
	info, err := fs.Lstat(c.SocketPath)
	if err != nil {
		return err
	}
	uid, ok := serviceFileUID(info)
	if !ok || uid != uint64(c.OwnerUID) || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0600 {
		return errors.New("image service socket ownership or mode changed")
	}
	for parent := filepath.Dir(c.SocketPath); ; parent = filepath.Dir(parent) {
		info, err := fs.Lstat(parent)
		if err != nil {
			return err
		}
		if !info.IsDir() || !serviceRootOwned(info) {
			return errors.New("image service socket has a mutable or symbolic parent")
		}
		if parent == "/" {
			break
		}
	}
	return nil
}

// Installation configuration is small and closed. Refuse duplicate keys rather
// than letting JSON's last value silently select a different executable or owner.
func uniqueServiceConfigKeys(d *json.Decoder, depth int) error {
	if depth > 8 {
		return errors.New("image service configuration nesting exceeds its bound")
	}
	t, err := d.Token()
	if err != nil {
		return err
	}
	delimiter, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return errors.New("image service configuration has duplicate keys")
			}
			seen[name] = true
			if err := uniqueServiceConfigKeys(d, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := uniqueServiceConfigKeys(d, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("invalid image service configuration delimiter")
	}
	_, err = d.Token()
	return err
}

func decodeClosedServiceObject(raw []byte, target any) error {
	if err := uniqueServiceConfigKeys(json.NewDecoder(bytes.NewReader(raw)), 0); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("image service configuration contains trailing data")
	}
	return nil
}
