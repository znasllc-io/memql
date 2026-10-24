package appsession

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/znasllc-io/memql-cockpit/internal/worker/apps"
	"github.com/znasllc-io/memql-cockpit/internal/worker/transfer"
)

// A Codex ref identifies state inside CODEX_HOME. Independent conversations
// need independent homes; attach must reuse the original home after its bearer
// has been deleted. This index contains only a provider-ref hash and a relative
// directory name, never provider history or credentials. It stays within the
// run workspace's excluded scaffolding and is not copied from the user's home.
func providerRefPath(dir, ref string) string {
	return filepath.Join(dir, "codex-refs", fmt.Sprintf("%x", sha256.Sum256([]byte(ref))))
}

func (m *mcpConfig) codexHome(dir string) (string, error) {
	var home string
	ref := strings.TrimSpace(m.settings.ResumeRef)
	if ref == "" {
		var err error
		home, err = os.MkdirTemp(dir, "codex-")
		if err != nil {
			return "", err
		}
	} else {
		name, err := os.ReadFile(providerRefPath(dir, ref))
		switch {
		case err == nil:
			base := string(name)
			if filepath.Base(base) != base || (!strings.HasPrefix(base, "codex-") && base != "codex") {
				return "", fmt.Errorf("invalid retained Codex home for the requested conversation")
			}
			home = filepath.Join(dir, base)
		case os.IsNotExist(err):
			// Before homes were isolated, this workspace's conversations all
			// lived here. Let the provider resolve the exact old ref there;
			// do not search unrelated workspaces or the person's own history.
			home = filepath.Join(dir, "codex")
		default:
			return "", fmt.Errorf("read retained Codex home: %w", err)
		}
		info, err := os.Lstat(home)
		if err != nil || !info.IsDir() {
			return "", fmt.Errorf("the requested Codex conversation has no retained home in this workspace")
		}
	}
	// The OS releases the lock on process death. Never remove its inode: a
	// second attach must fail before replacing a live conversation's credential.
	release, err := transfer.LockLocal(filepath.Join(home, ".memql-home.lock"))
	if err != nil {
		return "", fmt.Errorf("Codex conversation home is unavailable or already in use: %w", err)
	}
	m.releaseHome = release
	return home, nil
}

func (m *mcpConfig) rememberProviderRef(ref string) error {
	if m == nil || m.app != apps.IDCodex || strings.TrimSpace(ref) == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.removed {
		return fmt.Errorf("cannot retain Codex conversation reference after session cleanup")
	}
	dir := filepath.Join(m.workspace, sessionScaffoldDir)
	path := providerRefPath(dir, strings.TrimSpace(ref))
	name := filepath.Base(filepath.Dir(m.configPath))
	release, err := transfer.LockLocal(path + ".lock")
	if err != nil {
		return fmt.Errorf("lock Codex conversation reference: %w", err)
	}
	defer release()
	if existing, err := os.ReadFile(path); err == nil {
		if string(existing) != name {
			return fmt.Errorf("Codex conversation reference already belongs to another home")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read Codex conversation reference: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), configDirMode); err != nil {
		return err
	}
	if err := writeFileAtomic(path, []byte(name), configFileMode); err != nil {
		return fmt.Errorf("retain Codex conversation reference: %w", err)
	}
	return nil
}
