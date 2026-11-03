//go:build unix

package worker

import (
	"os"
	"path/filepath"
	"testing"
)

// A worker started with MEMQL_SERVICE=app takes its log from launchd when
// launchd opened one (a per-user plist's StandardOutPath, which the owner
// may have pointed anywhere) and supplies ~/.memql/state/worker.log only
// where launchd gave it /dev/null (the agent registered from the bundle).
func TestServiceOutputKeepsTheFileLaunchdOpened(t *testing.T) {
	log, err := os.Create(filepath.Join(t.TempDir(), "custom.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	if !isRegularFile(log) {
		t.Error("a log file launchd opened from StandardOutPath must be kept")
	}
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	if isRegularFile(null) {
		t.Error("/dev/null, where launchd sends a bundled agent's output, must be replaced by the worker log")
	}
}
