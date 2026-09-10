package worker

import (
	"strings"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// LabelMachineId is the cockpit-persisted stable id for one physical install.
// It rides registration.labels so a re-pair with a new worker token can reclaim
// the existing registration instead of minting a duplicate row for the same Mac.
//
// It is NOT a display name and NOT a hostname: those collide across machines.
// The cockpit mints it once under ~/.memql/machine-id and sends it on every
// Register. Absence means an older cockpit; reclaim then falls back to the
// weaker host|os|arch key only when that triple is complete.
const LabelMachineId = "machineId"

// MachineKey identifies one physical machine for registration reclaim.
// Empty means "no stable key", so the handshake must not invent a match.
func MachineKeyFromLabelsAndPlatform(labels map[string]string, platform map[string]any) string {
	if id := strings.TrimSpace(labels[LabelMachineId]); id != "" {
		return "id:" + id
	}
	host := strings.ToLower(strings.TrimSpace(platformString(platform, "hostname")))
	osName := strings.TrimSpace(platformString(platform, "os"))
	arch := strings.TrimSpace(platformString(platform, "arch"))
	if host == "" || osName == "" || arch == "" {
		return ""
	}
	return "host:" + host + "|" + osName + "|" + arch
}

// MachineKeyFromRegister is the Register-message form of MachineKeyFromLabelsAndPlatform.
func MachineKeyFromRegister(register *memqlv1.Register) string {
	if register == nil {
		return ""
	}
	platform := map[string]any{}
	if p := register.GetPlatform(); p != nil {
		platform["hostname"] = p.GetHostname()
		platform["os"] = p.GetOs()
		platform["arch"] = p.GetArch()
	}
	return MachineKeyFromLabelsAndPlatform(register.GetLabels(), platform)
}

// MachineKeyFromRow is the persisted-row form used when scanning WorkersForUser.
func MachineKeyFromRow(row RegistrationRow) string {
	return MachineKeyFromLabelsAndPlatform(row.Labels, row.Platform)
}

func platformString(platform map[string]any, key string) string {
	if platform == nil {
		return ""
	}
	v, ok := platform[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}
