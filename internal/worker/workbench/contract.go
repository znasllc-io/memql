// Package workbench runs isolated, resource-bounded workspaces on fleet machines.
// It never executes a request on the host or mounts a host directory.
package workbench

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"strings"
	"time"
)

const (
	Capability = "WORKBENCH"
	Contract   = 1
	// Multi-platform official Python image, pinned including its filesystem.
	Image        = "docker.io/library/python@sha256:2d9aefe2fef018a7eb2c13064c89c71929800fd2e5dccdbf52ea5da5bb8d929a"
	Slots        = 2
	Queue        = 32
	MemoryMiB    = 1536
	WorkspaceMiB = 256
	MaxOutput    = 1 << 20
	MaxInput     = 16 << 20
	IdleTTL      = 5 * time.Minute
	Lifetime     = time.Hour
)

// Identity is supplied by the authenticated enrollment, never command args.
// Each borrower/run/enrollment gets a separate container and filesystem.
type Identity struct {
	Home, Owner, Caller, Registration, Machine, Run string
	ExpiresAt                                       time.Time
}

func (id Identity) Key() (string, error) {
	parts := []string{id.Home, id.Owner, id.Caller, id.Registration, id.Machine, id.Run}
	for _, p := range parts {
		if strings.TrimSpace(p) == "" || len(p) > 512 || strings.ContainsRune(p, 0) {
			return "", fmt.Errorf("workbench authority is incomplete")
		}
	}
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:]), nil
}

type Request struct {
	Action string         `json:"action"`
	Args   map[string]any `json:"args"`
}
type Result struct {
	OK           bool   `json:"ok"`
	Action       string `json:"action"`
	Payload      any    `json:"payload,omitempty"`
	ErrorCode    string `json:"errorCode,omitempty"`
	ErrorMessage string `json:"errorMessage,omitempty"`
}

func Refused(action, code, message string) Result {
	return Result{Action: action, ErrorCode: code, ErrorMessage: message}
}

// Paths are interpreted only inside /workspace. Symlinks are resolved again by
// the in-container program; a traversal must never address another workspace.
func Validate(req Request) error {
	switch req.Action {
	case "exec", "fs_read", "fs_write", "fs_list", "fs_stat":
	default:
		return fmt.Errorf("unsupported fleet workbench action %q", req.Action)
	}
	for _, key := range []string{"path", "cwd"} {
		raw, exists := req.Args[key]
		if !exists {
			continue
		}
		s, ok := raw.(string)
		if !ok || strings.ContainsRune(s, 0) || path.IsAbs(s) || path.Clean(s) == ".." || strings.HasPrefix(path.Clean(s), "../") {
			return fmt.Errorf("%s must stay inside the workbench", key)
		}
	}
	return nil
}
