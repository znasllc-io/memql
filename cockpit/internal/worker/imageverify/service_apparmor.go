package imageverify

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
)

const serviceAppArmorProfiles = "/sys/kernel/security/apparmor/policy/profiles"
const serviceAppArmorProfileLimit = 4096

type serviceAppArmorReader interface {
	AppArmorProfileEntries() ([]string, error)
}

var serviceAppArmorEntry = regexp.MustCompile(`^(rootlesskit|usr\.bin\.rootlesskit)\.[0-9]+$`)

// Sysctls do not identify the loaded executable policy. On AppArmor hosts,
// require the existing distro RootlessKit profile's actual attachment and
// kernel hash, plus immutable source bytes. Never read the privileged global
// profiles listing, install a profile, or treat unreadable policy as disabled.
// The per-profile kernel files are read-only; actual access and execution in
// the fixed service still have to pass the owner's real Linux qualification.
func collectServiceAppArmorFacts(c ServiceConfig, r serviceHostReader, policy map[string]string) error {
	enabled := policy["/sys/module/apparmor/parameters/enabled"]
	restricted := policy["/proc/sys/kernel/apparmor_restrict_unprivileged_userns"]
	if restricted != "absent" && restricted != "0" && restricted != "1" {
		return errors.New("invalid AppArmor namespace restriction")
	}
	switch enabled {
	case "N", "0", "absent":
		if restricted == "1" || strings.Contains(policy["/sys/kernel/security/lsm"], "apparmor") {
			return errors.New("inconsistent AppArmor enabled state")
		}
		return nil
	case "Y", "1":
	default:
		return errors.New("invalid AppArmor enabled state")
	}
	if c.Tools["rootlesskit"].Path != "/usr/bin/rootlesskit" {
		return errors.New("AppArmor qualification requires the existing distro RootlessKit path")
	}
	reader, ok := r.(serviceAppArmorReader)
	if !ok {
		return errors.New("loaded RootlessKit policy is unavailable")
	}
	entries, err := reader.AppArmorProfileEntries()
	if err != nil || len(entries) > serviceAppArmorProfileLimit {
		return errors.New("loaded RootlessKit policy directory is unavailable or exceeds its bound")
	}
	selected := ""
	for _, name := range entries {
		if serviceAppArmorEntry.MatchString(name) {
			if selected != "" {
				return errors.New("ambiguous loaded RootlessKit policy")
			}
			selected = name
		}
	}
	if selected == "" {
		return errors.New("RootlessKit executable policy is not loaded")
	}
	read := func(path string, limit int64) (string, error) {
		raw, e := r.Read(path, limit)
		if e != nil || len(raw) > int(limit) {
			return "", errors.New("RootlessKit policy evidence is unavailable or exceeds its bound")
		}
		return strings.TrimSpace(string(raw)), nil
	}
	base := filepath.Join(serviceAppArmorProfiles, selected)
	values := map[string]string{}
	for _, field := range []string{"name", "mode", "attach", "sha256"} {
		values[field], err = read(filepath.Join(base, field), 4096)
		if err != nil {
			return err
		}
	}
	if values["name"] != "rootlesskit" && values["name"] != "/usr/bin/rootlesskit" {
		return errors.New("unexpected RootlessKit policy name")
	}
	if values["attach"] != c.Tools["rootlesskit"].Path || (values["mode"] != "enforce" && values["mode"] != "unconfined") {
		return errors.New("unexpected RootlessKit policy attachment or mode")
	}
	digest, err := hex.DecodeString(values["sha256"])
	if err != nil || len(digest) != sha256.Size || values["sha256"] != strings.ToLower(values["sha256"]) {
		return errors.New("loaded RootlessKit policy hash is unavailable")
	}
	sourcePath := "/etc/apparmor.d/rootlesskit"
	if values["name"] == "/usr/bin/rootlesskit" {
		sourcePath = "/etc/apparmor.d/usr.bin.rootlesskit"
	}
	// The normal host reader validates root ownership and every immutable
	// ancestor for /etc paths. The kernel hash covers the loaded include closure;
	// hashing the source alone would not prove that those bytes were loaded.
	raw, err := r.Read(sourcePath, serviceHostFileMaxBytes)
	if err != nil || len(raw) == 0 || len(raw) > serviceHostFileMaxBytes {
		return errors.New("immutable RootlessKit policy source is unavailable")
	}
	sourceDigest := sha256.Sum256(raw)
	for field, value := range values {
		policy["apparmor.rootlesskit."+field] = value
	}
	policy["apparmor.rootlesskit.sourcePath"] = sourcePath
	policy["apparmor.rootlesskit.sourceSHA256"] = hex.EncodeToString(sourceDigest[:])
	return nil
}
