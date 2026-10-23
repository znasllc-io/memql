package consent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestHomes_AWindowForOneClusterAdmitsNothingForAnother
// (memql-cockpit#433). One consent window used to answer for every
// cluster the machine served: opened while working with cluster A, it
// admitted exec and fs_write dispatched by cluster B.
func TestHomes_AWindowForOneClusterAdmitsNothingForAnother(t *testing.T) {
	homes := NewHomes()
	a, b := homes.For("a"), homes.For("b")
	if _, err := a.Grant(time.Hour, false, nil); err != nil {
		t.Fatal(err)
	}
	if dec := a.Allows("workerHost", "exec"); !dec.Allowed {
		t.Fatalf("a's own window must admit a's call: %s", dec.Reason)
	}
	if dec := b.Allows("workerHost", "exec"); dec.Allowed {
		t.Fatal("a window opened for a must admit nothing b dispatches")
	}
	if homes.For("a") != a {
		t.Fatal("For must return the same Manager for the same home")
	}
	if got := strings.Join(homes.IDs(), ","); got != "a,b" {
		t.Fatalf("IDs = %q, want registration order", got)
	}
}

// Pick resolves the home a request is for, and never guesses between
// two.
func TestHomes_PickNeverGuessesBetweenTwo(t *testing.T) {
	homes := NewHomes()
	if _, _, err := homes.Pick(""); !isPick(err, CodeNoHomes) {
		t.Fatalf("no homes: %v, want %s", err, CodeNoHomes)
	}
	homes.For("only")
	if id, _, err := homes.Pick(""); err != nil || id != "only" {
		t.Fatalf("one home: (%q, %v), want it picked", id, err)
	}
	homes.For("second")
	if _, _, err := homes.Pick(""); !isPick(err, CodeHomeRequired) {
		t.Fatalf("two homes, none named: %v, want %s", err, CodeHomeRequired)
	}
	if id, _, err := homes.Pick("second"); err != nil || id != "second" {
		t.Fatalf("named: (%q, %v)", id, err)
	}
	var pe *PickError
	if _, _, err := homes.Pick("nope"); !errors.As(err, &pe) || pe.Code != CodeUnknownHome || pe.Asked != "nope" {
		t.Fatalf("unknown: %v", err)
	}
}

func isPick(err error, code string) bool {
	var pe *PickError
	return errors.As(err, &pe) && pe.Code == code
}

// A refusal suggests the command that would work. On a machine serving
// several clusters that command names this one -- a grant without a
// cluster is refused there, and a suggestion that fails is worse than
// none. On a machine serving one, it stays the short form.
func TestManager_RefusalNamesTheClusterWhenThereAreSeveral(t *testing.T) {
	homes := NewHomes()
	only := homes.For("only")
	if r := only.Allows("workerHost", "exec").Reason; strings.Contains(r, "--cluster") {
		t.Fatalf("one cluster: the hint must stay short, got %q", r)
	}
	homes.For("other")
	if r := only.Allows("workerHost", "exec").Reason; !strings.Contains(r, "`memql worker consent grant --window=<duration> --cluster only`") {
		t.Fatalf("two clusters: the hint must name this one, got %q", r)
	}
}

