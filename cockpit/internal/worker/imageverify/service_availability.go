package imageverify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Advertising is a hint, never an execution grant. Refresh the expensive hashes
// at most every five minutes while immutable identities remain unchanged; every
// admitted attempt independently performs QualifiedService's fresh full check.
const serviceAvailabilityTTL = 5 * time.Minute
const serviceHostFileMaxBytes = 1 << 20

type serviceAvailabilityCache struct {
	sync.Mutex
	stamp     string
	until     time.Time
	available bool
}

var imageServiceAvailability serviceAvailabilityCache

func (s *serviceAvailabilityCache) check(ctx context.Context, now time.Time, stamp string, qualify func() error) bool {
	s.Lock()
	defer s.Unlock()
	if ctx.Err() != nil || stamp == "" {
		return false
	}
	if s.stamp == stamp && now.Before(s.until) {
		return s.available
	}
	s.available = qualify() == nil && ctx.Err() == nil
	s.stamp = stamp
	s.until = now.Add(serviceAvailabilityTTL)
	return s.available
}

// Identity checks read metadata, not executable contents. ctime, inode/device,
// owner/mode and mtime/size detect replacement and same-size rewrites. Ancestors
// are checked again, so replacing a directory or introducing an alias cannot
// reuse the cached attestation. An unknown stat representation refuses reuse.
func serviceAvailabilityStamp(fs serviceFiles, c ServiceConfig) (string, error) {
	if err := inspectServiceRuntimeAliases(fs, c); err != nil {
		return "", err
	}
	if err := inspectServicePreloadAbsent(fs); err != nil {
		return "", err
	}
	paths := []string{ServiceConfigPath, ServiceUnitPath, ServiceSocketPath, ServiceQualificationPath, c.Helper.Path}
	for _, name := range serviceToolNames {
		paths = append(paths, c.Tools[name].Path)
	}
	for path := range c.RuntimeFiles {
		paths = append(paths, path)
	}
	paths = append(paths, "/etc/passwd", "/etc/subuid", "/etc/subgid")
	identities := map[string]string{}
	for _, p := range paths {
		if _, err := immutableServiceFile(fs, p, false); err != nil {
			return "", err
		}
		for current := p; ; current = filepath.Dir(current) {
			if _, ok := identities[current]; !ok {
				info, err := fs.Lstat(current)
				if err != nil {
					return "", err
				}
				identity, err := serviceStatIdentity(info)
				if err != nil {
					return "", err
				}
				identities[current] = identity
			}
			if current == "/" {
				break
			}
		}
	}
	// Link text is compared above on every cache hit; no file hashing or ELF
	// resolver is needed to detect a retargeted loader/SONAME alias.
	for path, target := range c.RuntimeAliases {
		identities["alias:"+path] = target
	}
	return serviceDigest(identities)
}
func serviceStatIdentity(info os.FileInfo) (string, error) {
	if info == nil {
		return "", errors.New("missing immutable file identity")
	}
	v := reflect.ValueOf(info.Sys())
	if v.Kind() == reflect.Pointer && !v.IsNil() {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return "", errors.New("unsupported immutable file identity")
	}
	fields := map[string]string{"mode": strconv.FormatUint(uint64(info.Mode()), 10), "size": strconv.FormatInt(info.Size(), 10), "mtime": strconv.FormatInt(info.ModTime().UnixNano(), 10)}
	for _, name := range []string{"Dev", "Ino", "Uid", "Gid"} {
		f := v.FieldByName(name)
		if !f.IsValid() || !f.CanInterface() {
			return "", errors.New("incomplete immutable file identity")
		}
		fields[name] = fmt.Sprint(f.Interface())
	}
	ctime := v.FieldByName("Ctim")
	if !ctime.IsValid() {
		ctime = v.FieldByName("Ctimespec")
	}
	if !ctime.IsValid() || !ctime.CanInterface() {
		return "", errors.New("missing immutable change time")
	}
	fields["ctime"] = fmt.Sprint(ctime.Interface())
	return serviceDigest(fields)
}
func serviceDigest(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:]), nil
}

type serviceHostReader interface {
	Read(string, int64) ([]byte, error)
	ManagerVersion(context.Context, ServiceConfig) (string, error)
}

var serviceNamespaceFiles = []string{
	"/proc/sys/user/max_user_namespaces",
	"/proc/sys/kernel/unprivileged_userns_clone",
	"/proc/sys/kernel/apparmor_restrict_unprivileged_userns",
	"/sys/module/apparmor/parameters/enabled",
	"/sys/kernel/security/lsm",
	"/sys/fs/selinux/enforce",
}

