package deploy

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// No cluster or credential is reachable: every kubectl operation is a temporary
// JSON fixture, and KUBECONFIG names an empty file. Keys are generated per test.
const gitOpsFakeKubectl = `#!/usr/bin/env python3
import base64,json,os,sys
from pathlib import Path
args=sys.argv[1:]; root=Path(os.environ['GITOPS_FIXTURE'])
with (root/'calls').open('a') as f:f.write(json.dumps(args)+'\n')
def opt(key):return args[args.index(key)+1]
def load(name,default=None):
 p=root/name
 return json.loads(p.read_text()) if p.exists() else default
def save(name,value):(root/name).write_text(json.dumps(value))
def out(value):print(value if isinstance(value,str) else json.dumps(value))
def fail():sys.exit(7)
if args[:2]==['get','namespace'] or args[:2]==['get','crd']:out('present')
elif args[:2]==['get','application']:out('')
elif args[:2]==['get','appproject']:
 if os.environ.get('GITOPS_NO_PROJECT'):fail()
 out('appproject/memql' if opt('-o')=='name' else ' '.join(load('repos',[])))
elif args[:2]==['get','secret']:
 if os.environ.get('GITOPS_READ_ERROR'):fail()
 s=load('secret')
 if s:
  m=s['metadata'];d=s['data'];rows=[m['uid'],m['resourceVersion'],m['labels'].get('argocd.argoproj.io/secret-type','')]
  rows += [d.get(k,'<no value>') for k in ['url','type','githubAppID','githubAppInstallationID','githubAppEnterpriseBaseUrl']]
  out('\n'.join(rows+list(d)))
elif args[:3]==['create','secret','generic']:
 if os.environ.get('GITOPS_INVALID_PREPARATION'):out('{');sys.exit(0)
 data={}
 for a in args:
  if a.startswith('--from-literal='):
   k,v=a.split('=',1)[1].split('=',1);data[k]=base64.b64encode(v.encode()).decode()
  elif a.startswith('--from-file='):
   k,v=a.split('=',1)[1].split('=',1);data[k]=base64.b64encode(Path(v).read_bytes()).decode()
 out({'apiVersion':'v1','kind':'Secret','metadata':{'name':args[3],'namespace':opt('-n')},'data':data})
elif args[0]=='create' and '-f' in args:
 s=json.loads(Path(opt('-f')).read_text()); old=load('secret')
 if os.environ.get('GITOPS_WRITE_ERROR'):
  # The real client may include submitted data in an error. It must remain private.
  print(json.dumps(s),file=sys.stderr);fail()
 if args[0]=='create' and old:fail()
 s['metadata'].update(uid='fixture-uid',resourceVersion=str(int(old['metadata']['resourceVersion'])+1 if old else 1))
 save('secret',s);out('secret configured')
elif args[:2]==['patch','secret']:
 patch=json.loads(Path(opt('--patch-file')).read_text());s=load('secret')
 if not s or os.environ.get('GITOPS_RACE'):fail()
 if os.environ.get('GITOPS_WRITE_ERROR'):print(json.dumps(patch),file=sys.stderr);fail()
 if patch[:2] != [{'op':'test','path':'/metadata/uid','value':s['metadata']['uid']},{'op':'test','path':'/metadata/resourceVersion','value':s['metadata']['resourceVersion']}]:fail()
 if len(patch)!=3 or patch[2]['op']!='replace' or patch[2]['path']!='/data':fail()
 s['data']=patch[2]['value'];s['metadata']['resourceVersion']=str(int(s['metadata']['resourceVersion'])+1);save('secret',s);out('patched')
elif args[:2]==['patch','appproject']:
 patch=json.loads(opt('-p'));save('repos',load('repos',[])+[patch[0]['value']]);out('project configured')
else:fail()
`

type gitOpsFixture struct {
	t        *testing.T
	dir, key string
	script   string
	private  []byte
}

