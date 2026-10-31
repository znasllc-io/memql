package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// The decisions a pass makes are tested in integrations/pipelinesteps. What is
// tested here is the part a CronJob reads: that the binary routes to it, what its
// flags mean, the one JSON line on stdout, and the exit code.

// A binary built without the routing case would treat this name as an ordinary
// start-up argument and try to boot a whole node, under a ServiceAccount holding
// nothing, every five minutes. Run under both build variants (`go test` and
// `go test -tags identity`), this proves each table has the case.
func TestPipelineDeadlinePrioritySubcommandIsRouted(t *testing.T) {
	handled, code := dispatchSubcommand([]string{"pipeline-deadline-priority", "--help"})
	if !handled {
		t.Fatal("`memql pipeline-deadline-priority` is not routed; it would fall through to the server bootstrap")
	}
	if code != 0 {
		t.Fatalf("`pipeline-deadline-priority --help` exited %d, want 0", code)
	}
}

func TestPipelineDeadlinePriorityRefusesSettingsThatCannotWork(t *testing.T) {
	for _, args := range [][]string{
		{"--window-minutes=5"},
		{"--window-minutes=0"},
		{"--window-minutes=1441"},
		{"--window-minutes=99999999999999"},
		{"--aged-window-minutes=99999999999999"},
		{"--window-minutes=180", "--aged-window-minutes=180"},
		{"--aged-window-minutes=-2"},
		{"--boost-run=6381"},
		{"--queue="},
		{"--namespace="},
		{"--nonsense"},
		{"positional"},
	} {
		var stdout, stderr bytes.Buffer
		code := runPipelineDeadlinePriority(context.Background(), args, &stdout, &stderr, time.Now())
		if code != 2 {
			t.Errorf("%v exited %d, want 2; stderr: %s", args, code, stderr.String())
		}
		if stdout.Len() != 0 {
			t.Errorf("%v wrote to stdout: %s", args, stdout.String())
		}
	}
}

const pipelinePassRun = "79bb3043f59bf98ed9c0898cb74d823f"

// stepJobJSON is a suspended step Job as the API server returns one, written out
// rather than built, because the labels and annotations are the contract.
func stepJobJSON(name, deadline string) string {
	return fmt.Sprintf(`{"metadata":{"name":%q,"resourceVersion":"7",
		"labels":{"kueue.x-k8s.io/queue-name":"memql-background","app.kubernetes.io/managed-by":"memql-workbench",
			"kueue.x-k8s.io/priority-class":"memql-pipeline-check","memql.io/pipelines-run":%q},
		"annotations":{"memql.io/run-deadline":%q,"memql.io/run-ceiling-minutes":"720","memql.io/step-key":"tests.go-tests"}},
		"spec":{"suspend":true}}`, name, pipelinePassRun, deadline)
}

type pipelinePassAPI struct {
	jobs       []string
	listStatus int
	refuse     map[string]bool
	patched    map[string]string
	paths      []string
	bearers    []string
}

