package pipelinesteps

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// isolation_probe_test.go -- the isolation probe as the cluster receives it
// (Task 6b, rulings R12, R42-R44): its Indexed Job and its Secret, built by
// pure functions, and its two scripts, run here for real against local
// endpoints that answer as the probe's network would -- accepting, refusing,
// or never answering.

func TestBuildIsolationProbe(t *testing.T) {
	cfg := testConfig()
	name := IsolationProbeName(rtNode)
	job := BuildIsolationProbe(cfg, name)
	doc := wire(t, job)

	t.Run("one Indexed Job of three pods, so it takes one slot under the ceiling (R43)", func(t *testing.T) {
		wantField(t, doc, "Indexed", "spec", "completionMode")
		wantField(t, doc, 3.0, "spec", "completions")
		wantField(t, doc, 3.0, "spec", "parallelism")
		// Index 1 (the connector) ends failed with its verdict; with a
		// per-index limit of zero that ends neither index 0 (the listener)
		// nor the Job before the runner has read it.
		wantField(t, doc, 0.0, "spec", "backoffLimitPerIndex")
		if v, ok := field(doc, "spec", "backoffLimit"); ok {
			t.Errorf("spec.backoffLimit = %v: a Job-wide limit of 0 would end the listener the moment the connector fails", v)
		}
	})

	t.Run("named and labelled as the runner's own, never as a step's", func(t *testing.T) {
		wantField(t, doc, name, "metadata", "name")
		wantField(t, doc, cfg.Namespace, "metadata", "namespace")
		for _, labels := range []map[string]string{job.Metadata.Labels, job.Spec.Template.Metadata.Labels} {
			want := map[string]string{LabelManagedBy: ManagedBy, LabelProbe: ProbeIsolation}
			if !reflect.DeepEqual(labels, want) {
				t.Errorf("labels = %v, want %v: no run label, so no cancel of a run reaches the probe", labels, want)
			}
		}
	})

	t.Run("it ends on its own: a deadline past both of the runner's bounds, and a short TTL", func(t *testing.T) {
		ads := job.Spec.ActiveDeadlineSeconds
		if ads == nil || *ads < 90+60 || *ads > 600 {
			t.Errorf("activeDeadlineSeconds = %v, want past the runner's 90 s and 60 s bounds and within ten minutes", ads)
		}
		// A probe a crashed runner leaves holds a slot under the ceiling
		// until it is collected; nothing ever reads it afterwards.
		ttl := job.Spec.TTLSecondsAfterFinished
		if ttl == nil || *ttl <= 0 || *ttl > 300 {
			t.Errorf("ttlSecondsAfterFinished = %v, want a positive number of seconds, at most five minutes", ttl)
		}
	})

	pod := job.Spec.Template.Spec
	t.Run("no credential of any kind: the step identity, no token, no Service links", func(t *testing.T) {
		if pod.ServiceAccountName != cfg.StepServiceAccount {
			t.Errorf("serviceAccountName = %q, want the step identity %q", pod.ServiceAccountName, cfg.StepServiceAccount)
		}
		if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
			t.Error("the probe pod mounts a ServiceAccount token")
		}
		if pod.EnableServiceLinks == nil || *pod.EnableServiceLinks {
			t.Error("the probe pod gets Service links")
		}
		if pod.RestartPolicy != "Never" {
			t.Errorf("restartPolicy = %q, want Never", pod.RestartPolicy)
		}
	})

	t.Run("the step pod's security context, and requests a crowded node can place", func(t *testing.T) {
		if pod.SecurityContext == nil || pod.SecurityContext.SeccompProfile == nil || pod.SecurityContext.SeccompProfile.Type != "RuntimeDefault" {
			t.Errorf("pod securityContext = %+v, want seccomp RuntimeDefault", pod.SecurityContext)
		}
		containers := append(append([]Container{}, pod.InitContainers...), pod.Containers...)
		if len(containers) != 2 {
			t.Fatalf("%d containers, want the listener and the connector", len(containers))
		}
		for _, c := range containers {
			if c.Image != cfg.CloneImage {
				t.Errorf("%s runs %q, want the clone image %q", c.Name, c.Image, cfg.CloneImage)
			}
			if c.SecurityContext == nil || c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation {
				t.Errorf("%s may gain privileges", c.Name)
			}
			if c.SecurityContext != nil && c.SecurityContext.RunAsUser != nil {
				t.Errorf("%s pins uid %d; the probe needs no uid of its own", c.Name, *c.SecurityContext.RunAsUser)
			}
			// Every resource the LimitRange defaults, the probe states for
			// itself: its defaults are a step's, and the probe's four
			// containers would ask a whole CPU, 2 GiB and 4 GiB of the node's
			// disk -- more than a step, on a node a step must fit beside.
			for _, res := range []string{"cpu", "memory", "ephemeral-storage"} {
				if c.Resources == nil || c.Resources.Requests[res] == "" || c.Resources.Limits[res] == "" {
					t.Errorf("%s resources = %+v, want an explicit %s request and limit: the LimitRange's defaults are a step's", c.Name, c.Resources, res)
				}
			}
		}
	})

	t.Run("the listener is a native sidecar that waits for nothing", func(t *testing.T) {
		if len(pod.InitContainers) != 1 {
			t.Fatalf("%d init containers, want the listener alone", len(pod.InitContainers))
		}
		l := pod.InitContainers[0]
		if l.Name != ContainerProbeListener || l.RestartPolicy == nil || *l.RestartPolicy != "Always" {
			t.Errorf("init container %q restartPolicy %v, want %q as a native sidecar (Always)", l.Name, l.RestartPolicy, ContainerProbeListener)
		}
		if !reflect.DeepEqual(l.Command, []string{"/bin/sh", "-c", probeListenerScript}) || probeListenerScript == "" {
			t.Errorf("listener command = %q, want /bin/sh -c probeListenerScript", l.Command)
		}
		// Ready means accepting: the kubelet's own connection, which no
		// NetworkPolicy governs (measured on k3s v1.35). One failed probe is
		// enough to say otherwise, so a listener that stopped accepting reads
		// not ready within a second, not three (fix round 1).
		if l.ReadinessProbe == nil || l.ReadinessProbe.TCPSocket == nil || l.ReadinessProbe.TCPSocket.Port != 8080 || l.ReadinessProbe.Exec != nil ||
			l.ReadinessProbe.PeriodSeconds != 1 || l.ReadinessProbe.FailureThreshold != 1 {
			t.Errorf("listener readiness = %+v, want a TCP probe of port 8080 every second, failing at the first miss", l.ReadinessProbe)
		}
		if len(l.Env) != 0 {
			t.Errorf("listener env = %+v, want none: it must start, and be ready, before the probe Secret exists", l.Env)
		}
	})

	t.Run("the connector waits for the probe Secret, and knows its own Job", func(t *testing.T) {
		if len(pod.Containers) != 1 {
			t.Fatalf("%d main containers, want the connector alone", len(pod.Containers))
		}
		c := pod.Containers[0]
		if c.Name != ContainerProbeConnector || !reflect.DeepEqual(c.Command, []string{"/bin/bash", "-c", probeConnectorScript}) || probeConnectorScript == "" {
			t.Errorf("main container %q runs %q, want %q running /bin/bash -c probeConnectorScript", c.Name, c.Command, ContainerProbeConnector)
		}
		target := IsolationTargetName(name)
		for _, w := range []struct{ env, key string }{{probeTargetVar, probeTargetKey}, {probeJobUIDVar, probeJobUIDKey}} {
			e, ok := envNamed(c, w.env)
			if !ok || e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
				t.Errorf("%s = %+v, want a reference to the probe Secret", w.env, e)
				continue
			}
			ref := e.ValueFrom.SecretKeyRef
			if ref.Name != target || ref.Key != w.key || ref.Optional != nil {
				t.Errorf("%s refers to %+v, want key %q of %q, NOT optional: the container waits until the Secret exists", w.env, ref, w.key, target)
			}
		}
		self, ok := envNamed(c, probeSelfUIDVar)
		if !ok || self.ValueFrom == nil || self.ValueFrom.FieldRef == nil ||
			self.ValueFrom.FieldRef.FieldPath != "metadata.labels['batch.kubernetes.io/controller-uid']" {
			t.Errorf("%s = %+v, want the pod's own controller-uid label", probeSelfUIDVar, self)
		}
		if c.TerminationMessagePolicy != failureFromLogs {
			t.Errorf("terminationMessagePolicy = %q, want %q: the runner reads the connector's summary from it", c.TerminationMessagePolicy, failureFromLogs)
		}
	})
}

