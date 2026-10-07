package pipelinesteps

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/znasllc-io/memql/component/deploycontrol"
)

func TestArtifactCollectorChecksIdentityAcrossTheStream(t *testing.T) {
	for _, fault := range []string{"", "label-only", "running-producer", "wrong-owner", "pod-replaced", "job-replaced", "container-replaced", "lost-status"} {
		t.Run(fault, func(t *testing.T) {
			run := rtRun()
			run.Artifacts = []string{"proof"}
			cfg := testConfig()
			job, err := BuildJob(cfg, run, JobName(run.RunID, run.StepKey, run.Attempt))
			if err != nil {
				t.Fatal(err)
			}
			job.Metadata.UID = "job-uid"
			pod := Pod{Metadata: ObjectMeta{Name: "artifact-pod", UID: "pod-uid", OwnerReferences: []OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Metadata.Name, UID: job.Metadata.UID, Controller: ptr(true)}}}, Spec: job.Spec.Template.Spec, Status: PodStatus{ContainerStatuses: []ContainerStatus{
				{Name: ContainerStep, ContainerID: "containerd://producer", State: ContainerState{Terminated: &ContainerStateTerminated{ExitCode: 0, FinishedAt: time.Now()}}},
				{Name: ContainerArtifacts, ContainerID: "containerd://collector", State: ContainerState{Running: &ContainerStateRunning{StartedAt: time.Now()}}},
			}}}
			observed := pod
			switch fault {
			case "label-only":
				pod.Metadata.OwnerReferences = nil
				pod.Metadata.Labels = map[string]string{"controller-uid": job.Metadata.UID}
			case "running-producer":
				pod.Status.ContainerStatuses[0].State = ContainerState{Running: &ContainerStateRunning{}}
			case "wrong-owner":
				job.Metadata.Annotations[AnnotOwner] = "other-owner"
			}
			var archive bytes.Buffer
			tw := tar.NewWriter(&archive)
			_ = tw.WriteHeader(&tar.Header{Name: "proof", Mode: 0600, Size: 3})
			_, _ = tw.Write([]byte("abc"))
			_ = tw.Close()
			var mu sync.Mutex
			streams := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/exec") {
					mu.Lock()
					streams++
					mu.Unlock()
					if r.URL.Query().Get("container") != ContainerArtifacts {
						t.Error("wrong collector")
					}
					c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"v5.channel.k8s.io"}})
					if err != nil {
						return
					}
					defer c.CloseNow()
					_ = c.Write(r.Context(), websocket.MessageBinary, append([]byte{1}, archive.Bytes()...))
					mu.Lock()
					switch fault {
					case "pod-replaced":
						pod.Metadata.UID = "new-pod"
					case "job-replaced":
						job.Metadata.UID = "new-job"
					case "container-replaced":
						pod.Status.ContainerStatuses[1].ContainerID = "containerd://new-collector"
					}
					mu.Unlock()
					if fault != "lost-status" {
						_ = c.Write(r.Context(), websocket.MessageBinary, append([]byte{3}, `{"status":"Success"}`...))
					}
					_ = c.Close(websocket.StatusNormalClosure, "")
					return
				}
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/pods") {
					_ = json.NewEncoder(w).Encode(PodList{Items: []Pod{pod}})
					return
				}
				if strings.HasSuffix(r.URL.Path, "/"+job.Metadata.Name) {
					_ = json.NewEncoder(w).Encode(job)
					return
				}
				t.Error("unexpected API path", r.URL.Path)
				w.WriteHeader(500)
			}))
			defer srv.Close()
			kube := NewKube(deploycontrol.NewClusterAPIWith(srv.URL, "fixture", srv.Client()), cfg.Namespace)
			snapshot, err := kube.CollectArtifacts(t.Context(), run, &observed, 1<<20)
			if (err != nil) != (fault != "") {
				t.Fatalf("snapshot=%v err=%v", snapshot, err)
			}
			if snapshot != nil {
				defer snapshot.Close()
				if len(snapshot.Files) != 1 || snapshot.Files[0].Size != 3 {
					t.Fatal("wrong verified file")
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if (fault == "label-only" || fault == "running-producer" || fault == "wrong-owner") && streams != 0 {
				t.Fatal("read refused producer")
			}
		})
	}
}

func TestArtifactCollectorHasOnlyAReadOnlyExport(t *testing.T) {
	job, err := BuildJob(testConfig(), testRun(), testJobName)
	if err != nil {
		t.Fatal(err)
	}
	var collector *Container
	for n := range job.Spec.Template.Spec.Containers {
		if job.Spec.Template.Spec.Containers[n].Name == ContainerArtifacts {
			collector = &job.Spec.Template.Spec.Containers[n]
		}
	}
	if collector == nil || len(collector.Env) != 0 || len(collector.VolumeMounts) != 1 || collector.VolumeMounts[0].Name != artifactVolume || !collector.VolumeMounts[0].ReadOnly || collector.SecurityContext.ReadOnlyRootFilesystem == nil || !*collector.SecurityContext.ReadOnlyRootFilesystem || *collector.SecurityContext.AllowPrivilegeEscalation {
		t.Fatalf("collector can reach writable/private resources: %+v", collector)
	}
}
