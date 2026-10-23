//go:build linux

package hardware

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/znasllc-io/memql/cockpit/internal/worker/models"
)

// platformProbe answers the Linux facts.
//
// /proc and /etc rather than lscpu, free and lsb_release: three
// subprocesses to read three files this process can open, on the
// Register path of a systemd user unit.
func platformProbe() Probe {
	return Probe{
		GOOS:        "linux",
		GOARCH:      runtime.GOARCH,
		Chip:        linuxChip,
		MemoryBytes: linuxMemoryBytes,
		OSVersion:   linuxOSVersion,
		CPUCores:    runtime.NumCPU,
		GPUs:        models.DetectGPUs,
		Backend:     models.GPUBackend,
		DiskFree:    modelVolumeFree,
		Runtimes:    detectRuntimes,
		Now:         now,
	}
}

// linuxChip reads the first "model name" in /proc/cpuinfo, which is the
// vendor's own marketing string on both x86 vendors.
//
// On arm64 there is usually no "model name" at all -- the kernel prints
// "CPU implementer" and a hex id instead -- so an arm64 Linux machine
// reports an absent chip rather than a hex number nobody recognises.
// That machine is already below the floor (Linux inference is x86_64
// only), so nothing downstream depends on the answer.
func linuxChip() (string, error) {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return "", err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		k, v, ok := strings.Cut(s.Text(), ":")
		if !ok {
			continue
		}
		if strings.TrimSpace(k) == "model name" {
			return strings.TrimSpace(v), nil
		}
	}
	return "", os.ErrNotExist
}

// linuxMemoryBytes reads MemTotal, which /proc/meminfo states in kB.
func linuxMemoryBytes() (uint64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		k, v, ok := strings.Cut(s.Text(), ":")
		if !ok || strings.TrimSpace(k) != "MemTotal" {
			continue
		}
		fields := strings.Fields(v)
		if len(fields) == 0 {
			return 0, os.ErrInvalid
		}
		kb, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return 0, err
		}
		return kb * 1024, nil
	}
	return 0, os.ErrNotExist
}

// linuxOSVersion prefers PRETTY_NAME from /etc/os-release, which is the
// distribution as its own maintainers write it ("Ubuntu 24.04.1 LTS").
// The kernel version is deliberately NOT used: the question this field
// answers is whether a runtime will install here, and that is a
// distribution question.
func linuxOSVersion() (string, error) {
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return "", err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		k, v, ok := strings.Cut(s.Text(), "=")
		if !ok || k != "PRETTY_NAME" {
			continue
		}
		return strings.Trim(strings.TrimSpace(v), `"`), nil
	}
	return "", os.ErrNotExist
}
