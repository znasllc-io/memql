package transfer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

var ErrPaused = errors.New("transfer paused")
var ErrRunning = errors.New("this transfer is already running on this machine")

type Runner struct {
	Control    Control
	StateDir   string
	CheckRead  func(string) error
	CheckWrite func(string) error
	HTTP       *http.Client
	// Test-only scaling: production always uses the wire's 64 MiB blocks.
	testBlockBytes int64
}

func (r *Runner) blockSize() int64 {
	if r.testBlockBytes > 0 {
		return r.testBlockBytes
	}
	return BlockBytes
}
func (r *Runner) client() *http.Client {
	var c http.Client
	if r.HTTP != nil {
		c = *r.HTTP
	} else {
		c.Transport = &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext, TLSHandshakeTimeout: 15 * time.Second, ResponseHeaderTimeout: 45 * time.Second, MaxIdleConns: 4, IdleConnTimeout: 90 * time.Second}
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

// Only bytes go to the signed storage destination. No cluster bearer follows
// this request, and neither signed URLs nor response bodies enter diagnostics.
func (r *Runner) request(ctx context.Context, g Grant, body []byte) (*http.Response, error) {
	u, e := url.Parse(g.URL)
	if e != nil || u.Host == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" && strings.Contains(u.RawQuery, "sig=") {
		return nil, errors.New("invalid direct storage grant")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && !(r.HTTP != nil && u.Scheme == "http" && ip != nil && ip.IsLoopback()) {
		return nil, errors.New("storage requires HTTPS")
	}
	if time.Until(g.ExpiresAt) < 5*time.Second {
		return nil, errors.New("storage authorization expired; requesting a fresh grant")
	}
	ctx, cancel := context.WithCancel(ctx)
	activity := &atomic.Int64{}
	activity.Store(time.Now().UnixNano())
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if time.Since(time.Unix(0, activity.Load())) > time.Minute {
					cancel()
					return
				}
			}
		}
	}()
	stop := func() { cancel(); <-finished }
	var input io.Reader
	if body != nil {
		input = &activityReader{Reader: bytes.NewReader(body), last: activity}
	}
	req, e := http.NewRequestWithContext(ctx, g.Method, g.URL, input)
	if e != nil {
		stop()
		return nil, errors.New("could not prepare the storage request")
	}
	for k, v := range g.Headers {
		if strings.EqualFold(k, "Host") {
			stop()
			return nil, errors.New("invalid storage host override")
		}
		req.Header.Set(k, v)
	}
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	resp, e := r.client().Do(req)
	if e != nil {
		stop()
		return nil, errors.New("storage connection interrupted; progress is kept")
	}
	resp.Body = &activityBody{ReadCloser: resp.Body, last: activity, stop: stop}
	return resp, nil
}
func (r *Runner) Run(ctx context.Context, j Job) (Job, error) {
	if !safeID.MatchString(j.ID) || j.WorkerID == "" || r.Control == nil || r.StateDir == "" {
		return j, errors.New("invalid transfer identity or journal")
	}
	if j.Desired != "running" {
		return j, ErrPaused
	}
	unlock, err := LockLocal(filepath.Join(r.StateDir, "transfers", j.ID+".lock"))
	if err != nil {
		return j, err
	}
	defer unlock()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	transferID := j.ID
	done := make(chan struct{})
	defer func() { cancel(); <-done }()
	go func() {
		defer close(done)
		timer := time.NewTicker(3 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				state, err := r.Read(ctx, transferID)
				if err == nil && state.Desired != "running" {
					cancel()
					return
				}
			}
		}
	}()
	if j.Kind == "download" {
		return r.download(ctx, j)
	}
	f, m, err := r.source(ctx, j)
	if err != nil {
		return j, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return j, err
	}
	if j.State == "waiting" || j.State == "error" {
		j, err = r.Action(ctx, j.ID, "resume")
		if err != nil {
			return j, err
		}
	}
	if err = r.Control.Call(ctx, "Prepare", map[string]any{"id": j.ID, "size": m.Size, "sha256": m.SHA256}, &j); err != nil {
		return j, err
	}
	if j.State == "complete" {
		return j, nil
	}
	if j.Desired != "running" || j.State != "uploading" {
		return j, ErrPaused
	}
	// Reconcile before sending: a successful PUT or commit may have lost its reply.
	if err = r.Control.Call(ctx, "Checkpoint", map[string]any{"id": j.ID, "final": false}, &j); err != nil {
		return j, err
	}
	window := j.CheckpointBlocks
	if window < 1 || window > 16 {
		window = 1
	}
	pendingBlocks := 0
	lastCheckpoint := time.Now()
	for j.Committed < m.Size {
		if j.Desired != "running" {
			return j, ErrPaused
		}
		if err = ctx.Err(); err != nil {
			return j, err
		}
		if err = r.CheckRead(j.Path); err != nil {
			return j, err
		}
		n := int(j.Committed / r.blockSize())
		if j.Committed%r.blockSize() != 0 || n >= len(m.Chunks) {
			return j, errors.New("stored progress is not a complete source block")
		}
		block := m.Chunks[n]
		data := make([]byte, block.Size)
		if _, err = f.ReadAt(data, j.Committed); err != nil {
			return j, err
		}
		if checksum(data) != block {
			return j, errors.New("source bytes changed; cancel this transfer before starting a new copy")
		}
		var g Grant
		if err = r.Control.Call(ctx, "Block", map[string]any{"id": j.ID, "ordinal": n, "md5": block.MD5, "sha256": block.SHA256}, &g); err != nil {
			return j, err
		}
		resp, e := r.request(ctx, g, data)
		if e != nil {
			return j, e
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			return j, fmt.Errorf("storage refused an upload block (%d); progress is kept", resp.StatusCode)
		}
		// This local offset includes durably staged blocks. The server advances
		// committedBytes only after a fenced prefix commit; recovery inventories
		// staging first, so a lost response never forces known blocks to be resent.
		j.Committed += block.Size
		pendingBlocks++
		if pendingBlocks >= window || time.Since(lastCheckpoint) >= 30*time.Second || j.Committed == m.Size {
			if err = r.Control.Call(ctx, "Checkpoint", map[string]any{"id": j.ID, "final": false}, &j); err != nil {
				return j, err
			}
			pendingBlocks = 0
			lastCheckpoint = time.Now()
		}
	}
	after, err := f.Stat()
	named, statErr := os.Lstat(j.Path)
	if err != nil || statErr != nil || !sameSource(before, after) || !sameSource(before, named) {
		return j, errors.New("the source changed before backup completion")
	}
	err = r.Control.Call(ctx, "Checkpoint", map[string]any{"id": j.ID, "final": true}, &j)
	return j, err
}

