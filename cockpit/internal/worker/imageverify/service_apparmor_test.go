package imageverify

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

type appArmorFixture struct {
	*hostFactsFixture
	entries  []string
	entryErr error
}

func (r *appArmorFixture) AppArmorProfileEntries() ([]string, error) {
	return r.entries, r.entryErr
}

func validAppArmorFixture() (ServiceConfig, *appArmorFixture) {
	c := serviceConfigFixture()
	tool := c.Tools["rootlesskit"]
	tool.Path = "/usr/bin/rootlesskit"
	c.Tools["rootlesskit"] = tool
	r := &appArmorFixture{hostFactsFixture: validHostFactsFixture(), entries: []string{"unrelated.1", "rootlesskit.2"}}
	r.files["/sys/module/apparmor/parameters/enabled"] = []byte("Y")
	r.files["/sys/kernel/security/lsm"] = []byte("capability,landlock,apparmor")
	r.files["/proc/sys/kernel/apparmor_restrict_unprivileged_userns"] = []byte("1")
	for field, value := range map[string]string{"name": "rootlesskit", "mode": "unconfined", "attach": "/usr/bin/rootlesskit", "sha256": strings.Repeat("a", 64)} {
		r.files[serviceAppArmorProfiles+"/rootlesskit.2/"+field] = []byte(value + "\n")
	}
	r.files["/etc/apparmor.d/rootlesskit"] = []byte("# fixture source only; no policy is installed\n")
	return c, r
}

func TestImageServiceAppArmorRequiresLoadedExactExecutablePolicy(t *testing.T) {
	c, r := validAppArmorFixture()
	reference, err := collectServiceHostFacts(context.Background(), c, r)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"valid", "enforce", "renumbered", "path named", "source drift", "loaded hash drift", "nonstandard executable", "unreadable directory", "too many profiles", "missing profile", "ambiguous profile", "path escape", "wrong name", "wrong attach", "complain", "empty hash", "short hash", "uppercase hash", "hash unreadable", "hash missing", "oversize name", "source unreadable", "source missing", "empty source", "oversize source", "disabled with restriction", "disabled with active LSM", "invalid enabled", "invalid restriction"} {
		t.Run(kind, func(t *testing.T) {
			c, r := validAppArmorFixture()
			ok, different := false, false
			base := serviceAppArmorProfiles + "/rootlesskit.2/"
			switch kind {
			case "valid":
				ok = true
			case "enforce":
				r.files[base+"mode"] = []byte("enforce")
				ok = true
				different = true
			case "renumbered":
				r.entries = []string{"rootlesskit.50"}
				for _, f := range []string{"name", "mode", "attach", "sha256"} {
					r.files[serviceAppArmorProfiles+"/rootlesskit.50/"+f] = r.files[base+f]
				}
				ok = true
			case "path named":
				r.entries = []string{"usr.bin.rootlesskit.50"}
				for _, f := range []string{"name", "mode", "attach", "sha256"} {
					r.files[serviceAppArmorProfiles+"/usr.bin.rootlesskit.50/"+f] = r.files[base+f]
				}
				r.files[serviceAppArmorProfiles+"/usr.bin.rootlesskit.50/name"] = []byte("/usr/bin/rootlesskit")
				r.files["/etc/apparmor.d/usr.bin.rootlesskit"] = r.files["/etc/apparmor.d/rootlesskit"]
				ok = true
				different = true
			case "source drift":
				r.files["/etc/apparmor.d/rootlesskit"] = []byte("changed")
				ok = true
				different = true
			case "loaded hash drift":
				r.files[base+"sha256"] = []byte(strings.Repeat("b", 64))
				ok = true
				different = true
			case "nonstandard executable":
				tool := c.Tools["rootlesskit"]
				tool.Path = "/opt/rootlesskit"
				c.Tools["rootlesskit"] = tool
			case "unreadable directory":
				r.entryErr = os.ErrPermission
			case "too many profiles":
				r.entries = make([]string, serviceAppArmorProfileLimit+1)
			case "missing profile":
				r.entries = []string{"other.1"}
			case "ambiguous profile":
				r.entries = append(r.entries, "rootlesskit.3")
			case "path escape":
				r.entries = []string{"rootlesskit.2/../../elsewhere"}
			case "wrong name":
				r.files[base+"name"] = []byte("other")
			case "wrong attach":
				r.files[base+"attach"] = []byte("/usr/bin/*")
			case "complain":
				r.files[base+"mode"] = []byte("complain")
			case "empty hash":
				r.files[base+"sha256"] = nil
			case "short hash":
				r.files[base+"sha256"] = []byte("abc")
			case "uppercase hash":
				r.files[base+"sha256"] = []byte(strings.Repeat("A", 64))
			case "hash unreadable":
				r.failures[base+"sha256"] = os.ErrPermission
			case "hash missing":
				delete(r.files, base+"sha256")
			case "oversize name":
				r.files[base+"name"] = []byte(strings.Repeat("x", 4097))
			case "source unreadable":
				r.failures["/etc/apparmor.d/rootlesskit"] = errors.New("immutable validation refused")
			case "source missing":
				delete(r.files, "/etc/apparmor.d/rootlesskit")
			case "empty source":
				r.files["/etc/apparmor.d/rootlesskit"] = nil
			case "oversize source":
				r.files["/etc/apparmor.d/rootlesskit"] = make([]byte, serviceHostFileMaxBytes+1)
			case "disabled with restriction":
				r.files["/sys/module/apparmor/parameters/enabled"] = []byte("N")
			case "disabled with active LSM":
				r.files["/sys/module/apparmor/parameters/enabled"] = []byte("N")
				r.files["/proc/sys/kernel/apparmor_restrict_unprivileged_userns"] = []byte("0")
			case "invalid enabled":
				r.files["/sys/module/apparmor/parameters/enabled"] = []byte("unknown")
			case "invalid restriction":
				r.files["/proc/sys/kernel/apparmor_restrict_unprivileged_userns"] = []byte("unknown")
			}
			got, e := collectServiceHostFacts(context.Background(), c, r)
			if (e == nil) != ok {
				t.Fatalf("accepted=%v want=%v err=%v", e == nil, ok, e)
			}
			if ok && (got.NamespacePolicySHA256 == reference.NamespacePolicySHA256) == different {
				t.Fatal("loaded/source policy identity was not bound accurately")
			}
			for _, p := range r.calls {
				if p == "/sys/kernel/security/apparmor/profiles" {
					t.Fatal("privileged global policy listing requested")
				}
			}
		})
	}
}

func TestImageServiceAppArmorCannotBorrowUnavailablePolicyReader(t *testing.T) {
	c, r := validAppArmorFixture()
	if _, err := collectServiceHostFacts(context.Background(), c, r.hostFactsFixture); err == nil {
		t.Fatal("enabled AppArmor without loaded evidence accepted")
	}
}
