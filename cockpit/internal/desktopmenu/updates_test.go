package desktopmenu

import (
	"bytes"
	"context"
	"errors"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func availableUpdate() updateReply {
	return updateReply{OK: true, CurrentVersion: "0.22.2+gabc", LatestVersion: "0.23.0", Available: true, CanInstall: true}
}

func TestUpdateClickPinsReleaseAndGuardsConcurrentOperations(t *testing.T) {
	s := updateState{}
	if _, ok := s.beginInstall(); ok {
		t.Fatal("installation allowed before a release was checked")
	}
	if !s.beginCheck() || s.beginCheck() {
		t.Fatal("check was not serialized")
	}
	s.checked(availableUpdate(), nil)
	p := s.presentation()
	if !p.badge || !p.canInstall || !strings.Contains(p.installLabel, "0.23.0") {
		t.Fatalf("release not reachable from tray: %+v", p)
	}
	version, ok := s.beginInstall()
	if !ok || version != "0.23.0" {
		t.Fatalf("click did not bind the displayed version: %q, %t", version, ok)
	}
	if _, ok := s.beginInstall(); ok || s.beginCheck() {
		t.Fatal("a second operation started while replacing the worker")
	}
	s.progressChanged(updateReply{Phase: "reconnecting", Message: "Waiting for saved servers…"})
	p = s.presentation()
	if p.canCheck || p.canInstall || p.showInstall || !p.badge || p.status != "Waiting for saved servers…" {
		t.Fatalf("pending operation gave illegal or misleading actions: %+v", p)
	}
	s.progressChanged(updateReply{Phase: "complete", Message: "Old update complete"})
	if s.presentation().status != "Waiting for saved servers…" {
		t.Fatal("an earlier receipt completed the current operation")
	}
}

func TestReceiptFailureDoesNotOfferToInstallAnAlreadyUpdatedWorker(t *testing.T) {
	s := updateState{}
	s.checked(availableUpdate(), nil)
	s.beginInstall()
	s.installed(updateReply{Updated: true, CurrentVersion: "0.23.0", WorkerReconnected: true}, errors.New("The update completed, but its receipt could not be saved"))
	p := s.presentation()
	if p.canInstall || p.badge || p.problem == "" || !strings.Contains(p.status, "updated") {
		t.Fatalf("lost successful replacement behind receipt failure: %+v", p)
	}
}

func TestInterruptedUpdateOffersRestartWithoutClaimingNewRelease(t *testing.T) {
	s := updateState{}
	s.checked(updateReply{OK: true, CurrentVersion: "1.1.0", LatestVersion: "1.1.0", RestartRequired: true, CanInstall: true}, nil)
	p := s.presentation()
	if !p.badge || !p.canInstall || p.installLabel != "Restart to finish update" {
		t.Fatalf("recovery action missing: %+v", p)
	}
	if target, ok := s.beginInstall(); !ok || target != "1.1.0" {
		t.Fatalf("recovery did not bind installed target: %s %t", target, ok)
	}
}

func TestFailedCheckRetainsKnownReleaseAndInstallFailureCanRetry(t *testing.T) {
	s := updateState{}
	s.checked(availableUpdate(), nil)
	s.beginCheck()
	s.checked(updateReply{}, errors.New("Release server unavailable. Try again."))
	p := s.presentation()
	if !p.badge || !p.canInstall || p.problem == "" || p.checkLabel != "Retry update check" {
		t.Fatalf("offline check erased available update or hid remedy: %+v", p)
	}
	if version, ok := s.beginInstall(); !ok || version != "0.23.0" {
		t.Fatal("known release could not be retried")
	}
	s.installed(updateReply{}, errors.New("Worker is busy. Try again when the task finishes."))
	p = s.presentation()
	if !p.canInstall || !p.canCheck || !p.badge || !strings.Contains(p.installLabel, "Retry update") || !strings.Contains(p.problem, "busy") {
		t.Fatalf("failure lost its update or retry: %+v", p)
	}
	s.beginInstall()
	s.installed(updateReply{OK: true, CurrentVersion: "0.23.0", Updated: true, WorkerReconnected: false}, nil)
	p = s.presentation()
	if p.badge || p.showInstall || !strings.Contains(p.status, "waiting for worker connection") {
		t.Fatalf("install falsely claimed a cluster connection: %+v", p)
	}
}

func TestUnsupportedInstallExplainsRemedyWithoutEnablingAction(t *testing.T) {
	r := availableUpdate()
	r.CanInstall = false
	r.Message = "This installation is managed by your administrator."
	s := updateState{}
	s.checked(r, nil)
	p := s.presentation()
	if !p.badge || p.showInstall || p.canInstall || p.problem != r.Message {
		t.Fatalf("unavailable installation action was offered: %+v", p)
	}
	if _, ok := s.beginInstall(); ok {
		t.Fatal("unsupported install was launched")
	}
}

func TestUpdateResultRequiresSuccessfulCommandAndValidEnvelope(t *testing.T) {
	for _, tc := range []struct {
		body string
		err  error
	}{
		{"not JSON", nil},
		{`{"ok":false,"error":"Checksum did not match"}`, nil},
		{`{"ok":true,"updated":true}`, errors.New("exit status 1")},
		{`{}`, nil},
	} {
		if _, err := decodeUpdate([]byte(tc.body), tc.err); err == nil {
			t.Fatalf("accepted failed update: %s", tc.body)
		}
	}
	result, err := decodeUpdate([]byte(`{"ok":true,"updated":true,"worker_reconnected":false,"current_version":"0.23.0"}`), nil)
	if err != nil || !result.Updated || result.WorkerReconnected {
		t.Fatalf("partial connection state lost: %+v, %v", result, err)
	}
}

func TestUpdateCommandsUseSelectedExecutableWithoutShellExpansion(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "memql with spaces")
	receipt := filepath.Join(root, "arguments")
	t.Setenv("MEMQL_MENU_UPDATE_ARGS", receipt)
	// Exercise the real subprocess adapter, including argument boundaries.
	script := "#!/bin/sh\nmain() { printf '%s\\n' \"$@\" > \"$MEMQL_MENU_UPDATE_ARGS\"; printf '%s\\n' '{\"ok\":true,\"available\":true,\"latest_version\":\"0.23.0\"}'; }\nmain \"$@\"\n"
	if err := os.WriteFile(executable, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := requestUpdate(ctx, executable, "--check"); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(receipt)
	if string(args) != "update\n--json\n--check\n" {
		t.Fatalf("wrong check invocation: %q", args)
	}
	if _, err := installUpdate(executable, "0.23.0"); err != nil {
		t.Fatal(err)
	}
	args, _ = os.ReadFile(receipt)
	if string(args) != "update\n--install\n--version=0.23.0\n--json\n" {
		t.Fatalf("installation did not pin release: %q", args)
	}
}

func TestUpdateBadgeRetainsMarkAndIsVisuallyDistinct(t *testing.T) {
	badged := updateIcon()
	if bytes.Equal(badged, icon) {
		t.Fatal("available update has no visual badge")
	}
	base, err := png.Decode(bytes.NewReader(icon))
	if err != nil {
		t.Fatal(err)
	}
	result, err := png.Decode(bytes.NewReader(badged))
	if err != nil || result.Bounds() != base.Bounds() {
		t.Fatalf("badge changed icon dimensions: %v", err)
	}
	for y := 0; y < 36; y++ {
		for x := 0; x < 64; x++ {
			br, bg, bb, ba := base.At(x, y).RGBA()
			rr, rg, rb, ra := result.At(x, y).RGBA()
			if br != rr || bg != rg || bb != rb || ba != ra {
				t.Fatalf("badge changed the mark at %d,%d", x, y)
			}
		}
	}
	if aboutVersion("0.23.0+g123", "computeruse") != "Version 0.23.0+g123 (computeruse)" {
		t.Fatal("About lost version provenance or installed variant")
	}
}
