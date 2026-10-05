package pipelinesteps

import (
	"strings"
	"testing"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

// Kubernetes treats a container's env values, its command and args, and an
// exec probe's command as templates (the API reference for Container.env.value
// and Container.command): $(VAR) becomes the value of a variable defined
// earlier, $$ becomes $, and a reference to a variable that is not defined is
// left as written. A value from a secretKeyRef is not a template. kubeExpand is
// that rule, written from the documentation, standing in for the kubelet;
// TestKubeExpandIsWhatTheClusterDid holds it to what a k3s v1.32.13 cluster
// was measured to do.

// kubeExpand applies the template rule to s. defined holds the variables a
// $(NAME) reference resolves to.
func kubeExpand(s string, defined map[string]string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '$' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		switch next := s[i+1]; next {
		case '$':
			b.WriteByte('$')
			i++
		case '(':
			end := strings.IndexByte(s[i+2:], ')')
			if end < 0 {
				// No closing parenthesis: nothing to expand.
				b.WriteString("$(")
				i++
				continue
			}
			name := s[i+2 : i+2+end]
			if v, ok := defined[name]; ok {
				b.WriteString(v)
			} else {
				b.WriteString("$(" + name + ")")
			}
			i += 2 + end
		default:
			b.WriteByte('$')
			b.WriteByte(next)
			i++
		}
	}
	return b.String()
}

// tplServiceVars are variables the kubelet defines in every container whatever
// its spec says (the cluster's own API Service), so a reference to one always
// resolves.
func tplServiceVars() map[string]string {
	return map[string]string{"KUBERNETES_SERVICE_HOST": "10.43.0.1", "KUBERNETES_SERVICE_PORT": "443"}
}

// tplContainerEnv is what a container's environment holds once the kubelet has
// built it: each literal value expanded against the variables before it, each
// secretKeyRef read from the Secret as stored.
func tplContainerEnv(t *testing.T, c Container, secret Secret) map[string]string {
	t.Helper()
	defined := tplServiceVars()
	for _, e := range c.Env {
		switch {
		case e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil:
			if v, ok := secret.Data[e.ValueFrom.SecretKeyRef.Key]; ok {
				defined[e.Name] = string(v)
			}
		default:
			defined[e.Name] = kubeExpand(e.Value, defined)
		}
	}
	return defined
}

// tplStaticEnv is what an exec probe's command is expanded against: the
// container's literal values as written, unexpanded.
func tplStaticEnv(c Container) map[string]string {
	m := map[string]string{}
	for _, e := range c.Env {
		if e.ValueFrom == nil {
			m[e.Name] = e.Value
		}
	}
	return m
}

// TestKubeExpandIsWhatTheClusterDid: the reference rule gives exactly what the
// containers of a k3s v1.32.13 cluster received for these declared values
// (2026-10-04, BuildJob's own Jobs before ruling R25).
func TestKubeExpandIsWhatTheClusterDid(t *testing.T) {
	for _, c := range []struct {
		name, declared string
		defined        map[string]string
		received       string
	}{
		{
			name:     "a step command, MEMQL_RUN_ID defined before it",
			declared: `kill -9 $$; echo $(MEMQL_RUN_ID) $$(MEMQL_RUN_ID) $(echo hi) 5$`,
			defined:  map[string]string{"MEMQL_RUN_ID": "run-7f3a"},
			received: `kill -9 $; echo run-7f3a $(MEMQL_RUN_ID) $(echo hi) 5$`,
		},
		{
			name:     "a step command printing a quoted text",
			declared: `echo 'command: $(MEMQL_RUN_ID) a$$b $$(X) end$'; printf 'event: %s\n' "$MEMQL_EVENT"`,
			defined:  map[string]string{"MEMQL_RUN_ID": "t4-r25-before"},
			received: `echo 'command: t4-r25-before a$b $(X) end$'; printf 'event: %s\n' "$MEMQL_EVENT"`,
		},
		{
			// MEMQL_EVENT sorts before MEMQL_RUN_ID, so nothing is defined yet.
			name:     "a contract value, MEMQL_RUN_ID not yet defined",
			declared: `{"title":"price $5 a$$b $(MEMQL_RUN_ID) $$(X) end$"}`,
			defined:  map[string]string{},
			received: `{"title":"price $5 a$b $(MEMQL_RUN_ID) $(X) end$"}`,
		},
		{
			name:     "a service's ready check, DB_NOTE in its env",
			declared: `echo 'probe saw: a$$b $(DB_NOTE) end$' > /tmp/probe-saw; redis-cli ping`,
			defined:  map[string]string{"DB_NOTE": "hello"},
			received: `echo 'probe saw: a$b hello end$' > /tmp/probe-saw; redis-cli ping`,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := kubeExpand(c.declared, c.defined); got != c.received {
				t.Errorf("kubeExpand(%q)\n  = %q\nthe cluster gave %q", c.declared, got, c.received)
			}
		})
	}
}

