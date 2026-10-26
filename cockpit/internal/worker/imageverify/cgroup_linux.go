//go:build linux

package imageverify

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const cgroupRoot = "/sys/fs/cgroup"

func kernelCgroupPath(group string) (string, error) {
	if !strings.HasPrefix(group, "/system.slice/") || filepath.Clean(group) != group || strings.ContainsAny(group, "\x00\r\n") || !strings.Contains(group, "memql-imageverify@") {
		return "", errors.New("unexpected image service cgroup")
	}
	return filepath.Join(cgroupRoot, group), nil
}

func readCgroupLimits(group string) (map[string]string, error) {
	p, err := kernelCgroupPath(group)
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	for _, key := range []string{"cpu.max", "memory.max", "memory.swap.max", "pids.max"} {
		b, e := os.ReadFile(filepath.Join(p, key))
		if e != nil {
			return nil, e
		}
		values[key] = string(b)
	}
	return values, nil
}

// Delegate grants the service its subtree, not systemd's parent resource
// settings. Move into a temporary leaf before enabling controllers, then move
// the entire helper into the bounded build leaf before any source execution.
func admitChildGroup(parent string, limits Limits) (string, error) {
	root, err := kernelCgroupPath(parent)
	if err != nil {
		return "", err
	}
	current, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	actual, err := parseUnifiedCgroup(string(current))
	if err != nil || actual != parent {
		return "", errors.New("helper is not in the expected service group")
	}
	for _, leaf := range []string{"supervisor", "build"} {
		if err = os.Mkdir(filepath.Join(root, leaf), 0755); err != nil {
			return "", err
		}
	}
	pid := []byte(strconv.Itoa(os.Getpid()))
	if err = os.WriteFile(filepath.Join(root, "supervisor", "cgroup.procs"), pid, 0600); err != nil {
		return "", err
	}
	processes, err := os.ReadFile(filepath.Join(root, "cgroup.procs"))
	if err != nil || strings.TrimSpace(string(processes)) != "" {
		return "", errors.New("service parent contains unexpected processes")
	}
	if err = os.WriteFile(filepath.Join(root, "cgroup.subtree_control"), []byte("+cpu +memory +pids"), 0600); err != nil {
		return "", err
	}
	child := filepath.Join(parent, "build")
	for key, value := range map[string]string{"cpu.max": fmt.Sprintf("%d 100000", limits.CPUMilli*100), "memory.max": strconv.FormatInt(int64(limits.MemoryMiB)<<20, 10), "memory.swap.max": "0", "pids.max": "512"} {
		if err = os.WriteFile(filepath.Join(root, "build", key), []byte(value), 0600); err != nil {
			return "", err
		}
	}
	values, err := readCgroupLimits(child)
	if err != nil {
		return "", err
	}
	if err = checkCgroupLimits(values, limits); err != nil {
		return "", err
	}
	if err = os.WriteFile(filepath.Join(root, "build", "cgroup.procs"), pid, 0600); err != nil {
		return "", err
	}
	current, err = os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	actual, err = parseUnifiedCgroup(string(current))
	if err != nil || actual != child {
		return "", errors.New("helper did not enter its bounded child group")
	}
	return child, nil
}

// An absent, previously observed exact cgroup is kernel evidence that it no
// longer contains a process. A loaded/unloaded unit state alone is insufficient.
func confirmGroupEmpty(group string) error {
	p, err := kernelCgroupPath(group)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(p, "cgroup.events"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !strings.Contains(string(data), "populated 0\n") {
		return errors.New("image service still has processes")
	}
	return filepath.WalkDir(p, func(path string, entry fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if !entry.IsDir() {
			return nil
		}
		data, e := os.ReadFile(filepath.Join(path, "cgroup.procs"))
		if e != nil || strings.TrimSpace(string(data)) != "" {
			return errors.New("image service descendant still has processes")
		}
		return nil
	})
}