// TestIsolationProbeScriptsReachTheContainerAsWritten: the probe's commands
// are templates too, and the kubelet would rewrite a $$ or a $(NAME) naming a
// variable the container defines before the shell saw it.
func TestIsolationProbeScriptsReachTheContainerAsWritten(t *testing.T) {
	job := BuildIsolationProbe(testConfig(), IsolationProbeName(rtNode))
	secret := Secret{Data: map[string][]byte{probeTargetKey: []byte("10.42.1.3"), probeJobUIDKey: []byte("uid-7")}}
	containers := append(append([]Container{}, job.Spec.Template.Spec.InitContainers...), job.Spec.Template.Spec.Containers...)
	if len(containers) == 0 {
		t.Fatal("the probe has no containers")
	}
	for _, c := range containers {
		env := tplContainerEnv(t, c, secret)
		for i, arg := range c.Command {
			if got := kubeExpand(arg, env); got != arg {
				t.Errorf("%s: command[%d] would reach the shell rewritten:\n%s", c.Name, i, got)
			}
		}
	}
}

func TestBuildIsolationTarget(t *testing.T) {
	cfg := testConfig()
	probe := BuildIsolationProbe(cfg, IsolationProbeName(rtNode))
	probe.Metadata.UID = "uid-probe-7"

	s, err := BuildIsolationTarget(cfg, probe, "10.42.1.3")
	if err != nil {
		t.Fatalf("BuildIsolationTarget: %v", err)
	}
	if s.Metadata.Name != IsolationTargetName(probe.Metadata.Name) || s.Metadata.Namespace != cfg.Namespace || s.Type != "Opaque" {
		t.Errorf("secret %s/%s type %q, want %s/%s Opaque", s.Metadata.Namespace, s.Metadata.Name, s.Type, cfg.Namespace, IsolationTargetName(probe.Metadata.Name))
	}
	if want := map[string]string{LabelManagedBy: ManagedBy, LabelProbe: ProbeIsolation}; !reflect.DeepEqual(s.Metadata.Labels, want) {
		t.Errorf("labels = %v, want %v", s.Metadata.Labels, want)
	}
	// Owned by the probe Job from its first moment: collected with it, and
	// never a Secret the orphan sweep judges, which judges the ownerless.
	want := []OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: probe.Metadata.Name, UID: "uid-probe-7", Controller: ptrTo(false), BlockOwnerDeletion: ptrTo(false)}}
	if !reflect.DeepEqual(s.Metadata.OwnerReferences, want) {
		t.Errorf("ownerReferences = %+v, want %+v", s.Metadata.OwnerReferences, want)
	}
	if got := map[string]string{probeTargetKey: string(s.Data[probeTargetKey]), probeJobUIDKey: string(s.Data[probeJobUIDKey])}; len(s.Data) != 2 ||
		got[probeTargetKey] != "10.42.1.3" || got[probeJobUIDKey] != "uid-probe-7" {
		t.Errorf("data = %q, want the listener's address and the probe Job's uid, nothing else", s.Data)
	}

	t.Run("refused without the Job's uid or the listener's address", func(t *testing.T) {
		noUID := probe
		noUID.Metadata.UID = ""
		for _, c := range []struct {
			name string
			job  Job
			ip   string
		}{{"no uid", noUID, "10.42.1.3"}, {"no address", probe, " "}} {
			if s, err := BuildIsolationTarget(cfg, c.job, c.ip); err == nil || !reflect.DeepEqual(s, Secret{}) {
				t.Errorf("%s: = %+v, %v; want an error and no Secret", c.name, s, err)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// The scripts, run for real
// ---------------------------------------------------------------------------

// isoListen is a local endpoint that accepts and closes every connection, and
// counts them; stop closes it, so later attempts are refused. Its address is
// 127.0.0.1:port.
type isoListen struct {
	ln    net.Listener
	mu    sync.Mutex
	times []time.Time
}

func isoAccepting(t *testing.T) *isoListen {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	l := &isoListen{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			l.mu.Lock()
			l.times = append(l.times, time.Now())
			l.mu.Unlock()
			_ = c.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return l
}

func (l *isoListen) port() int { return l.ln.Addr().(*net.TCPAddr).Port }

func (l *isoListen) accepted() []time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]time.Time(nil), l.times...)
}

// isoRefusing is a port nothing listens on: an attempt is refused. The port
// stays bound to a socket that never listens, for the test's life, rather
// than being closed and handed back. A port handed back can be given to
// another case's listener -- the cases run in parallel -- and the connection
// meant to be refused then reaches that listener: "DNS answered the first
// attempt only" closes its listener on its first accept, so such a stray
// read as DNS refused on every attempt (seen once under a full -race run). A
// bound socket that never listens still answers a connection with a reset,
// and without SO_REUSEADDR no other socket can take its port.
func isoRefusing(t *testing.T) int {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socket: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	sa, err := syscall.Getsockname(fd)
	if err != nil {
		t.Fatalf("getsockname: %v", err)
	}
	return sa.(*syscall.SockaddrInet4).Port
}

// isoFreePort is a port free for a process the test starts to listen on: the
// listener script binds it itself, so it is handed back, unlike isoRefusing's.
func isoFreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// isoSilent is a port that never answers: a listener whose accept queue is
// full -- a backlog of zero, filled by one connection nobody accepts -- drops
// every SYN after it, so an attempt times out (measured, Linux).
func isoSilent(t *testing.T) int {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("a full accept queue drops a SYN -- the silence this fixture stands for -- on Linux; another kernel may answer it")
	}
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socket: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := syscall.Listen(fd, 0); err != nil {
		t.Fatalf("listen: %v", err)
	}
	sa, err := syscall.Getsockname(fd)
	if err != nil {
		t.Fatalf("getsockname: %v", err)
	}
	port := sa.(*syscall.SockaddrInet4).Port
	filler, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		t.Fatalf("filling the accept queue: %v", err)
	}
	t.Cleanup(func() { _ = filler.Close() })
	return port
}

