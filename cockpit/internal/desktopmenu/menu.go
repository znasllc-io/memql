// Package desktopmenu is the Linux desktop companion. It controls the existing
// worker through its CLI/socket; it never loads enrollment tokens or runs a worker.
package desktopmenu

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"time"
)

// Rendered from native/macos/mark.svg with the shared MemQLAppIcon renderer.
//
//go:embed icon.png
var icon []byte

type Home struct {
	ID      string `json:"id"`
	Server  string `json:"server"`
	OSURL   string `json:"os_url"`
	Enabled bool   `json:"enabled"`
	State   string `json:"state"`
}
type Status struct {
	Version          string `json:"version"`
	Homes            []Home `json:"homes"`
	PermissionDetail string `json:"permission_detail"`
}
type Reply struct {
	OK     bool    `json:"ok"`
	Error  string  `json:"error"`
	Status *Status `json:"status"`
}

func request(executable string, args ...string) (Reply, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, executable, append([]string{"worker", "control"}, args...)...).Output()
	var reply Reply
	if json.Unmarshal(output, &reply) != nil {
		return reply, errors.New("Background worker unavailable. Check the MemQL worker service.")
	}
	if !reply.OK {
		if reply.Error == "" {
			reply.Error = "Worker request failed"
		}
		return reply, errors.New(reply.Error)
	}
	return reply, err
}

func safeOSURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("The worker did not report a secure MemQL OS address")
	}
	return u.String(), nil
}

func summary(status *Status) string {
	if status == nil {
		return "Background worker unavailable"
	}
	connected := 0
	for _, h := range status.Homes {
		if h.State == "Connected" {
			connected++
		}
	}
	if len(status.Homes) == 0 {
		return "No servers configured"
	}
	return fmt.Sprintf("%d of %d servers connected", connected, len(status.Homes))
}
