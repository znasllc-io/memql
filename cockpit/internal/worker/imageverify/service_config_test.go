package imageverify

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type serviceTestInfo struct {
	name string
	size int64
	mode os.FileMode
	uid  uint32
}

func (i serviceTestInfo) Name() string       { return i.name }
func (i serviceTestInfo) Size() int64        { return i.size }
func (i serviceTestInfo) Mode() os.FileMode  { return i.mode }
func (i serviceTestInfo) ModTime() time.Time { return time.Time{} }
func (i serviceTestInfo) IsDir() bool        { return i.mode.IsDir() }
func (i serviceTestInfo) Sys() any           { return struct{ Uid uint32 }{i.uid} }

type serviceTestFiles struct {
	info map[string]serviceTestInfo
	data map[string][]byte
}

func (f serviceTestFiles) Lstat(p string) (os.FileInfo, error) {
	i, ok := f.info[p]
	if !ok {
		return nil, os.ErrNotExist
	}
	return i, nil
}
func (f serviceTestFiles) Open(p string) (io.ReadCloser, error) {
	b, ok := f.data[p]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
func (f serviceTestFiles) Readlink(p string) (string, error) {
	i, err := f.Lstat(p)
	if err != nil {
		return "", err
	}
	if i.Mode()&os.ModeSymlink == 0 {
		return "", os.ErrInvalid
	}
	return string(f.data[p]), nil
}
func (f serviceTestFiles) put(p string, b []byte, mode os.FileMode) {
	f.data[p] = b
	f.info[p] = serviceTestInfo{filepath.Base(p), int64(len(b)), mode, 0}
	for d := filepath.Dir(p); ; d = filepath.Dir(d) {
		f.info[d] = serviceTestInfo{filepath.Base(d), 0, 0755 | os.ModeDir, 0}
		if d == "/" {
			break
		}
	}
}
func serviceConfigFixture() ServiceConfig {
	c := ServiceConfig{OwnerUID: 1000, OwnerGID: 1000, AttemptRoot: "/var/lib/memql-imageverify/1000", WorkspaceRoot: "/home/fixtureuser/.memql/pipelines", SocketPath: "/run/memql-imageverify/1000.sock", MaxCPUMilli: 3000, MaxMemoryMiB: 8192, MaxRuntimeSeconds: 1800, MaxStorageBytes: 15 << 30, Tools: map[string]ServiceExecutable{}}
	digest := func(s string) string { x := sha256.Sum256([]byte(s)); return hex.EncodeToString(x[:]) }
	c.Helper = ServiceExecutable{"/usr/local/libexec/memql-imageverify", digest("helper")}
	for _, name := range serviceToolNames {
		c.Tools[name] = ServiceExecutable{"/usr/bin/" + name, digest(name)}
	}
	c.RuntimeAliases = map[string]string{}
	c.RuntimeFiles = map[string]string{"/etc/ld.so.cache": digest("/etc/ld.so.cache"), "/usr/lib/libfixture.so.1": digest("/usr/lib/libfixture.so.1")}
	return c
}
func serviceFilesFixture(t *testing.T, c ServiceConfig) serviceTestFiles {
	t.Helper()
	fs := serviceTestFiles{map[string]serviceTestInfo{}, map[string][]byte{}}
	data, _ := json.Marshal(c)
	fs.put(ServiceConfigPath, data, 0644)
	socket, service, err := RenderServiceUnits(c)
	if err != nil {
		t.Fatal(err)
	}
	fs.put(ServiceSocketPath, socket, 0644)
	fs.put(ServiceUnitPath, service, 0644)
	fs.put(c.Helper.Path, []byte("helper"), 0755)
	for name, e := range c.Tools {
		mode := os.FileMode(0755)
		if name == "newuidmap" || name == "newgidmap" {
			mode |= os.ModeSetuid
		}
		fs.put(e.Path, []byte(name), mode)
	}
	for path := range c.RuntimeFiles {
		fs.put(path, []byte(path), 0644)
	}
	return fs
}
func TestServiceInstallationChecksImmutableAncestorsAndExactBytes(t *testing.T) {
	for _, kind := range []string{"valid", "foreign helper", "writable ancestor", "symbolic ancestor", "writable tool", "changed executable", "changed unit", "changed config", "missing tool", "oversize executable", "short read"} {
		t.Run(kind, func(t *testing.T) {
			c := serviceConfigFixture()
			fs := serviceFilesFixture(t, c)
			switch kind {
			case "foreign helper":
				i := fs.info[c.Helper.Path]
				i.uid = 1000
				fs.info[c.Helper.Path] = i
			case "writable ancestor":
				i := fs.info["/usr/local"]
				i.mode |= 0020
				fs.info["/usr/local"] = i
			case "symbolic ancestor":
				i := fs.info["/usr/bin"]
				i.mode = os.ModeSymlink | 0755
				fs.info["/usr/bin"] = i
			case "writable tool":
				i := fs.info[c.Tools["newuidmap"].Path]
				i.mode |= 0002
				fs.info[c.Tools["newuidmap"].Path] = i
			case "changed executable":
				fs.put(c.Tools["buildkitd"].Path, []byte("changed"), 0755)
			case "changed unit":
				fs.put(ServiceUnitPath, append(fs.data[ServiceUnitPath], []byte("Environment=SECRET=bad\n")...), 0644)
			case "changed config":
				other := c
				other.MaxCPUMilli++
				b, _ := json.Marshal(other)
				fs.put(ServiceConfigPath, b, 0644)
			case "missing tool":
				delete(fs.info, c.Tools["git"].Path)
			case "oversize executable":
				i := fs.info[c.Helper.Path]
				i.size = serviceExecutableMaxBytes + 1
				fs.info[c.Helper.Path] = i
			case "short read":
				fs.data[c.Helper.Path] = []byte("x")
			}
			err := inspectServiceInstallation(fs, c)
			if (err == nil) != (kind == "valid") {
				t.Fatalf("%s: %v", kind, err)
			}
		})
	}
}
func TestServiceConfigRejectsUnboundedAuthorityAndInterpolation(t *testing.T) {
	for _, kind := range []string{"root", "root group", "unknown tool", "missing tool", "writable helper", "overlap", "unit interpolation", "command separator", "socket substitution", "runtime", "storage", "CPU", "memory", "bad digest"} {
		t.Run(kind, func(t *testing.T) {
			c := serviceConfigFixture()
			switch kind {
			case "root":
				c.OwnerUID = 0
			case "root group":
				c.OwnerGID = 0
			case "unknown tool":
				c.Tools["shell"] = c.Helper
			case "missing tool":
				delete(c.Tools, "nft")
			case "writable helper":
				c.Helper.Path = "/home/fixtureuser/.memql/bin/memql"
			case "overlap":
				c.AttemptRoot = c.WorkspaceRoot + "/attempt"
			case "unit interpolation":
				c.AttemptRoot = "/var/lib/%u"
			case "command separator":
				c.Helper.Path = "/usr/bin/helper --command"
			case "socket substitution":
				c.SocketPath = "/run/user/1000/bus"
			case "runtime":
				c.MaxRuntimeSeconds = 21601
			case "storage":
				c.MaxStorageBytes = 16 << 30
			case "CPU":
				c.MaxCPUMilli = 0
			case "memory":
				c.MaxMemoryMiB = 0
			case "bad digest":
				c.Helper.SHA256 = "sha256:" + c.Helper.SHA256
			}
			if ValidateServiceConfig(c) == nil {
				t.Fatal("invalid installation accepted")
			}
		})
	}
	c := serviceConfigFixture()
	raw, _ := json.Marshal(c)
	for _, raw := range [][]byte{append(append([]byte{}, raw...), []byte(` {}`)...), []byte(strings.Repeat(" ", serviceConfigMaxBytes+1)), append([]byte(`{"unexpected":true,`), raw[1:]...), append([]byte(`{"ownerUID":1001,`), raw[1:]...)} {
		if _, err := decodeServiceConfig(raw); err == nil {
			t.Fatal("unbounded or unknown config accepted")
		}
	}
}
func TestRenderedImageServiceCannotSelectManagerCommands(t *testing.T) {
	c := serviceConfigFixture()
	socket, unit, err := RenderServiceUnits(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"ExecStart=" + c.Helper.Path + " --serve-image-verification-socket\n", "ProtectSystem=strict\n", "ProtectHome=read-only\n", "PrivateUsers=no\n", "NoNewPrivileges=no\n", "KillMode=control-group\n", "RuntimeMaxSec=1800s\n", "TemporaryFileSystem=/tmp:rw,size=16M,mode=1777\n", "ReadWritePaths=" + c.AttemptRoot + "\n"} {
		if !strings.Contains(string(unit), required) {
			t.Fatal("fixed boundary absent", required)
		}
	}
	if strings.Count(string(unit), "ExecStart=") != 1 || strings.Contains(string(unit), "systemd-run") || strings.Contains(string(unit), "%i") || strings.Contains(string(unit), "polkit") || !strings.Contains(string(socket), "Accept=yes\nMaxConnections=1\n") {
		t.Fatal("unit widened request authority")
	}
}
func serviceIdentityFixture(c ServiceConfig) (HelloIdentity, ExpectedIdentity, LoadedService) {
	h := HelloIdentity{Challenge: strings.Repeat("a", 32), RequestSHA256: strings.Repeat("b", 64), HelperSHA256: c.Helper.SHA256, Unit: "memql-imageverify@0-1234-1000.service", MainPID: 3210, InvocationID: strings.Repeat("c", 32), ControlGroup: "/system.slice/system-memql-imageverify.slice/memql-imageverify@0-1234-1000.service", OwnerUID: c.OwnerUID}
	p := map[string]string{"LoadState": "loaded", "ActiveState": "active", "SubState": "running", "Type": "exec", "User": "1000", "Group": "1000", "Restart": "no", "KillMode": "control-group", "SendSIGKILL": "yes", "Delegate": "yes", "UMask": "0077", "LimitCORE": "0", "ProtectSystem": "strict", "ProtectHome": "read-only", "PrivateUsers": "no", "NoNewPrivileges": "no", "ReadWritePaths": c.AttemptRoot, "WorkingDirectory": "/", "StandardInput": "socket", "StandardOutput": "socket", "StandardError": "null", "MemoryMax": "8589934592", "MemorySwapMax": "0", "TasksMax": "512", "Environment": "", "EnvironmentFiles": "", "PassEnvironment": "", "RootDirectory": "", "RootImage": "", "BindPaths": "", "BindReadOnlyPaths": "", "TemporaryFileSystem": "/tmp:rw,size=16M,mode=1777", "ExecStartPre": "", "ExecStartPost": "", "ExecStop": "", "ExecStopPost": "", "TimeoutStopUSec": "10s", "RuntimeMaxUSec": "30min", "CPUQuotaPerSecUSec": "3s"}
	return h, ExpectedIdentity{h.Challenge, h.RequestSHA256}, LoadedService{Unit: h.Unit, MainPID: h.MainPID, InvocationID: h.InvocationID, ControlGroup: h.ControlGroup, FragmentPath: ServiceUnitPath, ExecStart: []string{c.Helper.Path, "--serve-image-verification-socket"}, Properties: p}
}
func TestServiceHelloRequiresFreshExactLoadedInvocation(t *testing.T) {
	for _, kind := range []string{"valid", "challenge", "request", "helper", "owner", "PID", "invocation", "cgroup", "escape", "unit", "dropin", "fragment", "transient", "argv", "weaker memory", "missing property", "extra mount", "manager environment", "timeout", "CPU", "unbounded tmp", "missing private tmp"} {
		t.Run(kind, func(t *testing.T) {
			c := serviceConfigFixture()
			h, e, l := serviceIdentityFixture(c)
			switch kind {
			case "challenge":
				h.Challenge = strings.Repeat("d", 32)
			case "request":
				h.RequestSHA256 = strings.Repeat("e", 64)
			case "helper":
				h.HelperSHA256 = strings.Repeat("f", 64)
			case "owner":
				h.OwnerUID++
			case "PID":
				l.MainPID++
			case "invocation":
				l.InvocationID = strings.Repeat("f", 32)
			case "cgroup":
				l.ControlGroup += "/child"
			case "escape":
				h.ControlGroup = "/system.slice/../user.slice/" + h.Unit
				l.ControlGroup = h.ControlGroup
			case "unit":
				h.Unit = "ssh.service"
				l.Unit = h.Unit
			case "dropin":
				l.DropInPaths = []string{"/run/systemd/system/memql-imageverify@.service.d/change.conf"}
			case "fragment":
				l.FragmentPath = "/run/systemd/transient/" + h.Unit
			case "transient":
				l.Transient = true
			case "argv":
				l.ExecStart = append(l.ExecStart, "--exec=shell")
			case "weaker memory":
				l.Properties["MemoryMax"] = "infinity"
			case "missing property":
				delete(l.Properties, "NoNewPrivileges")
			case "extra mount":
				l.Properties["BindPaths"] = "/home"
			case "manager environment":
				l.Properties["PassEnvironment"] = "TOKEN"
			case "timeout":
				l.Properties["RuntimeMaxUSec"] = "infinity"
			case "CPU":
				l.Properties["CPUQuotaPerSecUSec"] = "4s"
			case "unbounded tmp":
				l.Properties["TemporaryFileSystem"] = "/tmp:rw"
			case "missing private tmp":
				l.Properties["TemporaryFileSystem"] = ""
			}
			err := verifyServiceIdentity(c, h, e, l)
			if (err == nil) != (kind == "valid") {
				t.Fatalf("%s: %v", kind, err)
			}
		})
	}
}

func TestImageServiceSocketCannotBeReplacedOrWidened(t *testing.T) {
	for _, kind := range []string{"valid", "listen", "mode", "owner", "parent", "symlink", "dropin", "accept", "concurrency", "template", "inactive"} {
		t.Run(kind, func(t *testing.T) {
			c := serviceConfigFixture()
			fs := serviceFilesFixture(t, c)
			fs.put(c.SocketPath, []byte{}, os.ModeSocket|0600)
			i := fs.info[c.SocketPath]
			i.uid = 1000
			fs.info[c.SocketPath] = i
			l := LoadedSocket{FragmentPath: ServiceSocketPath, ListenStream: c.SocketPath, Properties: map[string]string{"Id": "memql-imageverify.socket", "LoadState": "loaded", "ActiveState": "active", "SubState": "listening", "Accept": "yes", "MaxConnections": "1", "SocketUser": "1000", "SocketGroup": "1000", "SocketMode": "0600", "DirectoryMode": "0755", "Service": "memql-imageverify@.service"}}
			switch kind {
			case "listen":
				l.ListenStream = "/run/other.sock"
			case "mode":
				i.mode = os.ModeSocket | 0666
				fs.info[c.SocketPath] = i
			case "owner":
				i.uid = 1001
				fs.info[c.SocketPath] = i
			case "parent":
				i = fs.info[filepath.Dir(c.SocketPath)]
				i.uid = 1000
				fs.info[filepath.Dir(c.SocketPath)] = i
			case "symlink":
				i.mode = os.ModeSymlink | 0600
				fs.info[c.SocketPath] = i
			case "dropin":
				l.DropInPaths = []string{"/run/systemd/system/memql-imageverify.socket.d/override.conf"}
			case "accept":
				l.Properties["Accept"] = "no"
			case "concurrency":
				l.Properties["MaxConnections"] = "2"
			case "template":
				l.Properties["Service"] = "other@.service"
			case "inactive":
				l.Properties["ActiveState"] = "inactive"
			}
			err := verifyServiceSocket(fs, c, l)
			if (err == nil) != (kind == "valid") {
				t.Fatalf("%s: %v", kind, err)
			}
		})
	}
}

func TestImageServiceImplicitHelpersCannotBeShadowedByOtherToolDirectories(t *testing.T) {
	for _, kind := range []string{"valid", "earlier executable", "symbolic shadow", "missing basename", "nonexecutable before pinned"} {
		t.Run(kind, func(t *testing.T) {
			c := serviceConfigFixture()
			tool := c.Tools["buildkitd"]
			tool.Path = "/opt/memql-tools/buildkitd"
			c.Tools["buildkitd"] = tool
			fs := serviceFilesFixture(t, c)
			switch kind {
			case "earlier executable":
				fs.put("/opt/memql-tools/newuidmap", []byte("different"), 0755)
			case "symbolic shadow":
				fs.put("/opt/memql-tools/ip", []byte("alias"), os.ModeSymlink|0755)
			case "missing basename":
				e := c.Tools["ip"]
				delete(fs.info, e.Path)
			case "nonexecutable before pinned":
				fs.put("/opt/memql-tools/ip", []byte("not executable"), 0644)
			}
			err := inspectServiceImplicitTools(fs, c)
			valid := kind == "valid" || kind == "nonexecutable before pinned"
			if (err == nil) != valid {
				t.Fatalf("%s: %v", kind, err)
			}
			if ServiceToolPath(c) != "/opt/memql-tools:/usr/bin" {
				t.Fatal("runtime PATH order differs")
			}
		})
	}
}