// isoConnector runs the connector's script for real under bash, as index 1 of
// the probe, with the five tunables at its head pointed at local endpoints --
// the one edit the harness makes, and it refuses to run if a tunable is not
// there to edit: the nameserver read from a resolv.conf of the test's, the
// DNS and listener ports, the settle (seconds) and a one-second limit per
// attempt. env adds to and overrides the probe's variables.
func isoConnector(t *testing.T, resolv string, dnsPort, listenerPort int, settle string, env map[string]string) shellRun {
	t.Helper()
	requireTools(t, "bash", "timeout", "awk")
	path := filepath.Join(t.TempDir(), "resolv.conf")
	if err := os.WriteFile(path, []byte(resolv), 0o644); err != nil {
		t.Fatal(err)
	}
	script := probeConnectorScript
	// The path is a t.TempDir() one, single-quoted as runScript quotes it.
	quoted := "'" + strings.ReplaceAll(path, "'", `'"'"'`) + "'"
	for _, edit := range [][2]string{
		{"resolv=/etc/resolv.conf\n", "resolv=" + quoted + "\n"},
		{"dns_port=53\n", "dns_port=" + strconv.Itoa(dnsPort) + "\n"},
		{"listener_port=8080\n", "listener_port=" + strconv.Itoa(listenerPort) + "\n"},
		{"settle=5\n", "settle=" + settle + "\n"},
		{"limit=5\n", "limit=1\n"},
	} {
		if strings.Count(script, edit[0]) != 1 {
			t.Fatalf("the connector script no longer sets %q once at its head; this harness edits it", strings.TrimSpace(edit[0]))
		}
		script = strings.Replace(script, edit[0], edit[1], 1)
	}
	vars := map[string]string{
		"PATH":                 os.Getenv("PATH"),
		"JOB_COMPLETION_INDEX": "1",
		probeTargetVar:         "127.0.0.1",
		probeJobUIDVar:         "uid-probe-7",
		probeSelfUIDVar:        "uid-probe-7",
	}
	for k, v := range env {
		vars[k] = v
	}
	var environ []string
	for k, v := range vars {
		if v != "\x00unset" {
			environ = append(environ, k+"="+v)
		}
	}
	return runBash(t, exec.Command("/bin/bash", "-c", script), environ)
}