// RunPending survives browser closure. Failed network attempts wait and retry
// the same journal identity, while pauses/cancellations remain user-controlled.
func (r *Runner) RunPending(ctx context.Context, registration func() string) {
	delay := 5 * time.Second
	for ctx.Err() == nil {
		worker := registration()
		failed := false
		if worker != "" {
			cursor := ""
			for ctx.Err() == nil {
				var page struct {
					Transfers []Job  `json:"transfers"`
					Next      string `json:"nextCursor"`
				}
				if e := r.Control.Call(ctx, "List", map[string]any{"workerId": worker, "after": cursor, "scope": "active"}, &page); e != nil {
					failed = true
					break
				}
				for _, j := range page.Transfers {
					if j.Kind == "sync_file" || j.Desired != "running" || j.State == "complete" || j.State == "bin" || j.State == "deleted" || j.State == "cancelled" {
						continue
					}
					if _, e := r.Run(ctx, j); e != nil && ctx.Err() == nil && !errors.Is(e, ErrPaused) && !errors.Is(e, ErrRunning) {
						failed = true
						_ = r.Control.Call(ctx, "Report", map[string]any{"id": j.ID, "state": "waiting", "message": e.Error(), "offset": j.Committed}, nil)
					}
				}
				if page.Next == "" {
					break
				}
				cursor = page.Next
			}
		}
		if failed {
			delay = min(delay*2, time.Minute)
		} else {
			delay = 5 * time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

type activityReader struct {
	io.Reader
	last *atomic.Int64
}

func (r *activityReader) Read(p []byte) (int, error) {
	n, e := r.Reader.Read(p)
	if n > 0 {
		r.last.Store(time.Now().UnixNano())
	}
	return n, e
}

type activityBody struct {
	io.ReadCloser
	last *atomic.Int64
	stop func()
}

func (b *activityBody) Read(p []byte) (int, error) {
	n, e := b.ReadCloser.Read(p)
	if n > 0 {
		b.last.Store(time.Now().UnixNano())
	}
	return n, e
}
func (b *activityBody) Close() error { defer b.stop(); return b.ReadCloser.Close() }
