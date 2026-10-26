package imageverify

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"time"
	"unicode"
)

const ServiceQualificationPath = "/etc/memql/image-verification-qualified.json"

// Host facts come from the worker's bounded read-only collector, never from a
// pipeline request or the stored qualification itself. Fingerprints cover the
// complete selected UID/GID mappings, relevant namespace/LSM policy and delegated
// controller facts. An unavailable fact cannot borrow a previous observation.
type ServiceHostFacts struct {
	Architecture            string `json:"architecture"`
	KernelRelease           string `json:"kernelRelease"`
	SystemdVersion          string `json:"systemdVersion"`
	OwnerUID                int    `json:"ownerUID"`
	OwnerGID                int    `json:"ownerGID"`
	SubUIDSHA256            string `json:"subUIDSHA256"`
	SubGIDSHA256            string `json:"subGIDSHA256"`
	NamespacePolicySHA256   string `json:"namespacePolicySHA256"`
	CgroupControllersSHA256 string `json:"cgroupControllersSHA256"`
}
type ServiceQualificationFixture struct {
	Name           string `json:"name"`
	Result         string `json:"result"`
	EvidenceSHA256 string `json:"evidenceSHA256"`
}
type ServiceQualification struct {
	SchemaVersion      int                           `json:"schemaVersion"`
	Profile            string                        `json:"profile"`
	VerifiedAt         string                        `json:"verifiedAt"`
	InstallationSHA256 string                        `json:"installationSHA256"`
	Host               ServiceHostFacts              `json:"host"`
	Fixtures           []ServiceQualificationFixture `json:"fixtures"`
}

var serviceQualificationFixtures = []string{"target-export", "resource-limits", "namespace-isolation", "network-isolation", "cancel-and-timeout", "worker-restart-recovery", "ambiguous-start-and-cleanup", "mapped-state-accounting-cleanup"}

// ServiceInstallationDigest invalidates a qualification when fixed code, tools,
// limits, owner, paths or exact unit definitions change. It is not proof that
// those files are installed; InspectServiceInstallation establishes that first.
func ServiceInstallationDigest(c ServiceConfig) (string, error) {
	socket, service, err := RenderServiceUnits(c)
	if err != nil {
		return "", err
	}
	value := struct {
		SchemaVersion int           `json:"schemaVersion"`
		Capability    string        `json:"capability"`
		Config        ServiceConfig `json:"config"`
		Socket        string        `json:"socket"`
		Service       string        `json:"service"`
	}{1, "pipeline_stepv5/pipelineImageVerification:1", c, string(socket), string(service)}
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:]), nil
}

// VerifyServiceQualification consumes only the separately provisioned root-owned
// attestation. Presence of configuration or successful startup is not isolation
// proof. This performs bounded reads, never build probes or service mutations.
// The operator retains genuine fixture evidence named by its hashes; this file
// records that reviewed result, not arbitrary caller-set availability.
func VerifyServiceQualification(c ServiceConfig, current ServiceHostFacts, now time.Time) error {
	if err := InspectServiceInstallation(c); err != nil {
		return err
	}
	raw, err := readImmutableServiceFile(installedServiceFiles{}, ServiceQualificationPath, serviceConfigMaxBytes)
	if err != nil {
		return err
	}
	return verifyServiceQualification(c, current, raw, now)
}
func verifyServiceQualification(c ServiceConfig, current ServiceHostFacts, raw []byte, now time.Time) error {
	if err := ValidateServiceConfig(c); err != nil {
		return err
	}
	if len(raw) == 0 || len(raw) > serviceConfigMaxBytes || now.IsZero() {
		return errors.New("image service qualification is absent or unbounded")
	}
	var q ServiceQualification
	if err := decodeClosedServiceObject(raw, &q); err != nil {
		return err
	}
	digest, err := ServiceInstallationDigest(c)
	if err != nil {
		return err
	}
	validVersion := func(v string) bool {
		return len(v) > 0 && strings.TrimSpace(v) == v && len(v) <= 256 && strings.IndexFunc(v, unicode.IsControl) < 0
	}
	if current.Architecture != "linux/amd64" || !validVersion(current.KernelRelease) || !validVersion(current.SystemdVersion) || current.OwnerUID != c.OwnerUID || current.OwnerGID != c.OwnerGID {
		return errors.New("image service current host facts are missing or foreign")
	}
	for _, hash := range []string{current.SubUIDSHA256, current.SubGIDSHA256, current.NamespacePolicySHA256, current.CgroupControllersSHA256} {
		if !serviceHash.MatchString(hash) {
			return errors.New("image service host policy fingerprint is missing")
		}
	}
	if q.SchemaVersion != 1 || q.Profile != "rootless-buildkit-v1" || q.InstallationSHA256 != digest || !reflect.DeepEqual(q.Host, current) {
		return errors.New("image service qualification no longer matches this installation and host")
	}
	at, err := time.Parse(time.RFC3339, q.VerifiedAt)
	if err != nil || at.IsZero() || at.After(now.Add(5*time.Minute)) {
		return errors.New("image service qualification timestamp is invalid")
	}
	if len(q.Fixtures) != len(serviceQualificationFixtures) {
		return errors.New("image service qualification is incomplete")
	}
	expected := map[string]bool{}
	for _, name := range serviceQualificationFixtures {
		expected[name] = true
	}
	for _, fixture := range q.Fixtures {
		if !expected[fixture.Name] || fixture.Result != "passed" || !serviceHash.MatchString(fixture.EvidenceSHA256) {
			return errors.New("image service qualification has missing, repeated or failed real fixtures")
		}
		delete(expected, fixture.Name)
	}
	return nil
}