func runBash(t *testing.T, cmd *exec.Cmd, env []string) shellRun {
	t.Helper()
	cmd.Dir = t.TempDir()
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		return shellRun{stdout.String(), stderr.String(), exit.ExitCode()}
	case err != nil:
		t.Fatalf("running the script: %v", err)
	}
	return shellRun{stdout.String(), stderr.String(), 0}
}

const isoResolv = "search memql-pipelines.svc.cluster.local svc.cluster.local\nnameserver 127.0.0.1\noptions ndots:5\n"

// TestIsolationProbeConnectorReadsTheThreeRoundsTogether: the connector's
// exit code is the R42 table, read off three rounds of a DNS attempt and a
// listener attempt (R44), each verdict printed as the line the runner quotes.
func TestIsolationProbeConnectorReadsTheThreeRoundsTogether(t *testing.T) {
	for _, c := range []struct {
		name     string
		dns      func(t *testing.T) int
		listener func(t *testing.T) int
		env      map[string]string
		code     int
		said     []string
		// linuxOnly, when set, is why the case holds on Linux alone.
		linuxOnly string
	}{
		{
			name: "every listener attempt refused while DNS answered every one: isolated",
			dns:  func(t *testing.T) int { return isoAccepting(t).port() }, listener: isoRefusing,
			code: probeExitIsolated, said: []string{"connected, connected, connected", "refused, refused, refused"},
		},
		{
			name: "every listener attempt timed out while DNS answered every one: isolated",
			dns:  func(t *testing.T) int { return isoAccepting(t).port() }, listener: isoSilent,
			code: probeExitIsolated, said: []string{"connected, connected, connected", "timed out, timed out, timed out"},
		},
		{
			name: "a listener attempt connected: not isolated, whatever DNS did",
			dns:  isoRefusing, listener: func(t *testing.T) int { return isoAccepting(t).port() },
			code: probeExitConnected, said: []string{"refused, refused, refused", "listener 127.0.0.1:"},
		},
		{
			name: "positive control reaches DNS and the listener every round",
			dns:  func(t *testing.T) int { return isoAccepting(t).port() }, listener: func(t *testing.T) int { return isoAccepting(t).port() },
			env: map[string]string{"JOB_COMPLETION_INDEX": "2"}, code: probeExitControlPassed,
		},
		{
			name: "positive control cannot reach the listener",
			dns:  func(t *testing.T) int { return isoAccepting(t).port() }, listener: isoRefusing,
			env: map[string]string{"JOB_COMPLETION_INDEX": "2"}, code: probeExitControlFailed,
		},
		{
			name: "positive control reaches the listener but not DNS",
			dns:  isoRefusing, listener: func(t *testing.T) int { return isoAccepting(t).port() },
			env: map[string]string{"JOB_COMPLETION_INDEX": "2"}, code: probeExitControlFailed,
		},
		{
			name: "DNS failed every attempt: inconclusive",
			dns:  isoRefusing, listener: isoRefusing,
			code: probeExitDNS, said: []string{"dns 127.0.0.1:"},
		},
		{
			name: "DNS answered the first attempt only: mixed answers are inconclusive",
			dns: func(t *testing.T) int {
				l := isoAccepting(t)
				go func() {
					for len(l.accepted()) == 0 {
						time.Sleep(time.Millisecond)
					}
					_ = l.ln.Close()
				}()
				return l.port()
			},
			listener: isoRefusing,
			code:     probeExitDNS, said: []string{"connected, refused, refused"},
		},
		{
			// TCP to a broadcast address is refused by the kernel itself,
			// "Network is unreachable": neither a refusal nor a timeout.
			name: "a listener attempt failed some other way: inconclusive",
			dns:  func(t *testing.T) int { return isoAccepting(t).port() }, listener: isoRefusing,
			env:  map[string]string{probeTargetVar: "255.255.255.255"},
			code: probeExitUnclear, said: []string{"Network is unreachable"},
			linuxOnly: "Linux refuses TCP to a broadcast address in connect() itself, as \"Network is unreachable\"; " +
				"another kernel refuses it otherwise",
		},
		{
			name: "the target Secret names another Job: inconclusive",
			dns:  func(t *testing.T) int { return isoAccepting(t).port() }, listener: isoRefusing,
			env:  map[string]string{probeJobUIDVar: "uid-of-an-earlier-probe"},
			code: probeExitForeignTarget, said: []string{"not this probe Job's"},
		},
		{
			name: "the pod does not know its own Job: inconclusive",
			dns:  func(t *testing.T) int { return isoAccepting(t).port() }, listener: isoRefusing,
			env:  map[string]string{probeSelfUIDVar: "", probeJobUIDVar: ""},
			code: probeExitForeignTarget,
		},
		{
			name: "no listener address: inconclusive",
			dns:  func(t *testing.T) int { return isoAccepting(t).port() }, listener: isoRefusing,
			env:  map[string]string{probeTargetVar: ""},
			code: probeExitForeignTarget,
		},
		{
			name: "a part for no index: inconclusive",
			dns:  func(t *testing.T) int { return isoAccepting(t).port() }, listener: isoRefusing,
			env:  map[string]string{"JOB_COMPLETION_INDEX": "3"},
			code: probeExitNoPart, said: []string{"completion index 3"},
		},
		{
			name: "no completion index at all: inconclusive",
			dns:  func(t *testing.T) int { return isoAccepting(t).port() }, listener: isoRefusing,
			env:  map[string]string{"JOB_COMPLETION_INDEX": "\x00unset"},
			code: probeExitNoPart,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.linuxOnly != "" && runtime.GOOS != "linux" {
				t.Skip(c.linuxOnly)
			}
			// Each case has its own endpoints, and most wait out three
			// rounds a second apart.
			t.Parallel()
			run := isoConnector(t, isoResolv, c.dns(t), c.listener(t), "0", c.env)
			if run.code != c.code {
				t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", run.code, c.code, run.stdout, run.stderr)
			}
			if strings.Count(run.stdout, "\n") != 1 || !strings.HasPrefix(run.stdout, "memql: isolation probe: ") {
				t.Errorf("stdout = %q, want one line saying what the probe saw", run.stdout)
			}
			for _, w := range c.said {
				if !strings.Contains(run.stdout, w) {
					t.Errorf("stdout = %q, want it to say %q", run.stdout, w)
				}
			}
		})
	}

	t.Run("no nameserver to try: inconclusive", func(t *testing.T) {
		t.Parallel()
		run := isoConnector(t, "search cluster.local\n", isoRefusing(t), isoRefusing(t), "0", nil)
		if run.code != probeExitNoNameserver || !strings.Contains(run.stdout, "names no nameserver") {
			t.Errorf("exit %d, stdout %q; want %d and a sentence", run.code, run.stdout, probeExitNoNameserver)
		}
	})

	t.Run("it settles first, then tries three times a second apart (R44)", func(t *testing.T) {
		t.Parallel()
		dns := isoAccepting(t)
		start := time.Now()
		run := isoConnector(t, isoResolv, dns.port(), isoRefusing(t), "2", nil)
		if run.code != probeExitIsolated {
			t.Fatalf("exit %d, want %d\nstdout: %s", run.code, probeExitIsolated, run.stdout)
		}
		at := dns.accepted()
		if len(at) != 3 {
			t.Fatalf("DNS was tried %d times, want 3", len(at))
		}
		if d := at[0].Sub(start); d < 2*time.Second {
			t.Errorf("the first attempt came %v after the start, want the 2 s settle first", d)
		}
		for i := 1; i < len(at); i++ {
			if d := at[i].Sub(at[i-1]); d < 900*time.Millisecond {
				t.Errorf("attempt %d came %v after the one before, want a second apart", i+1, d)
			}
		}
	})

	t.Run("index 0 holds its pod, and with it the listener, up", func(t *testing.T) {
		t.Parallel()
		requireTools(t, "bash", "timeout")
		cmd := exec.Command("timeout", "1", "/bin/bash", "-c", probeConnectorScript)
		run := runBash(t, cmd, []string{"PATH=" + os.Getenv("PATH"), "JOB_COMPLETION_INDEX=0"})
		if run.code != 124 {
			t.Errorf("index 0 exited %d (stdout %q) within a second; want it still running when the timeout ended it", run.code, run.stdout)
		}
	})
}

