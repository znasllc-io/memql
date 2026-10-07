package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	checks := map[string]bool{}
	_, _, e := syscall.RawSyscall(syscall.SYS_PTRACE, 0, 0, 0)
	checks["ptrace_denied"] = e == syscall.EPERM
	_, _, e = syscall.RawSyscall6(syscall.SYS_PROCESS_VM_READV, uintptr(os.Getpid()), 0, 0, 0, 0, 0)
	checks["process_vm_read_denied"] = e == syscall.EPERM
	_, _, e = syscall.RawSyscall6(syscall.SYS_PROCESS_VM_WRITEV, uintptr(os.Getpid()), 0, 0, 0, 0, 0)
	checks["process_vm_write_denied"] = e == syscall.EPERM
	f, err := os.OpenFile("/proc/sys/kernel/hostname", os.O_WRONLY, 0)
	checks["kernel_settings_not_writable"] = err != nil
	if f != nil {
		f.Close()
	}
	for key, path := range map[string]string{"no_docker_socket": "/var/run/docker.sock", "no_cluster_token": "/var/run/secrets/kubernetes.io/serviceaccount/token"} {
		_, err := os.Stat(path)
		checks[key] = os.IsNotExist(err)
	}
	raw, err := os.ReadFile("/proc/self/uid_map")
	uidMap := strings.Fields(string(raw))
	checks["root_maps_to_unprivileged_user"] = err == nil && len(uidMap) >= 3 && uidMap[0] == "0" && uidMap[1] == "1000"
	raw, err = os.ReadFile("/proc/self/status")
	checks["seccomp_filter_active"] = err == nil && strings.Contains(string(raw), "Seccomp:\t2")
	checks["no_other_container_process"] = true
	files, _ := filepath.Glob("/proc/[0-9]*/cmdline")
	for _, p := range files {
		b, _ := os.ReadFile(p)
		if strings.Contains(string(b), "memql-outer-sentinel-only") {
			checks["no_other_container_process"] = false
		}
	}
	for key, addr := range map[string]string{"private_listener_denied": os.Args[1], "metadata_denied": "169.254.169.254:80", "azure_wireserver_denied": "168.63.129.16:80"} {
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		checks[key] = err != nil
		if c != nil {
			c.Close()
		}
	}
	c, err := net.DialTimeout("tcp", "github.com:443", 10*time.Second)
	checks["public_https_reachable"] = err == nil
	if c != nil {
		c.Close()
	}
	json.NewEncoder(os.Stdout).Encode(checks)
	for key, passed := range checks {
		if !passed {
			fmt.Fprintln(os.Stderr, "failed:", key)
			os.Exit(1)
		}
	}
}
