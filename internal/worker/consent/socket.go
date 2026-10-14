package consent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DefaultSocketPath returns the canonical socket file path for the
// running user. The cockpit worker LISTENS at this path; the
// `memql worker consent ...` subcommand DIALS it.
//
// The socket lives under ~/.memql/ because that's the existing
// per-user cockpit state surface; the parent dir is created at
// mode 0700 and the socket file at mode 0600 -- only the running
// user can connect, matching the security boundary the worker
// token shares.
func DefaultSocketPath() string {
	if h := os.Getenv("MEMQL_WORKER_CONSENT_SOCKET"); h != "" {
		return h
	}
	if home := os.Getenv("HOME"); home != "" {
		return filepath.Join(home, ".memql", "worker.sock")
	}
	return filepath.Join(os.TempDir(), "memql-worker.sock")
}

// Request is the line-oriented JSON command shape callers POST to
// the socket. One JSON object per line; the server replies with a
// single JSON response (Response). The WATCH op replies with a
// stream of Events instead -- one per line, until the client
// disconnects.
type Request struct {
	Op            string `json:"op"`
	WindowSeconds int    `json:"window_seconds,omitempty"`
	Strict        bool   `json:"strict,omitempty"`

	// ApprovalId carries the per-action approval handle on
	// "approve" / "deny" ops. The TUI receives the id on an
	// EventApprovalRequested broadcast and echoes it back on the
	// response op. Required for both approve and deny; ignored on
	// other ops.
	ApprovalId string `json:"approval_id,omitempty"`

	// Region is the optional strict-mode in-region exemption rect
	// on the "grant" op (memql-cockpit#131). Nil = no region.
	// Ignored on every other op and on non-strict grants.
	Region *Region `json:"region,omitempty"`

	// Home names the cluster the op is for (memql-cockpit#433): which
	// window a grant opens, which one a revoke closes, whose state a
	// status reports, whose events a watch streams. Empty on a grant
	// means the only cluster there is, and is refused when there are
	// several; empty on revoke closes EVERY window; empty on status and
	// watch means all of them.
	Home string `json:"home,omitempty"`
}

// Response is the unified reply shape for non-WATCH ops.
type Response struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	// Code is set on an error a client may want to phrase itself -- the
	// Pick codes (CodeHomeRequired, CodeUnknownHome, CodeNoHomes).
	Code string `json:"code,omitempty"`
	// Status is the state of Home: the cluster the op named, or the only
	// one there is. Zero when the op concerned several.
	Status Status `json:"status,omitempty"`
	Home   string `json:"home,omitempty"`
	// Homes is every cluster this worker serves and its state, in the
	// order the worker registered them.
	Homes []HomeStatus `json:"homes,omitempty"`

	// Pending is populated on the initial WATCH response so a
	// reconnecting TUI can re-render its approval queue without
	// waiting for the next EventApprovalRequested broadcast.
	// Empty on every other op.
	Pending []PendingApprovalInfo `json:"pending,omitempty"`
}

// Server wraps every home's Manager with one Unix-socket interface.
type Server struct {
	homes  *Homes
	logger *slog.Logger
	path   string

	mu       sync.Mutex
	listener net.Listener
	stopped  bool
}

// NewServer builds a Server over every home's consent. One socket for the
// whole worker, however many clusters it serves: the operator has one
// place to go, and the request says which cluster it means.
func NewServer(homes *Homes, path string, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	if homes == nil {
		homes = NewHomes()
	}
	return &Server{homes: homes, logger: logger, path: path}
}

// Path returns the socket file path the server listens at.
func (s *Server) Path() string { return s.path }

