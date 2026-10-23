package worker

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Metrics is a tiny self-contained counter / histogram surface
// exposed at 127.0.0.1:9100/metrics in prometheus text format.
//
// Loopback-only by design: the worker is the user's own machine
// and the metrics endpoint is a debugging aid, not a publicly
// exposed scrape target. Operators can `curl
// http://127.0.0.1:9100/metrics` from the same box. No
// authentication; binding to 127.0.0.1 is the gate.
//
// We hand-roll the text format rather than importing
// prometheus/client_golang because the cockpit binary should stay
// small and CGO-free on the headless build.
//
// EVERY PER-STREAM SERIES CARRIES A `home` LABEL (memql-cockpit#433). One
// process serves every cluster the machine is paired with, and a counter
// that summed them could say that something flaps but never which
// cluster -- the one question a person reading it has. Prometheus sums
// across the label when the total is what is wanted; nothing can split a
// sum that was never labelled.
type Metrics struct {
	startedAt time.Time

	durationBucketsMs []int64

	mu    sync.RWMutex
	homes map[string]*homeMetrics

	server   *http.Server
	listener net.Listener
}

// homeMetrics is one cluster stream's series.
type homeMetrics struct {
	callsTotal       atomic.Int64
	callsByOutcome   map[string]*atomic.Int64
	callsByOutcomeMu sync.RWMutex

	bucketCounts  []atomic.Int64
	durationSumMs atomic.Int64
	durationCount atomic.Int64

	reconnects atomic.Int64
	connected  atomic.Bool
	refused    atomic.Bool
}

// NewMetrics constructs a Metrics surface.
func NewMetrics() *Metrics {
	// Histogram buckets: 50ms, 100ms, 250ms, 500ms, 1s, 2.5s, 5s, 10s, 30s, 60s, +Inf.
	buckets := []int64{50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000, 60000}
	return &Metrics{
		startedAt:         time.Now().UTC(),
		durationBucketsMs: buckets,
		homes:             make(map[string]*homeMetrics),
	}
}

// home returns the series for one stream, creating them on first use.
func (m *Metrics) home(id string) *homeMetrics {
	m.mu.RLock()
	h, ok := m.homes[id]
	m.mu.RUnlock()
	if ok {
		return h
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if h, ok := m.homes[id]; ok {
		return h
	}
	h = &homeMetrics{
		callsByOutcome: make(map[string]*atomic.Int64),
		bucketCounts:   make([]atomic.Int64, len(m.durationBucketsMs)+1), // +1 for +Inf overflow
	}
	m.homes[id] = h
	return h
}

// RecordCall observes a single tool-dispatch outcome on one home's stream.
func (m *Metrics) RecordCall(home, outcome string, durationMs int64) {
	if m == nil {
		return
	}
	h := m.home(home)
	h.callsTotal.Add(1)
	h.incrementOutcome(outcome)
	h.durationSumMs.Add(durationMs)
	h.durationCount.Add(1)
	for i, b := range m.durationBucketsMs {
		if durationMs <= b {
			h.bucketCounts[i].Add(1)
			return
		}
	}
	h.bucketCounts[len(h.bucketCounts)-1].Add(1)
}

// RecordReconnect counts one failed connect attempt on one home's stream.
func (m *Metrics) RecordReconnect(home string) {
	if m == nil {
		return
	}
	m.home(home).reconnects.Add(1)
}

// SetConnected records whether one home's stream is up right now.
func (m *Metrics) SetConnected(home string, up bool) {
	if m == nil {
		return
	}
	m.home(home).connected.Store(up)
}

// SetRefused records whether the cluster is refusing this machine on one
// home (memql-cockpit#427).
func (m *Metrics) SetRefused(home string, refused bool) {
	if m == nil {
		return
	}
	m.home(home).refused.Store(refused)
}

func (h *homeMetrics) incrementOutcome(outcome string) {
	h.callsByOutcomeMu.RLock()
	c, ok := h.callsByOutcome[outcome]
	h.callsByOutcomeMu.RUnlock()
	if ok {
		c.Add(1)
		return
	}
	h.callsByOutcomeMu.Lock()
	defer h.callsByOutcomeMu.Unlock()
	if c, ok := h.callsByOutcome[outcome]; ok {
		c.Add(1)
		return
	}
	c = &atomic.Int64{}
	c.Add(1)
	h.callsByOutcome[outcome] = c
}

// Listen starts the HTTP server on 127.0.0.1:port. Binding to
// loopback is a hard requirement -- the metrics endpoint is
// unauthenticated.
//
// If the requested port is already in use (commonly :9100, the
// Prometheus node_exporter default, which a developer's machine
// frequently has running), this falls back through the next four
// ports (port+1 ... port+4) before giving up. Callers see
// success on the first port that bound; the chosen port is
// readable via ListenAddr().
func (m *Metrics) Listen(port int) error {
	if m == nil {
		return nil
	}
	const fallbackAttempts = 5
	var (
		listener net.Listener
		addr     string
		lastErr  error
	)
	for offset := 0; offset < fallbackAttempts; offset++ {
		addr = fmt.Sprintf("127.0.0.1:%d", port+offset)
		l, err := net.Listen("tcp", addr)
		if err == nil {
			listener = l
			break
		}
		lastErr = err
	}
	if listener == nil {
		return fmt.Errorf("metrics: listen 127.0.0.1:%d-%d: %w", port, port+fallbackAttempts-1, lastErr)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", m.handleMetrics)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	m.listener = listener
	m.server = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		_ = m.server.Serve(listener)
	}()
	return nil
}

// ListenAddr returns the loopback address the metrics server
// bound to, e.g. "127.0.0.1:9101" when 9100 was taken and the
// fallback search picked the next free port.
func (m *Metrics) ListenAddr() string {
	if m == nil || m.listener == nil {
		return ""
	}
	return m.listener.Addr().String()
}

// Stop closes the metrics listener.
func (m *Metrics) Stop() {
	if m == nil || m.server == nil {
		return
	}
	_ = m.server.Close()
	if m.listener != nil {
		_ = m.listener.Close()
	}
}

func (m *Metrics) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = io.WriteString(w, m.render())
}

