package desktopmenu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// updateReply is the credential-free contract of the same installed command
// that owns the worker. The tray never reads enrollment or downloads a release.
type updateReply struct {
	OK                bool   `json:"ok"`
	CurrentVersion    string `json:"current_version"`
	LatestVersion     string `json:"latest_version"`
	Available         bool   `json:"available"`
	RestartRequired   bool   `json:"restart_required"`
	CanInstall        bool   `json:"can_install"`
	Updated           bool   `json:"updated"`
	WorkerReconnected bool   `json:"worker_reconnected"`
	Phase             string `json:"phase"`
	Message           string `json:"message"`
	Error             string `json:"error"`
}

func decodeUpdate(output []byte, commandErr error) (updateReply, error) {
	var result updateReply
	if err := json.Unmarshal(output, &result); err != nil {
		return result, errors.New("Could not read the update result. Check again.")
	}
	if !result.OK {
		if result.Error != "" {
			return result, errors.New(result.Error)
		}
		return result, errors.New("The update request failed. Try again.")
	}
	if commandErr != nil {
		return result, errors.New("The update command did not finish successfully. Check again.")
	}
	return result, nil
}

func requestUpdate(ctx context.Context, executable string, args ...string) (updateReply, error) {
	output, err := exec.CommandContext(ctx, executable, append([]string{"update", "--json"}, args...)...).Output()
	return decodeUpdate(output, err)
}

// Install has its own bounded download/restart operations, so it must not
// inherit a short status-request deadline. Quit is disabled while it runs;
// an OS shutdown can still interrupt the service, and recovery belongs to
// the installer. No shell, token or enrollment is involved.
func installUpdate(executable, version string) (updateReply, error) {
	output, err := exec.Command(executable, "update", "--install", "--version="+version, "--json").Output()
	return decodeUpdate(output, err)
}

type updateState struct {
	result        updateReply
	checking      bool
	installing    bool
	progress      string
	problem       string
	installFailed bool
}

type updatePresentation struct {
	status       string
	checkLabel   string
	installLabel string
	showInstall  bool
	canCheck     bool
	canInstall   bool
	badge        bool
	problem      string
}

func (s *updateState) beginCheck() bool {
	if s.checking || s.installing {
		return false
	}
	s.checking = true
	return true
}

func (s *updateState) checked(result updateReply, err error) {
	s.checking = false
	if err != nil {
		// Retain a known release when a later check is offline. The installer
		// validates the pinned version again before replacing anything.
		s.problem = err.Error()
		return
	}
	s.result = result
	s.problem = ""
	s.installFailed = false
}

func (s *updateState) beginInstall() (string, bool) {
	if s.checking || s.installing || (!s.result.Available && !s.result.RestartRequired) || !s.result.CanInstall || s.result.LatestVersion == "" {
		return "", false
	}
	s.installing = true
	s.progress = "Preparing update…"
	s.problem = ""
	return s.result.LatestVersion, true
}

func (s *updateState) installed(result updateReply, err error) {
	s.installing = false
	s.progress = ""
	if err != nil {
		if result.Updated {
			// Keep the completed replacement and any explicit restart recovery.
			// A receipt-only failure must not offer another installation.
			s.result = result
			s.result.Available = false
			if !result.RestartRequired {
				s.result.CanInstall = false
			}
		}
		s.problem = err.Error()
		s.installFailed = !result.Updated
		return
	}
	s.result = result
	s.problem = ""
	s.installFailed = false
}

func (s *updateState) progressChanged(result updateReply) {
	if !s.installing || result.Message == "" {
		return
	}
	// A previous operation's persisted completion can remain until the new
	// installer obtains its lock. Only the active process's final response
	// may finish this operation or report its failure.
	switch result.Phase {
	case "starting", "downloading", "verifying", "installing", "restarting", "reconnecting":
		s.progress = result.Message
	}
}

func (s updateState) presentation() updatePresentation {
	p := updatePresentation{checkLabel: "Check for updates", canCheck: !s.checking && !s.installing, badge: s.result.Available || s.result.RestartRequired, problem: s.problem}
	switch {
	case s.installing:
		p.status = s.progress
		if p.status == "" {
			p.status = "Updating Cockpit…"
		}
		p.checkLabel = "Update in progress…"
	case s.checking:
		p.status = "Checking for updates…"
		p.checkLabel = "Checking for updates…"
	case s.result.RestartRequired:
		p.status = "Restart to finish updating Cockpit"
	case s.result.Updated:
		p.status = s.result.Message
		if p.status == "" {
			if s.result.WorkerReconnected {
				p.status = "Cockpit updated; worker connected"
			} else {
				p.status = "Cockpit updated; waiting for worker connection"
			}
		}
	case s.result.Available:
		p.status = fmt.Sprintf("Cockpit %s is available", s.result.LatestVersion)
	case s.result.OK:
		p.status = "Cockpit is up to date"
	default:
		p.status = "Updates have not been checked"
	}
	if s.problem != "" {
		p.checkLabel = "Retry update check"
	}
	p.showInstall = (s.result.Available || s.result.RestartRequired) && s.result.CanInstall && !s.installing
	p.canInstall = p.showInstall && !s.checking && s.result.LatestVersion != ""
	p.installLabel = fmt.Sprintf("Update to %s and restart", s.result.LatestVersion)
	if s.installFailed {
		p.installLabel = fmt.Sprintf("Retry update to %s", s.result.LatestVersion)
	}
	if s.result.RestartRequired {
		p.installLabel = "Restart to finish update"
	}
	if !s.result.CanInstall && (s.result.Available || s.result.RestartRequired) && p.problem == "" {
		p.problem = s.result.Message
	}
	return p
}

func aboutVersion(version, variant string) string {
	if strings.TrimSpace(version) == "" {
		version = "unknown"
	}
	if variant == "" {
		return "Version " + version
	}
	return "Version " + version + " (" + variant + ")"
}
