package pipelinerun

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/pipelines"
)

func TestChannelConfigurationIsOwnerScopedAndSendsNothing(t *testing.T) {
	h := newHarness(t)
	ctx := personCtx(ownerID)
	req := ChannelSaveRequest{Name: "releases", Kind: "email", Recipients: []string{"owner@example.test", "OWNER@example.test"}}
	first, err := h.integ.SaveChannel(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Recipients) != 1 || first.Status != channelActive {
		t.Fatalf("channel: %+v", first)
	}
	second, err := h.integ.SaveChannel(ctx, req)
	if err != nil || first.ID != second.ID || len(h.store.channelCreates) != 1 {
		t.Fatalf("upsert duplicated: %+v %v", second, err)
	}
	foreign, err := h.integ.SaveChannel(personCtx(otherID), req)
	if err != nil || foreign.ID == first.ID {
		t.Fatalf("owners shared a channel: %+v %v", foreign, err)
	}
	if _, err = h.integ.ArchiveChannel(ctx, "releases"); err != nil {
		t.Fatal(err)
	}
	stored, _ := h.store.channel(first.ID)
	if stored.Status != channelArchived {
		t.Fatalf("not archived: %+v", stored)
	}
	stored, err = h.integ.SaveChannel(ctx, ChannelSaveRequest{Name: "releases", Kind: "discord", SecretRef: "DISCORD_RELEASES"})
	if err != nil || stored.Status != channelActive || len(stored.Recipients) != 0 || stored.ID != first.ID {
		t.Fatalf("resume/kind change: %+v %v", stored, err)
	}
	if len(h.store.staged) != 0 {
		t.Fatal("saving configuration sent a message")
	}
}

func TestChannelConfigurationRejectsInvalidDestinationsBeforeWriting(t *testing.T) {
	bad := []ChannelSaveRequest{
		{Name: "../escape", Kind: "email", Recipients: []string{"a@example.test"}},
		{Name: strings.Repeat("a", 41), Kind: "email", Recipients: []string{"a@example.test"}},
		{Name: "releases", Kind: "sms"},
		{Name: "releases", Kind: "discord", SecretRef: "MEMQL_PRIVATE"},
		{Name: "releases", Kind: "discord", SecretRef: "https://discord.com/api/webhooks/1/secret"},
		{Name: "releases", Kind: "discord", SecretRef: "HOOK", Recipients: []string{"a@example.test"}},
		{Name: "releases", Kind: "email", SecretRef: "HOOK", Recipients: []string{"a@example.test"}},
		{Name: "releases", Kind: "email"},
		{Name: "releases", Kind: "email", Recipients: []string{"Name <a@example.test>"}},
		{Name: "releases", Kind: "email", Recipients: []string{"a@example.test\nBcc: b@example.test"}},
		{Name: "releases", Kind: "email", Recipients: []string{"a@example.test", ""}},
		{Name: "releases", Kind: "email", Recipients: slices.Repeat([]string{"a@example.test"}, 21)},
	}
	for n, req := range bad {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			h := newHarness(t)
			if _, err := h.integ.SaveChannel(personCtx(ownerID), req); err == nil {
				t.Fatal("invalid destination accepted")
			}
			if len(h.store.channelCreates)+len(h.store.channelUpdates)+len(h.store.staged) != 0 {
				t.Fatal("invalid configuration wrote state")
			}
		})
	}
	h := newHarness(t)
	if _, err := h.integ.SaveChannel(context.Background(), ChannelSaveRequest{}); !errors.Is(err, ErrNoCaller) {
		t.Fatalf("anonymous: %v", err)
	}
	for _, raw := range []any{"a@example.test", []any{"a@example.test", 42}, []any{"a@example.test", ""}} {
		if _, err := h.integ.handleChannelSave(personCtx(ownerID), map[string]any{"name": "releases", "kind": "email", "recipients": raw}, 0); err == nil {
			t.Fatalf("invalid wire list accepted: %T", raw)
		}
	}
}

