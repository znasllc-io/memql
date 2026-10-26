//go:build linux

package imageverify

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestReceiptDestinationRejectsSourceSymlinkAndSwap(t *testing.T) {
	t.Run("preexisting symlink", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(root, ".memql-image-build")); err != nil {
			t.Fatal(err)
		}
		if d, err := reserveReceiptDestination(root); err == nil {
			d.Close()
			t.Fatal("repository symlink accepted")
		}
		if _, err := os.Stat(filepath.Join(outside, "verification.json")); !os.IsNotExist(err) {
			t.Fatal("wrote outside checkout")
		}
	})
	t.Run("directory swap", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		d, err := reserveReceiptDestination(root)
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		if err = os.Rename(d.path, d.path+"-moved"); err != nil {
			t.Fatal(err)
		}
		if err = os.Symlink(outside, d.path); err != nil {
			t.Fatal(err)
		}
		if d.Write([]byte(`{}`)) == nil {
			t.Fatal("swapped destination accepted")
		}
		if _, err = os.Stat(filepath.Join(outside, "verification.json")); !os.IsNotExist(err) {
			t.Fatal("wrote through replacement symlink")
		}
	})
	t.Run("exclusive file", func(t *testing.T) {
		root := t.TempDir()
		d, err := reserveReceiptDestination(root)
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		if err = d.Write([]byte(`{}`)); err != nil {
			t.Fatal(err)
		}
		if d.Write([]byte(`{}`)) == nil {
			t.Fatal("existing receipt replaced")
		}
	})
}
func TestNamespaceCancellationStopsIgnoringProcessAndJoinsOutput(t *testing.T) {
	if os.Getenv("MEMQL_IMAGE_TEST_CHILD") == "1" {
		signal.Ignore(syscall.SIGTERM)
		for {
			time.Sleep(time.Second)
		}
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestNamespaceCancellationStopsIgnoringProcessAndJoinsOutput$")
	cmd.Env = []string{"MEMQL_IMAGE_TEST_CHILD=1"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	cmd.Stdout = writer
	cmd.Stderr = writer
	if err = cmd.Start(); err != nil {
		writer.Close()
		t.Fatal(err)
	}
	writer.Close()
	defer syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	result := joinNamespaceProcess(ctx, cmd, reader, Record{}, nil)
	if result.Cleaned || result.Error == "" {
		t.Fatal("cancelled child fabricated cleanup")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("cancellation waited for outer service maximum")
	}
	if cmd.ProcessState == nil || !cmd.ProcessState.Exited() && cmd.ProcessState.Success() {
		t.Fatal("child was not reaped")
	}
	if err = syscall.Kill(cmd.Process.Pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatal("child survived cancellation")
	}
}
func TestMappedCleanupRetainsMarkerForRestart(t *testing.T) {
	r := attemptFixture(t)
	if err := createAttempt(ServiceConfig{}, r, serviceRequest{}); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(r.Root, "ns-current")
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	if err := clearMappedState(namespaceInput{Record: r, RootlessState: state}); err != nil {
		t.Fatal(err)
	}
	if err := checkAttemptMarker(r); err != nil {
		t.Fatal("cleanup removed restart identity", err)
	}
	for _, p := range []string{"home", "state", "output", "runtime"} {
		if _, err := os.Lstat(filepath.Join(r.Root, p)); !os.IsNotExist(err) {
			t.Fatal("mapped output survived")
		}
	}
}
func TestBuildArgumentsOnlyExportPrivateOCI(t *testing.T) {
	q := sampleRequest()
	q.Args["TEXT"] = "$(touch /tmp/forbidden)"
	r := Record{Root: "/state/exact"}
	args := imageBuildArgs(r, q, "/source", "/source/Dockerfile")
	joined := strings.Join(args, "\n")
	for _, want := range []string{"type=oci,dest=/state/exact/output/image.oci.tar", "build-arg:TEXT=$(touch /tmp/forbidden)", "platform=linux/amd64"} {
		if !strings.Contains(joined, want) {
			t.Fatal("typed argument missing")
		}
	}
	for _, bad := range []string{"push=true", "type=registry", "--allow", "--secret", "--ssh", "--export-cache"} {
		if strings.Contains(joined, bad) {
			t.Fatal("publication or host capability injected")
		}
	}
}
