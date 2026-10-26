package imageverify

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

type hostFactsFixture struct {
	files      map[string][]byte
	failures   map[string]error
	version    string
	versionErr error
	calls      []string
}

func (r *hostFactsFixture) Read(p string, max int64) ([]byte, error) {
	r.calls = append(r.calls, p)
	if e := r.failures[p]; e != nil {
		return nil, e
	}
	b, ok := r.files[p]
	if !ok {
		return nil, os.ErrNotExist
	}
	return b, nil
}
func (r *hostFactsFixture) ManagerVersion(context.Context, ServiceConfig) (string, error) {
	return r.version, r.versionErr
}
func validHostFactsFixture() *hostFactsFixture {
	return &hostFactsFixture{files: map[string][]byte{
		"/proc/sys/kernel/osrelease": []byte("6.8.0-fixture\n"), "/etc/passwd": []byte("root:x:0:0:root:/root:/bin/sh\nfixtureuser:x:1000:1000:Fixture:/home/fixtureuser:/bin/sh\n"),
		"/etc/subuid": []byte("other:200000:65536\nfixtureuser:100000:65536\n"), "/etc/subgid": []byte("1000:100000:65536\n"), "/proc/sys/user/max_user_namespaces": []byte("10000\n"),
		"/sys/fs/cgroup/cgroup.controllers": []byte("cpu memory pids io\n"), "/sys/fs/cgroup/system.slice/cgroup.controllers": []byte("pids cpu memory\n"),
	}, failures: map[string]error{}, version: "255-fixture"}
}
func TestImageServiceHostFactsAreExactBoundedReadOnlyInputs(t *testing.T) {
	c := serviceConfigFixture()
	reference, e := collectServiceHostFacts(context.Background(), c, validHostFactsFixture())
	if e != nil {
		t.Fatal(e)
	}
	for _, kind := range []string{"valid", "unrelated mapping", "numeric mapping", "missing account", "wrong group", "duplicate account", "malformed account", "missing mappings", "overlap", "short mapping", "owner overlap", "userns disabled", "missing userns", "namespace permission", "missing controller", "duplicate controller", "manager failure", "oversize", "changed policy", "changed mapping", "changed controllers", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			r := validHostFactsFixture()
			ctx := context.Background()
			wantOK := false
			wantDifferent := false
			switch kind {
			case "valid":
				wantOK = true
			case "unrelated mapping":
				r.files["/etc/subuid"] = []byte("other:300000:65536\nfixtureuser:100000:65536\n")
				wantOK = true
			case "numeric mapping":
				r.files["/etc/subuid"] = []byte("1000:100000:65536\n")
				wantOK = true
			case "missing account":
				r.files["/etc/passwd"] = []byte("root:x:0:0:root:/root:/bin/sh\n")
			case "wrong group":
				r.files["/etc/passwd"] = []byte("fixtureuser:x:1000:1001:Fixture:/home/fixtureuser:/bin/sh\n")
			case "duplicate account":
				r.files["/etc/passwd"] = append(r.files["/etc/passwd"], []byte("other:x:1000:1000:Fixture:/home/other:/bin/sh\n")...)
			case "malformed account":
				r.files["/etc/passwd"] = []byte("not-a-passwd-row")
			case "missing mappings":
				delete(r.files, "/etc/subgid")
			case "overlap":
				r.files["/etc/subuid"] = []byte("fixtureuser:100000:65536\n1000:120000:65536\n")
			case "short mapping":
				r.files["/etc/subuid"] = []byte("fixtureuser:100000:65535\n")
			case "owner overlap":
				r.files["/etc/subgid"] = []byte("fixtureuser:999:65536\n")
			case "userns disabled":
				r.files["/proc/sys/kernel/unprivileged_userns_clone"] = []byte("0")
			case "missing userns":
				delete(r.files, "/proc/sys/user/max_user_namespaces")
			case "namespace permission":
				r.failures["/sys/kernel/security/lsm"] = os.ErrPermission
			case "missing controller":
				r.files["/sys/fs/cgroup/system.slice/cgroup.controllers"] = []byte("cpu memory")
			case "duplicate controller":
				r.files["/sys/fs/cgroup/cgroup.controllers"] = []byte("cpu cpu memory pids")
			case "manager failure":
				r.versionErr = errors.New("unavailable")
			case "oversize":
				r.files["/etc/subuid"] = []byte(strings.Repeat("x", serviceHostFileMaxBytes+1))
			case "changed policy":
				r.files["/proc/sys/kernel/apparmor_restrict_unprivileged_userns"] = []byte("0")
				wantOK = true
				wantDifferent = true
			case "changed mapping":
				r.files["/etc/subuid"] = []byte("fixtureuser:200000:65536")
				wantOK = true
				wantDifferent = true
			case "changed controllers":
				r.files["/sys/fs/cgroup/cgroup.controllers"] = []byte("cpu memory pids")
				wantOK = true
				wantDifferent = true
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			got, err := collectServiceHostFacts(ctx, c, r)
			if (err == nil) != wantOK {
				t.Fatalf("accepted=%v error=%v", err == nil, err)
			}
			if wantOK {
				if (got == reference) == wantDifferent {
					t.Fatalf("host fingerprint drift: different=%v got=%+v", wantDifferent, got)
				}
			}
		})
	}
}
func TestImageServiceAdvertisementDoesNotReuseChangedIdentityOrExpiredHash(t *testing.T) {
	var cache serviceAvailabilityCache
	now := time.Now()
	calls := 0
	qualify := func() error { calls++; return nil }
	if !cache.check(context.Background(), now, "first", qualify) || !cache.check(context.Background(), now.Add(time.Minute), "first", qualify) || calls != 1 {
		t.Fatal("unchanged advertisement rehashed or refused")
	}
	if !cache.check(context.Background(), now.Add(time.Minute), "changed", qualify) || calls != 2 {
		t.Fatal("changed file identity reused")
	}
	if !cache.check(context.Background(), now.Add(7*time.Minute), "changed", qualify) || calls != 3 {
		t.Fatal("expired hash reused")
	}
	if cache.check(context.Background(), now.Add(8*time.Minute), "failed", func() error { calls++; return errors.New("changed bytes") }) {
		t.Fatal("failed installation advertised")
	}
	if cache.check(context.Background(), now.Add(9*time.Minute), "failed", qualify) || calls != 4 {
		t.Fatal("failure became a cached success")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if cache.check(ctx, now, "first", qualify) || calls != 4 {
		t.Fatal("cancelled observation qualified")
	}
	if cache.check(context.Background(), now, "", qualify) {
		t.Fatal("unknown identity qualified")
	}
}

type availabilityStat struct {
	serviceTestInfo
	ctime int64
}

func (i availabilityStat) Sys() any {
	return struct {
		Dev, Ino uint64
		Uid, Gid uint32
		Ctim     struct{ Sec, Nsec int64 }
	}{1, 2, i.uid, 0, struct{ Sec, Nsec int64 }{i.ctime, 0}}
}

type availabilityFiles struct {
	serviceTestFiles
	changes map[string]int64
	opens   int
}

func (f *availabilityFiles) Lstat(p string) (os.FileInfo, error) {
	info, ok := f.info[p]
	if !ok {
		return nil, os.ErrNotExist
	}
	return availabilityStat{info, f.changes[p]}, nil
}
func (f *availabilityFiles) Open(p string) (io.ReadCloser, error) {
	f.opens++
	return f.serviceTestFiles.Open(p)
}
func TestImageServiceCachedIdentityChecksAncestorsAndNeverReadsExecutables(t *testing.T) {
	c := serviceConfigFixture()
	fs := &availabilityFiles{serviceTestFiles: serviceFilesFixture(t, c), changes: map[string]int64{}}
	for _, p := range []string{ServiceQualificationPath, "/etc/passwd", "/etc/subuid", "/etc/subgid"} {
		fs.put(p, []byte("fixture"), 0644)
	}
	first, e := serviceAvailabilityStamp(fs, c)
	if e != nil {
		t.Fatal(e)
	}
	fs.changes[c.Tools["buildkitd"].Path] = 1
	changed, e := serviceAvailabilityStamp(fs, c)
	if e != nil || changed == first {
		t.Fatal("same-size executable rewrite was not detected", e)
	}
	fs.changes["/usr/local"] = 1
	ancestor, e := serviceAvailabilityStamp(fs, c)
	if e != nil || ancestor == changed {
		t.Fatal("ancestor replacement was not detected", e)
	}
	info := fs.info["/usr/local"]
	info.mode |= 0020
	fs.info["/usr/local"] = info
	if _, e = serviceAvailabilityStamp(fs, c); e == nil {
		t.Fatal("writable ancestor cached")
	}
	if fs.opens != 0 {
		t.Fatal("metadata cache read executable contents")
	}
}
func TestImageServiceSubordinateGroupRangeUsesActualHostGID(t *testing.T) {
	if _, err := serviceSubIDFingerprint("fixtureuser:100000:65536", "fixtureuser", 1000, 100001); err == nil {
		t.Fatal("subordinate range overlapping actual GID accepted")
	}
}