func newGitOpsFixture(t *testing.T) *gitOpsFixture {
	t.Helper()
	for _, name := range []string{"python3", "openssl", "jq"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Fatalf("isolated fixture prerequisite %s: %v", name, err)
		}
	}
	f := &gitOpsFixture{t: t, dir: t.TempDir(), script: aksScript(t, "register-gitops-repo.sh")}
	f.key = filepath.Join(f.dir, "app-key.pem")
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f.private = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	for name, data := range map[string][]byte{"kubectl": []byte(gitOpsFakeKubectl), "app-key.pem": f.private, "empty-kubeconfig": {}, "token": []byte("isolated-fixture-token\n"), "ssh": []byte("isolated-fixture-ssh-key")} {
		mode := os.FileMode(0600)
		if name == "kubectl" {
			mode = 0700
		}
		if err := os.WriteFile(filepath.Join(f.dir, name), data, mode); err != nil {
			t.Fatal(err)
		}
	}
	return f
}
func (f *gitOpsFixture) run(env []string, extra ...string) (map[string]any, int) {
	f.t.Helper()
	args := []string{f.script, "--repoUrl=https://github.com/example/product.git", "--transport=github-app", "--githubAppId=123", "--githubAppInstallationId=456", "--githubAppPrivateKeyFile=" + f.key, "--secretName=product-repo"}
	args = append(args, extra...)
	cmd := exec.Command("bash", args...)
	cmd.Env = append(os.Environ(), "PATH="+f.dir+":"+os.Getenv("PATH"), "GITOPS_FIXTURE="+f.dir, "KUBECONFIG="+filepath.Join(f.dir, "empty-kubeconfig"))
	cmd.Env = append(cmd.Env, env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		code = cmd.ProcessState.ExitCode()
	}
	calls, _ := os.ReadFile(filepath.Join(f.dir, "calls"))
	for _, value := range [][]byte{f.private, []byte(base64.StdEncoding.EncodeToString(f.private)), []byte("isolated-fixture-token"), []byte("isolated-fixture-ssh-key")} {
		if bytes.Contains(stdout.Bytes(), value) || bytes.Contains(stderr.Bytes(), value) || bytes.Contains(calls, value) {
			f.t.Fatal("credential appeared in output or process arguments")
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
		var args []string
		if json.Unmarshal([]byte(line), &args) != nil {
			continue
		}
		for _, arg := range args {
			if strings.HasPrefix(arg, "--from-file=") {
				path := strings.SplitN(strings.TrimPrefix(arg, "--from-file="), "=", 2)[1]
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					f.t.Fatal("temporary credential survived script exit")
				}
			}
		}
	}
	var result map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		f.t.Fatalf("not one JSON envelope, exit %d: %s; %s", code, stdout.String(), stderr.String())
	}
	return result, code
}
func (f *gitOpsFixture) secret() map[string]any {
	f.t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.dir, "secret"))
	if err != nil {
		f.t.Fatal(err)
	}
	var s map[string]any
	if err := json.Unmarshal(raw, &s); err != nil {
		f.t.Fatal(err)
	}
	return s
}
func (f *gitOpsFixture) saveSecret(s map[string]any) {
	f.t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		f.t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(f.dir, "secret"), raw, 0600); err != nil {
		f.t.Fatal(err)
	}
}
func (f *gitOpsFixture) noWrite() {
	f.t.Helper()
	if _, err := os.Stat(filepath.Join(f.dir, "secret")); !os.IsNotExist(err) {
		f.t.Fatal("refused/dry-run request wrote a Secret")
	}
	if _, err := os.Stat(filepath.Join(f.dir, "repos")); !os.IsNotExist(err) {
		f.t.Fatal("refused/dry-run request patched AppProject")
	}
}

func TestRegisterGitOpsAppConnection(t *testing.T) {
	f := newGitOpsFixture(t)
	result, code := f.run(nil, "--dryRun=true")
	if code != 0 || result["changed"] != false {
		t.Fatalf("dry run: %d %v", code, result)
	}
	f.noWrite()
	result, code = f.run(nil)
	if code != 0 || result["changed"] != true {
		t.Fatalf("configure: %d %v", code, result)
	}
	first := f.secret()
	data := first["data"].(map[string]any)
	for k, want := range map[string]string{"url": "https://github.com/example/product.git", "type": "git", "githubAppID": "123", "githubAppInstallationID": "456", "githubAppPrivateKey": string(f.private)} {
		raw, err := base64.StdEncoding.DecodeString(data[k].(string))
		if err != nil || string(raw) != want {
			t.Fatalf("incorrect %s codec", k)
		}
	}
	if len(data) != 5 {
		t.Fatal("unexpected authentication field count")
	}
	metadata := first["metadata"].(map[string]any)
	metadata["annotations"] = map[string]any{"argocd.argoproj.io/sync-options": "Prune=false,Delete=false", "operator.example/managed": "keep"}
	metadata["labels"].(map[string]any)["operator.example/managed"] = "keep"
	f.saveSecret(first)
	if _, code = f.run(nil); code != 0 {
		t.Fatalf("repeat configuration exit %d", code)
	}
	second := f.secret()
	secondMetadata := second["metadata"].(map[string]any)
	if secondMetadata["annotations"].(map[string]any)["argocd.argoproj.io/sync-options"] != "Prune=false,Delete=false" || secondMetadata["labels"].(map[string]any)["operator.example/managed"] != "keep" {
		t.Fatal("connection update removed protection metadata")
	}
	if _, added := secondMetadata["annotations"].(map[string]any)["kubectl.kubernetes.io/last-applied-configuration"]; added {
		t.Fatal("connection copied credentials into last-applied annotation")
	}
	if first["metadata"].(map[string]any)["uid"] != second["metadata"].(map[string]any)["uid"] {
		t.Fatal("repeat created another Secret")
	}
	before, _ := json.Marshal(first["data"])
	after, _ := json.Marshal(second["data"])
	if !bytes.Equal(before, after) {
		t.Fatal("repeat changed credential values")
	}
	repos, _ := os.ReadFile(filepath.Join(f.dir, "repos"))
	if string(repos) != "[\"https://github.com/example/product.git\"]" {
		t.Fatalf("repeated project admission: %s", repos)
	}
}