func TestChannelPermissionPreservesOtherGrantsAndRefusesForeignRows(t *testing.T) {
	h := newHarness(t)
	p := Pipeline{ID: "owned-pipeline", OwnerUserID: ownerID, Repository: "acme/shop", Name: "shop", Status: PipelineActive, ChannelIDs: []string{"previous"}}
	h.store.addPipeline(p)
	channel, err := h.integ.SaveChannel(personCtx(ownerID), ChannelSaveRequest{Name: "releases", Kind: "discord", SecretRef: "HOOK"})
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 2; n++ {
		ids, err := h.integ.AllowChannel(personCtx(ownerID), p.ID, "releases", true)
		if err != nil || len(ids) != 2 || !slices.Contains(ids, "previous") || !slices.Contains(ids, channel.ID) {
			t.Fatalf("grant: %v %v", ids, err)
		}
	}
	if _, err = h.integ.AllowChannel(personCtx(otherID), p.ID, "releases", true); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("foreign pipeline: %v", err)
	}
	if _, err = h.integ.ArchiveChannel(personCtx(otherID), "releases"); err == nil {
		t.Fatal("stranger archived the channel")
	}
	if _, err = h.integ.ArchiveChannel(personCtx(ownerID), "releases"); err != nil {
		t.Fatal(err)
	}
	if _, err = h.integ.AllowChannel(personCtx(ownerID), p.ID, "releases", true); err == nil {
		t.Fatal("granted archived channel")
	}
	ids, err := h.integ.AllowChannel(personCtx(ownerID), p.ID, "releases", false)
	if err != nil || !slices.Equal(ids, []string{"previous"}) {
		t.Fatalf("revoke: %v %v", ids, err)
	}
}

func TestChannelSavesAndGrantsSerializeAcrossReplicas(t *testing.T) {
	h := newHarness(t)
	// Different integrations share only their durable store and cross-node gate.
	other := New(h.integ.snapshot())
	p := Pipeline{ID: "owned-pipeline", OwnerUserID: ownerID, Repository: "acme/shop", Name: "shop", Status: PipelineActive}
	h.store.addPipeline(p)
	ctx := personCtx(ownerID)
	var wg sync.WaitGroup
	for n := 0; n < 12; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			i := h.integ
			if n%2 != 0 {
				i = other
			}
			name := fmt.Sprintf("channel-%d", n%3)
			if _, err := i.SaveChannel(ctx, ChannelSaveRequest{Name: name, Kind: "discord", SecretRef: "HOOK"}); err != nil {
				t.Error(err)
				return
			}
			if _, err := i.AllowChannel(ctx, p.ID, name, true); err != nil {
				t.Error(err)
			}
		}(n)
	}
	wg.Wait()
	channels, err := h.store.ChannelsForOwner(ctx)
	if err != nil || len(channels) != 3 {
		t.Fatalf("duplicate channels: %d %v", len(channels), err)
	}
	current, err := h.store.PipelineForOwner(ctx, p.ID)
	if err != nil || len(current.ChannelIDs) != 3 {
		t.Fatalf("lost concurrent grants: %+v %v", current, err)
	}
	for _, name := range []string{"a", "a-b", "0", strings.Repeat("a", 40)} {
		if !pipelines.ValidName(name) {
			t.Fatal(name)
		}
	}
}

func TestChannelRevocationOnAnotherReplicaOverridesAClaimedRun(t *testing.T) {
	dh := newNotifyHarness(t, notifyManifest)
	// Freeze the original driver view, then revoke through a separate integration.
	dr := &runDriver{p: dh.p, d: dh.integ.snapshot()}
	other := New(dh.integ.snapshot())
	if _, err := other.AllowChannel(personCtx(dh.p.OwnerUserID), dh.p.ID, "releases", false); err != nil {
		t.Fatal(err)
	}
	if len(dr.p.ChannelIDs) == 0 {
		t.Fatal("test lost the original driver's granted snapshot")
	}
	if _, failure := dr.targetOf(context.Background(), pipelines.Step{Channel: "releases"}); failure == nil || failure.Code != pipelines.CodeChannelNotAllowed {
		t.Fatalf("stale run still admitted revoked channel: %+v", failure)
	}
	if len(dh.store.stagedNotifications()) != 0 || dh.asked.count("DISCORD_RELEASES") != 0 {
		t.Fatal("revocation resolved a secret or staged a delivery")
	}
}
