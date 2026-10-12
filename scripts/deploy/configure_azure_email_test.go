package deploy

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func emailInstallArgs() []string {
	return []string{"--subscriptionId=12345678-1234-1234-1234-123456789012", "--resourceGroup=cluster-rg", "--clusterName=example", "--emailResourceGroup=email-rg", "--emailIdentityName=email-identity", "--emailService=example-email", "--communicationService=example-delivery", "--emailLocation=westus2", "--emailDataLocation=United States"}
}

// Fake both external CLIs. Every effect is a temporary JSON fixture; no test
// depends on a signed-in Azure account or the operator's Kubernetes context.
const emailInstallFake = `#!/usr/bin/env python3
import sys,os,json
from pathlib import Path
args=sys.argv[1:]; root=Path(os.environ['EMAIL_TEST_STATE']); tool=Path(sys.argv[0]).name
with (root/'calls').open('a') as f:f.write(json.dumps([tool]+args)+'\n')
def opt(k,d=''):
 return args[args.index(k)+1] if k in args else d
def load(n,d=None):
 p=root/n
 return json.loads(p.read_text()) if p.exists() else d
def save(n,v): (root/n).write_text(json.dumps(v));return v
def out(v):
 print(v if isinstance(v,str) else json.dumps(v))
def fail():sys.exit(9)
issuer=os.environ.get('EMAIL_TEST_ISSUER','https://issuer.example/cluster-one/')
if tool=='kubectl':
 if 'get' in args:out(load('config',''))
 elif 'create' in args:
  save('config',json.load(sys.stdin));out('created')
 else:fail()
 sys.exit()
key=' '.join(args[:2])
if key=='account show':out('11111111-1111-1111-1111-111111111111')
elif key=='aks show':out({'id':'/subscriptions/'+opt('--subscription')+'/resourceGroups/'+opt('--resource-group')+'/providers/Microsoft.ContainerService/managedClusters/'+opt('--name'),'oidcIssuerProfile':{'issuerUrl':issuer}})
elif key=='aks get-credentials':Path(opt('--file')).write_text('apiVersion: v1\n')
elif key=='group exists':out('true' if load('group') else 'false')
elif key=='group show':out(load('group'))
elif key=='group create':save('group',{'location':opt('--location'),'tags':{'memql-email-issuer':issuer}})
elif key=='provider show':
 if os.environ.get('EMAIL_TEST_PROVIDER_ERROR'):fail()
 out(load('provider-'+opt('--namespace'),'NotRegistered'))
elif key=='provider register':save('provider-'+opt('--namespace'),'Registered')
elif key=='identity list':out([load('identity')] if load('identity') else [])
elif key=='identity create':out(save('identity',{'name':opt('--name'),'clientId':'22222222-2222-2222-2222-222222222222','principalId':'33333333-3333-3333-3333-333333333333','tags':{'memql-email-issuer':issuer}}))
elif key=='identity federated-credential':
 values=load('federations',[])
 if args[2]=='list':out(values)
 elif args[2]=='create':values.append({'issuer':opt('--issuer'),'subject':opt('--subject'),'audiences':[opt('--audiences')]});save('federations',values)
 else:fail()
elif key=='role assignment':
 if args[2]=='list':out(load('roles',[]))
 elif args[2]=='create':save('roles',[{'principalId':opt('--assignee-object-id'),'scope':opt('--scope'),'roleDefinitionId':'/roles/'+opt('--role')}])
 else:fail()
else:fail()
`

func TestConfigureAzureEmailInstallation(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required for isolated CLI fixture")
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq required")
	}
	fixture := t.TempDir()
	for _, name := range []string{"az", "kubectl"} {
		if err := os.WriteFile(filepath.Join(fixture, name), []byte(emailInstallFake), 0755); err != nil {
			t.Fatal(err)
		}
	}
	run := func(extraEnv []string, extraArgs ...string) (map[string]any, int, string) {
		t.Helper()
		args := append([]string{aksScript(t, "configure-azure-email.sh")}, emailInstallArgs()...)
		args = append(args, extraArgs...)
		cmd := exec.Command("bash", args...)
		cmd.Env = append(os.Environ(), "PATH="+fixture+":"+os.Getenv("PATH"), "EMAIL_TEST_STATE="+fixture)
		cmd.Env = append(cmd.Env, extraEnv...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		code := 0
		if err != nil {
			code = cmd.ProcessState.ExitCode()
		}
		var envelope map[string]any
		if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
			t.Fatalf("not one result envelope: %s; %s", stdout.String(), stderr.String())
		}
		return envelope, code, stderr.String()
	}
	result, code, logs := run(nil, "--dryRun=true")
	if code != 0 || result["changed"] != false {
		t.Fatalf("dry run: %v %d %s", result, code, logs)
	}
	if _, err := os.Stat(filepath.Join(fixture, "calls")); !os.IsNotExist(err) {
		t.Fatal("dry run contacted an external CLI")
	}
	result, code, logs = run(nil)
	if code != 0 || result["changed"] != true {
		t.Fatalf("first install: %v %d %s", result, code, logs)
	}
	result, code, logs = run(nil)
	if code != 0 || result["changed"] != false {
		t.Fatalf("repeat install: %v %d %s", result, code, logs)
	}
	raw, _ := os.ReadFile(filepath.Join(fixture, "calls"))
	calls := string(raw)
	for _, forbidden := range []string{"get-access-token", "client-secret", "--admin", "account\", \"set"} {
		if strings.Contains(calls, forbidden) {
			t.Fatalf("installation copied or widened authorization: %s", forbidden)
		}
	}
	for _, sa := range []string{"memql-engine", "memql-deploy", "memql-engine-workbench"} {
		if !strings.Contains(calls, "system:serviceaccount:memql:"+sa) {
			t.Fatalf("missing federation for %s", sa)
		}
	}
	if !strings.Contains(calls, "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/email-rg") {
		t.Fatal("missing group-scoped role")
	}
	config, _ := os.ReadFile(filepath.Join(fixture, "config"))
	for _, secret := range []string{"accessToken", "refreshToken", "clientSecret", "private"} {
		if strings.Contains(string(config), secret) {
			t.Fatal("credential copied into public configuration")
		}
	}
	_, code, _ = run([]string{"EMAIL_TEST_ISSUER=https://issuer.example/cluster-two/"})
	if code != 3 {
		t.Fatalf("sibling installation reused identity: exit %d", code)
	}
	_, code, _ = run(nil, "--emailResourceGroup=cluster-rg")
	if code != 2 {
		t.Fatalf("cluster resource group accepted: exit %d", code)
	}
	// An extra federation would give another cluster this identity's rights.
	if err := os.WriteFile(filepath.Join(fixture, "federations"), []byte(`[{"issuer":"https://other/","subject":"system:serviceaccount:memql:memql-engine","audiences":["api://AzureADTokenExchange"]}]`), 0600); err != nil {
		t.Fatal(err)
	}
	_, code, _ = run(nil)
	if code != 3 {
		t.Fatalf("extra federation accepted: exit %d", code)
	}
}