func TestRegisterGitOpsAppRefusesInvalidInputsBeforeWrites(t *testing.T) {
	cases := map[string][]string{
		"zero app": {"--githubAppId=0"}, "negative installation": {"--githubAppInstallationId=-1"}, "fraction": {"--githubAppId=1.2"}, "overflow": {"--githubAppId=9223372036854775808"},
		"token mix": {"--tokenFile=/unused"}, "ssh mix": {"--sshKeyFile=/unused"}, "username mix": {"--username=git"},
		"http": {"--repoUrl=http://github.com/example/product"}, "userinfo": {"--repoUrl=https://user@github.com/example/product"}, "port": {"--repoUrl=https://github.com:443/example/product"}, "query": {"--repoUrl=https://github.com/example/product?x=1"}, "encoded": {"--repoUrl=https://github.com/example/%70roduct"}, "traversal": {"--repoUrl=https://github.com/example/.."}, "other host": {"--repoUrl=https://git.example.com/example/product"}, "enterprise mismatch": {"--githubAppEnterpriseBaseUrl=https://git.example.com/api/v3"}, "missing key": {"--githubAppPrivateKeyFile=/does-not-exist"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			f := newGitOpsFixture(t)
			_, code := f.run(nil, args...)
			if code != 2 {
				t.Fatalf("exit %d, expected bad parameters", code)
			}
			f.noWrite()
		})
	}
	t.Run("invalid key", func(t *testing.T) {
		f := newGitOpsFixture(t)
		os.WriteFile(f.key, []byte("not a private key"), 0600)
		_, code := f.run(nil)
		if code != 2 {
			t.Fatalf("exit %d", code)
		}
		f.noWrite()
	})
	t.Run("enterprise", func(t *testing.T) {
		f := newGitOpsFixture(t)
		_, code := f.run(nil, "--repoUrl=https://git.example.com/example/product.git", "--githubAppEnterpriseBaseUrl=https://git.example.com/api/v3")
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
		if f.secret()["data"].(map[string]any)["githubAppEnterpriseBaseUrl"] != base64.StdEncoding.EncodeToString([]byte("https://git.example.com/api/v3")) {
			t.Fatal("Enterprise API value lost")
		}
	})
}

