package imageverify

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func runtimeAliasFixture(t *testing.T) (ServiceConfig, serviceTestFiles) {
	t.Helper()
	c := serviceConfigFixture()
	c.RuntimeAliases = map[string]string{"/lib": "usr/lib", "/usr/lib/loader-alias.so": "libfixture.so.1"}
	fs := serviceFilesFixture(t, c)
	fs.put("/lib", []byte("usr/lib"), os.ModeSymlink|0777)
	fs.put("/usr/lib/loader-alias.so", []byte("libfixture.so.1"), os.ModeSymlink|0777)
	return c, fs
}
func TestRuntimeAliasesBindLoaderSelectionNotOnlyRetainedFileBytes(t *testing.T) {
	for _, kind := range []string{"valid", "retargeted SONAME", "retargeted directory", "foreign alias", "regular replacement", "undeclared ancestor", "alias cycle", "excess chain", "uncovered target", "missing map", "mutable alias", "canonical file shadow"} {
		t.Run(kind, func(t *testing.T) {
			c, fs := runtimeAliasFixture(t)
			switch kind {
			case "retargeted SONAME":
				fs.data["/usr/lib/loader-alias.so"] = []byte("other.so")
			case "retargeted directory":
				fs.data["/lib"] = []byte("elsewhere")
			case "foreign alias":
				i := fs.info["/lib"]
				i.uid = 1000
				fs.info["/lib"] = i
			case "regular replacement":
				fs.put("/lib", []byte("usr/lib"), 0644)
			case "undeclared ancestor":
				fs.put("/usr", []byte("elsewhere"), os.ModeSymlink|0777)
			case "alias cycle":
				c.RuntimeAliases["/usr/lib/loader-alias.so"] = "loader-alias.so"
			case "excess chain":
				c.RuntimeAliases = map[string]string{}
				for i := 0; i < 257; i++ {
					c.RuntimeAliases["/alias"+strings.Repeat("x", i+1)] = "/usr/lib/libfixture.so.1"
				}
			case "uncovered target":
				c.RuntimeAliases["/lib"] = "/elsewhere"
			case "missing map":
				c.RuntimeAliases = nil
			case "mutable alias":
				c.RuntimeAliases[c.AttemptRoot+"/loader"] = "/usr/lib/libfixture.so.1"
			case "canonical file shadow":
				c.RuntimeAliases["/usr/lib/libfixture.so.1"] = "/usr/lib/other.so"
			}
			if err := ValidateServiceConfig(c); err != nil {
				if kind == "alias cycle" || kind == "excess chain" || kind == "uncovered target" || kind == "missing map" || kind == "mutable alias" || kind == "canonical file shadow" {
					return
				}
				t.Fatal(err)
			}
			if err := inspectServiceInstallation(fs, c); (err == nil) != (kind == "valid") {
				t.Fatalf("accepted=%v kind=%s err=%v", err == nil, kind, err)
			}
		})
	}
}
func TestRuntimeAliasesResolveCanonicalParentsAndInvalidateProof(t *testing.T) {
	c, fs := runtimeAliasFixture(t)
	got, err := resolveServiceRuntimePath("/lib/loader-alias.so", c.RuntimeAliases)
	if err != nil || got != "/usr/lib/libfixture.so.1" {
		t.Fatalf("resolved=%s err=%v", got, err)
	}
	af := &availabilityFiles{serviceTestFiles: fs, changes: map[string]int64{}}
	for _, p := range []string{ServiceQualificationPath, "/etc/passwd", "/etc/subuid", "/etc/subgid"} {
		af.put(p, []byte("fixture"), 0644)
	}
	if _, err = serviceAvailabilityStamp(af, c); err != nil {
		t.Fatal(err)
	}
	af.data["/usr/lib/loader-alias.so"] = []byte("other.so")
	if _, err = serviceAvailabilityStamp(af, c); err == nil {
		t.Fatal("changed loader route reused cached proof")
	}
	if af.opens != 0 {
		t.Fatal("metadata advertisement hashed file contents")
	}
	host, q, now := qualificationFixture(c)
	raw, _ := json.Marshal(q)
	c.RuntimeAliases["/lib"] = "/usr/lib/"
	if verifyServiceQualification(c, host, raw, now) == nil {
		t.Fatal("changed alias declaration reused retained proof")
	}
}
