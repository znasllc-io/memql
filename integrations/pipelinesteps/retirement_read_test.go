package pipelinesteps

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/znasllc-io/memql/component/deploycontrol"
	pl "github.com/znasllc-io/memql/component/pipelines"
)

type retirementReadTransport func(*http.Request) (*http.Response, error)

func (f retirementReadTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Inject at the caller's transport so net/http cannot silently retry a reused
// connection and hide the runner's missing retry. Other replicas still use the
// original cluster client and share only the API server's durable objects.
func retirementReadClient(t *testing.T, h *rtHarness, fault retirementReadTransport) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(h.c.serve))
	t.Cleanup(server.Close)
	base := server.Client().Transport
	client := &http.Client{Transport: retirementReadTransport(func(r *http.Request) (*http.Response, error) {
		if response, err := fault(r); response != nil || err != nil {
			return response, err
		}
		return base.RoundTrip(r)
	})}
	h.r.kube = NewKube(deploycontrol.NewClusterAPIWith(server.URL, "fixture", client), h.cfg.Namespace)
}

func retirementReadStatus(r *http.Request, code int) *http.Response {
	return &http.Response{StatusCode: code, Status: fmt.Sprint(code), Request: r,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   io.NopCloser(strings.NewReader(`{"message":"injected retirement read failure"}`))}
}

func TestRetirementReadsRecoverBeforeEachCreationGate(t *testing.T) {
	for _, marker := range []string{testJobName + retirementSuffix, runRetirementName(rtRun().RunID)} {
		for _, gate := range []int32{1, 2, 3} { // before Secret, before claim, after claim
			for _, code := range []int{0, http.StatusServiceUnavailable, http.StatusTooManyRequests} {
				t.Run(fmt.Sprintf("%s/gate-%d/status-%d", marker, gate, code), func(t *testing.T) {
					h := newRunnerHarness(t)
					var reads atomic.Int32
					retirementReadClient(t, h, func(r *http.Request) (*http.Response, error) {
						if r.Method == http.MethodGet && r.URL.Path == kubeSecrets+"/"+marker && reads.Add(1) == gate {
							if code == 0 {
								return nil, io.EOF
							}
							return retirementReadStatus(r, code), nil
						}
						return nil, nil
					})
					h.c.script(testJobName, rtFinishingScript(testJobName, 0, captureKubeLine(rtAt(1100), "executed once")))
					result := h.run(t, rtRun())
					if result.Status != pl.OutcomeSucceeded {
						t.Fatalf("transient retirement read failed the attempt: %+v (%+v)", result, result.Failure)
					}
					if reads.Load() < gate+1 || len(h.c.requestsFor(http.MethodPost, kubeJobs)) != 1 || len(h.lib.stored()) != 1 {
						t.Fatal("retry did not recover one execution and one archived log")
					}
				})
			}
		}
	}
}

func TestRetirementReadRetryObservesAnotherReplicasCancellation(t *testing.T) {
	h := newRunnerHarness(t)
	config := h.cfg
	config.NodeID = rtOther
	other := h.newRunner(config)
	var reads atomic.Int32
	retirementReadClient(t, h, func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet && r.URL.Path == kubeSecrets+"/"+runRetirementName(rtRun().RunID) && reads.Add(1) == 3 {
			// The first runner holds a creation claim but has sent no Job POST.
			// Cleanup cannot confirm absence yet; the durable stop must still win.
			_, _ = other.CancelRun(context.Background(), CancelRequest{RunID: rtRun().RunID})
			return nil, io.EOF
		}
		return nil, nil
	})
	result := h.run(t, rtRun())
	if result.Status != pl.OutcomeCancelled || reads.Load() < 4 || len(h.c.requestsFor(http.MethodPost, kubeJobs)) != 0 {
		t.Fatalf("a retry escaped the other replica's stop: %+v (%+v), reads=%d", result, result.Failure, reads.Load())
	}
}

func TestRetirementReadRetriesRemainBoundedAndRefuseUnverifiedCreation(t *testing.T) {
	for _, code := range []int{http.StatusServiceUnavailable, http.StatusForbidden} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			h := newRunnerHarness(t)
			var reads atomic.Int32
			retirementReadClient(t, h, func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodGet && r.URL.Path == kubeSecrets+"/"+testJobName+retirementSuffix {
					reads.Add(1)
					return retirementReadStatus(r, code), nil
				}
				return nil, nil
			})
			result := h.run(t, rtRun())
			wantFailure(t, result, pl.OutcomeFailed, pl.CodeExecutionUncertain)
			want := int32(1)
			if code == http.StatusServiceUnavailable {
				want = apiAttempts
			}
			if reads.Load() != want || len(h.c.requestsFor(http.MethodPost, kubeJobs)) != 0 || len(h.tokens.called()) != 0 {
				t.Fatalf("unverified creation or wrong retry bound: reads=%d, want %d", reads.Load(), want)
			}
		})
	}
}
