package safety

import (
	"errors"
	"testing"
)

func fsRead(paths ...string) ActionDescriptor {
	return NewFSAction(SurfaceWorkbench, ActionFSRead, paths, CallerContext{})
}
func fsWrite(paths ...string) ActionDescriptor {
	return NewFSAction(SurfaceWorkbench, ActionFSWrite, paths, CallerContext{})
}

func TestFSReadSensitive(t *testing.T) {
	deny := []string{
		"/Users/jose/.ssh/id_rsa",
		"/root/.ssh/identity",
		"/home/alice/.aws/credentials",
		"/Users/jose/.gnupg/secring.gpg",
		"/etc/shadow",
		"/etc/sudoers",
		"/Users/jose/.docker/config.json",
		"/app/.env",
		"/app/.env.production",
	}
	for _, p := range deny {
		cls, err := ruleFSReadSensitive(fsRead(p))
		if err != nil || cls.Tier != TierHigh || !hasCategory(cls.Categories, CategoryCredentialAccess) {
			t.Errorf("sensitive read %q: expected high credential_access, got %+v err=%v", p, cls, err)
		}
	}
	allow := []string{
		"/tmp/foo",
		"/Users/jose/Documents/notes.txt",
		"/var/log/syslog",
		"/etc/hostname",
		"/app/environment.go", // .env-LIKE name should NOT match (basename check)
	}
	for _, p := range allow {
		if _, err := ruleFSReadSensitive(fsRead(p)); !errors.Is(err, ErrNoClassifierOpinion) {
			t.Errorf("benign read %q: expected escalation", p)
		}
	}
	// Non-FSRead actions escape.
	if _, err := ruleFSReadSensitive(fsWrite("/etc/shadow")); !errors.Is(err, ErrNoClassifierOpinion) {
		t.Error("fs_write should not trigger fs_read_sensitive rule")
	}
}

func TestFSWriteSystem(t *testing.T) {
	deny := []string{
		"/Users/jose/.ssh/authorized_keys",
		"/Users/jose/.bashrc",
		"/etc/cron.d/evil",
		"/etc/sudoers",
		"/Library/LaunchAgents/com.evil.plist",
		"/usr/bin/evil",
		"/usr/local/bin/x",
	}
	for _, p := range deny {
		cls, err := ruleFSWriteSystem(fsWrite(p))
		if err != nil || cls.Tier != TierHigh {
			t.Errorf("sensitive write %q: expected high, got %+v err=%v", p, cls, err)
		}
	}
	allow := []string{
		"/tmp/foo",
		"/Users/jose/project/src/main.go",
	}
	for _, p := range allow {
		if _, err := ruleFSWriteSystem(fsWrite(p)); !errors.Is(err, ErrNoClassifierOpinion) {
			t.Errorf("benign write %q: expected escalation", p)
		}
	}
}

// Any-match: a batch of paths fires the rule if ANY path is sensitive.
func TestFSReadSensitiveAnyMatch(t *testing.T) {
	desc := fsRead("/tmp/safe.txt", "/etc/shadow")
	cls, err := ruleFSReadSensitive(desc)
	if err != nil || cls.Tier != TierHigh {
		t.Errorf("batch with one sensitive: expected high, got %+v err=%v", cls, err)
	}
}
