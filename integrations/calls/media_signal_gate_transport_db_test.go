package calls

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun/driver/pgdriver"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/database"
)

type signalGateWire struct {
	net.Conn
	delay   time.Duration
	queries *atomic.Int64
}

func (c *signalGateWire) Write(p []byte) (int, error) {
	if len(p) >= 5 && p[0] == 'Q' {
		c.queries.Add(1)
		time.Sleep(c.delay)
	}
	return c.Conn.Write(p)
}

type signalTransportOutcome struct {
	Name        string
	Succeeded   int
	Failure     string
	DurationsMS []int64
}

func (o signalTransportOutcome) summary() string {
	sort.Slice(o.DurationsMS, func(i, j int) bool { return o.DurationsMS[i] < o.DurationsMS[j] })
	max, p95 := int64(0), int64(0)
	if n := len(o.DurationsMS); n > 0 {
		max = o.DurationsMS[n-1]
		p95 = o.DurationsMS[(95*n+99)/100-1]
	}
	b, _ := json.Marshal(map[string]any{"operation": o.Name, "succeeded": o.Succeeded, "failure": o.Failure, "maxMS": max, "p95MS": p95, "samplesMS": o.DurationsMS})
	return string(b)
}

// Regression for gate transport contention alongside normal room polling. Real
// funded account credentials, two services/pools over the same native graph,
// actual five-key gates, two Forward streams, Workflow(snapshot) and guarded
// provider reconciliation. The provider remains the existing fake inventory;
// this proves signalling progress under injected latency, not production RTP.
func TestMediaSignalDatabaseGateTransportWithBackgroundPolling(t *testing.T) {
	for _, wireDelay := range []time.Duration{60 * time.Millisecond} {
		t.Run(wireDelay.String(), func(t *testing.T) {
			cw := newCallWorld(t, "signal-gate-wire", 10)
			if cw == nil {
				return
			}
			ctx := context.Background()
			_, host, _ := allowanceAccount(t, cw, "day")
			_, guest, _ := allowanceAccount(t, cw, "day")
			start, err := cw.svc.Start(ctx, host, callrules.StartBody{Name: "Transport host", Confirm: confirmed})
			require.NoError(t, err)
			host, err = cw.svc.Session(ctx, cw.ref, host.Token)
			require.NoError(t, err)
			joined, guest, err := cw.rejoin(guest, start.Call.GuestToken)
			require.NoError(t, err)
			replica := allowanceReplica(t, cw)
			traces := &lockedBuffer{}
			cw.svc.logger = slog.New(slog.NewJSONHandler(traces, nil))
			replica.logger = cw.svc.logger
			hostLease, err := cw.svc.BeginMediaSignal(ctx, start.Join.Pass)
			require.NoError(t, err)
			require.NoError(t, hostLease.Confirm(ctx))
			guestLease, err := replica.BeginMediaSignal(ctx, joined.Pass)
			require.NoError(t, err)
			cw.media.setPresent(start.Call.ID, person(hostLease.Identity), person(guestLease.Identity))
			require.NoError(t, guestLease.Confirm(ctx))
			var gateQueries atomic.Int64
			pools := []*sql.DB{}
			for _, service := range []*Service{cw.svc, replica} {
				connector := pgdriver.NewConnector(pgdriver.WithDSN(sharedBoot.dsn), pgdriver.WithConnParams(map[string]any{"statement_timeout": "0", "idle_in_transaction_session_timeout": fmt.Sprint(database.DefaultGateIdleInTxTimeout.Milliseconds())}))
				dial := connector.Config().Dialer
				connector.Config().Dialer = func(ctx context.Context, network, addr string) (net.Conn, error) {
					c, e := dial(ctx, network, addr)
					if e != nil {
						return nil, e
					}
					return &signalGateWire{Conn: c, delay: wireDelay, queries: &gateQueries}, nil
				}
				pool := sql.OpenDB(connector)
				pool.SetMaxOpenConns(4)
				pool.SetMaxIdleConns(2)
				pools = append(pools, pool)
				original := service.gateDB
				service.gateDB = func() *sql.DB { return pool }
				defer func() { service.gateDB = original }()
			}
			defer func() {
				for _, pool := range pools {
					_ = pool.Close()
				}
			}()
			originals := []Engine{cw.svc.store.engine, replica.store.engine}
			readers := []*mediaSignalAuthorityLatencyEngine{}
			for i, service := range []*Service{cw.svc, replica} {
				reads := &mediaSignalAuthorityLatencyEngine{Engine: originals[i], delay: 30 * time.Millisecond}
				service.store.engine = reads
				readers = append(readers, reads)
				defer func() { service.store.engine = originals[i] }()
			}
			room, err := cw.svc.store.roomForMediaRoom(ctx, start.Call.ID)
			require.NoError(t, err)
			require.NotNil(t, room)
			keys, err := cw.svc.mediaSignalGateKeys(ctx, *room, mediaSignalBinding{})
			require.NoError(t, err)
			require.Len(t, keys, 5)
			t.Logf("shape gatekeys=%d gate_transport_query_delay=%s native_named_read_delay=30ms replicas=2 two_forward_streams=12frames_each workflow_snapshot_two_sessions_every=5s audit_every=5s cycles=3", len(keys), wireDelay)
			run, cancel := context.WithTimeout(ctx, 45*time.Second)
			defer cancel()
			begin := make(chan struct{})
			results := make(chan signalTransportOutcome, 5)
			var wg sync.WaitGroup
			var callbacks atomic.Int64
			started := time.Now()
			for i, lease := range []*callrules.MediaSignalAdmission{hostLease, guestLease} {
				i, lease := i, lease
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-begin
					o := signalTransportOutcome{Name: fmt.Sprintf("forward-%d", i)}
					for n := 0; n < 12; n++ {
						bounded, stop := context.WithTimeout(run, 5*time.Second)
						at := time.Now()
						e := lease.Forward(bounded, func(context.Context) error { callbacks.Add(1); return nil })
						o.DurationsMS = append(o.DurationsMS, time.Since(at).Milliseconds())
						stop()
						if e != nil {
							o.Failure = mediaSignalFailure(e)
							results <- o
							return
						}
						o.Succeeded++
						select {
						case <-run.Done():
							o.Failure = mediaSignalFailure(run.Err())
							results <- o
							return
						case <-time.After(200 * time.Millisecond):
						}
					}
					results <- o
				}()
			}
			waitUntil := func(at time.Time) error {
				if d := time.Until(at); d > 0 {
					select {
					case <-run.Done():
						return run.Err()
					case <-time.After(d):
					}
				}
				return nil
			}
			for i, session := range []callrules.Session{host, guest} {
				i, session := i, session
				service := []*Service{cw.svc, replica}[i]
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-begin
					o := signalTransportOutcome{Name: fmt.Sprintf("snapshot-%d", i)}
					for n := 0; n < 3; n++ {
						if e := waitUntil(started.Add(time.Duration(n) * 5 * time.Second)); e != nil {
							o.Failure = mediaSignalFailure(e)
							results <- o
							return
						}
						at := time.Now()
						_, e := service.Workflow(run, cw.ref, session.Token, callrules.WorkflowRequest{Action: "snapshot"})
						o.DurationsMS = append(o.DurationsMS, time.Since(at).Milliseconds())
						if e != nil {
							o.Failure = mediaSignalFailure(e)
							results <- o
							return
						}
						o.Succeeded++
					}
					results <- o
				}()
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-begin
				o := signalTransportOutcome{Name: "audit"}
				for n := 0; n < 3; n++ {
					if e := waitUntil(started.Add(time.Duration(n) * 5 * time.Second)); e != nil {
						o.Failure = mediaSignalFailure(e)
						results <- o
						return
					}
					at := time.Now()
					_, e := cw.svc.ReconcileMediaSignals(run, start.Call.ID)
					o.DurationsMS = append(o.DurationsMS, time.Since(at).Milliseconds())
					if e != nil {
						o.Failure = mediaSignalFailure(e)
						results <- o
						return
					}
					o.Succeeded++
				}
				results <- o
			}()
			close(begin)
			wg.Wait()
			close(results)
			expected := map[string]int{"forward-0": 12, "forward-1": 12, "snapshot-0": 3, "snapshot-1": 3, "audit": 3}
			for o := range results {
				t.Log(o.summary())
				want, known := expected[o.Name]
				require.True(t, known, "unexpected or duplicate worker outcome: %s", o.Name)
				delete(expected, o.Name)
				if o.Succeeded != want {
					t.Errorf("%s completed %d operations, want %d", o.Name, o.Succeeded, want)
				}
				if o.Failure != "" {
					t.Errorf("%s failed: %s", o.Name, o.Failure)
				}
			}
			require.Empty(t, expected, "all workers must report after joining")
			require.Equal(t, int64(24), callbacks.Load(), "all frames must reach their gated callback")
			require.GreaterOrEqual(t, gateQueries.Load(), int64(6), "real gate query packets must be observed and delayed")
			require.Positive(t, readers[0].reads.Load()+readers[1].reads.Load(), "real named reads must be observed and delayed")
			t.Logf("characterization elapsed=%s gate_query_roundtrips=%d native_named_reads=%d; all goroutines joined", time.Since(started), gateQueries.Load(), readers[0].reads.Load()+readers[1].reads.Load())
			hostLease.Finish()
			guestLease.Finish()
			for _, line := range strings.Split(traces.String(), "\n") {
				var v map[string]any
				if json.Unmarshal([]byte(line), &v) == nil && v["event"] == "media_signal_terminal" {
					b, _ := json.Marshal(map[string]any{"duration_ms": v["duration_ms"], "first_failure_phase": v["first_failure_phase"], "first_failure": v["first_failure"], "phases": v["phases"]})
					t.Logf("closed_trace_includes_admission_setup %s", b)
				}
			}
		})
	}
}
