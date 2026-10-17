package pipelinesteps

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type cacheFixture struct {
	root, scope, pressure string
	env                   []string
}

func newCacheFixture(t *testing.T) cacheFixture {
	t.Helper()
	requireTools(t, "flock", "realpath", "find")
	root := t.TempDir()
	scope := filepath.Join(root, "owners", strings.Repeat("a", 24), "repos", strings.Repeat("b", 24), "trusted")
	pressure := filepath.Join(scope, "go-u1000", "build", "pressure")
	if err := os.MkdirAll(filepath.Dir(pressure), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pressure, []byte("compiler cache"), 0600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	// Only the filesystem measurement is substituted. Locks, separate processes,
	// deletion and symlink traversal execute the production scripts unchanged.
	df := `#!/bin/sh
printf 'Filesystem 1K-blocks Used Available Use%% Mounted\n'
if [ -e "$PRESSURE_FILE" ]; then p=90; else p=10; fi
printf 'fixture 100 90 10 %s%% /cache\n' "$p"
`
	if err := os.WriteFile(filepath.Join(bin, "df"), []byte(df), 0755); err != nil {
		t.Fatal(err)
	}
	return cacheFixture{root, scope, pressure, []string{"PATH=" + bin + ":" + os.Getenv("PATH"), "CACHE_DIR=" + scope, "CACHE_GC_PERCENT=70", "PRESSURE_FILE=" + pressure}}
}

func (f cacheFixture) script() string {
	return strings.ReplaceAll(cachePrepScript, "/cache-root", f.root)
}

func TestCacheReclamationPreservesModulesAndScopeMounts(t *testing.T) {
	f := newCacheFixture(t)
	module := filepath.Join(f.scope, "go-u1000", "mod", "download")
	if err := os.MkdirAll(filepath.Dir(module), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(module, []byte("module"), 0444); err != nil {
		t.Fatal(err)
	}
	scopeBefore, err := os.Stat(f.scope)
	if err != nil {
		t.Fatal(err)
	}
	result := runShell(t, f.script(), f.env)
	if result.code != 0 {
		t.Fatalf("cleanup: %+v", result)
	}
	if _, err := os.Stat(f.pressure); !os.IsNotExist(err) {
		t.Fatalf("build cache survived: %v", err)
	}
	if _, err := os.Stat(module); err != nil {
		t.Fatalf("module removed unnecessarily: %v", err)
	}
	scopeAfter, err := os.Stat(f.scope)
	if err != nil || !os.SameFile(scopeBefore, scopeAfter) {
		t.Fatalf("mounted scope inode was replaced: %v", err)
	}
	lockBefore, err := os.Stat(filepath.Join(f.root, ".memql-locks", "users"))
	if err != nil {
		t.Fatal(err)
	}
	if result = runShell(t, f.script(), f.env); result.code != 0 {
		t.Fatalf("second initializer: %+v", result)
	}
	lockAfter, err := os.Stat(filepath.Join(f.root, ".memql-locks", "users"))
	if err != nil || !os.SameFile(lockBefore, lockAfter) {
		t.Fatalf("lock inode replaced: %v", err)
	}
}

func TestCacheReclamationFallsBackToOtherDisposableCaches(t *testing.T) {
	f := newCacheFixture(t)
	other := filepath.Join(f.scope, "npm-u1000", "pressure")
	if err := os.MkdirAll(filepath.Dir(other), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("npm cache"), 0600); err != nil {
		t.Fatal(err)
	}
	f.env = append(f.env[:len(f.env)-1], "PRESSURE_FILE="+other)
	result := runShell(t, f.script(), f.env)
	if result.code != 0 {
		t.Fatalf("cleanup: %+v", result)
	}
	if _, err := os.Stat(other); !os.IsNotExist(err) {
		t.Fatalf("secondary cache survived pressure: %v", err)
	}
}

func TestCacheReclamationDoesNotFollowCacheSymlinks(t *testing.T) {
	for _, linkedScope := range []bool{false, true} {
		t.Run(map[bool]string{false: "cache content", true: "scope root"}[linkedScope], func(t *testing.T) {
			f := newCacheFixture(t)
			outside := t.TempDir()
			protected := filepath.Join(outside, "keep")
			if err := os.WriteFile(protected, []byte("outside"), 0600); err != nil {
				t.Fatal(err)
			}
			if linkedScope {
				if err := os.RemoveAll(f.scope); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, f.scope); err != nil {
					t.Fatal(err)
				}
				f.env = append(f.env[:len(f.env)-1], "PRESSURE_FILE="+protected)
			} else {
				if err := os.Symlink(outside, filepath.Join(f.scope, "go-u2000")); err != nil {
					t.Fatal(err)
				}
			}
			result := runShell(t, f.script(), f.env)
			if linkedScope && result.code == 0 {
				t.Fatal("linked scope accepted")
			}
			if !linkedScope && result.code != 0 {
				t.Fatalf("cleanup: %+v", result)
			}
			if data, err := os.ReadFile(protected); err != nil || string(data) != "outside" {
				t.Fatalf("followed symlink: %q %v", data, err)
			}
		})
	}
}

func waitCacheCondition(t *testing.T, why string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(why)
}

func TestCacheReclamationWaitsForAnotherPodAndBlocksNewUsers(t *testing.T) {
	f := newCacheFixture(t)
	// Create lock inodes through the real initializer without applying pressure.
	initEnv := append([]string{}, f.env...)
	initEnv[len(initEnv)-1] = "PRESSURE_FILE=" + filepath.Join(f.root, "absent")
	if res := runShell(t, f.script(), initEnv); res.code != 0 {
		t.Fatalf("prepare: %+v", res)
	}
	locks := filepath.Join(f.root, ".memql-locks")
	active := filepath.Join(f.root, "active")
	holder := exec.Command("/bin/sh", "-c", `exec 9<"$1/users"; flock -s 9; : >"$2"; read -r finish`, "holder", locks, active)
	input, err := holder.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = input.Close(); _ = holder.Process.Kill() })
	waitCacheCondition(t, "old pod did not acquire shared lease", func() bool { _, e := os.Stat(active); return e == nil })
	cleaner := exec.Command("/bin/sh", "-c", f.script())
	cleaner.Env = f.env
	var output bytes.Buffer
	cleaner.Stdout = &output
	cleaner.Stderr = &output
	if err = cleaner.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cleaner.Process.Kill() })
	waitCacheCondition(t, "cleaner did not hold admission gate", func() bool { return exec.Command("flock", "-n", filepath.Join(locks, "gate"), "true").Run() != nil })
	if _, err = os.Stat(f.pressure); err != nil {
		t.Fatalf("cleaner removed an active build cache: %v", err)
	}
	workspace := t.TempDir()
	started := filepath.Join(workspace, "started")
	wrapper := strings.ReplaceAll(stepWrapper, "/workspace", workspace)
	wrapper = strings.ReplaceAll(wrapper, "/memql-cache-gate", filepath.Join(locks, "gate"))
	wrapper = strings.ReplaceAll(wrapper, "/memql-cache-users", filepath.Join(locks, "users"))
	newcomer := exec.Command("/bin/sh", "-c", wrapper)
	newcomer.Env = []string{"PATH=" + os.Getenv("PATH"), "MEMQL_CACHE_LOCKED=1", "MEMQL_STEP_COMMAND=touch started"}
	if err = newcomer.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = newcomer.Process.Kill() })
	time.Sleep(100 * time.Millisecond)
	if _, err = os.Stat(started); !os.IsNotExist(err) {
		t.Fatalf("new pod overtook pending reclamation: %v", err)
	}
	// Abrupt death releases the old pod's kernel lease. No stale PID file or
	// per-workbench in-memory state is needed for another replica to recover.
	if err = holder.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = holder.Wait()
	if err = cleaner.Wait(); err != nil {
		t.Fatalf("cleanup: %v %s", err, output.String())
	}
	if err = newcomer.Wait(); err != nil {
		t.Fatalf("new user: %v", err)
	}
	if _, err = os.Stat(f.pressure); !os.IsNotExist(err) {
		t.Fatalf("cache not reclaimed after old pod exited: %v", err)
	}
	if _, err = os.Stat(started); err != nil {
		t.Fatalf("new pod never resumed: %v", err)
	}
}

func TestCacheReclamationJobIsolation(t *testing.T) {
	cfg := testConfig()
	cfg.CacheGCPercent = 70
	job := mustBuild(t, cfg, testRun())
	prep := job.Spec.Template.Spec.InitContainers[0]
	if len(prep.SecurityContext.Capabilities.Add) != 1 || prep.SecurityContext.Capabilities.Add[0] != "DAC_OVERRIDE" {
		t.Fatalf("cache remover lacks bounded filesystem authority: %+v", prep.SecurityContext)
	}
	for _, c := range job.Spec.Template.Spec.Containers {
		for _, m := range c.VolumeMounts {
			if strings.HasPrefix(m.SubPath, ".memql-locks/") && !m.ReadOnly {
				t.Fatalf("step can replace lock: %+v", m)
			}
			if m.Name == cacheVolume && m.SubPath == "" {
				t.Fatalf("command sees other owners' caches: %+v", m)
			}
		}
	}
}