func (a *pipelinePassAPI) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.paths = append(a.paths, r.Method+" "+r.URL.Path)
		a.bearers = append(a.bearers, r.Header.Get("Authorization"))
		if r.Method == http.MethodGet {
			if a.listStatus != 0 {
				http.Error(w, `{"message":"jobs is forbidden"}`, a.listStatus)
				return
			}
			fmt.Fprintf(w, `{"metadata":{},"items":[%s]}`, strings.Join(a.jobs, ","))
			return
		}
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if a.refuse[name] {
			http.Error(w, `{"message":"jobs is forbidden"}`, http.StatusForbidden)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if a.patched == nil {
			a.patched = map[string]string{}
		}
		a.patched[name] = string(body)
		fmt.Fprint(w, `{}`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestAPassPrintsOneJSONLineOnStdoutAndExitsZero(t *testing.T) {
	now := time.Date(2026, 10, 29, 14, 30, 0, 0, time.UTC)
	api := &pipelinePassAPI{jobs: []string{stepJobJSON("mp-1", "2026-10-29T15:30:15.5Z")}}
	base := api.start(t)
	var stdout, stderr bytes.Buffer
	code := runPipelineDeadlinePriority(context.Background(), []string{"--api=" + base}, &stdout, &stderr, now)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr.String())
	}
	if lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n"); len(lines) != 1 {
		t.Fatalf("stdout is %d lines, want one JSON line: %q", len(lines), stdout.String())
	}
	var pass struct {
		Promoted []struct{ Job, To string }
	}
	if err := json.Unmarshal(stdout.Bytes(), &pass); err != nil {
		t.Fatalf("stdout is not JSON: %v: %s", err, stdout.String())
	}
	if len(pass.Promoted) != 1 || pass.Promoted[0].Job != "mp-1" || pass.Promoted[0].To != "memql-pipeline-expiring" {
		t.Fatalf("promoted = %+v", pass.Promoted)
	}
	if got := api.paths[0]; got != "GET /apis/batch/v1/namespaces/memql-pipelines/jobs" {
		t.Errorf("listed %s", got)
	}
	if !strings.Contains(api.patched["mp-1"], `"memql-pipeline-expiring"`) {
		t.Errorf("patch = %s", api.patched["mp-1"])
	}
	// --api is an operator's `kubectl proxy`, which adds their own credentials:
	// it stops at a request that already names one, even a blank one.
	for _, bearer := range api.bearers {
		if bearer != "" {
			t.Errorf("sent Authorization %q through a proxy", bearer)
		}
	}
}

func TestPipelineDeadlinePriorityFlagsReachThePass(t *testing.T) {
	now := time.Date(2026, 10, 29, 14, 30, 0, 0, time.UTC)
	for _, c := range []struct {
		name     string
		args     []string
		deadline string // now is 14:30, and the Job's run ceiling is 720 minutes
		want     string // the class patched, or "" for no patch
	}{
		{"derived, a quarter of 720", nil, "2026-10-29T17:30:00Z", "memql-pipeline-expiring"},
		{"a stated expiring window moves the edge", []string{"--window-minutes=60"}, "2026-10-29T17:30:00Z", "memql-pipeline-aged"},
		{"a stated aged window moves the edge", []string{"--aged-window-minutes=200"}, "2026-10-29T18:00:00Z", ""},
		{"zero turns the aged tier off", []string{"--aged-window-minutes=0"}, "2026-10-29T18:00:00Z", ""},
		{"a boost needs no window", []string{"--boost-run=" + pipelinePassRun}, "2026-10-30T02:00:00Z", "memql-pipeline-boosted"},
	} {
		t.Run(c.name, func(t *testing.T) {
			api := &pipelinePassAPI{jobs: []string{stepJobJSON("mp-1", c.deadline)}}
			base := api.start(t)
			var stdout, stderr bytes.Buffer
			if code := runPipelineDeadlinePriority(context.Background(), append([]string{"--api=" + base}, c.args...), &stdout, &stderr, now); code != 0 {
				t.Fatalf("exit %d, stderr: %s", code, stderr.String())
			}
			patch := api.patched["mp-1"]
			if c.want == "" {
				if patch != "" {
					t.Fatalf("patched %s, want nothing", patch)
				}
				return
			}
			if !strings.Contains(patch, `"`+c.want+`"`) {
				t.Fatalf("patch = %q, want class %s", patch, c.want)
			}
		})
	}
}

func TestADryRunPatchesNothingAndSaysSo(t *testing.T) {
	now := time.Date(2026, 10, 29, 14, 30, 0, 0, time.UTC)
	api := &pipelinePassAPI{jobs: []string{stepJobJSON("mp-1", "2026-10-29T15:30:15Z")}}
	base := api.start(t)
	var stdout, stderr bytes.Buffer
	if code := runPipelineDeadlinePriority(context.Background(), []string{"--api=" + base, "--dry-run"}, &stdout, &stderr, now); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr.String())
	}
	if len(api.patched) != 0 {
		t.Fatalf("a dry run patched %v", api.patched)
	}
	if !strings.Contains(stdout.String(), `"dryRun":true`) {
		t.Fatalf("stdout = %s", stdout.String())
	}
}

