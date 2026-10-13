// Package computerpermission defines the bounded, versioned permission shared by
// Nexus, the dispatching replica and Cockpit. It contains no authority source.
package computerpermission

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"strings"
)

const Contract = "workerHost.computer_permission"
const Version = 1

// Scope is produced by the server, never copied from a model's approval subject.
type Scope struct {
	Version        int      `json:"version"`
	Kind           string   `json:"kind"`
	RegistrationID string   `json:"registrationId"`
	MachineID      string   `json:"machineId"`
	MachineLabel   string   `json:"machineLabel"`
	Tool           string   `json:"tool"`
	Action         string   `json:"action"`
	Directory      string   `json:"directory,omitempty"`
	Command        string   `json:"command,omitempty"`
	CWD            string   `json:"cwd,omitempty"`
	ArgsHash       string   `json:"argsHash"`
	AllowedModes   []string `json:"allowedModes"`
}

type Request struct {
	OwnerID, AgentID, RunID, Tool, Action string
	Args                                  map[string]any
}

type Grant struct {
	ID    string
	Scope Scope
}

// Authority re-reads durable receipts. Claim is called only by the replica
// holding the selected stream, immediately before one dispatch.
type Authority interface {
	Find(context.Context, Request) (*Grant, error)
	Claim(context.Context, string, Request, string, string) (*Scope, error)
}

func Hash(tool, action string, args map[string]any) (string, error) {
	b, err := json.Marshal(struct {
		Tool   string         `json:"tool"`
		Action string         `json:"action"`
		Args   map[string]any `json:"args"`
	}{tool, action, args})
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

func Parse(v any) (Scope, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return Scope{}, err
	}
	var s Scope
	if err := json.Unmarshal(b, &s); err != nil {
		return Scope{}, err
	}
	return s, s.Validate()
}

func (s Scope) Map() map[string]any {
	b, _ := json.Marshal(s)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

// CleanAbsolute restricts this contract to explicit POSIX paths. Cockpit also
// proves their real filesystem identity and confines reads with os.OpenRoot.
func CleanAbsolute(p string) bool {
	return strings.HasPrefix(p, "/") && p != "/" && path.Clean(p) == p && !strings.ContainsAny(p, "\x00\\")
}
func Within(root, p string) bool {
	return CleanAbsolute(root) && CleanAbsolute(p) && (p == root || strings.HasPrefix(p, root+"/"))
}
func (s Scope) Validate() error {
	if s.Version != Version || s.RegistrationID == "" || s.MachineID == "" || s.MachineLabel == "" || s.Tool != "workerHost" || len(s.ArgsHash) != 64 {
		return fmt.Errorf("invalid computer permission identity or version")
	}
	if _, err := hex.DecodeString(s.ArgsHash); err != nil {
		return fmt.Errorf("invalid computer permission arguments")
	}
	if len(s.AllowedModes) != 2 || s.AllowedModes[0] != "once" || s.AllowedModes[1] != "always" {
		return fmt.Errorf("unsupported computer permission modes")
	}
	switch s.Kind {
	case "directory_read":
		if !CleanAbsolute(s.Directory) || !ReadAction(s.Action) || s.Command != "" || s.CWD != "" {
			return fmt.Errorf("directory permission needs an explicit directory and read action")
		}
	case "exact_command":
		if s.Action != "exec" || strings.TrimSpace(s.Command) == "" || !CleanAbsolute(s.CWD) || s.Directory != "" {
			return fmt.Errorf("command permission needs an exact command and explicit working directory")
		}
	default:
		return fmt.Errorf("unsupported computer permission kind")
	}
	return nil
}
func ReadAction(action string) bool {
	return action == "fs_read" || action == "fs_list" || action == "fs_stat"
}
func (s Scope) Matches(req Request, mode string) bool {
	if s.Validate() != nil || req.Tool != s.Tool {
		return false
	}
	h, err := Hash(req.Tool, req.Action, req.Args)
	if err != nil {
		return false
	}
	if mode == "once" || s.Kind == "exact_command" {
		return (mode == "once" || mode == "always") && req.Action == s.Action && h == s.ArgsHash
	}
	p, _ := req.Args["path"].(string)
	return mode == "always" && s.Kind == "directory_read" && ReadAction(req.Action) && Within(s.Directory, p)
}