// TestPlainValuesReachTheContainerAsWritten (ruling R25): every literal value
// BuildJob writes goes out with each $ doubled, so the template rule hands the
// container exactly what was written, whatever is defined around it.
func TestPlainValuesReachTheContainerAsWritten(t *testing.T) {
	defined := map[string]string{"MEMQL_RUN_ID": "run-7f3a", "X": "x-value", "KUBERNETES_SERVICE_HOST": "10.43.0.1"}
	for _, c := range []struct {
		value, wire string
	}{
		{`kill -9 $$`, `kill -9 $$$$`},
		{`echo $(MEMQL_RUN_ID)`, `echo $$(MEMQL_RUN_ID)`},
		{`a$$b`, `a$$$$b`},
		{`end$`, `end$$`},
		{`$$(X)`, `$$$$(X)`},
		{`$(echo hi) $(KUBERNETES_SERVICE_HOST)`, `$$(echo hi) $$(KUBERNETES_SERVICE_HOST)`},
		{`price $5`, `price $$5`},
		{`$`, `$$`},
		{`$(`, `$$(`},
		{`$$$`, `$$$$$$`},
		{`no dollar at all`, `no dollar at all`},
	} {
		t.Run(c.value, func(t *testing.T) {
			e := plainVar("V", c.value)
			if e.Value != c.wire {
				t.Errorf("written as %q, want %q", e.Value, c.wire)
			}
			if got := kubeExpand(e.Value, defined); got != c.value {
				t.Errorf("the container would see %q, want %q", got, c.value)
			}
		})
	}
}

// TestBuildJobEnvReachesEveryContainerAsWritten (ruling R25): text from the
// manifest and the event reaches the step's command, the contract values, a
// service's values, its ready check and the clone's URL. Each is planted with
// every shape the template rule rewrites, and each container, built as the
// kubelet builds it, holds exactly what was planted.
func TestBuildJobEnvReachesEveryContainerAsWritten(t *testing.T) {
	const hostile = `kill -9 $$; echo $(MEMQL_RUN_ID) a$$b $$(X) $(A_FIRST) $(KUBERNETES_SERVICE_HOST) end$`
	cfg := testConfig()
	run := testRun()
	run.Command = hostile
	run.Env["MEMQL_EVENT"] = `{"title":"price $5 ` + hostile + `"}` // sorts before MEMQL_RUN_ID
	run.Env["MEMQL_VERSION"] = hostile                              // sorts after it
	run.Services = map[string]pl.Service{"db": {
		Image: "postgres:16",
		Env:   map[string]string{"A_FIRST": "first", "DB_NOTE": hostile},
		Ready: `pg_isready -U memql && echo 'ready: ` + hostile + `'`,
	}}
	run.Repository.CloneURL = "https://github.com/acme/wid$$get-$(SHA)-$.git"
	job := mustBuild(t, cfg, run)
	secret := BuildSecret(cfg, run, testJobName, plantedClone)

	containers := map[string]Container{}
	for _, c := range allContainers(job) {
		containers[c.Name] = c
	}
	for _, check := range []struct {
		container string
		want      map[string]string
	}{
		{ContainerStep, func() map[string]string {
			want := map[string]string{stepCommandVar: run.Command}
			for k, v := range run.Env {
				want[k] = v
			}
			return want
		}()},
		{ServicePrefix + "db", run.Services["db"].Env},
		{ContainerClone, map[string]string{"CLONE_URL": run.Repository.CloneURL, "SHA": run.SHA}},
	} {
		c, ok := containers[check.container]
		if !ok {
			t.Fatalf("the Job has no %s container", check.container)
		}
		got := tplContainerEnv(t, c, secret)
		for name, want := range check.want {
			if got[name] != want {
				t.Errorf("%s sees %s = %q\nwant              %q", check.container, name, got[name], want)
			}
		}
	}

	svc := containers[ServicePrefix+"db"]
	if svc.StartupProbe == nil || svc.StartupProbe.Exec == nil {
		t.Fatal("the service has no ready check")
	}
	static := tplStaticEnv(svc)
	var ran []string
	for _, arg := range svc.StartupProbe.Exec.Command {
		ran = append(ran, kubeExpand(arg, static))
	}
	if want := []string{"/bin/sh", "-c", run.Services["db"].Ready}; strings.Join(ran, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("the ready check runs %q\nwant              %q", ran, want)
	}
}

// TestBuildJobScriptsReachTheContainerAsWritten: the platform's own scripts are
// the containers' commands, and commands are templates too. None may hold a $$
// or a $(NAME) that names a variable its container defines, or the kubelet
// would rewrite it before the shell sees it.
func TestBuildJobScriptsReachTheContainerAsWritten(t *testing.T) {
	cfg := testConfig()
	run := testRun()
	job := mustBuild(t, cfg, run)
	secret := BuildSecret(cfg, run, testJobName, plantedClone)
	for _, c := range allContainers(job) {
		env := tplContainerEnv(t, c, secret)
		for i, arg := range append(append([]string{}, c.Command...), c.Args...) {
			if got := kubeExpand(arg, env); got != arg {
				t.Errorf("%s: command[%d] would reach the shell rewritten:\n%s", c.Name, i, got)
			}
		}
	}
}

// TestSecretValuesAreNotEscaped: a secretKeyRef's value is not a template, so
// a secret goes into the Secret exactly as resolved, and the step reads it by
// reference -- doubling its $ would hand the step a different secret.
func TestSecretValuesAreNotEscaped(t *testing.T) {
	cfg := testConfig()
	run := testRun()
	const value = `p$$w$(MEMQL_RUN_ID)-` + "npm-zzzzzzzz" + `$`
	run.Secrets = map[string]string{"NPM_TOKEN": value}
	secret := BuildSecret(cfg, run, testJobName, "")
	if got := string(secret.Data["NPM_TOKEN"]); got != value {
		t.Errorf("the Secret holds %q, want %q as resolved", got, value)
	}
	step := stepOf(t, mustBuild(t, cfg, run))
	e, ok := envNamed(step, "NPM_TOKEN")
	if !ok || e.Value != "" || e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("the step's NPM_TOKEN is %+v, want a secretKeyRef and no literal value", e)
	}
	if got := tplContainerEnv(t, step, secret)["NPM_TOKEN"]; got != value {
		t.Errorf("the step would see %q, want %q", got, value)
	}
}
