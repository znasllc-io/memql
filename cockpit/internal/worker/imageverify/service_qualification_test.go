package imageverify

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func qualificationFixture(c ServiceConfig) (ServiceHostFacts, ServiceQualification, time.Time) {
	now := time.Date(2026, 10, 26, 20, 0, 0, 0, time.UTC)
	host := ServiceHostFacts{"linux/amd64", "6.8.0-fixture", "255-fixture", c.OwnerUID, c.OwnerGID, strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64), strings.Repeat("4", 64)}
	digest, _ := ServiceInstallationDigest(c)
	q := ServiceQualification{SchemaVersion: 1, Profile: "rootless-buildkit-v1", VerifiedAt: now.Add(-time.Hour).Format(time.RFC3339), InstallationSHA256: digest, Host: host}
	for _, name := range serviceQualificationFixtures {
		q.Fixtures = append(q.Fixtures, ServiceQualificationFixture{name, "passed", strings.Repeat("5", 64)})
	}
	return host, q, now
}
func TestImageServiceQualificationRequiresExactRealFixtureAttestation(t *testing.T) {
	for _, kind := range []string{"valid", "no result", "wrong profile", "wrong schema", "future", "invalid time", "missing fixture", "repeated fixture", "extra fixture", "failed fixture", "missing evidence", "host kernel", "host systemd", "blank version", "host mappings", "host policy", "host controllers", "host owner", "missing current facts", "changed helper", "changed tool", "changed ceiling", "changed path"} {
		t.Run(kind, func(t *testing.T) {
			c := serviceConfigFixture()
			host, q, now := qualificationFixture(c)
			switch kind {
			case "no result":
				q.Fixtures = nil
			case "wrong profile":
				q.Profile = "docker"
			case "wrong schema":
				q.SchemaVersion = 2
			case "future":
				q.VerifiedAt = now.Add(time.Hour).Format(time.RFC3339)
			case "invalid time":
				q.VerifiedAt = "yesterday"
			case "missing fixture":
				q.Fixtures = q.Fixtures[1:]
			case "repeated fixture":
				q.Fixtures[1] = q.Fixtures[0]
			case "extra fixture":
				q.Fixtures = append(q.Fixtures, ServiceQualificationFixture{"enabled", "passed", strings.Repeat("5", 64)})
			case "failed fixture":
				q.Fixtures[0].Result = "failed"
			case "missing evidence":
				q.Fixtures[0].EvidenceSHA256 = ""
			case "host kernel":
				host.KernelRelease = "changed"
			case "host systemd":
				host.SystemdVersion = "changed"
			case "blank version":
				host.SystemdVersion = " "
			case "host mappings":
				host.SubUIDSHA256 = strings.Repeat("a", 64)
			case "host policy":
				host.NamespacePolicySHA256 = strings.Repeat("b", 64)
			case "host controllers":
				host.CgroupControllersSHA256 = strings.Repeat("c", 64)
			case "host owner":
				host.OwnerUID++
			case "missing current facts":
				host.SubGIDSHA256 = ""
			case "changed helper":
				c.Helper.SHA256 = strings.Repeat("e", 64)
			case "changed tool":
				tool := c.Tools["runc"]
				tool.SHA256 = strings.Repeat("f", 64)
				c.Tools["runc"] = tool
			case "changed ceiling":
				c.MaxCPUMilli++
			case "changed path":
				c.AttemptRoot += "-new"
			}
			raw, _ := json.Marshal(q)
			err := verifyServiceQualification(c, host, raw, now)
			if (err == nil) != (kind == "valid") {
				t.Fatalf("%s: %v", kind, err)
			}
		})
	}
	c := serviceConfigFixture()
	host, q, now := qualificationFixture(c)
	raw, _ := json.Marshal(q)
	for _, bad := range [][]byte{nil, append(append([]byte{}, raw...), []byte(` {}`)...), append([]byte(`{"enabled":true,`), raw[1:]...), []byte(strings.Repeat("x", serviceConfigMaxBytes+1))} {
		if verifyServiceQualification(c, host, bad, now) == nil {
			t.Fatal("unbounded or caller-enabled attestation accepted")
		}
	}
}