func TestARefusedPatchFailsThePassAndStillPromotesTheOthers(t *testing.T) {
	now := time.Date(2026, 10, 29, 14, 30, 0, 0, time.UTC)
	api := &pipelinePassAPI{
		jobs:   []string{stepJobJSON("refused", "2026-10-29T15:30:15Z"), stepJobJSON("mp-2", "2026-10-29T15:30:15Z")},
		refuse: map[string]bool{"refused": true},
	}
	base := api.start(t)
	var stdout, stderr bytes.Buffer
	if code := runPipelineDeadlinePriority(context.Background(), []string{"--api=" + base}, &stdout, &stderr, now); code != 1 {
		t.Fatalf("exit %d, want 1; stderr: %s", code, stderr.String())
	}
	if _, promoted := api.patched["mp-2"]; !promoted {
		t.Fatal("one refused Job stopped the others")
	}
	if !strings.Contains(stdout.String(), `"job":"refused"`) {
		t.Fatalf("stdout does not name the refused Job: %s", stdout.String())
	}
}

func TestAPassThatCannotListFailsOnStderr(t *testing.T) {
	api := &pipelinePassAPI{listStatus: http.StatusForbidden}
	base := api.start(t)
	var stdout, stderr bytes.Buffer
	if code := runPipelineDeadlinePriority(context.Background(), []string{"--api=" + base}, &stdout, &stderr, time.Now()); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %s, want nothing: no pass happened", stdout.String())
	}
	if !strings.Contains(stderr.String(), "forbidden") {
		t.Fatalf("stderr does not say why: %s", stderr.String())
	}
}

func TestOutsideAClusterWithNoAPIThePassSaysSo(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	var stdout, stderr bytes.Buffer
	if code := runPipelineDeadlinePriority(context.Background(), nil, &stdout, &stderr, time.Now()); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "no_cluster_api") {
		t.Fatalf("stderr = %s", stderr.String())
	}
}

// The CronJob and the subcommand are one contract across a YAML file: a renamed
// subcommand would boot a whole node every five minutes, and a renamed flag would
// fail every pass. So the component's own command line is what runs here.
func TestTheComponentsCronJobRunsARoutedSubcommandItsFlagsAccept(t *testing.T) {
	raw, err := os.ReadFile("deploy/k8s/components/pipelines-capacity/deadline-priority.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var container struct {
		Name    string
		Command []string
		Args    []string
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	for {
		var object struct {
			Kind string
			Spec struct {
				JobTemplate struct {
					Spec struct {
						Template struct {
							Spec struct {
								Containers []struct {
									Name    string
									Command []string
									Args    []string
								}
							}
						}
					}
				} `yaml:"jobTemplate"`
			}
		}
		if err := decoder.Decode(&object); err != nil {
			t.Fatalf("no CronJob in the component: %v", err)
		}
		if object.Kind == "CronJob" {
			containers := object.Spec.JobTemplate.Spec.Template.Spec.Containers
			if len(containers) != 1 {
				t.Fatalf("containers = %+v", containers)
			}
			container.Name, container.Command, container.Args = containers[0].Name, containers[0].Command, containers[0].Args
			break
		}
	}
	if len(container.Command) != 2 || container.Command[0] != "/app/memql" {
		t.Fatalf("command = %v, want /app/memql and a subcommand", container.Command)
	}
	if handled, code := dispatchSubcommand([]string{container.Command[1], "--help"}); !handled || code != 0 {
		t.Fatalf("%s is not routed: handled=%v code=%d", container.Command[1], handled, code)
	}
	api := &pipelinePassAPI{}
	base := api.start(t)
	var stdout, stderr bytes.Buffer
	if code := runPipelineDeadlinePriority(context.Background(), append(append([]string{}, container.Args...), "--api="+base), &stdout, &stderr, time.Now()); code != 0 {
		t.Fatalf("the CronJob's arguments %v exit %d: %s", container.Args, code, stderr.String())
	}
}