// Listen opens the Unix socket and starts accepting connections in
// a goroutine. Returns once the listener is bound; ctx cancellation
// closes the listener and the accept loop exits.
//
// Refuses to bind if another process already holds the socket --
// the operator can manually remove ~/.memql/worker.sock if a stale
// file is left behind by an unclean shutdown. We do NOT silently
// unlink because that would let a malicious local process hijack
// the socket between our Stat and Bind.
func (s *Server) Listen(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("consent socket: mkdir %s: %w", filepath.Dir(s.path), err)
	}
	// A stale socket file left by an unclean shutdown can't be bound
	// over -- net.Listen returns EADDRINUSE. We only unlink when the
	// peer is verifiably gone (no live connect). This is a coarse
	// probe (dial + 200ms timeout) but it catches the common case.
	if info, err := os.Stat(s.path); err == nil && info.Mode()&os.ModeSocket != 0 {
		if c, derr := net.DialTimeout("unix", s.path, 200*time.Millisecond); derr != nil {
			// Looks dead -- safe to remove.
			_ = os.Remove(s.path)
		} else {
			_ = c.Close()
			return fmt.Errorf("consent socket: %s is already in use by another process", s.path)
		}
	}
	l, err := net.Listen("unix", s.path)
	if err != nil {
		return fmt.Errorf("consent socket: listen %s: %w", s.path, err)
	}
	if err := os.Chmod(s.path, 0o600); err != nil {
		_ = l.Close()
		return fmt.Errorf("consent socket: chmod %s: %w", s.path, err)
	}
	s.mu.Lock()
	s.listener = l
	s.mu.Unlock()
	s.logger.Info("worker consent socket listening", "path", s.path)

	go func() {
		<-ctx.Done()
		s.Close()
	}()
	go s.acceptLoop(l)
	return nil
}

// Close stops accepting connections and removes the socket file.
// Safe to call repeatedly.
func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	s.stopped = true
	if s.listener != nil {
		_ = s.listener.Close()
	}
	_ = os.Remove(s.path)
}

func (s *Server) acceptLoop(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			s.logger.Warn("consent socket: accept error", "error", err)
			return
		}
		go s.handle(c)
	}
}

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	// One request per connection -- keep the protocol simple. WATCH
	// holds the connection open and streams events until the client
	// disconnects.
	r := bufio.NewReader(c)
	line, err := r.ReadBytes('\n')
	if err != nil && err != io.EOF {
		s.logger.Warn("consent socket: read error", "error", err)
		return
	}
	if len(line) == 0 {
		return
	}
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		s.writeResp(c, Response{OK: false, Error: fmt.Sprintf("decode request: %v", err)})
		return
	}
	switch req.Op {
	case "grant":
		s.handleGrant(c, req)
	case "revoke":
		s.handleRevoke(c, req)
	case "status":
		s.handleStatus(c, req)
	case "watch":
		s.handleWatch(c, req)
	case "approve":
		s.handleApprove(c, req)
	case "deny":
		s.handleDeny(c, req)
	default:
		s.writeResp(c, Response{OK: false, Error: fmt.Sprintf("unknown op %q", req.Op)})
	}
}

func (s *Server) handleGrant(c net.Conn, req Request) {
	if req.WindowSeconds <= 0 {
		s.writeResp(c, Response{OK: false, Error: "window_seconds must be > 0"})
		return
	}
	home, mgr, err := s.homes.Pick(req.Home)
	if err != nil {
		s.writePickErr(c, err)
		return
	}
	if _, err := mgr.Grant(time.Duration(req.WindowSeconds)*time.Second, req.Strict, req.Region); err != nil {
		s.writeResp(c, Response{OK: false, Error: err.Error()})
		return
	}
	s.writeResp(c, s.stateOf(home, mgr))
}

// handleRevoke closes one window, or -- with no home named -- every
// window: the kill switch must never make the person say which.
func (s *Server) handleRevoke(c net.Conn, req Request) {
	if strings.TrimSpace(req.Home) == "" {
		var failures []error
		for _, mgr := range s.homes.Managers() {
			if err := mgr.Revoke(); err != nil {
				failures = append(failures, fmt.Errorf("%s: %w", mgr.Home(), err))
			}
		}
		if err := errors.Join(failures...); err != nil {
			s.writeResp(c, Response{OK: false, Error: err.Error(), Homes: s.homes.Statuses()})
			return
		}
		s.writeResp(c, Response{OK: true, Homes: s.homes.Statuses()})
		return
	}
	home, mgr, err := s.homes.Pick(req.Home)
	if err != nil {
		s.writePickErr(c, err)
		return
	}
	if err := mgr.Revoke(); err != nil {
		s.writeResp(c, Response{OK: false, Error: err.Error(), Home: home, Status: mgr.Snapshot()})
		return
	}
	s.writeResp(c, s.stateOf(home, mgr))
}

