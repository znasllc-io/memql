package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

func (c *Client) installable(ctx context.Context) error {
	if c.Arch != "arm64" && c.Arch != "amd64" {
		return errors.New("Automatic updates are not available for this processor")
	}
	if c.Variant != "headless" && c.Variant != "computeruse" {
		return errors.New("Automatic updates are not available for this build")
	}
	target := c.Executable
	switch c.OS {
	case "darwin":
		if c.bundle == "" {
			return errors.New("Install the current MemQL.app once to enable menu updates")
		}
		target = c.bundle
		body, err := c.run(ctx, "/bin/launchctl", "print", fmt.Sprintf("gui/%d/com.znasllc.memql-worker", os.Getuid()))
		matches := err == nil && launchdProgram(string(body), c.Executable)
		if err == nil && !matches {
			// SMAppService prints a bundle-relative program. Bind that form to
			// the PID of the local, path-verified worker so another copy with the
			// same bundle identifier cannot receive this app's restart.
			status, statusErr := c.status(ctx)
			matches = statusErr == nil && validateWorker(status, c.Executable) == nil && launchdBundledProgram(string(body), status.PID)
		}
		if !matches {
			return errors.New("The installed MemQL worker service is unavailable or points to another app. Repair the installation before updating")
		}
	case "linux":
		body, err := c.run(ctx, "systemctl", "--user", "show", "--property=ExecStart", "--value", "memql-worker.service")
		if err != nil || !systemdProgram(string(body), c.Executable) {
			return errors.New("Automatic updates need the MemQL user service. Update this installation using its installer")
		}
	default:
		return errors.New("Automatic updates are supported on macOS and Linux")
	}
	if err := writableDirectory(filepath.Dir(target)); err != nil {
		return errors.New("This installation needs administrator access. Update it with the installer, or install Cockpit in your user folder to enable menu updates")
	}
	return nil
}

func launchdBundledProgram(body string, pid int) bool {
	if pid < 1 {
		return false
	}
	havePID, haveBundle, haveProgram := false, false, false
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == fmt.Sprintf("pid = %d", pid):
			havePID = true
		case line == "parent bundle identifier = com.znasllc.memql-worker":
			haveBundle = true
		case line == "program identifier = Contents/MacOS/MemQL" || strings.HasPrefix(line, "program identifier = Contents/MacOS/MemQL ("):
			haveProgram = true
		}
	}
	return havePID && haveBundle && haveProgram
}