// Only read fixed host metadata. No model, build, service activation or
// namespace probe runs here. Optional kernel-specific policy files are recorded
// as absent only on ENOENT, never on permission or transport errors. Qualification
// and the runtime's actual isolation checks remain independent requirements.
func collectServiceHostFacts(ctx context.Context, c ServiceConfig, r serviceHostReader) (ServiceHostFacts, error) {
	var h ServiceHostFacts
	if err := ctx.Err(); err != nil {
		return h, err
	}
	read := func(path string) (string, error) {
		b, e := r.Read(path, serviceHostFileMaxBytes)
		if e != nil {
			return "", e
		}
		if len(b) > serviceHostFileMaxBytes {
			return "", errors.New("host fact exceeds its bound")
		}
		return strings.TrimSpace(string(b)), nil
	}
	kernel, err := read("/proc/sys/kernel/osrelease")
	if err != nil || kernel == "" {
		return h, errors.New("kernel release is unavailable")
	}
	passwd, err := read("/etc/passwd")
	if err != nil {
		return h, err
	}
	name, err := serviceOwnerName(passwd, c.OwnerUID, c.OwnerGID)
	if err != nil {
		return h, err
	}
	uidRaw, err := read("/etc/subuid")
	if err != nil {
		return h, err
	}
	uidHash, err := serviceSubIDFingerprint(uidRaw, name, c.OwnerUID, c.OwnerUID)
	if err != nil {
		return h, err
	}
	gidRaw, err := read("/etc/subgid")
	if err != nil {
		return h, err
	}
	gidHash, err := serviceSubIDFingerprint(gidRaw, name, c.OwnerUID, c.OwnerGID)
	if err != nil {
		return h, err
	}
	policy := map[string]string{}
	for _, path := range serviceNamespaceFiles {
		value, e := read(path)
		if e != nil {
			if path != "/proc/sys/user/max_user_namespaces" && os.IsNotExist(e) {
				policy[path] = "absent"
				continue
			}
			return h, e
		}
		if value == "" {
			return h, errors.New("empty namespace policy observation")
		}
		policy[path] = value
	}
	maxNS, err := strconv.ParseUint(policy["/proc/sys/user/max_user_namespaces"], 10, 64)
	if err != nil || maxNS == 0 {
		return h, errors.New("user namespaces are unavailable")
	}
	if value := policy["/proc/sys/kernel/unprivileged_userns_clone"]; value != "absent" && value != "1" {
		return h, errors.New("unprivileged user namespaces are unavailable")
	}
	if err := collectServiceAppArmorFacts(c, r, policy); err != nil {
		return h, err
	}
	controllers := map[string]string{}
	for _, path := range []string{"/sys/fs/cgroup/cgroup.controllers", "/sys/fs/cgroup/system.slice/cgroup.controllers"} {
		raw, e := read(path)
		if e != nil {
			return h, e
		}
		fields := strings.Fields(raw)
		seen := map[string]bool{}
		for _, field := range fields {
			if seen[field] {
				return h, errors.New("duplicate delegated controller")
			}
			seen[field] = true
		}
		for _, required := range []string{"cpu", "memory", "pids"} {
			if !seen[required] {
				return h, errors.New("required aggregate controller is unavailable")
			}
		}
		sort.Strings(fields)
		controllers[path] = strings.Join(fields, " ")
	}
	version, err := r.ManagerVersion(ctx, c)
	if err != nil {
		return h, err
	}
	policyHash, _ := serviceDigest(policy)
	controllerHash, _ := serviceDigest(controllers)
	h = ServiceHostFacts{"linux/amd64", kernel, version, c.OwnerUID, c.OwnerGID, uidHash, gidHash, policyHash, controllerHash}
	if err = ctx.Err(); err != nil {
		return ServiceHostFacts{}, err
	}
	return h, nil
}
func serviceOwnerName(raw string, uid, gid int) (string, error) {
	name := ""
	for _, line := range strings.Split(raw, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, ":")
		if len(f) != 7 {
			return "", errors.New("malformed local account metadata")
		}
		id, e := strconv.Atoi(f[2])
		if e != nil {
			return "", errors.New("malformed local account identity")
		}
		if id != uid {
			continue
		}
		group, e := strconv.Atoi(f[3])
		if e != nil || group != gid || name != "" || f[0] == "" {
			return "", errors.New("ambiguous fixed service account")
		}
		name = f[0]
	}
	if name == "" {
		return "", errors.New("fixed service account is absent")
	}
	return name, nil
}
func serviceSubIDFingerprint(raw, name string, owner, hostID int) (string, error) {
	type mapping struct{ Start, Count uint64 }
	var ranges []mapping
	for _, line := range strings.Split(raw, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, ":")
		if len(f) != 3 {
			return "", errors.New("malformed subordinate ID metadata")
		}
		if f[0] != name && f[0] != strconv.Itoa(owner) {
			continue
		}
		start, e := strconv.ParseUint(f[1], 10, 32)
		if e != nil || start == 0 {
			return "", errors.New("invalid subordinate ID range")
		}
		count, e := strconv.ParseUint(f[2], 10, 32)
		if e != nil || count == 0 || start+count > 1<<32 {
			return "", errors.New("invalid subordinate ID range")
		}
		ranges = append(ranges, mapping{start, count})
	}
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].Start < ranges[j].Start })
	var total uint64
	for i, r := range ranges {
		if i > 0 && ranges[i-1].Start+ranges[i-1].Count > r.Start {
			return "", errors.New("overlapping subordinate ID ranges")
		}
		if uint64(hostID) >= r.Start && uint64(hostID) < r.Start+r.Count {
			return "", errors.New("subordinate range overlaps the service owner")
		}
		total += r.Count
	}
	if total < 65536 {
		return "", errors.New("insufficient subordinate IDs")
	}
	return serviceDigest(ranges)
}
