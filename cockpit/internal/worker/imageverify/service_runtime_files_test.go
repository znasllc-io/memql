package imageverify

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestServiceRuntimeDependenciesRejectUnknownOrChangedCode(t *testing.T) {
	for _, kind := range []string{"valid", "missing closure", "missing cache", "too many files", "mutable path", "invalid digest", "changed library", "changed cache", "missing library", "symbolic library", "foreign library", "writable ancestor", "short library read", "oversize library", "preload file", "preload alias"} {
		t.Run(kind, func(t *testing.T) {
			c := serviceConfigFixture()
			const lib = "/usr/lib/libfixture.so.1"
			// Invalid operator configuration must refuse before file inspection.
			switch kind {
			case "missing closure":
				c.RuntimeFiles = nil
			case "missing cache":
				delete(c.RuntimeFiles, "/etc/ld.so.cache")
			case "too many files":
				for i := 0; i < 128; i++ {
					c.RuntimeFiles["/usr/lib/extra"+strconv.Itoa(i)] = strings.Repeat("a", 64)
				}
			case "mutable path":
				c.RuntimeFiles[c.AttemptRoot+"/library.so"] = strings.Repeat("a", 64)
			case "invalid digest":
				c.RuntimeFiles[lib] = "unverified"
			}
			if err := ValidateServiceConfig(c); err != nil {
				if kind == "missing closure" || kind == "missing cache" || kind == "too many files" || kind == "mutable path" || kind == "invalid digest" {
					return
				}
				t.Fatal(err)
			}
			fs := serviceFilesFixture(t, c)
			switch kind {
			case "changed library":
				fs.put(lib, []byte("changed"), 0644)
			case "changed cache":
				fs.put("/etc/ld.so.cache", []byte("changed"), 0644)
			case "missing library":
				delete(fs.info, lib)
			case "symbolic library":
				i := fs.info[lib]
				i.mode = os.ModeSymlink | 0777
				fs.info[lib] = i
			case "foreign library":
				i := fs.info[lib]
				i.uid = 1000
				fs.info[lib] = i
			case "writable ancestor":
				i := fs.info["/usr/lib"]
				i.mode |= 0020
				fs.info["/usr/lib"] = i
			case "short library read":
				fs.data[lib] = []byte("x")
			case "oversize library":
				i := fs.info[lib]
				i.size = serviceExecutableMaxBytes + 1
				fs.info[lib] = i
			case "preload file":
				fs.put("/etc/ld.so.preload", []byte("/usr/lib/libfixture.so.1"), 0644)
			case "preload alias":
				fs.put("/etc/ld.so.preload", nil, os.ModeSymlink|0777)
			}
			if err := inspectServiceInstallation(fs, c); (err == nil) != (kind == "valid") {
				t.Fatalf("accepted=%v kind=%s err=%v", err == nil, kind, err)
			}
		})
	}
}

func TestServiceRuntimeDependencyDriftInvalidatesCachedAndRetainedProof(t *testing.T) {
	c := serviceConfigFixture()
	fs := &availabilityFiles{serviceTestFiles: serviceFilesFixture(t, c), changes: map[string]int64{}}
	for _, p := range []string{ServiceQualificationPath, "/etc/passwd", "/etc/subuid", "/etc/subgid"} {
		fs.put(p, []byte("fixture"), 0644)
	}
	before, err := serviceAvailabilityStamp(fs, c)
	if err != nil {
		t.Fatal(err)
	}
	const lib = "/usr/lib/libfixture.so.1"
	fs.changes[lib] = 1
	after, err := serviceAvailabilityStamp(fs, c)
	if err != nil || after == before {
		t.Fatal("library identity drift retained cached proof", err)
	}
	if fs.opens != 0 {
		t.Fatal("advertisement identity check hashed library contents")
	}
	beforeDigest, err := ServiceInstallationDigest(c)
	if err != nil {
		t.Fatal(err)
	}
	host, attestation, now := qualificationFixture(c)
	raw, _ := json.Marshal(attestation)
	c.RuntimeFiles[lib] = strings.Repeat("b", 64)
	afterDigest, err := ServiceInstallationDigest(c)
	if err != nil || beforeDigest == afterDigest {
		t.Fatal("new library digest retained old attestation identity", err)
	}
	if verifyServiceQualification(c, host, raw, now) == nil {
		t.Fatal("retained attestation accepted changed dependency")
	}
	fs.put("/etc/ld.so.preload", []byte("added"), 0644)
	if _, err = serviceAvailabilityStamp(fs, c); err == nil {
		t.Fatal("new preload reused cached proof")
	}
}