func TestRegisterGitOpsAppPreservesConnectionBoundaries(t *testing.T) {
	for _, change := range []string{"url", "type", "githubAppID", "githubAppInstallationID", "githubAppEnterpriseBaseUrl", "password", "sshPrivateKey", "tlsClientCertKey", "label"} {
		t.Run(change, func(t *testing.T) {
			f := newGitOpsFixture(t)
			if _, code := f.run(nil); code != 0 {
				t.Fatal(code)
			}
			s := f.secret()
			if change == "label" {
				s["metadata"].(map[string]any)["labels"].(map[string]any)["argocd.argoproj.io/secret-type"] = "repo-creds"
			} else {
				s["data"].(map[string]any)[change] = base64.StdEncoding.EncodeToString([]byte("different"))
			}
			f.saveSecret(s)
			before, _ := os.ReadFile(filepath.Join(f.dir, "secret"))
			_, code := f.run(nil, "--dryRun=true")
			if code != 3 {
				t.Fatalf("dry run exit %d", code)
			}
			_, code = f.run(nil)
			if code != 3 {
				t.Fatalf("write exit %d", code)
			}
			after, _ := os.ReadFile(filepath.Join(f.dir, "secret"))
			if !bytes.Equal(before, after) {
				t.Fatal("refusal changed existing connection")
			}
		})
	}
	for _, env := range []string{"GITOPS_WRITE_ERROR=1", "GITOPS_READ_ERROR=1", "GITOPS_NO_PROJECT=1"} {
		t.Run(env, func(t *testing.T) {
			f := newGitOpsFixture(t)
			_, code := f.run([]string{env})
			if code == 0 {
				t.Fatal("provider refusal accepted")
			}
			f.noWrite()
		})
	}
	t.Run("concurrent edit", func(t *testing.T) {
		f := newGitOpsFixture(t)
		if _, code := f.run(nil); code != 0 {
			t.Fatal(code)
		}
		before, _ := os.ReadFile(filepath.Join(f.dir, "secret"))
		_, code := f.run([]string{"GITOPS_RACE=1"})
		if code != 5 {
			t.Fatalf("exit %d", code)
		}
		after, _ := os.ReadFile(filepath.Join(f.dir, "secret"))
		if !bytes.Equal(before, after) {
			t.Fatal("concurrent edit was overwritten")
		}
	})
}

// Bash 5.2 can retain the original nonzero status when the already-emitted
// result trap returns. Model that status explicitly on every test host, while
// still executing the actual registration script and its result machinery.
func TestRegisterGitOpsCleanupSurvivesNonzeroResultTrap(t *testing.T) {
	f := newGitOpsFixture(t)
	root := t.TempDir()
	script, err := os.ReadFile(f.script)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := os.ReadFile(filepath.Join(filepath.Dir(f.script), "../lib/capability.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(runtime), "function _cap_on_exit() {") != 1 {
		t.Fatal("result trap fixture no longer names the actual runtime")
	}
	runtime = []byte(strings.Replace(string(runtime), "function _cap_on_exit() {", "function _fixture_cap_on_exit() {", 1) + `
function _cap_on_exit() {
    local original_status=$?
    _fixture_cap_on_exit
    return "$original_status"
}
`)
	for path, data := range map[string][]byte{"deploy/register-gitops-repo.sh": script, "lib/capability.sh": runtime} {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	f.script = filepath.Join(root, "deploy/register-gitops-repo.sh")
	result, code := f.run([]string{"GITOPS_WRITE_ERROR=1"})
	if code != 5 || result["ok"] != false || result["error"].(map[string]any)["code"] != float64(5) {
		t.Fatalf("write refusal changed its result: exit %d, %v", code, result)
	}
	f.noWrite()
}

func TestRegisterGitOpsCleanupPreservesUnexpectedAbort(t *testing.T) {
	f := newGitOpsFixture(t)
	result, code := f.run([]string{"GITOPS_INVALID_PREPARATION=1"})
	if code != 1 || result["ok"] != false || result["error"].(map[string]any)["code"] != float64(1) {
		t.Fatalf("unexpected abort changed its result: exit %d, %v", code, result)
	}
	f.noWrite()
}

func TestRegisterGitOpsExistingTransports(t *testing.T) {
	for _, transport := range []string{"https", "ssh"} {
		t.Run(transport, func(t *testing.T) {
			f := newGitOpsFixture(t)
			// Empty App values are omitted by the public action executor; supply the
			// existing form directly rather than retaining the fixture's App defaults.
			args := []string{aksScript(t, "register-gitops-repo.sh"), "--transport=" + transport, "--secretName=product-repo"}
			if transport == "https" {
				args = append(args, "--repoUrl=https://github.com/example/product.git", "--tokenFile="+filepath.Join(f.dir, "token"))
			} else {
				args = append(args, "--repoUrl=git@github.com:example/product.git", "--sshKeyFile="+filepath.Join(f.dir, "ssh"))
			}
			for range 2 {
				cmd := exec.Command("bash", args...)
				cmd.Env = append(os.Environ(), "PATH="+f.dir+":"+os.Getenv("PATH"), "GITOPS_FIXTURE="+f.dir, "KUBECONFIG="+filepath.Join(f.dir, "empty-kubeconfig"))
				raw, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("existing %s: %v %s", transport, err, raw)
				}
				if strings.Contains(string(raw), "isolated-fixture-") {
					t.Fatal("credential output")
				}
			}
			data := f.secret()["data"].(map[string]any)
			if _, exists := data["githubAppPrivateKey"]; exists {
				t.Fatal("App credential added to existing mode")
			}
		})
	}
}