// Every event names its home, so a watcher of several can tell them
// apart.
func TestHomes_EventsCarryTheirHome(t *testing.T) {
	homes := NewHomes()
	m := homes.For("prod")
	ch, cancel := m.Subscribe()
	defer cancel()
	if _, err := m.Grant(time.Minute, false, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-ch:
		if ev.Home != "prod" || ev.Kind != EventGranted {
			t.Fatalf("event = %+v, want a granted event for prod", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event")
	}
}

// twoHomeServer is a socket over homes "local" and "production".
func twoHomeServer(t *testing.T) (*Homes, string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ws")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "w.sock")
	homes := NewHomes()
	homes.For("local")
	homes.For("production")
	srv := NewServer(homes, path, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := srv.Listen(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	return homes, path
}

func TestSocket_AGrantMustNameOneOfSeveralClusters(t *testing.T) {
	homes, path := twoHomeServer(t)

	resp, err := (&Client{Path: path, Timeout: time.Second}).Grant(time.Hour, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.OK || resp.Code != CodeHomeRequired || len(resp.Homes) != 2 {
		t.Fatalf("an unnamed grant on two clusters = %+v, want refused with %s and both homes listed", resp, CodeHomeRequired)
	}
	for _, m := range homes.Managers() {
		if m.Snapshot().Granted {
			t.Fatalf("a refused grant opened %s's window", m.Home())
		}
	}

	resp, err = (&Client{Path: path, Timeout: time.Second, Home: "production"}).Grant(time.Hour, true, nil)
	if err != nil || !resp.OK || resp.Home != "production" || !resp.Status.Granted || !resp.Status.Strict {
		t.Fatalf("a named grant = %+v, %v", resp, err)
	}
	status, err := (&Client{Path: path, Timeout: time.Second}).Status()
	if err != nil || !status.OK || len(status.Homes) != 2 {
		t.Fatalf("status = %+v, %v", status, err)
	}
	if status.Homes[0].Home != "local" || status.Homes[0].Status.Granted ||
		status.Homes[1].Home != "production" || !status.Homes[1].Status.Granted {
		t.Fatalf("status homes = %+v, want only production granted", status.Homes)
	}

	resp, _ = (&Client{Path: path, Timeout: time.Second, Home: "staging"}).Grant(time.Hour, false, nil)
	if resp.OK || resp.Code != CodeUnknownHome {
		t.Fatalf("an unknown cluster = %+v, want %s", resp, CodeUnknownHome)
	}
}

// The kill switch never asks which: an unnamed revoke closes every
// window; a named one closes only that one.
func TestSocket_AnUnnamedRevokeClosesEveryWindow(t *testing.T) {
	homes, path := twoHomeServer(t)
	for _, m := range homes.Managers() {
		if _, err := m.Grant(time.Hour, false, nil); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := (&Client{Path: path, Timeout: time.Second, Home: "local"}).Revoke()
	if err != nil || !resp.OK {
		t.Fatalf("named revoke = %+v, %v", resp, err)
	}
	_, prod, _ := homes.Pick("production")
	if !prod.Snapshot().Granted {
		t.Fatal("revoking local must leave production's window open")
	}
	resp, err = (&Client{Path: path, Timeout: time.Second}).Revoke()
	if err != nil || !resp.OK {
		t.Fatalf("unnamed revoke = %+v, %v", resp, err)
	}
	for _, m := range homes.Managers() {
		if m.Snapshot().Granted {
			t.Fatalf("an unnamed revoke left %s's window open", m.Home())
		}
	}
}

// A watch names each event's home, and a named watch sees only its own.
func TestSocket_WatchCarriesAndFiltersByHome(t *testing.T) {
	homes, path := twoHomeServer(t)
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(`{"op":"watch","home":"production"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(conn)
	if _, err := r.ReadBytes('\n'); err != nil { // the initial status
		t.Fatal(err)
	}
	_, local, _ := homes.Pick("local")
	_, prod, _ := homes.Pick("production")
	if _, err := local.Grant(time.Minute, false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := prod.Grant(time.Minute, false, nil); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	line, err := r.ReadBytes('\n')
	if err != nil {
		t.Fatalf("no event: %v", err)
	}
	var ev Event
	if err := json.Unmarshal(line, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Home != "production" || ev.Kind != EventGranted {
		t.Fatalf("event = %+v: a watch on production must see production's grant and not local's", ev)
	}
}

// Approvals are found by id whichever home holds them.
func TestSocket_ApproveFindsTheHomeHoldingTheApproval(t *testing.T) {
	homes, path := twoHomeServer(t)
	_, prod, _ := homes.Pick("production")
	prod.SetApprovalTimeout(5 * time.Second)
	if _, err := prod.Grant(time.Hour, true, nil); err != nil {
		t.Fatal(err)
	}
	ch, cancel := prod.Subscribe()
	defer cancel()
	decided := make(chan Decision, 1)
	go func() { decided <- prod.Allows("workerComputer", "key_type") }()
	var id string
	for id == "" {
		select {
		case ev := <-ch:
			if ev.Kind == EventApprovalRequested {
				id = ev.ApprovalId
			}
		case <-time.After(3 * time.Second):
			t.Fatal("no approval was requested")
		}
	}
	resp, err := (&Client{Path: path, Timeout: time.Second}).Approve(id)
	if err != nil || !resp.OK || resp.Home != "production" {
		t.Fatalf("approve = %+v, %v", resp, err)
	}
	if dec := <-decided; !dec.Allowed {
		t.Fatalf("approved call was denied: %s", dec.Reason)
	}
}

// A pending approval names its cluster, and a watch scoped to one
// cluster is handed only that cluster's pending approvals.
func TestSocket_PendingApprovalsCarryTheirHome(t *testing.T) {
	homes, path := twoHomeServer(t)
	_, prod, _ := homes.Pick("production")
	prod.SetApprovalTimeout(5 * time.Second)
	if _, err := prod.Grant(time.Hour, true, nil); err != nil {
		t.Fatal(err)
	}
	ch, cancel := prod.Subscribe()
	defer cancel()
	done := make(chan Decision, 1)
	go func() { done <- prod.Allows("workerComputer", "key_type") }()
	var id string
	for id == "" {
		select {
		case ev := <-ch:
			if ev.Kind == EventApprovalRequested {
				id = ev.ApprovalId
			}
		case <-time.After(3 * time.Second):
			t.Fatal("no approval was requested")
		}
	}
	if pending := homes.PendingApprovals(); len(pending) != 1 || pending[0].Home != "production" {
		t.Fatalf("pending = %+v, want one approval naming production", pending)
	}

	initialFor := func(home string) Response {
		conn, err := net.DialTimeout("unix", path, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.Write([]byte(`{"op":"watch","home":"` + home + `"}` + "\n")); err != nil {
			t.Fatal(err)
		}
		line, err := bufio.NewReader(conn).ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var resp Response
		if err := json.Unmarshal(line, &resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if got := initialFor("production").Pending; len(got) != 1 || got[0].Id != id {
		t.Fatalf("production's watch pending = %+v, want its approval", got)
	}
	if got := initialFor("local").Pending; len(got) != 0 {
		t.Fatalf("local's watch was handed another cluster's approvals: %+v", got)
	}
	if err := prod.Deny(id); err != nil {
		t.Fatal(err)
	}
	<-done
}
