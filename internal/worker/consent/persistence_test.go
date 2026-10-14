package consent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDirectoryReadConsentSurvivesExpiryAndRestartUntilLocalRevoke(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	homes := NewPersistentHomes(dir)
	homes.now = func() time.Time { return now }
	m := homes.For("cluster-a")
	if !m.AllowsDirectoryRead("fs_list").Allowed || m.Allows("workerHost", "fs_list").Allowed {
		t.Fatal("scoped receipt and unscoped request must have separate consent")
	}
	if _, err := m.Grant(time.Minute, false, nil); err != nil {
		t.Fatal(err)
	}
	now = now.Add(365 * 24 * time.Hour)
	if !m.AllowsDirectoryRead("fs_list").Allowed || m.Allows("workerHost", "exec").Allowed {
		t.Fatal("window expiry revoked scoped reads or retained command access")
	}
	m = NewPersistentHomes(dir).For("cluster-a")
	if !m.AllowsDirectoryRead("fs_read").Allowed {
		t.Fatal("restart revoked scoped reads")
	}
	if err := m.Revoke(); err != nil {
		t.Fatal(err)
	}
	if m.AllowsDirectoryRead("fs_list").Allowed {
		t.Fatal("local revoke ignored")
	}
	homes = NewPersistentHomes(dir)
	m = homes.For("cluster-a")
	if m.AllowsDirectoryRead("fs_stat").Allowed || !m.Snapshot().DirectoryReadsPaused {
		t.Fatal("restart forgot local stop")
	}
	if !homes.For("cluster-b").AllowsDirectoryRead("fs_list").Allowed {
		t.Fatal("local stop leaked between clusters")
	}
	if _, err := m.Grant(time.Minute, false, nil); err != nil {
		t.Fatal(err)
	}
	m = NewPersistentHomes(dir).For("cluster-a")
	if !m.AllowsDirectoryRead("fs_list").Allowed || m.Snapshot().Granted {
		t.Fatal("restart lost explicit resume or persisted a temporary control window")
	}
	for _, action := range []string{"exec", "fs_write", "http_fetch", "pipeline_step", "screenshot", "unknown"} {
		if m.AllowsDirectoryRead(action).Allowed {
			t.Fatalf("scoped gate admitted %s", action)
		}
	}
}

func TestDirectoryReadConsentFailsClosedOnDamagedOrUnwritableState(t *testing.T) {
	dir := t.TempDir()
	m := NewPersistentHomes(dir).For("cluster-a")
	if err := m.Revoke(); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"{", `{}`, `{"version":1,"home":"cluster-a"}`, `{"version":1,"home":"cluster-b","paused":false}`} {
		if err := os.WriteFile(m.statePath, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if NewPersistentHomes(dir).For("cluster-a").AllowsDirectoryRead("fs_read").Allowed {
			t.Fatalf("damaged state granted access: %s", body)
		}
	}
	// An explicit local grant repairs malformed state, never an incoming receipt.
	m = NewPersistentHomes(dir).For("cluster-a")
	if _, err := m.Grant(time.Minute, false, nil); err != nil {
		t.Fatal(err)
	}
	if !NewPersistentHomes(dir).For("cluster-a").AllowsDirectoryRead("fs_list").Allowed {
		t.Fatal("explicit local resume did not repair state")
	}
	info, err := os.Stat(m.statePath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("state permissions: %v %v", info, err)
	}
	// A file in place of the state directory exercises storage failure even as root.
	bad := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(bad, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	m = NewPersistentHomes(bad).For("cluster-a")
	if m.AllowsDirectoryRead("fs_list").Allowed {
		t.Fatal("unreadable state allowed access")
	}
	if err := m.Revoke(); err == nil {
		t.Fatal("failed persistence acknowledged")
	}
	if _, err := m.Grant(time.Minute, false, nil); err == nil {
		t.Fatal("failed resume acknowledged")
	}
	if m.AllowsDirectoryRead("fs_list").Allowed || m.Snapshot().Granted {
		t.Fatal("storage failure opened access")
	}
}