func (s *Server) handleStatus(c net.Conn, req Request) {
	if strings.TrimSpace(req.Home) != "" {
		home, mgr, err := s.homes.Pick(req.Home)
		if err != nil {
			s.writePickErr(c, err)
			return
		}
		s.writeResp(c, s.stateOf(home, mgr))
		return
	}
	resp := Response{OK: true, Homes: s.homes.Statuses()}
	if ids := s.homes.IDs(); len(ids) == 1 {
		home, mgr, _ := s.homes.Pick(ids[0])
		resp.Home, resp.Status = home, mgr.Snapshot()
	}
	s.writeResp(c, resp)
}

// handleApprove and handleDeny find the home holding the approval: ids
// are minted per request and unique, so the id alone says whose it is.
func (s *Server) handleApprove(c net.Conn, req Request) {
	s.resolveApproval(c, req, (*Manager).Approve)
}

func (s *Server) handleDeny(c net.Conn, req Request) {
	s.resolveApproval(c, req, (*Manager).Deny)
}

func (s *Server) resolveApproval(c net.Conn, req Request, resolve func(*Manager, string) error) {
	if req.ApprovalId == "" {
		s.writeResp(c, Response{OK: false, Error: "approval_id is required"})
		return
	}
	var lastErr error
	for _, mgr := range s.homes.Managers() {
		err := resolve(mgr, req.ApprovalId)
		if err == nil {
			s.writeResp(c, s.stateOf(mgr.Home(), mgr))
			return
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("consent: no pending approval with id %q (already resolved, revoked, or timed out)", req.ApprovalId)
	}
	s.writeResp(c, Response{OK: false, Error: lastErr.Error()})
}

func (s *Server) handleWatch(c net.Conn, req Request) {
	managers := s.homes.Managers()
	if strings.TrimSpace(req.Home) != "" {
		_, mgr, err := s.homes.Pick(req.Home)
		if err != nil {
			s.writePickErr(c, err)
			return
		}
		managers = []*Manager{mgr}
	}

	// One subscription per home, merged onto this connection. Each
	// forwarder exits when its subscription is cancelled or the watch
	// ends, whichever is first.
	merged := make(chan Event, 64)
	done := make(chan struct{})
	var forwarders sync.WaitGroup
	cancels := make([]func(), 0, len(managers))
	for _, mgr := range managers {
		ch, cancel := mgr.Subscribe()
		cancels = append(cancels, cancel)
		forwarders.Add(1)
		go func() {
			defer forwarders.Done()
			for ev := range ch {
				select {
				case merged <- ev:
				case <-done:
					return
				}
			}
		}()
	}
	defer func() {
		close(done)
		for _, cancel := range cancels {
			cancel()
		}
		forwarders.Wait()
	}()

	// Send the initial state first so the client can render its
	// dashboard before any events arrive. Includes any pending
	// approvals already in flight so a reconnecting TUI doesn't
	// drop on-screen modals.
	var pending []PendingApprovalInfo
	for _, mgr := range managers {
		pending = append(pending, mgr.PendingApprovals()...)
	}
	initial := Response{OK: true, Homes: s.homes.Statuses(), Pending: pending}
	if len(managers) == 1 {
		initial.Home, initial.Status = managers[0].Home(), managers[0].Snapshot()
	}
	if err := s.writeResp(c, initial); err != nil {
		return
	}
	// The client hanging up is noticed at once rather than at the next
	// event: a watch on a quiet worker would otherwise hold its
	// subscriptions until something happened to fail a write.
	gone := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, c)
		close(gone)
	}()
	enc := json.NewEncoder(c)
	for {
		select {
		case ev := <-merged:
			if err := enc.Encode(ev); err != nil {
				return
			}
		case <-gone:
			return
		}
	}
}

// stateOf is the OK response for an op on one home.
func (s *Server) stateOf(home string, mgr *Manager) Response {
	return Response{OK: true, Home: home, Status: mgr.Snapshot(), Homes: s.homes.Statuses()}
}

// writePickErr answers a request whose home could not be resolved, with
// the code and the list a client needs to phrase its own sentence.
func (s *Server) writePickErr(c net.Conn, err error) {
	resp := Response{OK: false, Error: err.Error(), Homes: s.homes.Statuses()}
	var pe *PickError
	if errors.As(err, &pe) {
		resp.Code = pe.Code
	}
	s.writeResp(c, resp)
}