// render is the exposition text, pulled out of the handler so a test can
// read it without a listener.
func (m *Metrics) render() string {
	var b strings.Builder
	uptime := time.Since(m.startedAt).Seconds()

	m.mu.RLock()
	ids := make([]string, 0, len(m.homes))
	for id := range m.homes {
		ids = append(ids, id)
	}
	homes := make(map[string]*homeMetrics, len(m.homes))
	for id, h := range m.homes {
		homes[id] = h
	}
	m.mu.RUnlock()
	sort.Strings(ids)

	fmt.Fprintln(&b, "# HELP worker_uptime_seconds Seconds since the worker process started.")
	fmt.Fprintln(&b, "# TYPE worker_uptime_seconds gauge")
	fmt.Fprintf(&b, "worker_uptime_seconds %.3f\n", uptime)

	fmt.Fprintln(&b, "# HELP worker_home_connected Whether the stream to this home's cluster is up (1) or not (0).")
	fmt.Fprintln(&b, "# TYPE worker_home_connected gauge")
	for _, id := range ids {
		fmt.Fprintf(&b, "worker_home_connected{home=%s} %d\n", promLabel(id), boolGauge(homes[id].connected.Load()))
	}

	fmt.Fprintln(&b, "# HELP worker_home_refused Whether this home's cluster is refusing this machine (1): its token, or its registration.")
	fmt.Fprintln(&b, "# TYPE worker_home_refused gauge")
	for _, id := range ids {
		fmt.Fprintf(&b, "worker_home_refused{home=%s} %d\n", promLabel(id), boolGauge(homes[id].refused.Load()))
	}

	fmt.Fprintln(&b, "# HELP worker_calls_total Total tool-dispatch calls observed.")
	fmt.Fprintln(&b, "# TYPE worker_calls_total counter")
	for _, id := range ids {
		fmt.Fprintf(&b, "worker_calls_total{home=%s} %d\n", promLabel(id), homes[id].callsTotal.Load())
	}

	fmt.Fprintln(&b, "# HELP worker_calls_by_outcome_total Tool-dispatch calls by terminal outcome.")
	fmt.Fprintln(&b, "# TYPE worker_calls_by_outcome_total counter")
	for _, id := range ids {
		h := homes[id]
		h.callsByOutcomeMu.RLock()
		keys := make([]string, 0, len(h.callsByOutcome))
		for k := range h.callsByOutcome {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "worker_calls_by_outcome_total{home=%s,outcome=%s} %d\n", promLabel(id), promLabel(k), h.callsByOutcome[k].Load())
		}
		h.callsByOutcomeMu.RUnlock()
	}

	fmt.Fprintln(&b, "# HELP worker_call_duration_ms Tool-dispatch call duration in milliseconds.")
	fmt.Fprintln(&b, "# TYPE worker_call_duration_ms histogram")
	for _, id := range ids {
		h := homes[id]
		label := promLabel(id)
		cumulative := int64(0)
		for i, bound := range m.durationBucketsMs {
			cumulative += h.bucketCounts[i].Load()
			fmt.Fprintf(&b, "worker_call_duration_ms_bucket{home=%s,le=\"%d\"} %d\n", label, bound, cumulative)
		}
		cumulative += h.bucketCounts[len(h.bucketCounts)-1].Load()
		fmt.Fprintf(&b, "worker_call_duration_ms_bucket{home=%s,le=\"+Inf\"} %d\n", label, cumulative)
		fmt.Fprintf(&b, "worker_call_duration_ms_sum{home=%s} %d\n", label, h.durationSumMs.Load())
		fmt.Fprintf(&b, "worker_call_duration_ms_count{home=%s} %d\n", label, h.durationCount.Load())
	}

	fmt.Fprintln(&b, "# HELP worker_reconnects_total Failed connect attempts since process start.")
	fmt.Fprintln(&b, "# TYPE worker_reconnects_total counter")
	for _, id := range ids {
		fmt.Fprintf(&b, "worker_reconnects_total{home=%s} %d\n", promLabel(id), homes[id].reconnects.Load())
	}
	return b.String()
}

// promLabel quotes a label value the way the text exposition format
// escapes it: backslash, double quote and line feed, and nothing else. A
// home id comes from a hand-editable file, so it is escaped rather than
// trusted -- an unescaped quote would end the value early and forge the
// rest of the line.
func promLabel(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return `"` + r.Replace(v) + `"`
}

func boolGauge(b bool) int {
	if b {
		return 1
	}
	return 0
}