func launchdProgram(body, path string) bool {
	for _, line := range strings.Split(body, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "program = "); ok {
			return sameExecutable(value, path)
		}
	}
	return false
}
func systemdProgram(body, path string) bool {
	// systemctl's structured ExecStart includes the exact executable separately
	// from argv. Never accept a substring (e.g. /tmp/memql versus /tmp/memql-old).
	_, after, ok := strings.Cut(body, "path=")
	if !ok {
		return false
	}
	value, _, ok := strings.Cut(after, ";")
	if !ok {
		return false
	}
	value = strings.TrimSpace(value)
	if strings.Contains(value, `\`) {
		decoded, err := strconv.Unquote(`"` + value + `"`)
		if err != nil {
			return false
		}
		value = decoded
	}
	return sameExecutable(value, path)
}

func sameExecutable(a, b string) bool {
	// Existing installers keep a friendly memql symlink while the release
	// variant has its own filename. Compare those resolved files, never a
	// substring or a PATH result. Exact strings are useful for absent-path
	// fixture checks; installability still requires the destination directory.
	if a == b {
		return true
	}
	left, err := filepath.EvalSymlinks(a)
	if err != nil {
		return false
	}
	right, err := filepath.EvalSymlinks(b)
	return err == nil && left == right
}

func (c *Client) restartService(ctx context.Context) error {
	var err error
	switch c.OS {
	case "darwin":
		_, err = c.run(ctx, "/bin/launchctl", "kickstart", "-k", fmt.Sprintf("gui/%d/com.znasllc.memql-worker", os.Getuid()))
	case "linux":
		_, err = c.run(ctx, "systemctl", "--user", "restart", "memql-worker.service")
	default:
		return errors.New("Worker service restart is unsupported")
	}
	if err != nil {
		return errors.New("The worker service could not restart")
	}
	return nil
}

func (c *Client) startService(ctx context.Context) error {
	var err error
	switch c.OS {
	case "darwin":
		_, err = c.run(ctx, "/bin/launchctl", "kickstart", fmt.Sprintf("gui/%d/com.znasllc.memql-worker", os.Getuid()))
	case "linux":
		_, err = c.run(ctx, "systemctl", "--user", "start", "memql-worker.service")
	default:
		return errors.New("Worker service start is unsupported")
	}
	if err != nil {
		return errors.New("The worker service could not start")
	}
	return nil
}

// serviceRunning is used only for an interrupted update's explicit recovery.
// A missing control socket does not establish that a service is stopped: it
// could still be doing work, or belong to a different installation.
func (c *Client) serviceRunning(ctx context.Context) (bool, error) {
	switch c.OS {
	case "linux":
		body, err := c.run(ctx, "systemctl", "--user", "show", "--property=MainPID", "--value", "memql-worker.service")
		if err != nil {
			return false, errors.New("The worker service state could not be confirmed")
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
		if err != nil || pid < 0 {
			return false, errors.New("The worker service returned an invalid process ID")
		}
		return pid > 0, nil
	case "darwin":
		body, err := c.run(ctx, "/bin/launchctl", "print", fmt.Sprintf("gui/%d/com.znasllc.memql-worker", os.Getuid()))
		if err != nil {
			return false, errors.New("The worker service state could not be confirmed")
		}
		stopped := false
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimSpace(line)
			if value, ok := strings.CutPrefix(line, "pid = "); ok {
				pid, err := strconv.Atoi(value)
				if err != nil || pid < 1 {
					return false, errors.New("The worker service returned an invalid process ID")
				}
				return true, nil
			}
			if line == "state = not running" || line == "state = waiting" {
				stopped = true
			}
		}
		if stopped {
			return false, nil
		}
		return false, errors.New("The worker service state is unknown")
	default:
		return false, errors.New("This platform has no supported worker service")
	}
}

func bundleValue(ctx context.Context, c *Client, app, key string) (string, error) {
	body, err := c.run(ctx, "/usr/libexec/PlistBuddy", "-c", "Print :"+key, filepath.Join(app, "Contents", "Info.plist"))
	return strings.TrimSpace(string(body)), err
}
func signatureLine(output, prefix string) string {
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix)
		}
	}
	return ""
}

type signingInfo struct {
	team, authority, requirement string
	adhoc                        bool
}

func (c *Client) signing(ctx context.Context, app, identifier string) (signingInfo, error) {
	body, err := c.run(ctx, "/usr/bin/codesign", "--display", "--verbose=4", "-r-", app)
	if err != nil {
		return signingInfo{}, errors.New("The app's signing identity could not be verified")
	}
	output := string(body)
	info := signingInfo{team: signatureLine(output, "TeamIdentifier="), authority: signatureLine(output, "Authority="), requirement: signatureLine(output, "designated => "), adhoc: signatureLine(output, "Signature=") == "adhoc"}
	if info.requirement == "" {
		info.requirement = signatureLine(output, "# designated => ")
	}
	if signatureLine(output, "Identifier=") != identifier || info.team == "" || info.requirement == "" || !info.adhoc && info.authority == "" {
		return signingInfo{}, errors.New("The app returned incomplete or unexpected signing identity information")
	}
	return info, nil
}

func (c *Client) verifyBundle(ctx context.Context, incoming string) error {
	if id, err := bundleValue(ctx, c, incoming, "CFBundleIdentifier"); err != nil || id != "com.znasllc.memql-worker" {
		return errors.New("The downloaded app has an unexpected identity")
	}
	helper := filepath.Join(incoming, "Contents", "Library", "LoginItems", "MemQL Menu.app")
	if id, err := bundleValue(ctx, c, helper, "CFBundleIdentifier"); err != nil || id != "com.znasllc.memql-cockpit-menubar" {
		return errors.New("The downloaded menu has an unexpected identity")
	}
	if _, err := c.run(ctx, "/usr/bin/codesign", "--verify", "--deep", "--strict", incoming); err != nil {
		return errors.New("The downloaded app's signature is invalid")
	}
	if _, err := os.Stat(filepath.Join(helper, "Contents", "MacOS", "MemQLCockpit")); err != nil {
		return errors.New("The downloaded app is missing its menu")
	}
	oldHelper := filepath.Join(c.bundle, "Contents", "Library", "LoginItems", "MemQL Menu.app")
	old, err := c.signing(ctx, c.bundle, "com.znasllc.memql-worker")
	if err != nil {
		return err
	}
	oldMenu, err := c.signing(ctx, oldHelper, "com.znasllc.memql-cockpit-menubar")
	if err != nil {
		return err
	}
	newSignature, err := c.signing(ctx, incoming, "com.znasllc.memql-worker")
	if err != nil {
		return err
	}
	newMenu, err := c.signing(ctx, helper, "com.znasllc.memql-cockpit-menubar")
	if err != nil {
		return err
	}
	if old.adhoc && oldMenu.adhoc {
		// Released apps currently use ad-hoc signatures. Their code hashes
		// necessarily change on upgrade; rejecting that change would disable all
		// published updates. Preserve bundle IDs, verify integrity, and leave any
		// macOS permission reapproval to the person. Never reset TCC grants.
		return nil
	}
	verifyRequirements := func() error {
		for _, pair := range []struct{ app, requirement string }{{incoming, old.requirement}, {helper, oldMenu.requirement}} {
			if _, err := c.run(ctx, "/usr/bin/codesign", "--verify", "--deep", "--strict", "-R="+pair.requirement, pair.app); err != nil {
				return errors.New("The new app does not preserve the installed signing identity. Nothing was installed")
			}
		}
		return nil
	}
	if old.team != "not set" || oldMenu.team != "not set" {
		if old.team != newSignature.team || oldMenu.team != newMenu.team {
			return errors.New("This release uses a different signing team. The installed app was kept")
		}
		return verifyRequirements()
	}
	// A local development installation deliberately uses a stable certificate.
	// Reuse only its already provisioned identity, and prove the resulting app
	// still satisfies the old requirement. Never generate a key or silently
	// replace a certificate-signed installation with an ad-hoc signature.
	if verifyRequirements() == nil {
		return nil
	}
	identity, err := os.ReadFile(filepath.Join(c.home, "Library", "Application Support", "MemQL", "Development", "signing-identity"))
	certificate := strings.TrimSpace(string(identity))
	if err != nil || !regexp.MustCompile(`^[0-9a-fA-F]{40}$`).MatchString(certificate) {
		return errors.New("This app uses a local signing certificate. Restore its saved signing identity before updating")
	}
	for _, app := range []string{helper, incoming} {
		if _, err = c.run(ctx, "/usr/bin/codesign", "--force", "--sign", certificate, app); err != nil {
			return errors.New("The installed signing certificate is unavailable. Unlock its Keychain and retry")
		}
	}
	return verifyRequirements()
}