func (s *Server) writeResp(c net.Conn, r Response) error {
	body, err := json.Marshal(r)
	if err != nil {
		return err
	}
	body = append(body, '\n')
	_, err = c.Write(body)
	return err
}

// ---------------------------------------------------------------------------
// Client helpers
// ---------------------------------------------------------------------------

// Client is the thin sync-RPC client the `worker consent ...`
// subcommand uses to talk to a running worker. Each call opens a
// fresh connection (the protocol is one-shot for non-WATCH ops).
type Client struct {
	Path    string
	Timeout time.Duration
	// Home is the cluster every request names (Request.Home); empty
	// means what Request.Home's empty means for each op.
	Home string
}

// DefaultClient returns a Client wired to DefaultSocketPath() with
// a 2-second dial timeout.
func DefaultClient() *Client {
	return &Client{Path: DefaultSocketPath(), Timeout: 2 * time.Second}
}

// Grant opens a consent window of the requested duration. Strict
// enables the per-action approval gate on the high-risk subset
// (workerComputer.key_type + workerComputer.mouse_click); see
// Manager.AllowsAt.
//
// region is the optional strict-mode in-region exemption rect
// (memql-cockpit#131); pass nil for none. It's ignored unless
// strict is true.
func (c *Client) Grant(window time.Duration, strict bool, region *Region) (Response, error) {
	if window <= 0 {
		return Response{}, errors.New("client: window must be positive")
	}
	return c.exec(Request{
		Op:            "grant",
		WindowSeconds: int(window.Seconds()),
		Strict:        strict,
		Region:        region,
		Home:          c.Home,
	})
}

// Revoke closes the active window of c.Home, or every window when c.Home
// is empty.
func (c *Client) Revoke() (Response, error) { return c.exec(Request{Op: "revoke", Home: c.Home}) }

// Approve resolves a strict-mode pending approval as ALLOW. The id
// comes from an EventApprovalRequested broadcast on the Watch
// stream. The worker side unblocks the dispatcher and the call
// proceeds.
func (c *Client) Approve(id string) (Response, error) {
	if id == "" {
		return Response{}, errors.New("client: approval id is required")
	}
	return c.exec(Request{Op: "approve", ApprovalId: id})
}

// Deny resolves a strict-mode pending approval as DENY. The
// dispatcher's blocked call returns with a typed `consent_required`
// failure carrying the deny reason.
func (c *Client) Deny(id string) (Response, error) {
	if id == "" {
		return Response{}, errors.New("client: approval id is required")
	}
	return c.exec(Request{Op: "deny", ApprovalId: id})
}

// Status returns the consent state: of c.Home, or of every home.
func (c *Client) Status() (Response, error) { return c.exec(Request{Op: "status", Home: c.Home}) }

// Watch opens a long-lived connection and yields each event /
// status update to the supplied callback. Returns when the
// connection is closed (either side).
func (c *Client) Watch(ctx context.Context, onEvent func([]byte)) error {
	conn, err := c.dial()
	if err != nil {
		return err
	}
	defer conn.Close()
	// Close the connection when ctx fires so the read loop exits.
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()
	body, err := json.Marshal(Request{Op: "watch", Home: c.Home})
	if err != nil {
		return err
	}
	body = append(body, '\n')
	if _, err := conn.Write(body); err != nil {
		return err
	}
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 16*1024), 1<<20)
	for scanner.Scan() {
		onEvent(scanner.Bytes())
	}
	return scanner.Err()
}

func (c *Client) exec(req Request) (Response, error) {
	conn, err := c.dial()
	if err != nil {
		return Response{}, err
	}
	defer conn.Close()
	body, err := json.Marshal(req)
	if err != nil {
		return Response{}, err
	}
	body = append(body, '\n')
	if _, err := conn.Write(body); err != nil {
		return Response{}, err
	}
	r := bufio.NewReader(conn)
	line, err := r.ReadBytes('\n')
	if err != nil && err != io.EOF {
		return Response{}, err
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return Response{}, fmt.Errorf("decode response: %w (raw=%q)", err, string(line))
	}
	return resp, nil
}

func (c *Client) dial() (net.Conn, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	conn, err := net.DialTimeout("unix", c.Path, timeout)
	if err != nil {
		return nil, fmt.Errorf("consent client: dial %s: %w (is the worker running?)", c.Path, err)
	}
	return conn, nil
}