// TestIsolationProbeListenerAcceptsUntilStopped: the listener sidecar accepts
// and closes every connection on its port, for as long as it runs, and a
// listener that cannot take its port ends at once, saying so: never ready,
// so the proof never passes on it.
func TestIsolationProbeListenerAcceptsUntilStopped(t *testing.T) {
	requireTools(t, "perl")
	port := isoFreePort(t)
	if strings.Count(probeListenerScript, "8080") == 0 {
		t.Fatal("the listener script no longer names port 8080; this harness points it at a free port")
	}
	script := strings.ReplaceAll(probeListenerScript, "8080", strconv.Itoa(port))
	cmd := exec.Command("/bin/sh", "-c", script)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	rtWaitUntil(t, "the listener", func() bool {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = c.Close()
		}
		return err == nil
	})
	for i := 0; i < 3; i++ {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			t.Fatalf("connection %d: %v", i+1, err)
		}
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		if n, err := c.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
			t.Errorf("connection %d read %d bytes, %v; want it closed by the listener (EOF)", i+1, n, err)
		}
		_ = c.Close()
	}

	second := exec.Command("/bin/sh", "-c", script)
	var said bytes.Buffer
	second.Stdout, second.Stderr = &said, &said
	err := second.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || !strings.Contains(said.String(), "cannot listen on") {
		t.Errorf("a second listener on the same port = %v, %q; want it to end at once, saying it cannot listen", err, said.String())
	}
}

