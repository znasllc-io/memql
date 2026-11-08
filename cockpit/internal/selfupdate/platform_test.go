package selfupdate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSystemdFriendlySymlinkResolvesInstalledVariant(t *testing.T) {
	root := t.TempDir()
	actual := filepath.Join(root, "memql-computeruse-linux-amd64")
	friendly := filepath.Join(root, "memql")
	if err := os.WriteFile(actual, []byte("binary"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(actual, friendly); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(actual)
	if err != nil {
		t.Fatal(err)
	}
	if !systemdProgram("{ path="+friendly+" ; argv[]="+friendly+" worker run ; }", resolved) {
		t.Fatal("installer's friendly symlink rejected")
	}
	if !launchdProgram("program = "+friendly, resolved) {
		t.Fatal("canonical equivalent rejected")
	}
}

func TestSMAppServiceRequiresBundleProgramAndExactPID(t *testing.T) {
	body := "parent bundle identifier = com.znasllc.memql-worker\nprogram identifier = Contents/MacOS/MemQL (mode: 2)\npid = 123\n"
	if !launchdBundledProgram(body, 123) {
		t.Fatal("SMAppService rejected")
	}
	for _, test := range []string{strings.ReplaceAll(body, "pid = 123", "pid = 1234"), strings.ReplaceAll(body, "memql-worker", "other-worker"), strings.ReplaceAll(body, "MacOS/MemQL ", "MacOS/MemQL-old ")} {
		if launchdBundledProgram(test, 123) {
			t.Error("unrelated service accepted")
		}
	}
}

func TestServiceStateFailsClosedAndRecoveryStartDoesNotKill(t *testing.T) {
	for _, test := range []struct {
		os, output       string
		running, wantErr bool
	}{{"linux", "0\n", false, false}, {"linux", "123\n", true, false}, {"linux", "unknown\n", false, true}, {"darwin", "state = not running\n", false, false}, {"darwin", "state = waiting\n", false, false}, {"darwin", "state = running\npid = 123\n", true, false}, {"darwin", "pid = bad\n", false, true}, {"darwin", "", false, true}} {
		c := &Client{OS: test.os, run: func(context.Context, string, ...string) ([]byte, error) { return []byte(test.output), nil }}
		got, err := c.serviceRunning(context.Background())
		if got != test.running || (err != nil) != test.wantErr {
			t.Errorf("%s %q: got %v %v", test.os, test.output, got, err)
		}
	}
	for _, platform := range []string{"darwin", "linux"} {
		c := &Client{OS: platform, run: func(ctx context.Context, command string, args ...string) ([]byte, error) {
			for _, arg := range args {
				if arg == "-k" || arg == "restart" {
					t.Fatal("recovery start kills existing worker")
				}
			}
			return nil, nil
		}}
		if err := c.startService(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSigningVerificationPreservesAppAndMenuIdentities(t *testing.T) {
	for _, kind := range []string{"adhoc", "incomplete", "wrong-team", "team-same-requirement", "team-changed-requirement", "local-cert-reused", "local-cert-missing", "helper-changed"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			incoming := filepath.Join(root, "new", "MemQL.app")
			installed := filepath.Join(root, "old", "MemQL.app")
			helper := filepath.Join(incoming, "Contents", "Library", "LoginItems", "MemQL Menu.app")
			if err := os.MkdirAll(filepath.Join(helper, "Contents", "MacOS"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(helper, "Contents", "MacOS", "MemQLCockpit"), []byte("menu"), 0755); err != nil {
				t.Fatal(err)
			}
			c := &Client{home: root, bundle: installed}
			signed := false
			requirementChecks := 0
			signs := 0
			identity := filepath.Join(root, "Library", "Application Support", "MemQL", "Development", "signing-identity")
			if kind == "local-cert-reused" || kind == "helper-changed" {
				_ = os.MkdirAll(filepath.Dir(identity), 0700)
				_ = os.WriteFile(identity, []byte(strings.Repeat("a", 40)), 0600)
			}
			c.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
				last := args[len(args)-1]
				if name == "/usr/libexec/PlistBuddy" {
					if strings.Contains(last, "MemQL Menu.app") {
						return []byte("com.znasllc.memql-cockpit-menubar\n"), nil
					}
					return []byte("com.znasllc.memql-worker\n"), nil
				}
				joined := strings.Join(args, " ")
				if strings.Contains(joined, "--display") {
					if kind == "incomplete" {
						return nil, nil
					}
					id := "com.znasllc.memql-worker"
					if strings.Contains(last, "MemQL Menu.app") {
						id = "com.znasllc.memql-cockpit-menubar"
					}
					team, authority := "not set", "Signature=adhoc"
					if kind != "adhoc" {
						authority = "Authority=MemQL Certificate"
						if strings.HasPrefix(kind, "team-") || kind == "wrong-team" {
							team = "ABCDEFGHIJ"
						}
					}
					if kind == "wrong-team" && strings.HasPrefix(last, incoming) {
						team = "KLMNOPQRST"
					}
					return []byte("Identifier=" + id + "\nTeamIdentifier=" + team + "\n" + authority + "\ndesignated => identifier \"" + id + "\" and certificate leaf = H\"abc\"\n"), nil
				}
				if strings.Contains(joined, "--force --sign") {
					signs++
					signed = true
					return nil, nil
				}
				if strings.Contains(joined, "-R=") {
					requirementChecks++
					if kind == "team-changed-requirement" || strings.HasPrefix(kind, "local-cert") && !signed || kind == "helper-changed" && strings.Contains(last, "MemQL Menu.app") {
						return nil, errors.New("requirement mismatch")
					}
				}
				return nil, nil
			}
			err := c.verifyBundle(context.Background(), incoming)
			wantErr := kind == "incomplete" || kind == "wrong-team" || kind == "team-changed-requirement" || kind == "local-cert-missing" || kind == "helper-changed"
			if (err != nil) != wantErr {
				t.Fatalf("err=%v wantErr=%v", err, wantErr)
			}
			if kind == "team-same-requirement" && requirementChecks != 2 {
				t.Fatalf("did not verify both requirements: %d", requirementChecks)
			}
			if kind == "local-cert-reused" && signs != 2 {
				t.Fatalf("did not sign helper and app: %d", signs)
			}
		})
	}
}