// TestIsolationProbeStopsOnTERM: deleting the probe Job stops its pods with
// TERM, and as each container's first process the listener and index 0's
// holder get no signal they have not asked for -- so each asks: it ends at
// once, cleanly, rather than at the end of the grace period.
func TestIsolationProbeStopsOnTERM(t *testing.T) {
	for _, c := range []struct {
		name, shell, script string
		env                 []string
		need                []string
	}{
		{"the listener", "/bin/sh", strings.ReplaceAll(probeListenerScript, "8080", strconv.Itoa(isoFreePort(t))), nil, []string{"perl"}},
		{"index 0's holder", "/bin/bash", probeConnectorScript, []string{"JOB_COMPLETION_INDEX=0"}, []string{"bash", "sleep"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			requireTools(t, c.need...)
			cmd := exec.Command(c.shell, "-c", c.script)
			cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, c.env...)
			if err := cmd.Start(); err != nil {
				t.Fatalf("start: %v", err)
			}
			time.Sleep(300 * time.Millisecond)
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatalf("TERM: %v", err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("it ended %v on TERM, want a clean exit", err)
				}
			case <-time.After(2 * time.Second):
				_ = cmd.Process.Kill()
				<-done
				t.Error("it did not end within two seconds of TERM")
			}
		})
	}
}
